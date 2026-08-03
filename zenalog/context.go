package zenalog

import (
	"context"

	"github.com/Howthemeaning/isaac0914-utils/zenserver/ginx"
)

// 私有类型做 context key，避免与其他包冲突
type operatorKey struct{}
type traceIDKey struct{}

// WithOperator 把操作人注入 context，在入口（HTTP handler、saga action、
// agent listener）调用一次，埋点 API 不用到处传操作人。
func WithOperator(ctx context.Context, operator string) context.Context {
	return context.WithValue(ctx, operatorKey{}, operator)
}

// WithTraceID 把 traceID 注入 context，异步任务用它沿用同一个 id。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

func operatorFrom(ctx context.Context) string {
	v, _ := ctx.Value(operatorKey{}).(string)
	return v
}

// traceIDFrom 显式注入优先，缺省回落 ginx 的 request id——和 zenserver 的
// RequestID 中间件天然衔接，HTTP 入口无需手动注入。
func traceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey{}).(string); ok && v != "" {
		return v
	}
	return ginx.RequestIDFrom(ctx)
}
