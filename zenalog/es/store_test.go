package es

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeES 记录全部请求并按 handler 应答，模拟 ES 6
type fakeES struct {
	mu      sync.Mutex
	reqs    []recordedReq
	srv     *httptest.Server
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
}

type recordedReq struct {
	method      string
	path        string
	body        []byte
	contentType string
}

func newFakeES(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) *fakeES {
	t.Helper()
	f := &fakeES{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedReq{r.Method, r.URL.Path, body, r.Header.Get("Content-Type")})
		f.mu.Unlock()
		f.handler(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// find 返回第一条匹配的请求，找不到返回 nil
func (f *fakeES) find(method, path string) *recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.reqs {
		if f.reqs[i].method == method && f.reqs[i].path == path {
			return &f.reqs[i]
		}
	}
	return nil
}

func (f *fakeES) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.reqs {
		if f.reqs[i].method == method && f.reqs[i].path == path {
			n++
		}
	}
	return n
}

// dig 沿 path 逐层取 JSON 对象里的值，缺失即 fatal
func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for i, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("dig %v: step %d (%s): not an object: %T", path, i, key, cur)
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("dig %v: key %q missing", path, key)
		}
	}
	return cur
}

func decodeBody(t *testing.T, req *recordedReq) map[string]any {
	t.Helper()
	if req == nil {
		t.Fatal("request not recorded")
	}
	var m map[string]any
	if err := json.Unmarshal(req.body, &m); err != nil {
		t.Fatalf("decode %s %s body: %v\nbody: %s", req.method, req.path, err, req.body)
	}
	return m
}

func storeConfig(url string) Config {
	return Config{
		Addresses: []string{url},
		Index:     "svc-activity-log",
		AttrKeys:  []string{"customer"},
	}
}

