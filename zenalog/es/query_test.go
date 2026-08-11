package es

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func queryConfig(url string) Config {
	cfg := storeConfig(url)
	cfg.SearchIndex = "svc-*"
	return cfg
}

// hasClause 在 bool 子句数组里找 {kind:{field:value}} 形状的叶子
func hasClause(arr []any, kind, field string, value any) bool {
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := m[kind].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := inner[field]; ok && v == value {
			return true
		}
	}
	return false
}

func searchOKBody() string {
	// ES 7 的 hits.total 是对象 {value, relation}，不再是数字
	return `{"hits":{"total":{"value":42,"relation":"eq"},"hits":[
		{"_source":{"trace_id":"t1","timestamp":"2026-07-31T07:03:05.123Z","ts_nanos":5,"operation":"create vll","status":"info"}}
	]}}`
}

func TestSearchIndexDefaultsToIndex(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(searchOKBody()))
	})
	cfg := storeConfig(f.srv.URL) // 不设 SearchIndex
	q := NewQuery(cfg)
	if _, _, err := q.Search(context.Background(), QueryRequest{Size: 20}); err != nil {
		t.Fatal(err)
	}
	if f.find(http.MethodPost, "/svc-activity-log/_search") == nil {
		t.Error("SearchIndex unset should fall back to Index in URL")
	}
}

func TestSearchRequestShape(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(searchOKBody()))
	})
	q := NewQuery(queryConfig(f.srv.URL))

	start := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	docs, total, err := q.Search(context.Background(), QueryRequest{
		Start: start, End: end,
		From: 20, Size: 20,
		Query: "acme",
		Conds: []Cond{
			{Field: "resource_type", Op: OpEq, Value: "vll"},
			{Field: "attrs.customer", Op: OpPrefix, Value: "ac"},
			{Field: "operator", Op: OpNe, Value: "bob"},
		},
		Excluded: []string{"tunnel path changed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := f.find(http.MethodPost, "/svc-*/_search")
	if req == nil {
		t.Fatal("expect POST /svc-*/_search (SearchIndex pattern in URL)")
	}
	body := decodeBody(t, req)

	if got := body["from"]; got != float64(20) {
		t.Errorf("from = %v, want 20", got)
	}
	if got := body["size"]; got != float64(20) {
		t.Errorf("size = %v, want 20", got)
	}
	// ES 7 不发 track_total_hits 时 total 超 10000 只回 gte 近似值，分页页数会算错，
	// 而 flat 模式的契约是 Total 恒精确，所以必须显式发
	if got := body["track_total_hits"]; got != true {
		t.Errorf("track_total_hits = %v, want true（ES 7 下不发则 total 被 10000 截断）", got)
	}
	// 排序：timestamp desc + ts_nanos tiebreaker
	sorts := body["sort"].([]any)
	if len(sorts) != 2 {
		t.Fatalf("sort should have 2 keys, got %v", sorts)
	}
	if got := sorts[0].(map[string]any)["timestamp"]; got != "desc" {
		t.Errorf("primary sort = %v, want timestamp desc", sorts[0])
	}
	if got := sorts[1].(map[string]any)["ts_nanos"]; got != "desc" {
		t.Errorf("tiebreaker sort = %v, want ts_nanos desc", sorts[1])
	}

	boolQ := dig(t, body, "query", "bool").(map[string]any)
	filter := boolQ["filter"].([]any)
	// 时间范围
	var rangeSeen bool
	for _, item := range filter {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := m["range"].(map[string]any); ok {
			ts := r["timestamp"].(map[string]any)
			if ts["gte"] != "2026-07-30T00:00:00.000Z" || ts["lte"] != "2026-07-31T00:00:00.000Z" {
				t.Errorf("range bounds = %v", ts)
			}
			rangeSeen = true
		}
	}
	if !rangeSeen {
		t.Error("filter should contain timestamp range")
	}
	// eq → term；attrs 前缀查 .keyword 子字段（库内部改写）
	if !hasClause(filter, "term", "resource_type", "vll") {
		t.Errorf("filter should contain term resource_type=vll, got %v", filter)
	}
	if !hasClause(filter, "prefix", "attrs.customer.keyword", "ac") {
		t.Errorf("prefix should target attrs.customer.keyword, got %v", filter)
	}
	// ne 与整组排除进 must_not
	mustNot := boolQ["must_not"].([]any)
	if !hasClause(mustNot, "term", "operator", "bob") {
		t.Errorf("must_not should contain term operator=bob, got %v", mustNot)
	}
	if !hasClause(mustNot, "term", "operation", "tunnel path changed") {
		t.Errorf("must_not should contain excluded operation, got %v", mustNot)
	}
	// 关键词：keyword 字段整值 term + text 字段 match_phrase + attrs 白名单键
	must := boolQ["must"].([]any)
	inner := dig(t, must[0].(map[string]any), "bool").(map[string]any)
	should := inner["should"].([]any)
	for _, field := range []string{"trace_id", "operator", "operation", "resource_path"} {
		if !hasClause(should, "term", field, "acme") {
			t.Errorf("should should contain term %s, got %v", field, should)
		}
	}
	for _, field := range []string{"message", "diff", "attrs.customer"} {
		if !hasClause(should, "match_phrase", field, "acme") {
			t.Errorf("should should contain match_phrase %s, got %v", field, should)
		}
	}
	if got := inner["minimum_should_match"]; got != float64(1) {
		t.Errorf("minimum_should_match = %v, want 1", got)
	}

	// 响应解析：精确 total + docs
	if total != 42 {
		t.Errorf("total = %d, want 42", total)
	}
	if len(docs) != 1 || docs[0].TraceID != "t1" || docs[0].Operation != "create vll" {
		t.Errorf("docs = %+v", docs)
	}
}

func TestSearchNoQueryNoMust(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(searchOKBody()))
	})
	q := NewQuery(queryConfig(f.srv.URL))
	if _, _, err := q.Search(context.Background(), QueryRequest{Size: 20}); err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, f.find(http.MethodPost, "/svc-*/_search"))
	boolQ := dig(t, body, "query", "bool").(map[string]any)
	if _, has := boolQ["must"]; has {
		t.Error("empty Query should not emit must clause")
	}
}

