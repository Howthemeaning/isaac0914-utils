# zenalog 设计文档

- 日期：2026-07-31（v2.4 修订）
- 状态：修订中——已纳入 CMI 四轮评审反馈与 zrbiz/ES 6.8 源码级复核，待定稿
- 模块路径：`github.com/Howthemeaning/isaac0914-utils/zenalog`

## 1. 背景与目标

zrbiz（Java）的活动日志是公司的既有标准：业务代码手动埋点 `ALOG.info` /
`ALOG.infoFinish`，外部 starter（`activity-log-boot-starter`）把日志写进 ES，
查询侧按 `traceId` 聚合，一次操作的多条日志展示成一条时间线。

Go 侧需要同样的能力。目标：把这套标准翻成 Go 库，使用方 `New` 一个 `Logger`
就能埋点，挂一个 handler 就有 `/activityLog` 查询——如同 zrbiz 服务调用 starter。

zrbiz 侧的架构（调研结论）：

```
业务手动埋点 ALOG.info / ALOG.infoFinish
  → starter 装配 ALogEntry（traceId/operator/operation/message/diff/success/...）
  → 写 ES
  → 查询：bool 过滤 + terms 按 traceId 聚合 + top_hits 取组内明细 + bucket_sort 分页
```

操作人在 Saga Action / Agent Listener 入口通过 `ALogContextService.registerOperator`
注册；diff 用 javers 对比变更前后对象（`*LDiff` 读 DB 快照、overlay 请求字段再 diff）。

不直接照搬的点：starter 是黑盒外部依赖，Go 版全部落在本库；javers 没有 Go 对应物，
自写反射 diff；Spring Data ES 换成 stdlib `net/http` 裸 REST。

### 第一个使用方：CMI

v1 设计时暂无使用方，查询侧完全按 zrbiz 的 traceId 时间线形状设计。CMI
（`cmi-server`，需求见 `cmi-workspace/doc/product.md` 71-77 行）评审后确认其
「操作日志」页面是**平铺可筛选表格**：展示时间、客户信息、操作类型、操作内容，
按时间段和客户信息筛选；登录日志与操作日志在同一个列表。v2 以 CMI 为设计伙伴
修订查询侧，并补充两个 zrbiz 没有的 Entry 维度（attrs 标签、显式 status 枚举）。

## 2. 范围

### 包含（v2）

| 能力 | 说明 |
|---|---|
| Entry 模型 | 对齐 zrbiz `ALogEntry`，v2 增加 attrs、status、结构化 changes |
| 写入 API | `Info` / `InfoFinish`，默认异步批量落 ES；`Sync()` 选项同步直写 |
| 反射 diff | before/after 结构体字段级对比；slice 用编辑距离对齐 |
| ES 存储 | stdlib `net/http` 裸调 Index/Bulk/Search API，零新增依赖 |
| 查询 | 双模式：flat（平铺 + Total，默认）/ by-trace（traceId 聚合时间线） |
| gin handler | 一行接入 `/activityLog`，走 `ginx` 响应壳 |
| 上下文助手 | operator / traceID 经 `context.Context` 传递 |
| 多索引查询 | 查询侧索引支持通配 pattern，跨服务日志一个列表 |

### 不包含（明确排除）

- **ResourceType 公司级枚举。** zrbiz 的枚举集中在 starter 里管理，Go 版用 `string`，
  业务自定义常量，有第二个使用方后再沉淀。
- **结构化 ResourcePath 树。** zrbiz 的 `PathGenerateServiceImpl` 为各资源构造
  层级路径树。v2 仍为字符串，路径拼法由业务决定。
- **审计日志系统。** zrbiz 里 `@AuditLogTrace` 那套是独立系统，不在本库范围。
  库的 `Sync()` 提供「日志写失败则业务失败」的留痕能力，但要如实认识窗口：
  业务改库与写日志是两个写，没有事务性 outbox 就存在双写窗口——先改库后写
  日志，ES 失败时是「业务已改、日志没落、还告诉调用方失败」，用户重试可能
  重复变更；先写日志后改库则可能「记了没发生的事」。`Sync()` 只把窗口缩到
  最小，不是取证保证。审批留痕要做到什么程度，待产品确认日志定位（见第 8 节）。
- **ES 索引生命周期管理（ILM）、集群运维。** 库只 ensure index 存在，rollover/retention
  是运维的事。

## 3. 从 zrbiz 借鉴与修正的点

### 借鉴

1. **手动埋点、两段式记录。** `Info` 记过程与开始，`InfoFinish` 记完成/失败。
   不做注解/AOP 式自动埋点——zrbiz 证明了手动埋点表达力足够，且 Go 没有注解。
2. **traceId 聚合展示。** 一次操作产生多条 ES 文档，按 traceId 聚合成时间线——
   保留为 by-trace 查询模式。
3. **操作人走上下文。** zrbiz 在 Saga/Agent 入口 `registerOperator`，Go 版对应
   `WithOperator(ctx, name)`，埋点 API 不用到处传操作人。
4. **diff 是核心卖点。** 变更类操作必须能回答"改了什么"。
5. **查询侧排除吵闹的 operation。** zrbiz 整组排除 `tunnel path changed` 之类
   切路日志防刷屏，保留为 `ExcludedOperations` 配置。
