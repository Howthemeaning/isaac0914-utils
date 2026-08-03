package zenalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isaac0914/utils/zenalog/es"
)

// ErrClosed Logger 已 Close 后再埋点（含 Sync）返回的错误。
var ErrClosed = errors.New("zenalog: logger closed")

// 配置默认值
const (
	defaultTimeout       = 10 * time.Second
	defaultQueueSize     = 1024
	defaultBatchSize     = 100
	defaultFlushInterval = 200 * time.Millisecond
)

// Config zenalog 配置。标量字段支持 env 覆盖，slice 字段不支持
// （zenserver/config 的 env tag 对 slice 不生效，配了会在加载时报错）。
type Config struct {
	Addresses          []string      `yaml:"addresses"`    // ES 节点，必填
	Index              string        `yaml:"index"`        // 写入索引，必填，约定 {service}-activity-log；必须是具体索引名，不能是 rollover 别名
	SearchIndex        string        `yaml:"search_index"` // 查询索引 pattern，缺省 = Index；只对 AttrKeys 一致的 zenalog 索引成立
	Username           string        `yaml:"username" env:"ES_USERNAME"`
	Password           string        `yaml:"password" env:"ES_PASSWORD"`
	Timeout            time.Duration `yaml:"timeout"`             // ES 请求超时，默认 10s
	QueueSize          int           `yaml:"queue_size"`          // 写缓冲，默认 1024
	BatchSize          int           `yaml:"batch_size"`          // bulk 批次，默认 100
	FlushInterval      time.Duration `yaml:"flush_interval"`      // 攒批上限，默认 200ms
	ExcludedOperations []string      `yaml:"excluded_operations"` // 查询时整组排除
	AttrKeys           []string      `yaml:"attr_keys"`           // attrs 标签键白名单，New 时建进 mapping；新增键要重启才生效
}

// validate 必填与非法值一次报全
func (c Config) validate() error {
	var errs []error
	if len(c.Addresses) == 0 {
		errs = append(errs, errors.New("zenalog: config addresses required"))
	}
	if c.Index == "" {
		errs = append(errs, errors.New("zenalog: config index required"))
	}
	if c.Timeout < 0 {
		errs = append(errs, errors.New("zenalog: config timeout must be >= 0"))
	}
	if c.QueueSize < 0 {
		errs = append(errs, errors.New("zenalog: config queue_size must be >= 0"))
	}
	if c.BatchSize < 0 {
		errs = append(errs, errors.New("zenalog: config batch_size must be >= 0"))
	}
	if c.FlushInterval < 0 {
		errs = append(errs, errors.New("zenalog: config flush_interval must be >= 0"))
	}
	return errors.Join(errs...)
}

// withDefaults 零值填默认
func (c Config) withDefaults() Config {
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	if c.QueueSize == 0 {
		c.QueueSize = defaultQueueSize
	}
	if c.BatchSize == 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = defaultFlushInterval
	}
	return c
}

// bulkStore 写入依赖，*es.Store 满足；测试用 fake 替换
type bulkStore interface {
	Bulk(ctx context.Context, docs []es.Doc) error
}

// entrySearcher 查询依赖，*es.Query 满足；测试用 fake 替换
type entrySearcher interface {
	Search(ctx context.Context, req es.QueryRequest) ([]es.Doc, int64, error)
	AggregateByTrace(ctx context.Context, req es.QueryRequest) ([]es.TraceBucket, error)
}

// Logger 活动日志记录器。New 创建，Close 释放；一个进程可起多个实例。
//
// goroutine 泄露分析：全库只有 1 个 flusher goroutine（run），生命周期 =
// newLogger → Close。Close 关闭输入 channel，flusher 排空缓冲、冲刷剩余后
// close(done) 退出，必终止。同步路径不创建 goroutine。
type Logger struct {
	cfg      Config
	store    bulkStore
	query    entrySearcher
	attrKeys map[string]bool

	ch      chan Entry
	done    chan struct{}
	mu      sync.RWMutex
	closed  bool
	dropped atomic.Int64 // 队列满丢弃的累计计数
}

