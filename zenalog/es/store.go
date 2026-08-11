package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// maxInnerResultWindow top_hits 组内明细的 from+size 上限。ES 默认 100
// （IndexScope + Dynamic），不抬 by-trace 的 top_hits size 2000 一查就被拒。
const maxInnerResultWindow = 2000

// Store 写入侧：ensure index 与批量写入。
type Store struct {
	c        *client
	index    string
	attrKeys []string
}

// NewStore 校验必填配置（一次报全），返回写入侧实例。
func NewStore(cfg Config) (*Store, error) {
	var errs []error
	if len(cfg.Addresses) == 0 {
		errs = append(errs, errors.New("es: addresses required"))
	}
	if cfg.Index == "" {
		errs = append(errs, errors.New("es: index required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Store{c: newClient(cfg), index: cfg.Index, attrKeys: cfg.AttrKeys}, nil
}

// EnsureIndex 索引不存在则建（PUT + settings + mappings）；已存在也要补——
// mapping：PUT /{index}/_mapping 幂等提交全量 properties，把 AttrKeys 缺的
// attrs 子字段补进去（AttrKeys 是会增长的，见设计 6.1 第 10 条）；
// settings：PUT /{index}/_settings 补 index.max_inner_result_window（动态设置）。
// 类型冲突 ES 会拒，返回 error 让调用方在启动期失败。number_of_shards 是静态
// 设置，只在建索引时生效，已存在的索引维持原分片数。
func (s *Store) EnsureIndex(ctx context.Context) error {
	// 先落 index template：ES 的 action.auto_create_index 默认开着，索引一旦被误删
	// 或被别的写入抢先创建，ES 会按动态 mapping 自动建出一份全 text 的索引，之后
	// 本函数补 keyword 就撞类型冲突、服务永久起不来（实测踩过）。有了 template，
	// 自动创建出来的索引也带正确 mapping；顺带让运维做 rollover 时不用手抄 mapping。
	if err := s.ensureTemplate(ctx); err != nil {
		return err
	}

	status, body, err := s.c.do(ctx, http.MethodHead, "/"+s.index, nil, "")
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return s.backfill(ctx)
	case http.StatusNotFound:
		return s.create(ctx)
	default:
		return fmt.Errorf("es: check index %s: http %d: %s", s.index, status, snippet(body))
	}
}

// ensureTemplate 幂等写入与写入索引同名（加 -template 后缀）的 index template。
// index_patterns 同时覆盖 {index} 和 {index}-* ，后者是给运维 rollover 留的。
func (s *Store) ensureTemplate(ctx context.Context) error {
	payload := map[string]any{
		"index_patterns": []string{s.index, s.index + "-*"},
		"settings": map[string]any{
			"number_of_shards":              1,
			"index.max_inner_result_window": maxInnerResultWindow,
		},
		// typeless：ES 7 起移除了 mapping type，properties 直接挂 mappings 下。
		// 套 "_doc" 会被 ES 7 拒：illegal_argument_exception「cannot be nested under
		// a type [_doc] unless include_type_name is set to true」，而那个兼容开关在
		// ES 8 已删除，所以不走它
		"mappings": map[string]any{"properties": properties(s.attrKeys)},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("es: marshal index template: %w", err)
	}
	status, respBody, err := s.c.do(ctx, http.MethodPut, "/_template/"+s.index+"-template", body, "application/json")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("es: put index template %s: http %d: %s", s.index, status, snippet(respBody))
	}
	return nil
}

// create 建索引：settings + mapping 一次带全
func (s *Store) create(ctx context.Context) error {
	payload := map[string]any{
		"settings": map[string]any{
			"number_of_shards":              1,
			"index.max_inner_result_window": maxInnerResultWindow,
		},
		// typeless：ES 7 起移除了 mapping type，properties 直接挂 mappings 下。
		// 套 "_doc" 会被 ES 7 拒：illegal_argument_exception「cannot be nested under
		// a type [_doc] unless include_type_name is set to true」，而那个兼容开关在
		// ES 8 已删除，所以不走它
		"mappings": map[string]any{"properties": properties(s.attrKeys)},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("es: marshal create index: %w", err)
	}
	status, respBody, err := s.c.do(ctx, http.MethodPut, "/"+s.index, body, "application/json")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("es: create index %s: http %d: %s", s.index, status, snippet(respBody))
	}
	return nil
}

