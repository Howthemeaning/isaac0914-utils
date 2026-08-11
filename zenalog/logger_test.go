package zenalog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Howthemeaning/isaac0914-utils/zenalog/es"
	"github.com/Howthemeaning/isaac0914-utils/zenserver/ginx"
)

// fakeStore 记录 Bulk 批次；entered/release 用来钉住 flusher 的时序
type fakeStore struct {
	mu      sync.Mutex
	batches [][]es.Doc
	err     error
	entered chan struct{} // 非 nil：每次 Bulk 进入先发信号（带缓冲）
	release chan struct{} // 非 nil：Bulk 阻塞等放行
}

func (f *fakeStore) Bulk(ctx context.Context, docs []es.Doc) error {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.batches = append(f.batches, append([]es.Doc(nil), docs...))
	return nil
}

func (f *fakeStore) docs() []es.Doc {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []es.Doc
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

func (f *fakeStore) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func testLoggerConfig() Config {
	return Config{
		Addresses:     []string{"http://ignored:9200"},
		Index:         "test-activity-log",
		AttrKeys:      []string{"customer"},
		FlushInterval: time.Hour, // 测试默认不靠定时器冲刷
	}
}

func newTestLogger(t *testing.T, cfg Config, fs *fakeStore) *Logger {
	t.Helper()
	l := newLogger(cfg, fs, nil)
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout waiting for: " + msg)
}

func TestNewValidatesConfig(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("empty config should fail validation")
	}
	for _, want := range []string{"addresses", "index"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q (report all at once), got: %v", want, err)
		}
	}
}

// ES_HOST env 通道：逗号分隔拆进 Addresses
func TestApplyAddressesCSV(t *testing.T) {
	got := (Config{AddressesCSV: "172.16.5.185,172.16.5.217,172.16.5.237"}).applyAddressesCSV()
	want := []string{"172.16.5.185", "172.16.5.217", "172.16.5.237"}
	if len(got.Addresses) != len(want) {
		t.Fatalf("Addresses = %v, want %v", got.Addresses, want)
	}
	for i := range want {
		if got.Addresses[i] != want[i] {
			t.Errorf("Addresses[%d] = %q, want %q", i, got.Addresses[i], want[i])
		}
	}
}

// 只填 yaml addresses、不填 ES_HOST 时原样保留
func TestApplyAddressesCSVEmpty(t *testing.T) {
	cfg := Config{Addresses: []string{"http://es:9200"}}
	if got := cfg.applyAddressesCSV(); len(got.Addresses) != 1 || got.Addresses[0] != "http://es:9200" {
		t.Errorf("Addresses should stay untouched, got %v", got.Addresses)
	}
}