6. **slice 用编辑距离对齐。** starter 的 `JAVERS.java:13-15` 显式配置
   `LEVENSHTEIN_DISTANCE`（javers 6.14.0 的默认是 Simple 按位比较），zrbiz 的全部
   diff 都经这个实例（`CloneFunction.diff()` → `JAVERS.compare`）。本库对齐这个行为。
7. **diff 双份存储。** starter 同时存 `diff`（可读文本）与 `jsonDiff`（javers
   序列化的 JSON 字符串）（`ALOG.java:97-98`）。本库同样双存：人读一份 + 机读一份。

### 修正

| zrbiz 的做法 | 本库的做法 | 理由 |
|---|---|---|
| starter 外部依赖，实现黑盒 | 全部落在本库 | Go 侧没有那个 starter，也不想要黑盒 |
| javers 做 diff | 自写反射 diff | javers 没有 Go 对应物 |
| `jsonDiff` 存 javers 序列化的 JSON 字符串 | `changes` 存**结构化对象数组** | JSON 字符串要查出来再解析才能用；对象数组的 field/from/to 可直接过滤，支撑前后对比 UI（双份存储本身是借鉴，这里是把机读那份从字符串升级成结构） |
| 查询只有 traceId 聚合一种形状 | **flat / by-trace 双模式**，flat 默认 | CMI 的页面是平铺表格，按行分页要 Total；时间线留作 opt-in |
| Spring Data ES + `ElasticsearchRestTemplate` | stdlib `net/http` 裸 REST | 用到的 ES 面只有 Bulk 和 Search 两个端点，引官方 client 会把传递依赖传染给每个使用方 |
| `ALOG` 静态类 + 静态 `ExecutorService`，每条日志一次 `repository.save()` 单条写 | `Logger` 实例 + 单 flusher 攒批 Bulk | 本库规约禁止包级可变全局状态（一个进程要能起多个实例）；单条写每条一次 HTTP 往返，攒批把往返合并 |
| ResourceType 枚举集中在 starter | `string`，业务自定义常量 | 有第二个使用方再沉淀 |
| ResourcePath 结构化树 | `string` | 层级树的消费端是 zrbiz 前端，Go 侧暂无此需求 |

### 依赖取舍

**零新增第三方依赖。** ES 通信用 stdlib `net/http` + `encoding/json`；gin handler
用的 gin 和 `ginx` 是模块既有依赖。目标是公司 ES 6 集群（见第 9 节），Bulk/Search
是稳定老接口，typed 线格式细节全部收敛在 es 包内，裸调的维护风险可控。

## 4. 架构

两个包，依赖单向：

```
zenalog/       Entry、Logger、反射 diff、context 助手、查询 API、gin handler
  └── 依赖 zenalog/es、zenserver/ginx（GinHandler 与 traceID 的 request id 回落）
zenalog/es/    Doc 线格式、Store（ensure index + bulk 写入）、Query（flat + 聚合查询）
  └── 零依赖（stdlib）
```

主包 → es 单向。`Entry` 是领域模型，es 包定义自己的 `Doc` 线格式，主包负责
Entry↔Doc 映射——避免循环依赖，也把 ES 线格式隔离在一个包里。

### 写入数据流

```
异步（默认）：
业务: logger.Info(ctx, "create vll", opts...)
  → 装配 Entry（operator/traceID 从 ctx 取）
  → 投入缓冲 channel（QueueSize）
  → flusher goroutine 攒批（满 BatchSize 或 FlushInterval 到）
  → es.Store.Bulk → POST /{index}/_bulk

同步（Sync() 选项）：
业务: logger.Info(ctx, "approve", zenalog.Sync(), ...)
  → 装配 Entry
  → 跳过队列，内联 es.Store.Bulk（单条）
  → ES 的 ack/error 直接返回调用方
```

**异步落库是默认的刻意取舍**：活动日志不能阻塞业务。代价是异步路径有丢失窗口
（见 6.1），所以审批留痕类操作必须用 `Sync()`——机制在库里，哪些操作用同步、
写失败时让不让业务失败，是使用方的策略。

**goroutine 泄露分析**：全库只有 1 个 flusher goroutine，生命周期 = New → Close。
`Close(ctx)` 关闭输入、冲刷剩余、等 flusher 退出，必终止。同步路径不创建
goroutine。Close 后再调 Info/InfoFinish（含同步）返回 `ErrClosed`。

### 查询数据流

```
flat 模式（默认）：
GinHandler / logger.List(ctx, req)
  → es.Query.Search → POST /{searchIndex}/_search
  → bool 过滤 + from/size 按行分页，hits.total 取总数（ES 6 恒精确）
  → []Entry + Total

by-trace 模式：
  → es.Query.AggregateByTrace → POST /{searchIndex}/_search
  → terms 按 trace_id 分组 + top_hits 取组内明细（时间升序）
    + bucket_sort 按组内最早时间倒序分页
  → []Bucket{TraceID, StartTime, Logs []Entry}
```

## 5. 各包接口

### 5.1 zenalog