func TestSearchUnknownOp(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(searchOKBody()))
	})
	q := NewQuery(queryConfig(f.srv.URL))
	_, _, err := q.Search(context.Background(), QueryRequest{
		Size:  20,
		Conds: []Cond{{Field: "operator", Op: "gt", Value: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "gt") {
		t.Errorf("unknown op should error naming the op, got: %v", err)
	}
}

func TestSearchHTTPError(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"parsing_exception","reason":"unknown key"}}`))
	})
	q := NewQuery(queryConfig(f.srv.URL))
	_, _, err := q.Search(context.Background(), QueryRequest{Size: 20})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("HTTP 400 should return error with status, got: %v", err)
	}
}

func TestAggregateByTraceShape(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":5,"relation":"eq"},"hits":[]},"aggregations":{"by_trace":{"buckets":[
			{"key":"t2","doc_count":2,"first_ts":{"value":1785481385123.0},"logs":{"hits":{"hits":[
				{"_source":{"trace_id":"t2","timestamp":"2026-07-31T07:03:05.123Z","ts_nanos":1,"operation":"create vll","status":"info"}},
				{"_source":{"trace_id":"t2","timestamp":"2026-07-31T07:03:06.000Z","ts_nanos":2,"operation":"create vll","status":"success"}}
			]}}},
			{"key":"t1","doc_count":1,"first_ts":{"value":1785481000000.0},"logs":{"hits":{"hits":[
				{"_source":{"trace_id":"t1","timestamp":"2026-07-31T06:56:40.000Z","ts_nanos":3,"operation":"login","status":"success"}}
			]}}}
		]}}}`))
	})
	q := NewQuery(queryConfig(f.srv.URL))
	buckets, err := q.AggregateByTrace(context.Background(), QueryRequest{From: 20, Size: 10})
	if err != nil {
		t.Fatal(err)
	}

	body := decodeBody(t, f.find(http.MethodPost, "/svc-*/_search"))
	if got := body["size"]; got != float64(0) {
		t.Errorf("aggregate query size = %v, want 0", got)
	}
	terms := dig(t, body, "aggs", "by_trace", "terms").(map[string]any)
	if terms["field"] != "trace_id" || terms["size"] != float64(10000) {
		t.Errorf("terms = %v, want field trace_id size 10000", terms)
	}
	if got := dig(t, body, "aggs", "by_trace", "aggs", "first_ts", "min", "field"); got != "timestamp" {
		t.Errorf("first_ts min field = %v", got)
	}
	topHits := dig(t, body, "aggs", "by_trace", "aggs", "logs", "top_hits").(map[string]any)
	if topHits["size"] != float64(2000) {
		t.Errorf("top_hits size = %v, want 2000", topHits["size"])
	}
	thSorts := topHits["sort"].([]any)
	if len(thSorts) != 2 || thSorts[0].(map[string]any)["timestamp"] != "asc" || thSorts[1].(map[string]any)["ts_nanos"] != "asc" {
		t.Errorf("top_hits sort = %v, want timestamp asc + ts_nanos asc", thSorts)
	}
	bucketSort := dig(t, body, "aggs", "by_trace", "aggs", "page", "bucket_sort").(map[string]any)
	if bucketSort["from"] != float64(20) || bucketSort["size"] != float64(10) {
		t.Errorf("bucket_sort paging = %v, want from 20 size 10", bucketSort)
	}
	bsSorts := bucketSort["sort"].([]any)
	if len(bsSorts) != 1 || bsSorts[0].(map[string]any)["first_ts"] != "desc" {
		t.Errorf("bucket_sort sort = %v, want first_ts desc", bsSorts)
	}

	// 响应解析：组序保持、StartTime 取 first_ts 的 epoch 毫秒、组内明细完整
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(buckets))
	}
	if buckets[0].TraceID != "t2" || buckets[1].TraceID != "t1" {
		t.Errorf("bucket order = %s,%s want t2,t1", buckets[0].TraceID, buckets[1].TraceID)
	}
	wantStart := time.UnixMilli(1785481385123).UTC()
	if !buckets[0].StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", buckets[0].StartTime, wantStart)
	}
	if len(buckets[0].Docs) != 2 || buckets[0].Docs[1].Status != "success" {
		t.Errorf("bucket docs = %+v", buckets[0].Docs)
	}
}

