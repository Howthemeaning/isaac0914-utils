package zenalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Howthemeaning/isaac0914-utils/zenalog/es"
)

// 查询默认值与上限
const (
	defaultPageSize   = 20
	maxPageSize       = 100
	maxResultWindow   = 10000 // ES max_result_window 默认值，from+size 超出直接拦
	defaultTimeWindow = 24 * time.Hour
)

// errInvalidRequest 查询参数校验失败的哨兵，GinHandler 用它区分 400 与 500
var errInvalidRequest = errors.New("zenalog: invalid list request")

// Mode 查询模式。
type Mode int

// 查询模式取值
const (
	ModeFlat    Mode = iota // 平铺：一行一条日志 + Total（默认，CMI 的操作日志页）
	ModeByTrace             // 按 traceId 聚合成时间线（zrbiz 形状）
)

// Op 精确过滤的比较方式。
type Op int

// Op 取值
const (
	OpEq     Op = iota // 等值
	OpNe               // 不等
	OpPrefix           // 前缀
)

// Condition 精确过滤条件。Field 取 resource_type | instance_id | operation |
// operator | attrs.<key>（key 必须在 AttrKeys 登记）；attrs 的等值/前缀查询由
// 库内部改写到 .keyword 子字段，调用方只写 attrs.<key>。
type Condition struct {
	Field string
	Op    Op
	Value string
}

// ListRequest 查询参数。
type ListRequest struct {
	Mode       Mode
	StartTime  time.Time // 默认近 24 小时
	EndTime    time.Time
	PageNum    int         // 默认 1
	PageSize   int         // 默认 20，上限 100；from+size 超 10000 返回错误（ES max_result_window）
	Query      string      // 关键词：message/diff/attrs 上匹配；keyword 字段整值相等才命中
	Conditions []Condition // 精确过滤
	InstanceID string      // 按资源实例过滤的快捷入口，等价于 Condition{Field:"instance_id",Op:OpEq}
}

// Bucket by-trace 模式的一组：同 traceId 的日志按时间升序。
type Bucket struct {
	TraceID   string    `json:"traceId"`
	StartTime time.Time `json:"startTime"` // 组内最早一条的时间
	Logs      []Entry   `json:"logs"`      // 组内明细，时间升序
}

// ListResult 用结构体包一层而不直接返回切片：将来加字段不动 List 签名。
// flat 模式填 Entries/Total，by-trace 模式填 Buckets。
type ListResult struct {
	Entries []Entry  `json:"entries"`           // flat 模式：当前页的行
	Total   int64    `json:"total"`             // flat 模式：命中总数（发 track_total_hits 保证精确）
	Buckets []Bucket `json:"buckets,omitempty"` // by-trace 模式：当前页的组
}

// conditionFields 精确过滤允许的非 attrs 字段
var conditionFields = map[string]bool{
	"resource_type": true,
	"instance_id":   true,
	"operation":     true,
	"operator":      true,
}

// List 查询活动日志。参数校验失败返回 errInvalidRequest 包装的错误（不发请求），
// 查询失败原样返回。
func (l *Logger) List(ctx context.Context, req ListRequest) (*ListResult, error) {
	esReq, err := l.buildQueryRequest(req)
	if err != nil {
		return nil, err
	}
	switch req.Mode {
	case ModeFlat:
		docs, total, err := l.query.Search(ctx, esReq)
		if err != nil {
			return nil, err
		}
		entries := make([]Entry, len(docs))
		for i := range docs {
			entries[i] = fromDoc(docs[i])
		}
		return &ListResult{Entries: entries, Total: total}, nil
	case ModeByTrace:
		traceBuckets, err := l.query.AggregateByTrace(ctx, esReq)
		if err != nil {
			return nil, err
		}
		buckets := make([]Bucket, len(traceBuckets))
		for i, tb := range traceBuckets {
			logs := make([]Entry, len(tb.Docs))
			for j := range tb.Docs {
				logs[j] = fromDoc(tb.Docs[j])
			}
			buckets[i] = Bucket{TraceID: tb.TraceID, StartTime: tb.StartTime, Logs: logs}
		}
		return &ListResult{Entries: []Entry{}, Buckets: buckets}, nil
	default:
		return nil, fmt.Errorf("%w: unknown mode %d", errInvalidRequest, req.Mode)
	}
}