func TestNewStoreValidation(t *testing.T) {
	_, err := NewStore(Config{})
	if err == nil {
		t.Fatal("expect error for empty config")
	}
	for _, want := range []string{"addresses", "index"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// index template 是防「索引被 ES 自动创建成全 text」的护栏：索引被误删后写入会让
// ES 按动态 mapping 重建，之后补 keyword 就撞类型冲突、服务永久起不来（实测踩过）。
// template 必须在检查索引之前落下，pattern 还要覆盖 rollover 出来的索引。
func TestEnsureIndexPutsTemplateFirst(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/_template/svc-activity-log-template":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Path == "/svc-activity-log":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	tpl := decodeBody(t, f.find(http.MethodPut, "/_template/svc-activity-log-template"))

	patterns, ok := tpl["index_patterns"].([]any)
	if !ok || len(patterns) != 2 || patterns[0] != "svc-activity-log" || patterns[1] != "svc-activity-log-*" {
		t.Errorf("index_patterns should cover the index and its rollover pattern, got %v", tpl["index_patterns"])
	}
	// template 的 mapping 必须与建索引用的是同一份（typeless，properties 直接挂 mappings 下）
	if got := dig(t, tpl, "mappings", "properties", "trace_id", "type"); got != "keyword" {
		t.Errorf("template trace_id type = %v, want keyword", got)
	}
	if got := dig(t, tpl, "mappings", "properties", "attrs", "dynamic"); got != "strict" {
		t.Errorf("template attrs dynamic = %v, want strict", got)
	}
	if got := dig(t, tpl, "settings", "index.max_inner_result_window"); got == nil {
		t.Error("template should carry max_inner_result_window")
	}

	// 顺序：template 先于索引检查，否则自动创建的窗口仍然存在
	tplIdx, headIdx := -1, -1
	for i := range f.reqs {
		if f.reqs[i].method == http.MethodPut && f.reqs[i].path == "/_template/svc-activity-log-template" && tplIdx < 0 {
			tplIdx = i
		}
		if f.reqs[i].method == http.MethodHead && f.reqs[i].path == "/svc-activity-log" && headIdx < 0 {
			headIdx = i
		}
	}
	if tplIdx < 0 || headIdx < 0 || tplIdx > headIdx {
		t.Errorf("template must be PUT before checking the index (tpl=%d head=%d)", tplIdx, headIdx)
	}
}

// 类型冲突时错误要给出可操作的处置办法，不能只丢一句 ES 原文
func TestEnsureIndexMappingConflictCarriesHint(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/_template/svc-activity-log-template":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Path == "/svc-activity-log":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log/_settings":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log/_mapping":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"reason":"mapper [changes.from] of different type, current_type [text], merged_type [keyword]"},"status":400}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	err = s.EnsureIndex(context.Background())
	if err == nil {
		t.Fatal("mapping type conflict must fail EnsureIndex")
	}
	if !strings.Contains(err.Error(), "auto-created") {
		t.Errorf("error should tell ops what to do, got: %v", err)
	}
}

func TestEnsureIndexCreate(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/_template/svc-activity-log-template":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Path == "/svc-activity-log":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	req := f.find(http.MethodPut, "/svc-activity-log")
	if req == nil {
		t.Fatal("expect PUT /svc-activity-log to create index")
	}
	body := decodeBody(t, req)

	// settings：单分片 + top_hits 窗口（ES 6 默认 100，不抬 by-trace 一查就被拒）
	if got := dig(t, body, "settings", "number_of_shards"); got != float64(1) {
		t.Errorf("number_of_shards = %v, want 1", got)
	}
	if got := dig(t, body, "settings", "index.max_inner_result_window"); got != float64(2000) {
		t.Errorf("max_inner_result_window = %v, want 2000", got)
	}

	// typeless mapping：properties 直接挂 mappings 下
	props := dig(t, body, "mappings", "properties").(map[string]any)
	if got := dig(t, props, "timestamp", "type"); got != "date" {
		t.Errorf("timestamp type = %v, want date", got)
	}
	if got := dig(t, props, "ts_nanos", "type"); got != "long" {
		t.Errorf("ts_nanos type = %v, want long", got)
	}
	for _, k := range []string{"trace_id", "operator", "resource_type", "instance_id", "resource_path", "operation", "status"} {
		if got := dig(t, props, k, "type"); got != "keyword" {
			t.Errorf("%s type = %v, want keyword", k, got)
		}
	}
	for _, k := range []string{"message", "diff"} {
		if got := dig(t, props, k, "type"); got != "text" {
			t.Errorf("%s type = %v, want text", k, got)
		}
	}
	// changes 三字段：keyword + ignore_above 256（防 immense term 整条拒收）
	for _, k := range []string{"field", "from", "to"} {
		if got := dig(t, props, "changes", "properties", k, "type"); got != "keyword" {
			t.Errorf("changes.%s type = %v, want keyword", k, got)
		}
		if got := dig(t, props, "changes", "properties", k, "ignore_above"); got != float64(256) {
			t.Errorf("changes.%s ignore_above = %v, want 256", k, got)
		}
	}
	// attrs：dynamic strict + 白名单键 text + keyword(256) 多字段
	if got := dig(t, props, "attrs", "dynamic"); got != "strict" {
		t.Errorf("attrs dynamic = %v, want strict", got)
	}
	if got := dig(t, props, "attrs", "properties", "customer", "type"); got != "text" {
		t.Errorf("attrs.customer type = %v, want text", got)
	}
	if got := dig(t, props, "attrs", "properties", "customer", "fields", "keyword", "type"); got != "keyword" {
		t.Errorf("attrs.customer.keyword type = %v, want keyword", got)
	}
	if got := dig(t, props, "attrs", "properties", "customer", "fields", "keyword", "ignore_above"); got != float64(256) {
		t.Errorf("attrs.customer.keyword ignore_above = %v, want 256", got)
	}

	// 新建路径不应再发补 mapping/settings 的请求
	if n := f.count(http.MethodPut, "/svc-activity-log/_mapping"); n != 0 {
		t.Errorf("create path should not PUT _mapping, got %d", n)
	}
}

func TestEnsureIndexExistsBackfills(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/_template/svc-activity-log-template":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Path == "/svc-activity-log":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log/_settings":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log/_mapping":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	if n := f.count(http.MethodPut, "/svc-activity-log"); n != 0 {
		t.Errorf("index exists, should not re-create, got %d PUT", n)
	}
	// 动态 settings 补齐
	sreq := f.find(http.MethodPut, "/svc-activity-log/_settings")
	if sreq == nil {
		t.Fatal("expect PUT _settings to backfill max_inner_result_window")
	}
	if got := dig(t, decodeBody(t, sreq), "index.max_inner_result_window"); got != float64(2000) {
		t.Errorf("backfilled max_inner_result_window = %v, want 2000", got)
	}
	// mapping 补差集：全量 properties 幂等提交
	mreq := f.find(http.MethodPut, "/svc-activity-log/_mapping")
	if mreq == nil {
		t.Fatal("expect PUT _mapping to backfill attrs keys")
	}
	mbody := decodeBody(t, mreq)
	if got := dig(t, mbody, "properties", "attrs", "properties", "customer", "type"); got != "text" {
		t.Errorf("backfilled attrs.customer type = %v, want text", got)
	}
	if _, has := mbody["mappings"]; has {
		t.Error("PUT _mapping body should not nest under mappings")
	}
}

func TestEnsureIndexMappingConflict(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/_template/svc-activity-log-template":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead && r.URL.Path == "/svc-activity-log":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log/_settings":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case r.Method == http.MethodPut && r.URL.Path == "/svc-activity-log/_mapping":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"illegal_argument_exception","reason":"mapper [attrs.customer] cannot be changed from type [long] to [text]"},"status":400}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	err = s.EnsureIndex(context.Background())
	if err == nil {
		t.Fatal("mapping conflict should return error")
	}
	if !strings.Contains(err.Error(), "cannot be changed") {
		t.Errorf("error should carry ES reason, got: %v", err)
	}
}