```go
type Config struct {
    Addresses    []string      `yaml:"addresses"`                 // ES 节点，必填
    Index        string        `yaml:"index"`                     // 写入索引，必填，约定 {service}-activity-log
    SearchIndex  string        `yaml:"search_index"`              // 查询索引 pattern，缺省 = Index；支持通配与逗号多索引，但只对 AttrKeys 一致的 zenalog 索引成立（见 6.1 第 9 条）
    Username     string        `yaml:"username" env:"ES_USERNAME"`
    Password     string        `yaml:"password" env:"ES_PASSWORD"`
    Timeout      time.Duration `yaml:"timeout"`                   // ES 请求超时，默认 10s
    QueueSize    int           `yaml:"queue_size"`                // 写缓冲，默认 1024
    BatchSize    int           `yaml:"batch_size"`                // bulk 批次，默认 100
    FlushInterval time.Duration `yaml:"flush_interval"`           // 攒批上限，默认 200ms
    ExcludedOperations []string `yaml:"excluded_operations"`      // 查询时整组排除
    AttrKeys     []string      `yaml:"attr_keys"`                 // attrs 允许的标签键白名单，New 时建进 mapping。用了 Attr() 就必须登记；不登记则任何 Attr() 都报错——安全默认。新增键要重启才生效（New 会补 mapping，见 6.1 第 10 条）；跨索引查询时各服务必须一致（第 9 条）
}

// New 校验必填（一次报全）、ensure index + 补齐 mapping 与动态 settings（AttrKeys
// 白名单、max_inner_result_window，见 5.3），失败返回 error。若 SearchIndex 与
// Index 不同，额外调 es.Query.CheckSearchMapping 拉目标 pattern 的 _mapping 与本地
// AttrKeys 比对，不一致记 warn（不 fail——目标索引可能还没建）。
func New(cfg Config) (*Logger, error)

// Close 停收、冲刷、等 flusher 退出。幂等。挂在 zenserver 的 OnShutdown 里。
func (l *Logger) Close(ctx context.Context) error
```

```go
// Status 日志条目的状态，显式三态（不用 *bool 指针三态——公开 API 发布后不改签名，
// 发布前就用枚举钉死）。
type Status string

const (
    StatusInfo    Status = "info"    // 过程日志（Info）
    StatusSuccess Status = "success" // 操作成功（InfoFinish）
    StatusFailed  Status = "failed"  // 操作失败（InfoFinish）
)

// Entry 活动日志条目，对齐 zrbiz ALogEntry，v2 增 Attrs/Status/Changes。
// Timestamp 由库在埋点时填，其余字段由 Option 与 ctx 装配。
type Entry struct {
    TraceID      string
    Timestamp    time.Time
    Operator     string
    ResourceType string            // 业务自定义，建议定义常量
    InstanceID   string
    ResourcePath string            // 资源层级路径，拼法由业务决定
    Operation    string            // 如 "create vll"
    Message      string
    Diff         string            // Changes 的格式化结果（人读）
    Changes      []Change          // 结构化 diff（机读，支撑前后对比 UI）
    Status       Status
    Attrs        map[string]string // 业务自定义标签，如 CMI 的 Attr("customer", name)；可过滤可模糊搜
}

// 埋点 API。参数用 Option 收敛，不因参数组合开新方法。
// 异步路径正常返回 nil：队列满丢弃也返回 nil（记 warn 带计数），不拿背压打扰业务。
// 带 Sync() 时内联写 ES 并返回写结果——审批留痕类操作用它，调用方决定写失败
// 是否让业务失败。Close 之后调用（含同步）返回 ErrClosed。
// Attr 的 key 未在 AttrKeys 登记也返回 error——那是调用方 bug 不是背压，不能记 warn
// 了事（warn 等于静默丢 attr，正是 6.1 第 9 条批评的残缺模式）。
// 但这条 error 的实际效力要打折：日志调用的返回值几乎没人检查，
// `logger.Info(ctx, "approve", ...)` 后面接 `if err != nil` 的代码很少见。所以它主要
// 在联调阶段起作用（联调时务必检查一次返回值），生产里真正的兜底是 mapping 的
// dynamic: strict + Bulk 响应体里的 strict_dynamic_mapping_exception 日志。
// 别指望这个 error 在生产里救人。
func (l *Logger) Info(ctx context.Context, operation string, opts ...Option) error
func (l *Logger) InfoFinish(ctx context.Context, operation string, success bool, opts ...Option) error

func Resource(resourceType, instanceID string) Option
func Path(resourcePath string) Option
func Message(msg string) Option
func Changes(changes []Change) Option   // 同时写入 Entry.Diff（格式化）与 Entry.Changes（原样）
func Attr(key, value string) Option     // 可重复；key 必须在 Config.AttrKeys 登记，否则埋点返回 error
func Sync() Option                      // 本次同步直写 ES，绕过异步队列。注意：优雅退出收尾后
                                        // （Logger 已 Close）仍在跑的超时在途请求调它会拿到
                                        // ErrClosed——业务会看到「日志写失败」，排查时别当成 ES 故障
```

```go
// 上下文助手。operator/traceID 在入口（HTTP handler、saga action、agent listener）注入。
func WithOperator(ctx context.Context, operator string) context.Context
func WithTraceID(ctx context.Context, traceID string) context.Context

// traceID 缺省回落 ginx.RequestIDFrom(ctx)——和 zenserver 的 RequestID 中间件
// 天然衔接，HTTP 入口无需手动注入；异步任务用 WithTraceID 沿用同一个 id。
```

