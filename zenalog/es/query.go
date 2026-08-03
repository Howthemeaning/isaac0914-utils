package es

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Cond 的比较方式
const (
	OpEq     = "eq"     // 等值
	OpNe     = "ne"     // 不等（进 must_not）
	OpPrefix = "prefix" // 前缀
)

// aggTermsSize by-trace 聚合的 trace 组数上限，时间范围内超过则截断（对齐 zrbiz）
const aggTermsSize = 10000

// Cond 精确过滤条件。attrs.<key> 字段由本包内部改写为 attrs.<key>.keyword。
type Cond struct {
	Field string
	Op    string
	Value string
}

// QueryRequest 查询参数，flat 与 by-trace 共用。
type QueryRequest struct {
	Start    time.Time // 时间范围（含边界）
	End      time.Time
	From     int      // flat：行偏移；by-trace：组偏移
	Size     int      // flat：行数；by-trace：组数
	Query    string   // 关键词：text 字段 match_phrase，keyword 字段整值 term
	Conds    []Cond   // 精确过滤
	Excluded []string // operation 整组排除（must_not）
}

// TraceBucket by-trace 聚合的一组：同 traceId 的日志按时间升序。
type TraceBucket struct {
	TraceID   string
	StartTime time.Time // 组内最早一条的时间
	Docs      []Doc
}

// Query 查询侧。
type Query struct {
	c           *client
	searchIndex string
	attrKeys    []string
}

// NewQuery 返回查询侧实例，SearchIndex 缺省回填为 Index。
func NewQuery(cfg Config) *Query {
	searchIndex := cfg.SearchIndex
	if searchIndex == "" {
		searchIndex = cfg.Index
	}
	return &Query{c: newClient(cfg), searchIndex: searchIndex, attrKeys: cfg.AttrKeys}
}

// attrField attrs.<key> 的等值/前缀查询要打在 .keyword 子字段上，库内部改写
func attrField(field string) string {
	if strings.HasPrefix(field, "attrs.") {
		return field + ".keyword"
	}
	return field
}

func term(field, value string) map[string]any {
	return map[string]any{"term": map[string]any{field: value}}
}

func matchPhrase(field, value string) map[string]any {
	return map[string]any{"match_phrase": map[string]any{field: value}}
}

// boolQuery 组装 bool 查询：filter 时间范围与精确条件、must_not 不等与整组排除、
// must 关键词多字段匹配（keyword 字段整值 term、text 字段 match_phrase）。
func (q *Query) boolQuery(req QueryRequest) (map[string]any, error) {
	filter := []any{map[string]any{
		"range": map[string]any{"timestamp": map[string]any{
			"gte": req.Start.UTC().Format(TimeLayout),
			"lte": req.End.UTC().Format(TimeLayout),
		}},
	}}
	var mustNot []any
	for _, c := range req.Conds {
		field := attrField(c.Field)
		switch c.Op {
		case OpEq:
			filter = append(filter, term(field, c.Value))
		case OpNe:
			mustNot = append(mustNot, term(field, c.Value))
		case OpPrefix:
			filter = append(filter, map[string]any{"prefix": map[string]any{field: c.Value}})
		default:
			return nil, fmt.Errorf("es: unknown condition op %q on field %q", c.Op, c.Field)
		}
	}
	for _, op := range req.Excluded {
		mustNot = append(mustNot, term("operation", op))
	}

	b := map[string]any{"filter": filter}
	if len(mustNot) > 0 {
		b["must_not"] = mustNot
	}
	if req.Query != "" {
		should := []any{
			term("trace_id", req.Query),
			term("operator", req.Query),
			term("operation", req.Query),
			term("resource_path", req.Query),
			matchPhrase("message", req.Query),
			matchPhrase("diff", req.Query),
		}
		for _, k := range q.attrKeys {
			should = append(should, matchPhrase("attrs."+k, req.Query))
		}
		b["must"] = []any{map[string]any{"bool": map[string]any{
			"should":               should,
			"minimum_should_match": 1,
		}}}
	}
	return map[string]any{"bool": b}, nil
}

// search 发查询请求并校验 HTTP 状态，响应体交给调用方解析
func (q *Query) search(ctx context.Context, payload map[string]any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("es: marshal search: %w", err)
	}
	status, respBody, err := q.c.do(ctx, http.MethodPost, "/"+q.searchIndex+"/_search", body, "application/json")
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("es: search %s: http %d: %s", q.searchIndex, status, snippet(respBody))
	}
	return respBody, nil
}