// buildQueryRequest 填默认值、校验上限与条件字段，转成 es.QueryRequest
func (l *Logger) buildQueryRequest(req ListRequest) (es.QueryRequest, error) {
	var zero es.QueryRequest
	var errs []error

	pageNum := req.PageNum
	switch {
	case pageNum == 0:
		pageNum = 1
	case pageNum < 0:
		errs = append(errs, fmt.Errorf("page_num must be >= 1, got %d", pageNum))
	}
	pageSize := req.PageSize
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 0 || pageSize > maxPageSize:
		errs = append(errs, fmt.Errorf("page_size must be in [1, %d], got %d", maxPageSize, pageSize))
	}

	end := req.EndTime
	if end.IsZero() {
		end = time.Now()
	}
	start := req.StartTime
	if start.IsZero() {
		start = end.Add(-defaultTimeWindow)
	}

	conds := make([]es.Cond, 0, len(req.Conditions)+1)
	for _, c := range req.Conditions {
		if err := l.validateConditionField(c.Field); err != nil {
			errs = append(errs, err)
			continue
		}
		op, err := opString(c.Op)
		if err != nil {
			errs = append(errs, fmt.Errorf("condition on %q: %w", c.Field, err))
			continue
		}
		conds = append(conds, es.Cond{Field: c.Field, Op: op, Value: c.Value})
	}
	if req.InstanceID != "" {
		conds = append(conds, es.Cond{Field: "instance_id", Op: es.OpEq, Value: req.InstanceID})
	}

	if len(errs) > 0 {
		return zero, fmt.Errorf("%w: %w", errInvalidRequest, errors.Join(errs...))
	}
	// 深分页拦截要在分页值合法之后算
	if from := (pageNum - 1) * pageSize; from+pageSize > maxResultWindow {
		return zero, fmt.Errorf("%w: page %d x %d exceeds ES max_result_window %d, narrow the time range instead",
			errInvalidRequest, pageNum, pageSize, maxResultWindow)
	}

	return es.QueryRequest{
		Start:    start,
		End:      end,
		From:     (pageNum - 1) * pageSize,
		Size:     pageSize,
		Query:    req.Query,
		Conds:    conds,
		Excluded: l.cfg.ExcludedOperations,
	}, nil
}

// validateConditionField 字段白名单：普通字段查表，attrs.<key> 查 AttrKeys——
// 未登记的 key 在 mapping 里没有字段，ES 对未映射字段是"不匹配"而非报错，
// 放过去就是静默空结果
func (l *Logger) validateConditionField(field string) error {
	if key, ok := strings.CutPrefix(field, "attrs."); ok {
		if !l.attrKeys[key] {
			return fmt.Errorf("attr key %q not registered in AttrKeys", key)
		}
		return nil
	}
	if !conditionFields[field] {
		return fmt.Errorf("unknown condition field %q", field)
	}
	return nil
}

func opString(op Op) (string, error) {
	switch op {
	case OpEq:
		return es.OpEq, nil
	case OpNe:
		return es.OpNe, nil
	case OpPrefix:
		return es.OpPrefix, nil
	default:
		return "", fmt.Errorf("unknown op %d", op)
	}
}

// fromDoc ES 线格式 → Entry。时间戳按 TimeLayout 解析，解析不动退 RFC3339，
// 再不行留零值——查询侧坏一条时间不至于整页失败
func fromDoc(d es.Doc) Entry {
	ts, err := time.Parse(es.TimeLayout, d.Timestamp)
	if err != nil {
		ts, _ = time.Parse(time.RFC3339Nano, d.Timestamp)
	}
	changes := make([]Change, len(d.Changes))
	for i, c := range d.Changes {
		changes[i] = Change{Field: c.Field, From: c.From, To: c.To}
	}
	if len(changes) == 0 {
		changes = nil
	}
	return Entry{
		TraceID:      d.TraceID,
		Timestamp:    ts,
		Operator:     d.Operator,
		ResourceType: d.ResourceType,
		InstanceID:   d.InstanceID,
		ResourcePath: d.ResourcePath,
		Operation:    d.Operation,
		Message:      d.Message,
		Diff:         d.Diff,
		Changes:      changes,
		Status:       Status(d.Status),
		Attrs:        d.Attrs,
	}
}