```go
// 反射 diff，对应 javers。
type Change struct {
    Field string // 嵌套路径，如 "spec.bandwidth"
    From  string
    To    string
}

func DiffStructs(before, after any) []Change
```

diff 规则：字段名取 `json` tag；嵌套结构体/指针递归；`zenalog:"-"` 忽略该字段；
`zenalog:"mask"` 脱敏（From/To 记 `***`）；循环引用用 visited 集合防御；不可比类型
跳过该字段而不是报错。

**slice 用编辑距离（Levenshtein）对齐**，不按索引逐位比：元素格式化为规范串后
DP 求最优对齐，配对的元素递归 struct diff（路径带原索引），未配对的报增删。
理由：配置子表这类有序列表在顶部插入一行时，按位比较会把后面每行都报成变更，
diff 没法看。javers 默认的 Simple 就是按位比，starter 专门显式配置了
`LEVENSHTEIN_DISTANCE`（`JAVERS.java`），本库与它对齐。
代价是 O(n×m) 的 DP，配置行数（几十到几百）下无感；超长 slice（>2000）退化为
按位比较并记一条 debug 日志。

```go
// 查询。
type Mode int

const (
    ModeFlat    Mode = iota // 平铺：一行一条日志 + Total（默认，CMI 的操作日志页）
    ModeByTrace             // 按 traceId 聚合成时间线（zrbiz 形状）
)

type ListRequest struct {
    Mode       Mode
    StartTime  time.Time   // 默认近 24 小时
    EndTime    time.Time
    PageNum    int         // 默认 1
    PageSize   int         // 默认 20，上限 100；(PageNum-1)*PageSize+PageSize 超 10000 返回错误（ES max_result_window）
    Query      string      // 关键词：message/diff/attrs（text 字段）上匹配搜索；trace_id/operator/operation/resource_path 是 keyword，整值相等才命中——与 zrbiz 搜索框行为一致（termQuery/matchPhrase 打在 keyword 上都是整值）
    Conditions []Condition // 精确过滤
    InstanceID string      // 按资源实例过滤的快捷入口
}

type Condition struct {
    Field string // resource_type | instance_id | operation | operator | attrs.<key>
    Op    Op     // OpEq | OpNe | OpPrefix
    Value string
}

type Bucket struct {
    TraceID   string
    StartTime time.Time // 组内最早一条的时间
    Logs      []Entry   // 组内明细，时间升序
}

// ListResult 用结构体包一层而不直接返回切片：将来加字段不动 List 签名——
// 公开 API 发布后不改签名。flat 模式填 Entries/Total，by-trace 填 Buckets。
type ListResult struct {
    Entries []Entry  // flat 模式：当前页的行
    Total   int64    // flat 模式：命中总数。ES 6 的 hits.total 恒精确，分页组件直接用
    Buckets []Bucket // by-trace 模式：当前页的组
}

func (l *Logger) List(ctx context.Context, req ListRequest) (*ListResult, error)

// GinHandler 提供 GET /activityLog，解析 query 参数走 ginx 响应壳。
// engine.GET("/activityLog", logger.GinHandler()) 一行接入。
func (l *Logger) GinHandler() gin.HandlerFunc
```

### 5.2 zenalog/es

```go
type Config struct {
    Addresses   []string
    Index       string   // 写入索引（具体单索引）
    SearchIndex string   // 查询索引 pattern，NewStore/NewQuery 处缺省回填为 Index
    AttrKeys    []string // attrs 标签键白名单：EnsureIndex 建/补 mapping、CheckSearchMapping 比对都用它
    Username    string
    Password    string
    Timeout     time.Duration
}

// Doc ES 文档线格式，json tag 与 mapping 对应（trace_id、timestamp、ts_nanos、
// operator、resource_type、instance_id、resource_path、operation、message、diff、
// changes、status、attrs）。
// timestamp 序列化钉死 UTC 毫秒串（"2006-01-02T15:04:05.000Z"）——不用 time.Time
// 默认 JSON：那是 RFC3339 纳秒带本地时区偏移，精度超出 ES date 的毫秒，时区还随
// 部署环境漂。ts_nanos = Entry.Timestamp.UnixNano()，专做同毫秒并列的排序
// tiebreaker（见 5.3 flat 查询）。
type Doc struct { ... }

// Store 写入侧。
func NewStore(cfg Config) (*Store, error)

// EnsureIndex 索引不存在则建（PUT + settings + mappings）；**已存在也要补**——
// mapping：PUT /{index}/_mapping/{type} 把 AttrKeys 里缺的 attrs 子字段加进去；
// settings：PUT /{index}/_settings 补 index.max_inner_result_window（动态设置）。
// 不能只做 "if not exists"：AttrKeys 是会增长的，索引早就存在，跳过补齐就会让
// dynamic: strict 拒收所有带新 key 的文档（见 6.1 第 10 条）。
// 字段类型冲突（同名 key 已是别的类型）ES 会拒，此时返回 error 让 New 失败——
// 启动期暴露，别拖到运行时。number_of_shards 是静态设置，只在建索引时生效，
// 已存在的索引维持原分片数，不算错。
func (s *Store) EnsureIndex(ctx context.Context) error

// Bulk 批量写入。Sync() 与攒批共用同一入口。
// **必须解析响应体判定成败，不能只看 HTTP 状态码**：ES 的 _bulk 即使有条目被拒
// 也返回 HTTP 200，失败信息在 body 里（顶层 "errors": true，每个 item 自带 status
// 与 error）。只看状态码就会把"文档没进去"当成写成功——静默丢日志。
// 实现要求：errors=true 时逐条取出失败项，返回带失败条数的 error，并把每条的
// error.type / error.reason 记进日志。特别是 strict_dynamic_mapping_exception，
// 那是 attrs 的 key 没在 AttrKeys 登记的信号（见 6.1 第 10 条）。
func (s *Store) Bulk(ctx context.Context, docs []Doc) error

// Query 查询侧。
func NewQuery(cfg Config) *Query

// CheckSearchMapping 拉 SearchIndex 的 _mapping，逐索引与 AttrKeys 比对，返回
// 索引名 → 缺失的 attr 键；pattern 匹配不到任何索引返回空 map 不报错（目标索引
// 可能还没建）。主包 New 在 SearchIndex != Index 时调它，不一致记 warn（见 5.1）。
func (q *Query) CheckSearchMapping(ctx context.Context) (map[string][]string, error)

func (q *Query) Search(ctx context.Context, req QueryRequest) ([]Doc, int64, error)          // flat
func (q *Query) AggregateByTrace(ctx context.Context, req QueryRequest) ([]TraceBucket, error) // by-trace
```