// 补 scheme：ES 节点习惯写成 host:port，client.do 是 addr+path 裸拼，
// 缺 scheme 会得到 unsupported protocol scheme
func TestNormalizeAddresses(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"host:port 补 http", []string{"172.16.5.185:9200"}, []string{"http://172.16.5.185:9200"}},
		{"多节点", []string{"10.0.0.1:9200", "10.0.0.2:9200"}, []string{"http://10.0.0.1:9200", "http://10.0.0.2:9200"}},
		{"已带 http 不动", []string{"http://es:9200"}, []string{"http://es:9200"}},
		{"已带 https 不动", []string{"https://es:9200"}, []string{"https://es:9200"}},
		{"混写", []string{"https://a:9200", "b:9200"}, []string{"https://a:9200", "http://b:9200"}},
		{"去空白", []string{" 172.16.5.185:9200 ", "\t10.0.0.2:9200"}, []string{"http://172.16.5.185:9200", "http://10.0.0.2:9200"}},
		{"丢空项", []string{"a:9200", "", "  "}, []string{"http://a:9200"}},
		{"全空 -> 空切片，交给 validate 报错", []string{"", " "}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := (Config{Addresses: c.in}).normalizeAddresses().Addresses
			if len(got) != len(c.want) {
				t.Fatalf("Addresses = %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("Addresses[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// ES_HOST 端到端：New 里 applyAddressesCSV 之后必须补 scheme，
// 否则 EnsureIndex 直接 unsupported protocol scheme
func TestApplyAddressesCSVThenNormalize(t *testing.T) {
	cfg := (Config{AddressesCSV: "172.16.5.185:9200,172.16.5.217:9200"}).
		applyAddressesCSV().normalizeAddresses()
	want := []string{"http://172.16.5.185:9200", "http://172.16.5.217:9200"}
	for i := range want {
		if cfg.Addresses[i] != want[i] {
			t.Errorf("Addresses[%d] = %q, want %q", i, cfg.Addresses[i], want[i])
		}
	}
}

func TestSyncAssemblesEntryAndWritesThrough(t *testing.T) {
	fs := &fakeStore{}
	l := newTestLogger(t, testLoggerConfig(), fs)

	ctx := WithOperator(WithTraceID(context.Background(), "t-1"), "alice")
	chs := []Change{{Field: "bandwidth", From: "100", To: "200"}}
	err := l.Info(ctx, "create vll",
		Resource("vll", "i-1"),
		Path("pop/sha"),
		Message("created"),
		Attr("customer", "acme"),
		Changes(chs),
		Sync(),
	)
	if err != nil {
		t.Fatalf("sync info: %v", err)
	}

	docs := fs.docs()
	if len(docs) != 1 {
		t.Fatalf("sync should write through immediately, got %d docs", len(docs))
	}
	d := docs[0]
	if d.TraceID != "t-1" || d.Operator != "alice" {
		t.Errorf("ctx fields not assembled: %+v", d)
	}
	if d.ResourceType != "vll" || d.InstanceID != "i-1" || d.ResourcePath != "pop/sha" {
		t.Errorf("resource fields not assembled: %+v", d)
	}
	if d.Operation != "create vll" || d.Message != "created" || d.Status != "info" {
		t.Errorf("operation fields not assembled: %+v", d)
	}
	if d.Attrs["customer"] != "acme" {
		t.Errorf("attrs = %v", d.Attrs)
	}
	if len(d.Changes) != 1 || d.Changes[0] != (es.Change{Field: "bandwidth", From: "100", To: "200"}) {
		t.Errorf("changes = %+v", d.Changes)
	}
	if d.Diff != "bandwidth: 100 -> 200" {
		t.Errorf("diff = %q, want formatted changes", d.Diff)
	}
	// 时间：UTC 毫秒串 + ts_nanos 一致
	ts, err := time.Parse(es.TimeLayout, d.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q not in TimeLayout: %v", d.Timestamp, err)
	}
	if d.TsNanos/1e6 != ts.UnixMilli() {
		t.Errorf("ts_nanos %d inconsistent with timestamp %q", d.TsNanos, d.Timestamp)
	}
}

func TestInfoFinishStatus(t *testing.T) {
	fs := &fakeStore{}
	l := newTestLogger(t, testLoggerConfig(), fs)

	if err := l.InfoFinish(context.Background(), "approve", true, Sync()); err != nil {
		t.Fatal(err)
	}
	if err := l.InfoFinish(context.Background(), "approve", false, Sync()); err != nil {
		t.Fatal(err)
	}
	docs := fs.docs()
	if len(docs) != 2 || docs[0].Status != "success" || docs[1].Status != "failed" {
		t.Errorf("statuses = %+v", docs)
	}
}

func TestTraceIDFallsBackToRequestID(t *testing.T) {
	fs := &fakeStore{}
	l := newTestLogger(t, testLoggerConfig(), fs)

	// 只有 request id：回落
	ctx := ginx.WithRequestID(context.Background(), "req-9")
	if err := l.Info(ctx, "x", Sync()); err != nil {
		t.Fatal(err)
	}
	// 显式 WithTraceID 优先
	ctx2 := WithTraceID(ctx, "t-override")
	if err := l.Info(ctx2, "x", Sync()); err != nil {
		t.Fatal(err)
	}
	docs := fs.docs()
	if len(docs) != 2 {
		t.Fatalf("want 2 docs, got %d", len(docs))
	}
	if docs[0].TraceID != "req-9" {
		t.Errorf("traceID = %q, want fallback req-9", docs[0].TraceID)
	}
	if docs[1].TraceID != "t-override" {
		t.Errorf("traceID = %q, want explicit t-override", docs[1].TraceID)
	}
}

func TestUnregisteredAttrKeyErrors(t *testing.T) {
	fs := &fakeStore{}
	l := newTestLogger(t, testLoggerConfig(), fs)

	// 同步与异步两条路径都要报错，且不落任何数据
	for _, opts := range [][]Option{
		{Attr("site", "sha"), Sync()},
		{Attr("site", "sha")},
	} {
		err := l.Info(context.Background(), "x", opts...)
		if err == nil || !strings.Contains(err.Error(), "site") {
			t.Errorf("unregistered attr key should error naming the key, got: %v", err)
		}
	}
	if got := fs.docs(); len(got) != 0 {
		t.Errorf("nothing should be written, got %+v", got)
	}
}

func TestAsyncBatchesBySize(t *testing.T) {
	fs := &fakeStore{}
	cfg := testLoggerConfig()
	cfg.BatchSize = 2
	l := newTestLogger(t, cfg, fs)

	if err := l.Info(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Info(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "batch of 2 flushed", func() bool { return len(fs.docs()) == 2 })
	if fs.batchCount() != 1 {
		t.Errorf("2 entries with BatchSize=2 should flush as one batch, got %d", fs.batchCount())
	}
}

func TestAsyncFlushesByInterval(t *testing.T) {
	fs := &fakeStore{}
	cfg := testLoggerConfig()
	cfg.FlushInterval = 20 * time.Millisecond
	l := newTestLogger(t, cfg, fs)

	if err := l.Info(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "interval flush", func() bool { return len(fs.docs()) == 1 })
}

func TestQueueFullDropsNewAndCounts(t *testing.T) {
	fs := &fakeStore{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
	cfg := testLoggerConfig()
	cfg.QueueSize = 1
	cfg.BatchSize = 1
	l := newTestLogger(t, cfg, fs)

	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	// #1 被 flusher 取走后卡在 Bulk 里
	if err := l.Info(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("flusher never reached Bulk")
	}
	// #2 填满队列（cap 1），#3 只能丢
	if err := l.Info(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if err := l.Info(context.Background(), "third"); err != nil {
		t.Fatalf("queue-full drop must not surface as error (no backpressure), got: %v", err)
	}
	if got := l.dropped.Load(); got != 1 {
		t.Errorf("dropped counter = %d, want 1", got)
	}
	if !strings.Contains(logBuf.String(), "queue full") {
		t.Errorf("drop should log a warn, got: %s", logBuf.String())
	}

	close(fs.release)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	docs := fs.docs()
	if len(docs) != 2 {
		t.Errorf("first+second should land, third dropped; got %d docs", len(docs))
	}
}

func TestSyncErrorPropagates(t *testing.T) {
	esDown := errors.New("es down")
	fs := &fakeStore{err: esDown}
	l := newTestLogger(t, testLoggerConfig(), fs)

	err := l.Info(context.Background(), "approve", Sync())
	if !errors.Is(err, esDown) {
		t.Errorf("sync must propagate store error as-is, got: %v", err)
	}
}

func TestCloseFlushesRemainingAndIdempotent(t *testing.T) {
	fs := &fakeStore{}
	l := newLogger(testLoggerConfig(), fs, nil) // 不用 cleanup，本例自己管 Close

	for _, op := range []string{"a", "b", "c"} {
		if err := l.Info(context.Background(), op); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := len(fs.docs()); got != 3 {
		t.Errorf("close must flush buffered entries, got %d/3", got)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Errorf("close must be idempotent, got: %v", err)
	}
	if err := l.Info(context.Background(), "late"); !errors.Is(err, ErrClosed) {
		t.Errorf("async after close = %v, want ErrClosed", err)
	}
	if err := l.Info(context.Background(), "late", Sync()); !errors.Is(err, ErrClosed) {
		t.Errorf("sync after close = %v, want ErrClosed", err)
	}
	if err := l.InfoFinish(context.Background(), "late", true); !errors.Is(err, ErrClosed) {
		t.Errorf("InfoFinish after close = %v, want ErrClosed", err)
	}
}

func TestCloseRespectsContext(t *testing.T) {
	fs := &fakeStore{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
	cfg := testLoggerConfig()
	cfg.BatchSize = 1
	l := newLogger(cfg, fs, nil)

	if err := l.Info(context.Background(), "stuck"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("flusher never reached Bulk")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("close with stuck flusher = %v, want DeadlineExceeded", err)
	}
	close(fs.release) // 放行，别留挂死的 goroutine
}

func TestNewAgainstFakeES(t *testing.T) {
	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/_template/cmi-activity-log-template":
			// EnsureIndex 先落 index template，防索引被 ES 自动创建成全 text mapping
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case r.Method == http.MethodHead && r.URL.Path == "/cmi-activity-log":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == "/cmi-activity-log":
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/all-*/_mapping":
			// 目标 pattern 里有个索引缺 customer 键 → 记 warn 不 fail
			_, _ = w.Write([]byte(`{"other-activity-log":{"mappings":{"_doc":{"properties":{"operation":{"type":"keyword"}}}}}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	l, err := New(Config{
		Addresses:   []string{srv.URL},
		Index:       "cmi-activity-log",
		SearchIndex: "all-*",
		AttrKeys:    []string{"customer"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = l.Close(context.Background()) }()

	logs := logBuf.String()
	if !strings.Contains(logs, "other-activity-log") || !strings.Contains(logs, "customer") {
		t.Errorf("mapping mismatch should be warned with index and key, got: %s", logs)
	}
}

func TestNewFailsFastWhenESUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	_, err := New(Config{Addresses: []string{deadURL}, Index: "x-activity-log"})
	if err == nil {
		t.Fatal("ES unreachable should fail New (fail fast at startup)")
	}
}