func TestBulkBodyByteExact(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(`{"took":1,"errors":false,"items":[{"index":{"status":201}},{"index":{"status":201}}]}`))
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	docs := []Doc{
		{
			TraceID:      "t1",
			Timestamp:    "2026-07-31T07:03:05.123Z",
			TsNanos:      1785481385123000000,
			Operator:     "alice",
			ResourceType: "vll",
			InstanceID:   "i-1",
			ResourcePath: "pop/sha",
			Operation:    "create vll",
			Message:      "created",
			Diff:         "bandwidth: 100 -> 200",
			Changes:      []Change{{Field: "bandwidth", From: "100", To: "200"}},
			Status:       "info",
			Attrs:        map[string]string{"customer": "acme"},
		},
		{
			Timestamp: "2026-07-31T07:03:05.124Z",
			TsNanos:   1,
			Operation: "login",
			Status:    "success",
		},
	}
	if err := s.Bulk(context.Background(), docs); err != nil {
		t.Fatalf("Bulk: %v", err)
	}

	req := f.find(http.MethodPost, "/svc-activity-log/_bulk")
	if req == nil {
		t.Fatal("expect POST /svc-activity-log/_bulk")
	}
	if req.contentType != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", req.contentType)
	}
	want := `{"index":{}}
{"trace_id":"t1","timestamp":"2026-07-31T07:03:05.123Z","ts_nanos":1785481385123000000,"operator":"alice","resource_type":"vll","instance_id":"i-1","resource_path":"pop/sha","operation":"create vll","message":"created","diff":"bandwidth: 100 -> 200","changes":[{"field":"bandwidth","from":"100","to":"200"}],"status":"info","attrs":{"customer":"acme"}}
{"index":{}}
{"timestamp":"2026-07-31T07:03:05.124Z","ts_nanos":1,"operation":"login","status":"success"}
`
	if got := string(req.body); got != want {
		t.Errorf("bulk ndjson mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestBulkEmptyNoop(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bulk(context.Background(), nil); err != nil {
		t.Fatalf("empty bulk should be no-op, got: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) != 0 {
		t.Errorf("empty bulk should not hit ES, got %d requests", len(f.reqs))
	}
}

func TestBulkHTTPStatusError(t *testing.T) {
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Bulk(context.Background(), []Doc{{Timestamp: "2026-07-31T00:00:00.000Z", Operation: "x", Status: "info"}})
	if err == nil {
		t.Fatal("HTTP 500 should return error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry status code, got: %v", err)
	}
}

func TestBulkItemRejected(t *testing.T) {
	// HTTP 200 + errors:true：只看状态码会把丢文档当成写成功
	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(`{"took":1,"errors":true,"items":[
			{"index":{"status":201}},
			{"index":{"status":400,"error":{"type":"strict_dynamic_mapping_exception","reason":"mapping set to strict, dynamic introduction of [site] within [attrs] is not allowed"}}}
		]}`))
	})
	s, err := NewStore(storeConfig(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	docs := []Doc{
		{Timestamp: "2026-07-31T00:00:00.000Z", Operation: "a", Status: "info"},
		{Timestamp: "2026-07-31T00:00:00.000Z", Operation: "b", Status: "info"},
	}
	err = s.Bulk(context.Background(), docs)
	if err == nil {
		t.Fatal("item-level rejection should return error")
	}
	if !strings.Contains(err.Error(), "1/2") {
		t.Errorf("error should carry rejected/total = 1/2, got: %v", err)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "strict_dynamic_mapping_exception") {
		t.Errorf("rejected item type should be logged, got logs:\n%s", logs)
	}
	if !strings.Contains(logs, "within [attrs]") {
		t.Errorf("rejected item reason should be logged, got logs:\n%s", logs)
	}
}

func TestAddressFailover(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // 端口已关：网络错误，应换下一个节点

	f := newFakeES(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write([]byte(`{"took":1,"errors":false,"items":[{"index":{"status":201}}]}`))
	})
	cfg := storeConfig(f.srv.URL)
	cfg.Addresses = []string{deadURL, f.srv.URL}
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bulk(context.Background(), []Doc{{Timestamp: "2026-07-31T00:00:00.000Z", Operation: "x", Status: "info"}}); err != nil {
		t.Fatalf("should fail over to the live node, got: %v", err)
	}
	if f.count(http.MethodPost, "/svc-activity-log/_bulk") != 1 {
		t.Error("live node should receive the bulk request")
	}
}