es 包不 import 主包；`TraceBucket` 是 es 自己的结果类型，主包负责转换成 `Bucket`。

### 5.3 ES 索引与线格式

写入索引名必填（约定 `{service}-activity-log`）。`New` 时 ensure。

settings（建索引时全量写入；索引已存在只补动态项）：

- `number_of_shards: 1`——ES 6 默认 5 分片，单服务活动日志量级用不着；单分片还让
  terms 聚合结果天然精确（多分片 top-N 合并才有误差）
- `index.max_inner_result_window: 2000`——top_hits 组内明细的 from+size 上限。
  **ES 6 默认 100**（6.8.5 源码 `IndexSettings`，属性 Dynamic + IndexScope），不抬
  by-trace 一查就被拒（"Top hits result window is too large"）。IndexScope 意味着
  就算 zrbiz 的 `activity_log` 被运维调过，也管不到本库新建的索引——必须自己带。
  动态设置，索引已存在时 `PUT /{index}/_settings` 补齐

mappings：

- `timestamp`：date——写入侧钉死 UTC 毫秒串（es.Doc 负责，见 5.2）
- `ts_nanos`：long——同毫秒并列的排序 tiebreaker（见下方 flat 查询）
- `trace_id` / `operator` / `resource_type` / `instance_id` / `resource_path` /
  `operation` / `status`：keyword（要过滤、聚合）
- `message` / `diff`：text（模糊搜索）
- `changes`：object 数组，`field`/`from`/`to` 均 keyword + `ignore_above: 256`——
  结构化 diff，支撑前后对比 UI。keyword 不设 ignore_above 时，单值超 32766 字节
  （Lucene immense term）会让**整条文档被拒**，还拒在异步路径上=静默丢日志；Java 侧
  `jsonDiff` 恰是靠"没写 @Field 注解、走动态 mapping 默认 keyword(256)"免疫这一条的。
  超长 from/to 不进倒排（等值过滤不到），但仍在 _source，前后对比 UI 展示不受影响
- `attrs`：object + `dynamic: strict`，`New` 按 `Config.AttrKeys` 把每个键显式建成
  text + keyword 多字段（keyword 显式声明、`ignore_above: 256` 定死）。动态 mapping
  的默认值一个都不吃：「键是有界常量」不能靠口头约定——`Attr(customerName, "customer")`
  这种 key/value 写反的 bug 会让每个客户名变成一个 mapping 字段，把
  `index.mapping.total_fields.limit`（默认 1000）打爆，而且爆在异步路径上（写失败
  slog.Error 丢批），正是 6.1 第 9 条的静默残缺模式。白名单在埋点时先拦一道
  （返回 error），strict mapping 兜底拒收未登记 key。等值/前缀查
  `attrs.<key>.keyword`，模糊搜查 `attrs.<key>`——库内部拼好，Condition 只写
  `attrs.<key>`。超过 256 字符的 value 不进 keyword 倒排（等值/前缀查不到），但仍在
  _source 与 text 倒排里、可模糊搜——刻意取舍：attrs 是筛选维度，筛选值就该是短标签

**mapping 是要长的，`EnsureIndex` 不能只做 "if not exists"。** `AttrKeys` 增加时索引
通常早已存在，跳过补齐就会让 `dynamic: strict` 拒收所有带新 key 的文档，而 Bulk 的
HTTP 200 会把这件事藏起来（见第 6 节）。所以索引已存在时也要
`PUT /{index}/_mapping/{type}` 补差集，类型冲突则 `New` 失败。见 6.1 第 10 条。

写入走 Bulk API：

```
POST /{index}/_bulk
{"index":{"_type":"_doc"}}
{"trace_id":"...","timestamp":"2026-07-31T07:03:05.123Z","ts_nanos":1785481385123456789,"operation":"create vll","status":"info","attrs":{"customer":"..."},...}
```