// backfill 索引已存在：补动态 settings 与 mapping 差集
func (s *Store) backfill(ctx context.Context) error {
	settings, err := json.Marshal(map[string]any{"index.max_inner_result_window": maxInnerResultWindow})
	if err != nil {
		return fmt.Errorf("es: marshal settings: %w", err)
	}
	status, respBody, err := s.c.do(ctx, http.MethodPut, "/"+s.index+"/_settings", settings, "application/json")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("es: put settings %s: http %d: %s", s.index, status, snippet(respBody))
	}

	mapping, err := json.Marshal(map[string]any{"properties": properties(s.attrKeys)})
	if err != nil {
		return fmt.Errorf("es: marshal mapping: %w", err)
	}
	status, respBody, err = s.c.do(ctx, http.MethodPut, "/"+s.index+"/_mapping", mapping, "application/json")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		// 类型冲突通常意味着这个索引不是本库建的（ES auto_create_index 按动态 mapping
		// 建过一份全 text 的），补 mapping 就永远失败、服务起不来。把处置办法写进
		// 错误里，别让运维只拿到一句 ES 原文
		hint := ""
		if bytes.Contains(respBody, []byte("of different type")) {
			hint = fmt.Sprintf(" (index %s was likely auto-created by ES with dynamic mapping;"+
				" reindex into a fresh index or delete it and restart to let the template apply)", s.index)
		}
		return fmt.Errorf("es: put mapping %s: http %d: %s%s", s.index, status, snippet(respBody), hint)
	}
	return nil
}

// properties Doc 的全量 mapping properties。attrs 按白名单显式建键、dynamic
// strict 拒收未登记 key；keyword 一律 ignore_above 256，防 immense term 整条拒收。
func properties(attrKeys []string) map[string]any {
	kw := map[string]any{"type": "keyword"}
	kwCapped := map[string]any{"type": "keyword", "ignore_above": 256}
	text := map[string]any{"type": "text"}

	attrProps := map[string]any{}
	for _, k := range attrKeys {
		attrProps[k] = map[string]any{
			"type":   "text",
			"fields": map[string]any{"keyword": kwCapped},
		}
	}
	return map[string]any{
		"timestamp":     map[string]any{"type": "date"},
		"ts_nanos":      map[string]any{"type": "long"},
		"trace_id":      kw,
		"operator":      kw,
		"resource_type": kw,
		"instance_id":   kw,
		"resource_path": kw,
		"operation":     kw,
		"status":        kw,
		"message":       text,
		"diff":          text,
		"changes": map[string]any{
			"properties": map[string]any{"field": kwCapped, "from": kwCapped, "to": kwCapped},
		},
		"attrs": map[string]any{"dynamic": "strict", "properties": attrProps},
	}
}

// bulkResponse _bulk 响应体，条目级失败藏在 items 里
type bulkResponse struct {
	Errors bool                            `json:"errors"`
	Items  []map[string]bulkResponseResult `json:"items"`
}

type bulkResponseResult struct {
	Status int `json:"status"`
	Error  *struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"error"`
}

// Bulk 批量写入。必须解析响应体判定成败：ES 的 _bulk 有条目被拒也返回 HTTP 200，
// errors=true 时逐条把 error.type/error.reason 记进日志（strict_dynamic_mapping_exception
// 就是 attrs 的 key 没登记的信号），并返回带失败条数的 error。
func (s *Store) Bulk(ctx context.Context, docs []Doc) error {
	if len(docs) == 0 {
		return nil
	}
	// 不做 HTML 转义：diff 里的 "->" 要原样进 _source，这是给人读的
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for i := range docs {
		buf.WriteString(`{"index":{}}` + "\n")
		if err := enc.Encode(&docs[i]); err != nil { // Encode 自带行尾 \n，正好是 ndjson
			return fmt.Errorf("es: marshal doc %d: %w", i, err)
		}
	}
	status, respBody, err := s.c.do(ctx, http.MethodPost, "/"+s.index+"/_bulk", buf.Bytes(), "application/x-ndjson")
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("es: bulk %s: http %d: %s", s.index, status, snippet(respBody))
	}
	var resp bulkResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("es: decode bulk response: %w", err)
	}
	if !resp.Errors {
		return nil
	}
	rejected := 0
	for _, item := range resp.Items {
		for action, result := range item {
			if result.Error == nil {
				continue
			}
			rejected++
			slog.Error("es: bulk item rejected",
				"index", s.index,
				"action", action,
				"status", result.Status,
				"error_type", result.Error.Type,
				"reason", result.Error.Reason,
			)
		}
	}
	return fmt.Errorf("es: bulk rejected %d/%d docs on %s", rejected, len(docs), s.index)
}