// New 校验必填（一次报全）、ensure index + 补齐 mapping 与动态 settings，失败
// 返回 error——启动期快速失败，配错的时候正是最需要知道的时候。若 SearchIndex
// 与 Index 不同，额外比对目标 pattern 的 _mapping 与本地 AttrKeys，缺键记 warn
// （不 fail——目标索引可能还没建；缺字段的索引在 attrs 筛选下会被静默排除）。
func New(cfg Config) (*Logger, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	esCfg := es.Config{
		Addresses:   cfg.Addresses,
		Index:       cfg.Index,
		SearchIndex: cfg.SearchIndex,
		AttrKeys:    cfg.AttrKeys,
		Username:    cfg.Username,
		Password:    cfg.Password,
		Timeout:     cfg.Timeout,
	}
	store, err := es.NewStore(esCfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := store.EnsureIndex(ctx); err != nil {
		return nil, fmt.Errorf("zenalog: ensure index: %w", err)
	}
	query := es.NewQuery(esCfg)
	if cfg.SearchIndex != "" && cfg.SearchIndex != cfg.Index {
		mismatch, err := query.CheckSearchMapping(ctx)
		if err != nil {
			slog.Warn("zenalog: check search mapping failed", "search_index", cfg.SearchIndex, "err", err)
		}
		for index, missing := range mismatch {
			slog.Warn("zenalog: search index missing attr keys, attrs filters will silently exclude it",
				"index", index, "missing", missing)
		}
	}
	return newLogger(cfg, store, query), nil
}

// newLogger 装配 Logger 并启动 flusher，依赖注入便于测试
func newLogger(cfg Config, store bulkStore, query entrySearcher) *Logger {
	cfg = cfg.withDefaults()
	attrKeys := make(map[string]bool, len(cfg.AttrKeys))
	for _, k := range cfg.AttrKeys {
		attrKeys[k] = true
	}
	l := &Logger{
		cfg:      cfg,
		store:    store,
		query:    query,
		attrKeys: attrKeys,
		ch:       make(chan Entry, cfg.QueueSize),
		done:     make(chan struct{}),
	}
	go l.run()
	return l
}

// run flusher：攒批（满 BatchSize 或 FlushInterval 到）→ Bulk。写失败记
// slog.Error 后丢弃该批，不阻塞、不重试；条目级失败的 error.type/reason 由
// es.Store.Bulk 逐条记进日志。输入 channel 关闭后排空缓冲、冲刷、close(done)。
func (l *Logger) run() {
	ticker := time.NewTicker(l.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]Entry, 0, l.cfg.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		docs := make([]es.Doc, len(batch))
		for i := range batch {
			docs[i] = toDoc(batch[i])
		}
		ctx, cancel := context.WithTimeout(context.Background(), l.cfg.Timeout)
		if err := l.store.Bulk(ctx, docs); err != nil {
			slog.Error("zenalog: async flush failed, batch dropped", "count", len(batch), "err", err)
		}
		cancel()
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-l.ch:
			if !ok { // Close 了：channel 已排空，冲刷剩余后退出
				flush()
				close(l.done)
				return
			}
			batch = append(batch, e)
			if len(batch) >= l.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close 停收、冲刷剩余、等 flusher 退出。幂等。挂在 zenserver 的 OnShutdown 里。
// ctx 到期先返回 ctx.Err()，flusher 在后台继续把手头的批写完。
func (l *Logger) Close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.ch)
	}
	l.mu.Unlock()

	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Info 记过程与开始。异步路径正常返回 nil（队列满丢弃也返回 nil，记 warn 带
// 计数——不拿背压打扰业务，丢不起的操作用 Sync()）；带 Sync() 时内联写 ES 并
// 返回写结果；Attr 的 key 未登记返回 error；Close 之后返回 ErrClosed。
func (l *Logger) Info(ctx context.Context, operation string, opts ...Option) error {
	return l.log(ctx, operation, StatusInfo, opts)
}

// InfoFinish 记完成/失败，success 决定 Status 是 success 还是 failed，其余行为
// 与 Info 一致。
func (l *Logger) InfoFinish(ctx context.Context, operation string, success bool, opts ...Option) error {
	status := StatusSuccess
	if !success {
		status = StatusFailed
	}
	return l.log(ctx, operation, status, opts)
}

// log 装配 Entry 并按同步/异步分发
func (l *Logger) log(ctx context.Context, operation string, status Status, opts []Option) error {
	o := entryOpts{entry: Entry{
		TraceID:   traceIDFrom(ctx),
		Timestamp: time.Now(),
		Operator:  operatorFrom(ctx),
		Operation: operation,
		Status:    status,
	}}
	for _, opt := range opts {
		opt(&o)
	}
	// attr key 白名单：未登记是调用方 bug，当场报错，不 warn 了事
	var errs []error
	for _, kv := range o.attrs {
		if !l.attrKeys[kv[0]] {
			errs = append(errs, fmt.Errorf("zenalog: attr key %q not registered in AttrKeys", kv[0]))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if len(o.attrs) > 0 {
		o.entry.Attrs = make(map[string]string, len(o.attrs))
		for _, kv := range o.attrs {
			o.entry.Attrs[kv[0]] = kv[1]
		}
	}

	if o.sync {
		l.mu.RLock()
		closed := l.closed
		l.mu.RUnlock()
		if closed {
			return ErrClosed
		}
		return l.store.Bulk(ctx, []es.Doc{toDoc(o.entry)})
	}

	// 异步入队：closed 判定和入队要在同一把读锁下，避免和 Close 的 close(ch) 竞态
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return ErrClosed
	}
	select {
	case l.ch <- o.entry:
	default:
		n := l.dropped.Add(1)
		slog.Warn("zenalog: queue full, entry dropped", "operation", operation, "dropped_total", n)
	}
	return nil
}

// toDoc Entry → ES 线格式：时间戳钉死 UTC 毫秒串 + UnixNano tiebreaker
func toDoc(e Entry) es.Doc {
	changes := make([]es.Change, len(e.Changes))
	for i, c := range e.Changes {
		changes[i] = es.Change{Field: c.Field, From: c.From, To: c.To}
	}
	if len(changes) == 0 {
		changes = nil
	}
	return es.Doc{
		TraceID:      e.TraceID,
		Timestamp:    e.Timestamp.UTC().Format(es.TimeLayout),
		TsNanos:      e.Timestamp.UnixNano(),
		Operator:     e.Operator,
		ResourceType: e.ResourceType,
		InstanceID:   e.InstanceID,
		ResourcePath: e.ResourcePath,
		Operation:    e.Operation,
		Message:      e.Message,
		Diff:         e.Diff,
		Changes:      changes,
		Status:       string(e.Status),
		Attrs:        e.Attrs,
	}
}