flat 查询（`SearchIndex` 可以是 pattern，ES 原生支持）：

```json
{
  "from": 0, "size": 20,
  "sort": [{ "timestamp": "desc" }, { "ts_nanos": "desc" }],
  "query": { "bool": { "filter": ["时间范围/精确条件（attrs 查 .keyword 子字段，含 prefix）"], "must_not": ["ExcludedOperations"], "must": ["Query 关键词（text 字段 match_phrase，keyword 字段整值）"] } }
}
```

排序带 `ts_nanos` 做 tiebreaker：date 只有毫秒精度，一次操作的 Info/InfoFinish 与
并发请求落同一毫秒是常态；只按 timestamp 排，同刻文档的相对序会随副本轮询、段
合并变化，from/size 翻页就会重复行、漏行。by-trace 的 top_hits 组内排序同理带上。

不发 `track_total_hits`：它是 ES 7.0 才加的搜索参数，ES 6 收到未知参数是解析报错
而不是忽略，flat 查询会整个失败。ES 6 的 `hits.total` 本来就是精确数字。

by-trace 查询沿用 zrbiz `ALogTraceIdAggRepositoryImpl` 的聚合结构（terms + min +
top_hits + bucket_sort），分页语义是修正而非照搬：zrbiz 的 bucket_sort 是 from 0 /
size 10000 全量取回、Java 层 `PageUtil.toPage` 内存分页，本库把 from/size 下推给
bucket_sort，ES 侧真分页：

```json
{
  "size": 0,
  "query": { "bool": { "filter": ["同上"], "must_not": ["同上"], "must": ["同上"] } },
  "aggs": {
    "by_trace": {
      "terms": { "field": "trace_id", "size": 10000 },
      "aggs": {
        "first_ts": { "min": { "field": "timestamp" } },
        "logs": { "top_hits": { "size": 2000, "sort": [{ "timestamp": "asc" }, { "ts_nanos": "asc" }] } },
        "page": { "bucket_sort": { "sort": [{ "first_ts": "desc" }], "from": 0, "size": 20 } }
      }
    }
  }
}
```

## 6. 错误处理

原则沿用本库规约：**库不终止调用方进程**，没有 `os.Exit`/`log.Fatal`/`panic`。

- 配置必填缺失 → `New` 一次报全，返回 error。
- `EnsureIndex` 失败（ES 不可达、认证失败、attrs 子字段类型冲突）→ `New` 返回
  error。启动期失败要快速失败，配错的时候正是最需要知道的时候。
- **Bulk 的"成功"必须验响应体，不能只看 HTTP 状态码。** ES 的 `_bulk` 有条目被拒
  时照样返回 HTTP 200，失败在 body 里（`"errors": true` + 每 item 的 status/error）。
  只看状态码 = 把丢文档当成写成功。这条同时管异步和同步两条路径。
- **同步写（`Sync()`）失败 → error 返回调用方**，不吞。既包括 HTTP 层失败，也包括
  上面那种 200 + 条目被拒。审批留痕的语义是「日志写失败则业务失败」，这个决定由
  业务做，库只负责把失败如实上报。
- 异步路径写失败（含条目级失败）→ `slog.Error` 记录后丢弃，不阻塞、不重试（v2 不变）。
  日志里必须带每条的 `error.type` / `error.reason`——`strict_dynamic_mapping_exception`
  就是 attrs 的 key 没登记，这是唯一能发现它的地方。
- 异步队列满 → 丢新日志 + `slog.Warn`（带累计丢弃计数）。不拿背压打扰业务；
  丢不起的操作应该走 `Sync()`，所以不提供阻塞策略配置。
- 查询失败 → 返回 error，handler 走 `ginx.Error`。
- `Close` 后埋点（含同步）→ 返回 `ErrClosed`。
- diff 遇到不可比类型/循环引用 → 跳过该字段，不报错；slice 超长退化按位比较。

### 6.1 已知限制

1. **异步路径有丢失窗口**：队列满丢弃、写失败丢批、进程崩溃丢缓冲（最多
   FlushInterval × 流量的量）。留痕类操作必须用 `Sync()`；`Sync()` 期间进程崩溃
   的窗口与业务操作本身同生共死，语义自洽。要 ES 长时间不可用也零丢失，需要
   本地落盘兜底，见第 8 节。
2. **by-trace 组内明细上限 2000 条**（top_hits size），与 zrbiz 一致；这个上限靠
   EnsureIndex 抬 `max_inner_result_window` 撑起（ES 6 默认 100），跨索引 by-trace
   要求每个参与索引都有该设置——都由 zenalog 建则自动满足。成本模型要心中有数：
   top_hits 为时间窗内**每个** trace bucket 取明细，bucket_sort 是 pipeline 聚合、
   裁页发生在这之后，所以代价正比于窗内命中文档数而非页大小——靠时间过滤收窄，
   真分页优化（两趟查询）见第 8 节。by-trace 没有精确 Total（组数靠 cardinality
   也只能近似），要精确分页用 flat。
3. **diff 以字符串 + 结构化两份存储**，查询侧对 diff 只能模糊匹配；对 changes 的
   field 可以精确过滤，但 from/to 不做范围查询，且超 256 字符不进倒排（等值过滤
   不到，_source 展示不受影响）——同 attrs 的取舍。
