package ginx

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestIDHeader 请求与响应中携带 request id 的头名
const RequestIDHeader = "X-Request-Id"

// requestIDKey 用私有类型做 context key，避免与其他包冲突
type requestIDKey struct{}

// RequestID 从请求头取 request id，没有则生成一个，写入 request context 和响应头。
//
// 必须写回 c.Request 才能让 request id 进入 context.Context，slog 才看得到，
// 所以 handler 里要用 c.Request.Context() 而不是 c 本身。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = rand.Text()
		}
		c.Header(RequestIDHeader, id)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		c.Next()
	}
}

// RequestIDFrom 从 context 取 request id，取不到返回空串
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// WithRequestID 把 request id 注入 context，供脱离 HTTP 请求的场景（如异步任务）沿用同一个 id
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// TraceAttrs 供 logx.Init 使用，把 request id 注入每条日志。
// 签名与 logx.Extractor 一致，直接传入即可，ginx 不必反向依赖 logx。
func TraceAttrs(ctx context.Context) []slog.Attr {
	if id := RequestIDFrom(ctx); id != "" {
		return []slog.Attr{slog.String("requestId", id)}
	}
	return nil
}

// AccessLog 记录每个请求的方法、路径、状态码和耗时。
//
// 要装在 Recovery 之前：panic 会跳过 c.Next() 之后的代码，只有让 Recovery 先把
// panic 收掉、正常返回，这里才记得到那条 500。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.InfoContext(c.Request.Context(), "http request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency", time.Since(start).String(),
			"clientIp", c.ClientIP(),
		)
	}
}

// Recovery 捕获 handler 里的 panic，记录堆栈并返回统一错误响应，不打断进程。
//
// 这里回 HTTP 500 而不是业务约定的 200：panic 是服务故障不是业务结果，
// 网关和监控要看得见。业务失败仍然走 200 + code。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			panicked := recover()
			if panicked == nil {
				return
			}
			slog.ErrorContext(c.Request.Context(), "panic recovered",
				"panic", panicked,
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"stack", string(debug.Stack()),
			)
			c.AbortWithStatusJSON(http.StatusInternalServerError, failResponse(CodeInternalError, "internal error"))
		}()
		c.Next()
	}
}