// Search flat 查询：bool 过滤 + from/size 按行分页 + ts_nanos tiebreaker，返回
// 当前页与精确总数（ES 6 的 hits.total 恒精确，不发 track_total_hits——ES 6 收到
// 未知参数解析报错）。
func (q *Query) Search(ctx context.Context, req QueryRequest) ([]Doc, int64, error) {
	bq, err := q.boolQuery(req)
	if err != nil {
		return nil, 0, err
	}
	respBody, err := q.search(ctx, map[string]any{
		"from": req.From,
		"size": req.Size,
		"sort": []any{
			map[string]any{"timestamp": "desc"},
			map[string]any{"ts_nanos": "desc"},
		},
		"query": bq,
	})
	if err != nil {
		return nil, 0, err
	}
	var resp struct {
		Hits struct {
			Total int64 `json:"total"`
			Hits  []struct {
				Source Doc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, 0, fmt.Errorf("es: decode search response: %w", err)
	}
	docs := make([]Doc, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		docs = append(docs, h.Source)
	}
	return docs, resp.Hits.Total, nil
}

// AggregateByTrace by-trace 查询：terms 按 trace_id 分组 + top_hits 取组内明细
// （时间升序，带 ts_nanos tiebreaker）+ bucket_sort 按组内最早时间倒序分页。
// top_hits size = maxInnerResultWindow，与 EnsureIndex 抬的窗口一致。
func (q *Query) AggregateByTrace(ctx context.Context, req QueryRequest) ([]TraceBucket, error) {
	bq, err := q.boolQuery(req)
	if err != nil {
		return nil, err
	}
	respBody, err := q.search(ctx, map[string]any{
		"size":  0,
		"query": bq,
		"aggs": map[string]any{
			"by_trace": map[string]any{
				"terms": map[string]any{"field": "trace_id", "size": aggTermsSize},
				"aggs": map[string]any{
					"first_ts": map[string]any{"min": map[string]any{"field": "timestamp"}},
					"logs": map[string]any{"top_hits": map[string]any{
						"size": maxInnerResultWindow,
						"sort": []any{
							map[string]any{"timestamp": "asc"},
							map[string]any{"ts_nanos": "asc"},
						},
					}},
					"page": map[string]any{"bucket_sort": map[string]any{
						"sort": []any{map[string]any{"first_ts": "desc"}},
						"from": req.From,
						"size": req.Size,
					}},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Aggregations struct {
			ByTrace struct {
				Buckets []struct {
					Key     string `json:"key"`
					FirstTs struct {
						Value float64 `json:"value"`
					} `json:"first_ts"`
					Logs struct {
						Hits struct {
							Hits []struct {
								Source Doc `json:"_source"`
							} `json:"hits"`
						} `json:"hits"`
					} `json:"logs"`
				} `json:"buckets"`
			} `json:"by_trace"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("es: decode aggregate response: %w", err)
	}
	buckets := make([]TraceBucket, 0, len(resp.Aggregations.ByTrace.Buckets))
	for _, b := range resp.Aggregations.ByTrace.Buckets {
		docs := make([]Doc, 0, len(b.Logs.Hits.Hits))
		for _, h := range b.Logs.Hits.Hits {
			docs = append(docs, h.Source)
		}
		buckets = append(buckets, TraceBucket{
			TraceID:   b.Key,
			StartTime: time.UnixMilli(int64(b.FirstTs.Value)).UTC(), // min 聚合对 date 返回 epoch 毫秒
			Docs:      docs,
		})
	}
	return buckets, nil
}

// CheckSearchMapping 拉 SearchIndex 的 _mapping，逐索引与 AttrKeys 比对，返回
// 索引名 → 缺失的 attr 键；pattern 匹配不到任何索引返回空 map 不报错（目标索引
// 可能还没建）。主包 New 在 SearchIndex != Index 时调它记 warn——跨索引查询要求
// 参与方 AttrKeys 一致，缺字段的索引在 attrs 筛选下会被静默排除。
func (q *Query) CheckSearchMapping(ctx context.Context) (map[string][]string, error) {
	status, respBody, err := q.c.do(ctx, http.MethodGet, "/"+q.searchIndex+"/_mapping", nil, "")
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return map[string][]string{}, nil
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("es: get mapping %s: http %d: %s", q.searchIndex, status, snippet(respBody))
	}
	// {index: {mappings: {<type>: {properties: {attrs: {properties: {key: ...}}}}}}}
	var resp map[string]struct {
		Mappings map[string]struct {
			Properties map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("es: decode mapping response: %w", err)
	}
	result := map[string][]string{}
	for index, m := range resp {
		present := map[string]bool{}
		for _, typ := range m.Mappings { // ES 6 typed mapping，单 type 但名字不定
			if attrs, ok := typ.Properties["attrs"]; ok {
				for key := range attrs.Properties {
					present[key] = true
				}
			}
		}
		var missing []string
		for _, k := range q.attrKeys {
			if !present[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			result[index] = missing
		}
	}
	return result, nil
}
