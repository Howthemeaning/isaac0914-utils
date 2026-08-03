package zenalog

import "time"

// Status 日志条目的状态，显式三态——公开 API 发布后不改签名，发布前就用枚举钉死。
type Status string

// Status 取值
const (
	StatusInfo    Status = "info"    // 过程日志（Info）
	StatusSuccess Status = "success" // 操作成功（InfoFinish success=true）
	StatusFailed  Status = "failed"  // 操作失败（InfoFinish success=false）
)

// Entry 活动日志条目，对齐 zrbiz ALogEntry，v2 增 Attrs/Status/Changes。
// Timestamp 由库在埋点时填，其余字段由 Option 与 ctx 装配。
// json tag 是 GinHandler 的响应形状（camelCase），ES 线格式在 es.Doc。
type Entry struct {
	TraceID      string            `json:"traceId"`
	Timestamp    time.Time         `json:"timestamp"`
	Operator     string            `json:"operator"`
	ResourceType string            `json:"resourceType"` // 业务自定义，建议定义常量
	InstanceID   string            `json:"instanceId"`
	ResourcePath string            `json:"resourcePath"` // 资源层级路径，拼法由业务决定
	Operation    string            `json:"operation"`    // 如 "create vll"
	Message      string            `json:"message"`
	Diff         string            `json:"diff"`              // Changes 的格式化结果（人读）
	Changes      []Change          `json:"changes,omitempty"` // 结构化 diff（机读，支撑前后对比 UI）
	Status       Status            `json:"status"`
	Attrs        map[string]string `json:"attrs,omitempty"` // 业务自定义标签；key 必须在 AttrKeys 登记
}