4. **slice 字段不支持环境变量覆盖**（`Addresses`、`AttrKeys`、`ExcludedOperations`）。
   zenserver/config 的 env tag 对 slice 不生效；其余标量字段的 env tag 正常。
5. **by-trace 聚合 terms size 固定 10000**，单次查询时间范围内 traceId 超过 10000
   时截断。活动日志按服务+时间范围看，量级远低于此。
6. **`attrs` 的键必须登记（`AttrKeys` 白名单 + mapping `dynamic: strict` 双闸）。**
   不用 flattened（ES 7.3+ 特性，公司是 ES 6），也不吃动态 mapping 默认值——见 5.3。
   等值/前缀查 `attrs.<key>.keyword`（库内部拼），`ignore_above` 显式定为 256：
   更长的值不进 keyword 倒排、等值/前缀查不到，但仍在 _source 与 text 倒排里可模糊搜。
   attrs 不支持范围与聚合——标签维度本来也不需要。
7. **flat 深分页有界。** `from+size` 受 ES `max_result_window`（默认 10000）限制，
   超出时库返回明确错误，提示缩小时间范围——日志查找应靠时间过滤，不是翻到
   几百页。游标式 `search_after` 见第 8 节。
8. **Total 在 ES 6 上恒精确**，`hits.total` 直接是数字，不发 `track_total_hits`
   （ES 6 收到未知参数解析报错）。将来集群升 ES 7+ 时需要引入 `track_total_hits`
   及其计数上限的取舍——那是 es 包内部的事，API 不变。
9. **跨索引查询要求参与方的 `AttrKeys` 一致**（不只是"都用 zenalog"）。
   `SearchIndex` 通配/多索引面向的是「多个 Go 服务各写各的 zenalog 索引、一个列表
   查」的场景，但 `AttrKeys` 是各服务自己配的——A 服务登记 `customer`、B 服务登记
   `tenant`，两边 mapping 就不一样。此时按 `attrs.customer.keyword` 跨索引筛，B 的
   索引里没这个字段，ES 对未映射字段的 term 查询是"不匹配"而非报错，**B 的日志被
   静默排除**——和下面骂 Java 索引的问题同一个形状，只是发生在 zenalog 家族内部。
   要求：参与同一个列表查询的服务必须共用同一份 `AttrKeys`。
   兜底：`New` 时若 `SearchIndex != Index`，拉一次目标 pattern 的 `_mapping` 与本地
   `AttrKeys` 比对，不一致记 `slog.Warn`（不 fail——目标索引可能还没建）。把静默
   变成日志里有痕，这是低成本能做到的最好程度。
   另外已核实 Java
   starter 的 `ALogEntry`（索引名固定 `activity_log`）：字段为 id/traceId/operator/
   path(嵌套 ResourcePath)/operation/message/diff/jsonDiff/finished/success/
   timestamp——没有本库的 attrs/status/changes，资源路径是嵌套对象而非
   `resource_path` 字符串。且 Java 侧的 ES 字段名是 camelCase（`traceId`、`jsonDiff`，
   zrbiz 查询代码 `termQuery("traceId", ...)` 为证），本库是 snake_case（`trace_id`）
   ——同名字段只有 operator/operation/message/diff/timestamp 五个。混查不报错，但
   by-trace 按 `trace_id` 聚合在 Java 索引上直接为空，按 attrs/status 筛选会**静默
   漏掉全部 Java 侧日志**——结果残缺而无任何报错，比报错更糟。**跨 Java-Go 索引混查不支持**；CMI 的登录
   日志整合（`✓N3`）路径见第 9 节。
10. **加一个 `AttrKeys` 要同时补 mapping，否则新 key 的文档全被拒。** `dynamic: strict`
   的代价：业务今天新增 `Attr("site", ...)` 并在配置里登记，但索引早已存在——
   `EnsureIndex` 如果只做 "if not exists" 就会跳过，mapping 里没有 `attrs.site`，
   ES 拒收每一条带这个 key 的文档。再叠上 Bulk 的 200 陷阱（见第 6 节第二条）就是
   彻底静默：配置改了、代码改了、`New` 没报错、日志就是不出现，极难排查。
   所以 `EnsureIndex` 在索引已存在时也要 `PUT _mapping` 补齐缺失子字段（ES 允许加
   字段、不允许改类型；类型冲突时 `New` 直接失败）。**已存在索引上删除或改名
   `AttrKeys` 仍需人工处理**——mapping 里的旧字段不会自动清掉，只是不再被写入。

## 7. 测试

验收标准沿用本库：

```
GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go test -race ./...
```

