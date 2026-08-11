// Package es 封装 zenalog 与 Elasticsearch 6 的通信：Doc 线格式、Store（建索引
// 与批量写入）、Query（flat 与聚合查询）。只用 stdlib，不 import 主包。
// 线格式（typeless mapping，ES 7+）、settings、mapping 细节全部收敛在本包内。
package es

import "time"

// TimeLayout Doc.Timestamp 的序列化格式：UTC 毫秒。不用 time.Time 默认 JSON——
// 那是 RFC3339 纳秒带本地时区偏移，精度超出 ES date 的毫秒，时区还随部署环境漂。
const TimeLayout = "2006-01-02T15:04:05.000Z"

// Config ES 连接与索引配置。
type Config struct {
	Addresses   []string      // ES 节点，必填
	Index       string        // 写入索引（具体单索引），必填
	SearchIndex string        // 查询索引 pattern，NewQuery 处缺省回填为 Index
	AttrKeys    []string      // attrs 标签键白名单：EnsureIndex 建/补 mapping、CheckSearchMapping 比对都用它
	Username    string        // basic auth 用户名，留空则不带认证
	Password    string        // basic auth 密码
	Timeout     time.Duration // 单次请求超时，默认 10s
}

// Doc ES 文档线格式，json tag 与 mapping 对应。Timestamp 是按 TimeLayout 格式化
// 好的字符串；TsNanos 是同一时刻的 UnixNano，做同毫秒并列的排序 tiebreaker。
type Doc struct {
	TraceID      string            `json:"trace_id,omitempty"`
	Timestamp    string            `json:"timestamp"`
	TsNanos      int64             `json:"ts_nanos"`
	Operator     string            `json:"operator,omitempty"`
	ResourceType string            `json:"resource_type,omitempty"`
	InstanceID   string            `json:"instance_id,omitempty"`
	ResourcePath string            `json:"resource_path,omitempty"`
	Operation    string            `json:"operation"`
	Message      string            `json:"message,omitempty"`
	Diff         string            `json:"diff,omitempty"`
	Changes      []Change          `json:"changes,omitempty"`
	Status       string            `json:"status"`
	Attrs        map[string]string `json:"attrs,omitempty"`
}

// Change 结构化 diff 的单条变更，随 Doc 存进 ES。
type Change struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}
