package zenalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Howthemeaning/isaac0914-utils/zenalog/es"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type envelope struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Msg     string `json:"msg"`
	Data    struct {
		Entries []map[string]any `json:"entries"`
		Total   int64            `json:"total"`
		Buckets []map[string]any `json:"buckets"`
	} `json:"data"`
}

func serveActivityLog(t *testing.T, fq *fakeSearcher, rawQuery string) envelope {
	t.Helper()
	l := newListLogger(t, fq)
	engine := gin.New()
	engine.GET("/activityLog", l.GinHandler())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/activityLog?"+rawQuery, nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("http status = %d, ginx envelope is always 200", w.Code)
	}
	var resp envelope
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func TestGinHandlerDefaults(t *testing.T) {
	fq := &fakeSearcher{
		docs:  []es.Doc{{TraceID: "t1", Timestamp: "2026-07-31T07:03:05.123Z", Operation: "create vll", Status: "info"}},
		total: 7,
	}
	resp := serveActivityLog(t, fq, "")
	if !resp.Success || resp.Code != "SUCCESS" {
		t.Fatalf("envelope = %+v", resp)
	}
	if resp.Data.Total != 7 || len(resp.Data.Entries) != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
	// Entry 的 JSON 形状是 camelCase
	e := resp.Data.Entries[0]
	if e["traceId"] != "t1" || e["operation"] != "create vll" {
		t.Errorf("entry json = %v", e)
	}
	if !fq.searchCalled {
		t.Error("default mode should hit flat Search")
	}
}

func TestGinHandlerParsesParams(t *testing.T) {
	fq := &fakeSearcher{buckets: []es.TraceBucket{{TraceID: "t1", StartTime: time.Now(), Docs: nil}}}
	q := url.Values{}
	q.Set("mode", "trace")
	q.Set("pageNum", "2")
	q.Set("pageSize", "10")
	q.Set("query", "acme")
	q.Set("instanceId", "i-1")
	q.Set("startTime", "2026-07-30T00:00:00Z")
	q.Set("endTime", "2026-07-31T00:00:00Z")
	q.Add("condition", "attrs.customer:prefix:ac")
	q.Add("condition", "operation:ne:noise")

	resp := serveActivityLog(t, fq, q.Encode())
	if !resp.Success {
		t.Fatalf("envelope = %+v", resp)
	}
	if !fq.aggCalled {
		t.Fatal("mode=trace should hit AggregateByTrace")
	}
	req := fq.lastReq
	if req.From != 10 || req.Size != 10 {
		t.Errorf("paging = from %d size %d, want 10/10", req.From, req.Size)
	}
	if req.Query != "acme" {
		t.Errorf("query = %q", req.Query)
	}
	if !req.Start.Equal(time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)) ||
		!req.End.Equal(time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("time range = %v..%v", req.Start, req.End)
	}
	want := []es.Cond{
		{Field: "attrs.customer", Op: es.OpPrefix, Value: "ac"},
		{Field: "operation", Op: es.OpNe, Value: "noise"},
		{Field: "instance_id", Op: es.OpEq, Value: "i-1"},
	}
	if len(req.Conds) != len(want) {
		t.Fatalf("conds = %+v, want %+v", req.Conds, want)
	}
	for i := range want {
		if req.Conds[i] != want[i] {
			t.Errorf("cond[%d] = %+v, want %+v", i, req.Conds[i], want[i])
		}
	}
	if len(resp.Data.Buckets) != 1 {
		t.Errorf("buckets in response = %+v", resp.Data)
	}
}

func TestGinHandlerBadParams(t *testing.T) {
	tests := []struct {
		name     string
		rawQuery string
	}{
		{"bad pageSize", "pageSize=abc"},
		{"bad pageNum", "pageNum=x"},
		{"bad startTime", "startTime=2026-07-30"},
		{"bad endTime", "endTime=notatime"},
		{"bad mode", "mode=tree"},
		{"bad condition format", "condition=" + url.QueryEscape("operation-eq-create")},
		{"bad condition op", "condition=" + url.QueryEscape("operation:gt:create")},
		{"page size over cap", "pageSize=101"},
		{"unregistered attr condition", "condition=" + url.QueryEscape("attrs.site:eq:x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fq := &fakeSearcher{}
			resp := serveActivityLog(t, fq, tt.rawQuery)
			if resp.Success || resp.Code != "BAD_REQUEST" {
				t.Errorf("envelope = success %v code %q, want BAD_REQUEST", resp.Success, resp.Code)
			}
			if fq.searchCalled || fq.aggCalled {
				t.Error("bad params must not reach ES")
			}
		})
	}
}

func TestGinHandlerSearchErrorIsInternal(t *testing.T) {
	fq := &fakeSearcher{err: errors.New("es down")}
	resp := serveActivityLog(t, fq, "")
	if resp.Success || resp.Code != "INTERNAL_ERROR" {
		t.Errorf("envelope = success %v code %q, want INTERNAL_ERROR", resp.Success, resp.Code)
	}
}

type handlerCtxKey struct{}

func TestGinHandlerUsesRequestContext(t *testing.T) {
	// handler 必须用 c.Request.Context() 而不是 c：request context 上挂着
	// requestId/操作人，用错了日志就断链——断言它真的到达查询层
	fq := &fakeSearcher{}
	l := newListLogger(t, fq)
	engine := gin.New()
	engine.GET("/activityLog", l.GinHandler())

	w := httptest.NewRecorder()
	ctx := context.WithValue(context.Background(), handlerCtxKey{}, "marker")
	req := httptest.NewRequest(http.MethodGet, "/activityLog", nil).WithContext(ctx)
	engine.ServeHTTP(w, req)

	if fq.lastCtx == nil || fq.lastCtx.Value(handlerCtxKey{}) != "marker" {
		t.Fatal("handler must pass c.Request.Context() down to the query layer")
	}
}
