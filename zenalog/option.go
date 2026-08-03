package zenalog

import (
	"fmt"
	"strings"
)

// entryOpts Option 的装配目标：Entry 字段 + 本次调用的行为开关
type entryOpts struct {
	entry Entry
	sync  bool
	attrs [][2]string // 先攒着，key 白名单校验在 Logger 里做
}

// Option 埋点参数。参数用 Option 收敛，不因参数组合开新方法。
type Option func(*entryOpts)

// Resource 资源类型与实例 id。
func Resource(resourceType, instanceID string) Option {
	return func(o *entryOpts) {
		o.entry.ResourceType = resourceType
		o.entry.InstanceID = instanceID
	}
}

// Path 资源层级路径，拼法由业务决定。
func Path(resourcePath string) Option {
	return func(o *entryOpts) { o.entry.ResourcePath = resourcePath }
}

// Message 日志正文。
func Message(msg string) Option {
	return func(o *entryOpts) { o.entry.Message = msg }
}

// Changes 结构化 diff。同时写入 Entry.Diff（格式化，人读）与
// Entry.Changes（原样，机读）。
func Changes(changes []Change) Option {
	return func(o *entryOpts) {
		o.entry.Changes = changes
		o.entry.Diff = formatChanges(changes)
	}
}

// Attr 业务自定义标签，可重复；key 必须在 Config.AttrKeys 登记，否则埋点返回
// error——那是调用方 bug 不是背压。注意这条 error 主要在联调阶段起作用（联调时
// 务必检查一次返回值），生产里真正的兜底是 mapping 的 dynamic: strict + Bulk
// 响应体里的 strict_dynamic_mapping_exception 日志。
func Attr(key, value string) Option {
	return func(o *entryOpts) { o.attrs = append(o.attrs, [2]string{key, value}) }
}

// Sync 本次同步直写 ES，绕过异步队列，写结果如实返回调用方——审批留痕类操作
// 用它。注意：优雅退出收尾后（Logger 已 Close）仍在跑的在途请求调它会拿到
// ErrClosed，排查时别当成 ES 故障。
func Sync() Option {
	return func(o *entryOpts) { o.sync = true }
}

// formatChanges 把结构化变更集格式化成人读的 Diff 文本，一行一条
func formatChanges(changes []Change) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range changes {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %s -> %s", c.Field, c.From, c.To)
	}
	return b.String()
}
