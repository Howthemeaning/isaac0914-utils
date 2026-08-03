// Package zenserver 用声明式配置启动一个 gin HTTP server，负责路由装配、
// 信号监听和优雅退出。
//
// 配置加载和日志初始化在 config 和 logx 子包，由调用方在 Run 之前自己完成——
// 这两步必须在 OnStart 之前就绪，把顺序摊在 main 里比埋在库内部清楚：
//
//	cfg := &Config{Addr: ":8080"}                    // 默认值写在这里
//	if err := config.LoadInto("config.yaml", cfg); err != nil {
//	    return err
//	}
//	if err := logx.Init(cfg.Log, ginx.TraceAttrs); err != nil {
//	    return err
//	}
//
//	srv := &zenserver.Server{
//	    Name:           "myapp",
//	    Addr:           cfg.Addr,
//	    RegisterRoutes: registerRoutes,
//	    OnStart:        func(ctx context.Context) error { return initDB(ctx, cfg.DB) },
//	}
//	return srv.Run()
package zenserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Howthemeaning/isaac0914-utils/zenserver/ginx"
)

// defaultShutdownTimeout 优雅退出的默认等待上限
const defaultShutdownTimeout = 30 * time.Second

// Server 一个 HTTP 服务实例。状态全部挂在实例上，没有包级可变全局，
// 所以一个进程里可以起多个。
type Server struct {
	// Addr HTTP 监听地址，如 ":8080"。必填。
	Addr string
	// RegisterRoutes 注册业务路由。必填。
	RegisterRoutes func(r *gin.Engine)

	// Name 服务名，出现在启动和退出日志里。
	Name string
	// ReleaseMode 为 true 时关闭 gin 的 debug 输出。注意 gin 的模式是全局的。
	ReleaseMode bool
	// Middlewares 追加在内置中间件（RequestID、AccessLog、Recovery）之后。
	Middlewares []gin.HandlerFunc
	// OnStart 在开始监听之前执行，用于连接数据库等初始化。
	// 返回 error 则不会进入监听，Run 直接返回该 error。
	OnStart func(ctx context.Context) error
	// OnShutdown 在 HTTP server 关闭之后执行，用于释放资源。
	OnShutdown func(ctx context.Context) error
	// ShutdownTimeout 优雅退出的最长等待时间，默认 30s。
	ShutdownTimeout time.Duration
}

// Run 启动服务并阻塞，收到 SIGINT 或 SIGTERM 后优雅退出。
func (s *Server) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return s.RunContext(ctx)
}

// RunContext 启动服务并阻塞，ctx 取消后优雅退出。
// 需要把服务嵌进更大的生命周期、或者不想让库接管信号时用这个。
func (s *Server) RunContext(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	if s.ReleaseMode {
		gin.SetMode(gin.ReleaseMode)
	}

	if s.OnStart != nil {
		if err := s.OnStart(ctx); err != nil {
			return fmt.Errorf("zenserver: OnStart: %w", err)
		}
	}

	// 先同步 bind，端口占用之类的错误立刻返回，不用和退出信号抢 select
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("zenserver: listen %s: %w", s.Addr, err)
	}

	srv := &http.Server{Handler: s.router()}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	slog.Info("http server listening", "service", s.Name, "addr", listener.Addr().String())

	select {
	case err := <-serveErr:
		return fmt.Errorf("zenserver: serve: %w", err)
	case <-ctx.Done():
	}

	return s.shutdown(srv)
}

// shutdown 先等在途请求结束，再执行 OnShutdown。
// 两步的错误汇总返回，不因为第一步失败就跳过第二步。
func (s *Server) shutdown(srv *http.Server) error {
	slog.Info("shutting down", "service", s.Name)

	timeout := s.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var errs []error
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
	}
	if s.OnShutdown != nil {
		if err := s.OnShutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("OnShutdown: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("zenserver: %w", err)
	}

	slog.Info("shutdown complete", "service", s.Name)
	return nil
}

// router 装配内置中间件、使用方中间件和业务路由
func (s *Server) router() *gin.Engine {
	r := gin.New()
	// AccessLog 必须在 Recovery 之前：panic 会跳过 c.Next() 之后的代码，
	// 只有让 Recovery 先收掉 panic 并正常返回，AccessLog 才记得到那条 500
	r.Use(ginx.RequestID(), ginx.AccessLog(), ginx.Recovery())
	r.Use(s.Middlewares...)
	s.RegisterRoutes(r)
	return r
}

// validate 校验必填字段，缺失时一次报全，不让人试一次改一个
func (s *Server) validate() error {
	var errs []error
	if s.Addr == "" {
		errs = append(errs, errors.New("Addr is required"))
	}
	if s.RegisterRoutes == nil {
		errs = append(errs, errors.New("RegisterRoutes is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("zenserver: invalid config: %w", err)
	}
	return nil
}
