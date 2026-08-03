package ginx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	// 中间件里的日志会走全局 slog，测试期间丢掉
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// do 用装好中间件的引擎跑一个请求
func do(t *testing.T, req *http.Request, middlewares []gin.HandlerFunc, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	r.Use(middlewares...)
	r.GET("/t", handler)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRequestIDPassesThroughInboundHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.Header.Set(RequestIDHeader, "caller-supplied")

	var seen string
	rec := do(t, req, []gin.HandlerFunc{RequestID()}, func(c *gin.Context) {
		seen = RequestIDFrom(c.Request.Context())
		c.Status(http.StatusOK)
	})

	if seen != "caller-supplied" {
		t.Errorf("context request id = %q, want caller-supplied", seen)
	}
	if got := rec.Header().Get(RequestIDHeader); got != "caller-supplied" {
		t.Errorf("response header = %q, want caller-supplied", got)
	}
}

func TestRequestIDGeneratesWhenMissing(t *testing.T) {
	var first, second string
	for _, seen := range []*string{&first, &second} {
		target := seen
		do(t, httptest.NewRequest(http.MethodGet, "/t", nil), []gin.HandlerFunc{RequestID()}, func(c *gin.Context) {
			*target = RequestIDFrom(c.Request.Context())
			c.Status(http.StatusOK)
		})
	}

	if first == "" || second == "" {
		t.Fatal("request id should be generated when the header is absent")
	}
	if first == second {
		t.Errorf("generated ids should differ, both were %q", first)
	}
}

// request id 必须进 request context，否则 slog 看不到
func TestRequestIDReachesRequestContext(t *testing.T) {
	rec := do(t, httptest.NewRequest(http.MethodGet, "/t", nil), []gin.HandlerFunc{RequestID()}, func(c *gin.Context) {
		attrs := TraceAttrs(c.Request.Context())
		if len(attrs) != 1 || attrs[0].Key != "requestId" {
			t.Errorf("TraceAttrs = %+v, want one requestId attr", attrs)
		}
		c.Status(http.StatusOK)
	})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestTraceAttrsEmptyWithoutRequestID(t *testing.T) {
	if attrs := TraceAttrs(t.Context()); attrs != nil {
		t.Errorf("TraceAttrs = %+v, want nil", attrs)
	}
}

func TestWithRequestID(t *testing.T) {
	ctx := WithRequestID(t.Context(), "async-1")
	if got := RequestIDFrom(ctx); got != "async-1" {
		t.Errorf("RequestIDFrom = %q, want async-1", got)
	}
}

func TestRecoveryTurnsPanicIntoEnvelope(t *testing.T) {
	rec := do(t, httptest.NewRequest(http.MethodGet, "/t", nil),
		[]gin.HandlerFunc{RequestID(), Recovery()},
		func(c *gin.Context) { panic("boom") })

	// panic 是服务故障不是业务结果，要让网关和监控看见
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not the envelope: %v (%s)", err, rec.Body.String())
	}
	if resp.Success {
		t.Error("Success = true, want false")
	}
	if resp.Code != CodeInternalError {
		t.Errorf("Code = %q, want %q", resp.Code, CodeInternalError)
	}
}

// AccessLog 装在 Recovery 之前才记得到 panic 那条请求
func TestAccessLogRunsAfterRecoveredPanic(t *testing.T) {
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	do(t, httptest.NewRequest(http.MethodGet, "/t", nil),
		[]gin.HandlerFunc{RequestID(), AccessLog(), Recovery()},
		func(c *gin.Context) { panic("boom") })

	out := buf.String()
	if !strings.Contains(out, "http request") {
		t.Errorf("AccessLog did not record the panicking request: %s", out)
	}
	if !strings.Contains(out, `"status":500`) {
		t.Errorf("AccessLog should record status 500, got: %s", out)
	}
}

func TestResponseHelpers(t *testing.T) {
	tests := []struct {
		name        string
		call        func(*gin.Context)
		wantSuccess bool
		wantRet     int
		wantCode    string
		wantMsg     string
	}{
		{"Success", func(c *gin.Context) { Success(c, map[string]int{"n": 1}) }, true, 0, CodeSuccess, "success"},
		{"SuccessWithMsg", func(c *gin.Context) { SuccessWithMsg(c, "done", nil) }, true, 0, CodeSuccess, "done"},
		{"Created", func(c *gin.Context) { Created(c, nil) }, true, 0, CodeCreated, "success"},
		{"Accepted", func(c *gin.Context) { Accepted(c, nil) }, true, 0, CodeAccepted, "success"},
		{"Fail", func(c *gin.Context) { Fail(c, CodeConflict, "taken") }, false, -1, CodeConflict, "taken"},
		{"Error", func(c *gin.Context) { Error(c, CodeDBError, errors.New("db down")) }, false, -1, CodeDBError, "db down"},
		{"Error nil", func(c *gin.Context) { Error(c, CodeDBError, nil) }, false, -1, CodeDBError, ""},
		{"BadRequest", func(c *gin.Context) { BadRequest(c, "bad name") }, false, -1, CodeBadRequest, "bad name"},
		{"NotFound", func(c *gin.Context) { NotFound(c, "no such vm") }, false, -1, CodeNotFound, "no such vm"},
		{"InternalError", func(c *gin.Context) { InternalError(c, errors.New("oops")) }, false, -1, CodeInternalError, "oops"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, httptest.NewRequest(http.MethodGet, "/t", nil), nil, tt.call)
			if rec.Code != http.StatusOK {
				t.Errorf("HTTP status = %d, want 200", rec.Code)
			}
			var resp Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
			}
			if resp.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v", resp.Success, tt.wantSuccess)
			}
			if resp.Ret != tt.wantRet {
				t.Errorf("Ret = %d, want %d", resp.Ret, tt.wantRet)
			}
			if resp.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", resp.Code, tt.wantCode)
			}
			if resp.Msg != tt.wantMsg {
				t.Errorf("Msg = %q, want %q", resp.Msg, tt.wantMsg)
			}
		})
	}
}

// data 为空时不出现在 JSON 里
func TestResponseOmitsEmptyData(t *testing.T) {
	rec := do(t, httptest.NewRequest(http.MethodGet, "/t", nil), nil, func(c *gin.Context) {
		Success(c, nil)
	})
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["data"]; ok {
		t.Errorf("data should be omitted when nil: %s", rec.Body.String())
	}
}