func TestCheckSearchMapping(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet && r.URL.Path == "/svc-*/_mapping" {
			_, _ = w.Write([]byte(`{
				"svc-a-activity-log":{"mappings":{"properties":{"attrs":{"dynamic":"strict","properties":{"customer":{"type":"text"}}}}}},
				"svc-b-activity-log":{"mappings":{"properties":{"attrs":{"dynamic":"strict","properties":{"tenant":{"type":"text"}}}}}},
				"svc-c-activity-log":{"mappings":{"properties":{"operation":{"type":"keyword"}}}}
			}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	q := NewQuery(queryConfig(f.srv.URL))
	got, err := q.CheckSearchMapping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, has := got["svc-a-activity-log"]; has {
		t.Errorf("svc-a has the key, should not be reported: %v", got)
	}
	if missing := got["svc-b-activity-log"]; len(missing) != 1 || missing[0] != "customer" {
		t.Errorf("svc-b missing = %v, want [customer]", missing)
	}
	if missing := got["svc-c-activity-log"]; len(missing) != 1 || missing[0] != "customer" {
		t.Errorf("svc-c (no attrs at all) missing = %v, want [customer]", missing)
	}
}

func TestCheckSearchMappingNoIndex(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"index_not_found_exception"}}`))
	})
	q := NewQuery(queryConfig(f.srv.URL))
	got, err := q.CheckSearchMapping(context.Background())
	if err != nil {
		t.Fatalf("pattern with no index should not error, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty result, got %v", got)
	}
}
