package zenalog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/isaac0914/utils/zenalog/es"
)

// fakeSearcher 记录最后一次查询请求与 ctx，返回预置结果
type fakeSearcher struct {
	lastReq      es.QueryRequest
	lastCtx      context.Context
	searchCalled bool
	aggCalled    bool
	docs         []es.Doc
	total        int64
	buckets      []es.TraceBucket
	err          error
}

func (f *fakeSearcher) Search(ctx context.Context, req es.QueryRequest) ([]es.Doc, int64, error) {
	f.searchCalled = true
	f.lastReq = req
	f.lastCtx = ctx
	return f.docs, f.total, f.err
}

func (f *fakeSearcher) AggregateByTrace(ctx context.Context, req es.QueryRequest) ([]es.TraceBucket, error) {
	f.aggCalled = true
	f.lastReq = req
	f.lastCtx = ctx
	return f.buckets, f.err
}

func newListLogger(t *testing.T, fq *fakeSearcher) *Logger {
	t.Helper()
	cfg := testLoggerConfig()
	cfg.ExcludedOperations = []string{"tunnel path changed"}
	l := newLogger(cfg, &fakeStore{}, fq)
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

func TestListDefaults(t *testing.T) {
	fq := &fakeSearcher{
		docs: []es.Doc{{
			TraceID:   "t1",
			Timestamp: "2026-07-31T07:03:05.123Z",
			TsNanos:   5,
			Operator:  "alice",
			Operation: "create vll",
			Status:    "info",
			Changes:   []es.Change{{Field: "f", From: "1", To: "2"}},
			Attrs:     map[string]string{"customer": "acme"},
		}},
		total: 42,
	}
	l := newListLogger(t, fq)

	res, err := l.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !fq.searchCalled || fq.aggCalled {
		t.Fatal("default mode should be flat (Search), not aggregate")
	}
	req := fq.lastReq
	if req.From != 0 || req.Size != 20 {
		t.Errorf("default paging = from %d size %d, want 0/20", req.From, req.Size)
	}
	// 默认近 24 小时
	if d := time.Since(req.End); d < 0 || d > 5*time.Second {
		t.Errorf("default End should be now, got %v", req.End)
	}
	if window := req.End.Sub(req.Start); window < 24*time.Hour-5*time.Second || window > 24*time.Hour+5*time.Second {
		t.Errorf("default window = %v, want ~24h", window)
	}
	// ExcludedOperations 从配置带下去
	if len(req.Excluded) != 1 || req.Excluded[0] != "tunnel path changed" {
		t.Errorf("excluded = %v", req.Excluded)
	}

	// 结果映射
	if res.Total != 42 {
		t.Errorf("total = %d, want 42", res.Total)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(res.Entries))
	}
	e := res.Entries[0]
	if e.TraceID != "t1" || e.Operator != "alice" || e.Operation != "create vll" || e.Status != StatusInfo {
		t.Errorf("entry = %+v", e)
	}
	wantTs := time.Date(2026, 7, 31, 7, 3, 5, 123000000, time.UTC)
	if !e.Timestamp.Equal(wantTs) {
		t.Errorf("timestamp = %v, want %v", e.Timestamp, wantTs)
	}
	if len(e.Changes) != 1 || e.Changes[0] != (Change{Field: "f", From: "1", To: "2"}) {
		t.Errorf("changes = %+v", e.Changes)
	}
	if e.Attrs["customer"] != "acme" {
		t.Errorf("attrs = %v", e.Attrs)
	}
}

func TestListPagination(t *testing.T) {
	fq := &fakeSearcher{}
	l := newListLogger(t, fq)

	if _, err := l.List(context.Background(), ListRequest{PageNum: 3, PageSize: 50}); err != nil {
		t.Fatal(err)
	}
	if fq.lastReq.From != 100 || fq.lastReq.Size != 50 {
		t.Errorf("paging = from %d size %d, want 100/50", fq.lastReq.From, fq.lastReq.Size)
	}
}

