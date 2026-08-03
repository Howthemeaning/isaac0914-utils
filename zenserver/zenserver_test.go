package zenserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// freeAddr 取一个当前空闲的地址。先 bind 再放开，紧接着让 server 占用。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never started listening on %s", addr)
}

func TestValidateRequiredFields(t *testing.T) {
	tests := []struct {
		name     string
		srv      Server
		wantMiss []string
	}{
		{"both missing", Server{}, []string{"Addr", "RegisterRoutes"}},
		{"no addr", Server{RegisterRoutes: func(*gin.Engine) {}}, []string{"Addr"}},
		{"no routes", Server{Addr: ":0"}, []string{"RegisterRoutes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.srv.RunContext(context.Background())
			if err == nil {
				t.Fatal("want error")
			}
			// 缺失项一次报全，不让人试一次改一个
			for _, field := range tt.wantMiss {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("error should mention %s, got: %v", field, err)
				}
			}
		})
	}
}

func TestValidPassesValidation(t *testing.T) {
	srv := Server{Addr: ":8080", RegisterRoutes: func(*gin.Engine) {}}
	if err := srv.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// OnStart 报错就不该进入监听
func TestOnStartErrorPreventsListening(t *testing.T) {
	addr := freeAddr(t)
	wantErr := errors.New("db unreachable")

	srv := &Server{
		Addr:           addr,
		RegisterRoutes: func(*gin.Engine) {},
		OnStart:        func(context.Context) error { return wantErr },
	}

	err := srv.RunContext(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunContext error = %v, want it to wrap %v", err, wantErr)
	}

	if conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		t.Errorf("nothing should be listening on %s after OnStart failed", addr)
	}
}

func TestPortAlreadyInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	srv := &Server{Addr: ln.Addr().String(), RegisterRoutes: func(*gin.Engine) {}}
	// bind 是同步的，所以这里会立刻返回而不是挂住
	if err := srv.RunContext(context.Background()); err == nil {
		t.Fatal("want error when the port is taken")
	}
}

// 核心用例：ctx 取消后在途请求必须跑完，而不是被砍断
func TestGracefulShutdownLetsInFlightRequestFinish(t *testing.T) {
	addr := freeAddr(t)
	handlerEntered := make(chan struct{})
	var shutdownCalled atomic.Bool

	srv := &Server{
		Name: "test",
		Addr: addr,
		RegisterRoutes: func(r *gin.Engine) {
			r.GET("/slow", func(c *gin.Context) {
				close(handlerEntered)
				time.Sleep(300 * time.Millisecond)
				c.String(http.StatusOK, "done")
			})
		},
		OnShutdown: func(context.Context) error {
			shutdownCalled.Store(true)
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.RunContext(ctx) }()

	waitListening(t, addr)

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			body <- "request failed: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		content, err := io.ReadAll(resp.Body)
		if err != nil {
			body <- "read failed: " + err.Error()
			return
		}
		body <- string(content)
	}()

	<-handlerEntered // handler 已经在跑了，此刻触发退出
	cancel()

	select {
	case got := <-body:
		if got != "done" {
			t.Fatalf("in-flight request was cut off: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return after ctx was cancelled")
	}

	if !shutdownCalled.Load() {
		t.Error("OnShutdown was not called")
	}
}

// 超时兜底：在途请求超过 ShutdownTimeout 时 Shutdown 返回错误，但 OnShutdown 仍要执行
func TestShutdownTimeoutStillRunsOnShutdown(t *testing.T) {
	addr := freeAddr(t)
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	var shutdownCalled atomic.Bool

	srv := &Server{
		Addr:            addr,
		ShutdownTimeout: 50 * time.Millisecond,
		RegisterRoutes: func(r *gin.Engine) {
			r.GET("/hang", func(c *gin.Context) {
				close(handlerEntered)
				<-releaseHandler
				c.String(http.StatusOK, "late")
			})
		},
		OnShutdown: func(context.Context) error {
			shutdownCalled.Store(true)
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.RunContext(ctx) }()

	waitListening(t, addr)
	go func() {
		resp, err := http.Get("http://" + addr + "/hang")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-handlerEntered
	cancel()

	select {
	case err := <-runErr:
		if err == nil {
			t.Error("want a shutdown timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return")
	}

	// 第一步失败不该跳过第二步
	if !shutdownCalled.Load() {
		t.Error("OnShutdown should run even when http shutdown times out")
	}
	close(releaseHandler)
}

func TestOnShutdownErrorIsReported(t *testing.T) {
	addr := freeAddr(t)
	wantErr := errors.New("close db failed")

	srv := &Server{
		Addr:           addr,
		RegisterRoutes: func(*gin.Engine) {},
		OnShutdown:     func(context.Context) error { return wantErr },
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.RunContext(ctx) }()

	waitListening(t, addr)
	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want it to wrap %v", err, wantErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return")
	}
}

// 内置中间件装上了，使用方中间件也接在后面
func TestBuiltinAndUserMiddlewaresAreInstalled(t *testing.T) {
	addr := freeAddr(t)
	var userMiddlewareRan atomic.Bool

	srv := &Server{
		Addr: addr,
		Middlewares: []gin.HandlerFunc{
			func(c *gin.Context) { userMiddlewareRan.Store(true); c.Next() },
		},
		RegisterRoutes: func(r *gin.Engine) {
			r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.RunContext(ctx) }()
	waitListening(t, addr)

	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// RequestID 中间件必须把 id 回写到响应头
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id missing from response, RequestID middleware not installed")
	}
	if !userMiddlewareRan.Load() {
		t.Error("user middleware did not run")
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("RunContext: %v", err)
	}
}

func TestRunContextReturnsWhenCtxAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv := &Server{Addr: freeAddr(t), RegisterRoutes: func(*gin.Engine) {}}

	done := make(chan error, 1)
	go func() { done <- srv.RunContext(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return for an already-cancelled ctx")
	}
}
