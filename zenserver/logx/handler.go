package logx

import (
	"context"
	"errors"
	"log/slog"
	"slices"
)

// multiHandler 把每条日志广播给多个 handler，各自按自己的级别决定收不收。
// 手写这几十行是为了省掉 samber/slog-multi 带来的三个传递依赖。
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) slog.Handler {
	return multiHandler{handlers: handlers}
}

// Enabled 任一 handler 收就算开启
func (h multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, sub := range h.handlers {
		if sub.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, sub := range h.handlers {
		if !sub.Enabled(ctx, r.Level) {
			continue
		}
		// Clone 避免多个 handler 共享 Record 的 attrs 底层数组
		if err := sub.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, sub := range h.handlers {
		next[i] = sub.WithAttrs(attrs)
	}
	return multiHandler{handlers: next}
}

func (h multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, sub := range h.handlers {
		next[i] = sub.WithGroup(name)
	}
	return multiHandler{handlers: next}
}

// traceHandler 记录日志前把 context 里的追踪字段补进 Record。
//
// 追踪字段必须落在顶层。如果调用方开过 WithGroup，直接往 Record 里 AddAttrs 会让
// requestId 掉进那个 group，按 requestId 查日志就失效了。所以开过 group 时从 root
// 重建一条链，把追踪字段挂在 group 之前；没开 group 时走 AddAttrs 快路径。
type traceHandler struct {
	root       slog.Handler                      // 最初的 handler，未应用任何 WithAttrs/WithGroup
	base       slog.Handler                      // root 应用完 mods 的结果，缓存下来供快路径用
	mods       []func(slog.Handler) slog.Handler // root → base 的重放链
	grouped    bool                              // 开过 group 才需要走重建路径
	extractors []Extractor
}

func newTraceHandler(base slog.Handler, extractors []Extractor) slog.Handler {
	if len(extractors) == 0 {
		return base
	}
	return traceHandler{root: base, base: base, extractors: extractors}
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := h.collect(ctx)

	if !h.grouped {
		if len(attrs) > 0 {
			// Handle 收到的 Record 不允许原地修改
			r = r.Clone()
			r.AddAttrs(attrs...)
		}
		return h.base.Handle(ctx, r)
	}

	target := h.root
	if len(attrs) > 0 {
		target = target.WithAttrs(attrs)
	}
	for _, mod := range h.mods {
		target = mod(target)
	}
	return target.Handle(ctx, r)
}

// collect 依次执行 extractor 收集追踪字段
func (h traceHandler) collect(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	for _, extract := range h.extractors {
		attrs = append(attrs, extract(ctx)...)
	}
	return attrs
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(func(inner slog.Handler) slog.Handler { return inner.WithAttrs(attrs) }, h.grouped)
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(func(inner slog.Handler) slog.Handler { return inner.WithGroup(name) }, true)
}

// with 记下一次派生操作。Clip 是为了两个 logger 从同一个父 logger 分叉时不互相踩 mods
func (h traceHandler) with(mod func(slog.Handler) slog.Handler, grouped bool) slog.Handler {
	return traceHandler{
		root:       h.root,
		base:       mod(h.base),
		mods:       append(slices.Clip(h.mods), mod),
		grouped:    grouped,
		extractors: h.extractors,
	}
}