func TestListValidation(t *testing.T) {
	tests := []struct {
		name    string
		req     ListRequest
		wantErr string
	}{
		{"page size over cap", ListRequest{PageSize: 101}, "page_size"},
		{"negative page num", ListRequest{PageNum: -1}, "page_num"},
		{"deep paging beyond max_result_window", ListRequest{PageNum: 501, PageSize: 20}, "max_result_window"},
		{"unknown condition field", ListRequest{Conditions: []Condition{{Field: "foo", Value: "x"}}}, "foo"},
		{"unregistered attr key", ListRequest{Conditions: []Condition{{Field: "attrs.site", Value: "x"}}}, "site"},
		{"unknown op", ListRequest{Conditions: []Condition{{Field: "operation", Op: Op(9), Value: "x"}}}, "op"},
		{"unknown mode", ListRequest{Mode: Mode(9)}, "mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fq := &fakeSearcher{}
			l := newListLogger(t, fq)
			_, err := l.List(context.Background(), tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("want error containing %q, got: %v", tt.wantErr, err)
			}
			if fq.searchCalled || fq.aggCalled {
				t.Error("invalid request must not reach ES")
			}
		})
	}

	// 边界：from+size 恰好 10000 允许
	fq := &fakeSearcher{}
	l := newListLogger(t, fq)
	if _, err := l.List(context.Background(), ListRequest{PageNum: 500, PageSize: 20}); err != nil {
		t.Errorf("from+size == 10000 should pass, got: %v", err)
	}
}

func TestListConditionsMapped(t *testing.T) {
	fq := &fakeSearcher{}
	l := newListLogger(t, fq)

	start := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	_, err := l.List(context.Background(), ListRequest{
		StartTime: start,
		EndTime:   end,
		Query:     "acme",
		Conditions: []Condition{
			{Field: "operation", Op: OpEq, Value: "create"},
			{Field: "operator", Op: OpNe, Value: "bob"},
			{Field: "attrs.customer", Op: OpPrefix, Value: "ac"},
		},
		InstanceID: "i-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := fq.lastReq
	if !req.Start.Equal(start) || !req.End.Equal(end) {
		t.Errorf("time range = %v..%v", req.Start, req.End)
	}
	if req.Query != "acme" {
		t.Errorf("query = %q", req.Query)
	}
	want := []es.Cond{
		{Field: "operation", Op: es.OpEq, Value: "create"},
		{Field: "operator", Op: es.OpNe, Value: "bob"},
		{Field: "attrs.customer", Op: es.OpPrefix, Value: "ac"},
		{Field: "instance_id", Op: es.OpEq, Value: "i-9"}, // InstanceID 快捷入口
	}
	if len(req.Conds) != len(want) {
		t.Fatalf("conds = %+v, want %+v", req.Conds, want)
	}
	for i := range want {
		if req.Conds[i] != want[i] {
			t.Errorf("cond[%d] = %+v, want %+v", i, req.Conds[i], want[i])
		}
	}
}

func TestListByTrace(t *testing.T) {
	ts := time.Date(2026, 7, 31, 7, 3, 5, 0, time.UTC)
	fq := &fakeSearcher{
		buckets: []es.TraceBucket{{
			TraceID:   "t1",
			StartTime: ts,
			Docs: []es.Doc{
				{TraceID: "t1", Timestamp: "2026-07-31T07:03:05.000Z", Operation: "create vll", Status: "info"},
				{TraceID: "t1", Timestamp: "2026-07-31T07:03:06.000Z", Operation: "create vll", Status: "success"},
			},
		}},
	}
	l := newListLogger(t, fq)

	res, err := l.List(context.Background(), ListRequest{Mode: ModeByTrace})
	if err != nil {
		t.Fatal(err)
	}
	if !fq.aggCalled || fq.searchCalled {
		t.Fatal("ModeByTrace should call AggregateByTrace")
	}
	if len(res.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(res.Buckets))
	}
	b := res.Buckets[0]
	if b.TraceID != "t1" || !b.StartTime.Equal(ts) {
		t.Errorf("bucket = %+v", b)
	}
	if len(b.Logs) != 2 || b.Logs[1].Status != StatusSuccess {
		t.Errorf("logs = %+v", b.Logs)
	}
}

func TestListSearchErrorPropagates(t *testing.T) {
	esErr := errors.New("es search failed")
	fq := &fakeSearcher{err: esErr}
	l := newListLogger(t, fq)

	_, err := l.List(context.Background(), ListRequest{})
	if !errors.Is(err, esErr) {
		t.Errorf("search error should propagate, got: %v", err)
	}
}