| 包 | 覆盖的行为 |
|---|---|
| `zenalog`（diff） | 表驱动：基本类型变更、嵌套结构体、指针 nil↔非 nil、**slice 编辑距离对齐（顶部插入只报一条新增、中间删除、元素修改、重排）**、超长 slice 退化、`zenalog:"-"` 忽略、`zenalog:"mask"` 脱敏、循环引用不炸 |
| `zenalog`（Logger） | fake Store 验证：Entry 装配（Option 各字段含 Attr/Changes）、ctx 注入 operator/traceID、traceID 缺省回落 request id、队列满丢新+计数、**Sync 绕过队列直写且错误透传**、**未登记的 attr key 返回 error**、Close 冲刷且幂等、Close 后（含 Sync）返回 ErrClosed |
| `zenalog`（查询+handler） | ListRequest 默认值与上限、**flat/by-trace 两种模式的结果填充**、gin handler 参数解析（含 mode、attrs 条件、prefix）、错误走 ginx 响应壳 |
| `zenalog/es` | `httptest.Server` 模拟 ES 6：索引不存在时建（**settings 带 number_of_shards=1 与 max_inner_result_window=2000**）、**已存在时不重复建但仍 `PUT _mapping` 补 AttrKeys 差集 + `PUT _settings` 补 max_inner_result_window**、**补 mapping 遇类型冲突返回 error**、mapping 含 attrs 白名单（dynamic: strict、显式 keyword ignore_above 256、typed 形式）与 changes 三字段的 ignore_above、Bulk 请求体 ndjson 逐字节正确（action 带 `_type: "_doc"`、**timestamp 为 UTC 毫秒串、ts_nanos 在场**）、flat 查询 JSON（from/size/attrs .keyword 子字段/prefix、**sort 带 ts_nanos tiebreaker**、**无 track_total_hits**）与精确 total 解析、聚合查询 JSON 结构（terms/top_hits 组内 sort 带 tiebreaker/bucket_sort/排除条件）、SearchIndex 通配出现在请求 URL、**CheckSearchMapping 缺键索引被点名、pattern 无匹配返回空不报错** |
| `zenalog/es`（失败路径） | **HTTP 非 2xx 返回 error**；**HTTP 200 + `"errors": true` 的条目级失败也返回 error**（含失败条数），且 `strict_dynamic_mapping_exception` 的 reason 出现在日志里——这条是本轮新增的重点，只看状态码就会把丢文档当成写成功；混合响应（部分成功部分被拒）断言 error 里带的是被拒条数而不是全批 |

测试断言真实行为：Bulk 那条断言服务器收到的 ndjson 逐字节正确，不断言"没报错"；
Close 那条断言缓冲队列里的日志真的被冲刷出去了；slice diff 断言插入场景下
变更集里**没有**后续行的假变更。

## 8. 后续（不属于 v2）

- **本地落盘兜底与重试**：ES 长时间不可用且业务不能失败的场景。**挂起待产品
  拍板**——CMI 的「操作日志」定位（可丢的活动日志 vs 不可丢的审计日志）产品
  尚未回答（`✓N3` 残留），「审批通过时 ES 写失败该怎么办」正是这个问题的具体
  形态。落盘兜底做不做、做成什么样，待定位确认后再定。把它推到 v2 之后是
  库主的决定，不是 CMI 的评审结论。
- **search_after 游标翻页**：真正的深翻页场景。v2 用 from/size + max_result_window
  上限拦截，先逼调用方用时间过滤。tiebreaker 字段 ts_nanos 已就位，届时直接可用。
- **by-trace 两趟查询**：第一趟 terms + min + bucket_sort（不带 top_hits）只拿本页
  的 trace_id，第二趟按这批 trace_id 过滤取明细——把 top_hits 的取回代价从"窗内
  全部 bucket"缩到"本页 20 个"。v2 靠时间过滤 + 量级假设撑着（见 6.1 第 2 条），
  量上来再做。
- 有真实使用方落地后再考虑：ResourceType 常量沉淀、结构化 ResourcePath、
  traceID 跨服务透传（HTTP header）、saga/agent 框架的 `WithOperator` 自动接入。

## 9. 待确认清单

实现不阻塞（代码按本设计写），以下三项影响 CMI 联调与后续版本：

1. **CMI「操作日志」定位（产品拍板）**：可丢的活动日志 vs 不可丢的审计日志。
   决定第 8 节落盘兜底做不做，以及审批写 ES 失败时业务侧的行为。
2. ~~ES 发行版与版本~~ **已确认：ES 6**（用户确认；旁证：starter 的
   `@Document(indexName = "activity_log", type = "_doc")` 是 spring-data-elasticsearch
   3.x / ES 6 时代的写法，4.0 起该属性废弃）。对设计的实际影响已落到正文：
   不发 `track_total_hits`（ES 6 解析报错）、`Total` 恒精确；attrs 用 object +
   白名单，不依赖发行版特性；线格式 typed——bulk action 带 `_type: "_doc"`、
   mapping 用 typed 形式、search 走 `/{index}/_search`（6.x 允许不带 type），
   全部收敛在 es 包内。
3. **CMI 登录日志整合路径（CMI 架构决策）**：让 Java iam 的登录日志和 cmi-server
   的业务日志进同一个列表（`✓N3`），候选：
   a) iam 侧适配写 zenalog 兼容 mapping——跨团队改 starter 用法，重，且让 iam
      索引与其他 Java 服务分叉；
   b) cmi-server 提供内部接口（或消费事件）代写登录日志——mapping 干净一致，
      iam 需要加通知钩子；
   c) cmi-server 应用层分别查两个索引再合并——不动 iam，但分页合并是近似的；
   d) 产品缩减范围，登录日志留在 iam 侧页面。
   库侧立场：zenalog 只保证同 mapping 索引的通配查询，选哪条由 CMI 定。
