# zenserver 设计文档

- 日期：2026-07-30
- 状态：已定稿
- 模块路径：`github.com/isaac0914/utils/zenserver`

## 1. 背景与目标

写 `nebula-server` 时，整个 server 骨架（配置加载、日志配置、gin 装配、优雅退出）
都是从零手写的。下一个 server 不想再写一遍。

目标：把这层骨架抽成一个库，新项目 `go get` 之后填几个参数就能起服务。

参考对象是同事的 `github.com/NeilXu2017/landau`——它的入口形态正是想要的效果：

```go
s := &entry.LandauServer{
    LogConfig:           conf.AppConfig.LogConfig,
    HTTPServicePort:     conf.AppConfig.Server.Port,
    RegisterHTTPHandles: handle.ApiRegister,
}
s.Start()
```

不直接用 landau 的原因：它的日志是自研框架（2240 行）而非 `log/slog`，绑定了公司内部的
服务发现、健康检查广播、企业微信告警，没有 context 传 request id 的机制。为了 447 行的
`entry` 骨架吃下 12000 行不划算。nebula 已在 slog + sqlx 上，迁过去等于重写。

## 2. 范围

### 包含（v1）

| 能力 | 说明 |
|---|---|
| 配置加载 | yaml 文件 + 环境变量覆盖 |
| 日志 | `log/slog` + 级别 + dev/prod 格式 + 文件轮转 + context 追踪字段 |
| HTTP 生命周期 | gin 装配、信号监听、优雅退出 |
| 内置中间件 | request id、panic 恢复、访问日志 |
| 统一响应 | `success/ret/code/msg/data` 响应壳与响应码常量 |

### 不包含（明确排除）

数据库初始化、定时任务、Prometheus、HTTP client 助手、分布式锁、gRPC。

排除理由：nebula 的 `conf/db.go`（MariaDB Galera + MaxScale 专门调优）和
`conf/scheduler.go`（绑定 xxl-job）恰好是最依赖具体部署环境的两块，而目前只有 nebula
一个样本。**最不确定的部分最后冻结。** 等第二个 server 落地、看清哪些是真共性再抽。

## 3. 从 nebula / landau 借鉴与修正的点

### 借鉴

1. **环境变量用 struct tag 声明，反射统一覆盖**（landau `config/config.go`）。
   nebula 手写了 25 行 `overrideEnv("DB_HOSTS", &cfg.DbConfig.Hosts)`，改成
   `env:"DB_HOSTS"` tag 后归零，加新配置项不会漏。
2. **默认值写在 Go 代码里，yaml 只做覆盖**（landau `zbg-server/conf/config.go`）。
   配置文件缺项时行为确定，而不是拿到零值。
3. **真正的优雅退出**（landau `entry/engine.go`）。
   nebula 现在是 `time.Sleep(5s)` + `os.Exit(0)`，进程退出时正在处理的请求被直接砍断。
   本库用 `http.Server.Shutdown(ctx)`。
4. **dev/prod 双格式 + 级别分流 + HOSTNAME 子目录**（nebula `conf/log.go`）。
   prod 下 `app.log` 收全量、`error.log` 与 stdout 只收 ERROR。

### 修正 landau 的问题

| landau 的问题 | 本库的做法 |
|---|---|
| `srv`/`grpcServer`/`reloadCallback` 等包级可变全局；`Start()` 里往别名包塞 `data.ServiceName = ...` | 状态全部挂在 `Server` 实例上。唯一的全局是 `logx` 里的 `slog.SetDefault`，这是 slog 本身的设计 |
| 库里 `sysLog.Fatalf` 直接杀调用方进程 | 一律 `return error`，库不决定进程生死 |
| `LandauServer` 43 个字段无必填标记，zbg-server 只用了 14 个 | `Server` 8 个字段，2 个必填，`Run()` 开头校验并返回明确错误 |
| `signal.Notify(make(chan os.Signal), ...)` 无缓冲 channel，`go vet` 报警，信号可能丢 | 用 `signal.NotifyContext`，缓冲问题由标准库解决 |
| 定时任务用方法名字符串反射调用，丢失编译期检查 | v1 不含定时任务；将来若加，用函数值不用字符串 |
| `loadFromEnvironment` 只递归 `reflect.Struct`，指针结构体里的 env tag 被静默忽略 | 递归非 nil 结构体指针；slice/map 的限制在文档里明确写出（见 6.1） |

### 依赖取舍

第三方依赖只有三个：`gin`、`lumberjack`、`yaml.v3`。

- 不用 `viper`：nebula 只用到 `ReadInConfig` + `Unmarshal`，`yaml.v3` 两行搞定，
  环境变量由 `env` tag 接管。省掉约 15 个传递依赖。
- 不用 `samber/slog-multi`：级别分流需要一个 fanout handler，手写 40 行，
  省掉 `samber/lo`、`slog-common`、`sourcegraph/conc` 三个依赖。
- 不用 `google/uuid`：request id 用 Go 1.24 的 `crypto/rand.Text()`，一行。

库的依赖会传染给每个使用方，所以吝啬是有价值的。

### 版本钉法

`go` 指令钉 `1.24.0`，gin 钉 v1.11.0，对齐 nebula-server 的工具链。

这不是随手选的：`go mod tidy` 默认会拉 gin v1.12.0，而它要求 go >= 1.25.0，会把
`go` 指令一起顶上去，nebula（go 1.24）就 import 不了这个库了。库的 `go` 指令必须
向下兼容使用方，升级前先确认使用方的工具链。

## 4. 架构

四个包，依赖单向，互不引用：

```
zenserver/            package zenserver — Server 结构体 + Run，装配 gin 与生命周期
├── config/           yaml + env tag 加载              依赖：yaml.v3
├── logx/             slog + 轮转 + context 追踪字段    依赖：lumberjack
└── ginx/             中间件 + 统一响应壳               依赖：gin
```

`zenserver` 只引用 `ginx`。`config` 与 `logx` 不被引用，由调用方在 `Run()` 之前自己调。

### 为什么不让 Run() 一手包办配置和日志

landau 的 `Start()` 内部调 `log.LoadLogConfig`。本库不这么做，原因：

1. 配置和日志必须在 `OnStart` 之前就绪（`OnStart` 里会打日志），把这个顺序摊在
   `main` 里比埋在库内部清楚。
2. 只写定时任务或 CLI 的二进制可以单独用 `logx` + `config`，不必被迫引入 gin。
3. `zenserver` 不依赖 `config`/`logx`，三块可以独立替换。

代价是 `main` 从 1 行变成 3 段，换来的是每一步都看得见。

### 调用方看到的样子

```go
package main

import (
    "log"

    "github.com/isaac0914/utils/zenserver"
    "github.com/isaac0914/utils/zenserver/config"
    "github.com/isaac0914/utils/zenserver/ginx"
    "github.com/isaac0914/utils/zenserver/logx"
)

type Config struct {
    Addr string     `yaml:"addr" env:"HTTP_ADDR"`
    Log  logx.Config `yaml:"log"`
    DB   DBConfig   `yaml:"db"`
}

func main() {
    // 默认值写在这里，yaml 只覆盖
    cfg := &Config{Addr: ":8080"}
    if err := config.LoadInto("config.yaml", cfg); err != nil {
        log.Fatal(err)
    }
    if err := logx.Init(cfg.Log, ginx.TraceAttrs); err != nil {
        log.Fatal(err)
    }

    srv := &zenserver.Server{
        Name:           "myapp",
        Addr:           cfg.Addr,
        RegisterRoutes: registerRoutes,
        OnStart:        func(ctx context.Context) error { return initDB(ctx, cfg.DB) },
        OnShutdown:     closeDB,
    }
    if err := srv.Run(); err != nil {
        log.Fatal(err)
    }
}
```

## 5. 各包接口

### 5.1 config

```go
// LoadInto 读 yaml 填充 out，再用环境变量覆盖带 env tag 的字段。
// out 必须是非 nil 结构体指针，可预先带默认值。
func LoadInto(path string, out any) error

// OverrideFromEnv 只做环境变量覆盖，供无配置文件的场景单独使用。
func OverrideFromEnv(out any) error
```

- 覆盖顺序：结构体默认值 → yaml → 环境变量。
- 环境变量未设置或为空串时跳过该字段（空串无法与"想设成空"区分，取跳过语义）。
- 支持类型：string、bool、int/int8-64、uint/uint8-64、float32/64、`time.Duration`
  （用 `time.ParseDuration`，所以 `"30s"` 可用，nebula 的 `SessionTimeout` 需要）。
- 递归进结构体和非 nil 结构体指针。
- 解析失败**汇总成一个 error 返回**，不静默忽略。nebula 现在是 `log.Info` 后继续，
  landau 是直接丢弃——两者都会让配错的值悄悄变成零值。

### 5.2 logx

```go
type Config struct {
    Level       string // debug|info|warn|error，默认 info
    Mode        string // dev|prod，默认 prod
    Dir         string // prod 日志目录，默认 logs
    MaxSize     int    // 单文件 MB，0 = lumberjack 默认的 100
    MaxAge      int    // 保留天数，0 = 不按时间删除
    MaxBackups  int    // 保留个数，0 = 全部保留
    Compress    bool
    SplitByHost bool   // 按 HOSTNAME 建子目录，k8s 多 pod 共享卷时开
}

// Extractor 从 context 提取要附加到每条日志的字段。
type Extractor func(ctx context.Context) []slog.Attr

func Init(cfg Config, extractors ...Extractor) error
```

`Config` 带好 `yaml` 和 `env` tag，使用方直接嵌进自己的配置结构体即用。

**Extractor 是解决"追踪字段"这条缝的关键。** nebula 的 `TraceHandler` 把
`requestId` 和 `taskId` 硬编码在里面——`requestId` 来自 HTTP 中间件，`taskId` 来自
saga 引擎，后者是纯项目概念。库不该知道 `taskId`。改成注册式后：

- `logx` 不认识任何具体的 key
- `ginx.TraceAttrs` 提供 request id
- 项目自己的 saga/job 提供各自的字段

`ginx.TraceAttrs` 的签名与 `logx.Extractor` 完全一致，靠 Go 的函数类型可赋值性直接
传入，所以 `ginx` 不需要 import `logx`。

行为：

- `dev`：`TextHandler` → stdout，不分流。
- `prod`：`JSONHandler`，`app.log` 收 ≥Level 全量，`error.log` + stdout 只收 ERROR，
  通过手写的 `multiHandler` 广播。
- Level 或 Mode 写错返回 error，不静默降级。配错的时候正是最需要知道的时候。
- **只给 Level/Mode/Dir 填默认值，轮转三项原样交给 lumberjack。** 这条是看 nebula 真实
  `config.yaml` 时发现的：它用 `max_age: 0` / `max_backups: 0` 表示"永不删除"，那是
  lumberjack 的正式语义。库如果把 0 当成"没配"顶成 7 天 / 10 个，会静默删掉本该保留的
  日志。零值有含义的字段不能做默认值加工。

**追踪字段必须落在日志顶层。** 这条在实现时被测试抓出来过：如果调用方开过
`WithGroup("g")`，`traceHandler` 直接往 Record 里 `AddAttrs` 会让 `requestId` 掉进
`"g":{...}` 里，按 `requestId` 查日志就失效了。所以 `traceHandler` 记下派生链，
开过 group 时从原始 handler 重建、把追踪字段挂在 group 之前；没开 group 时走
`AddAttrs` 快路径。

### 5.3 ginx

```go
func RequestID() gin.HandlerFunc                                  // 读或生成 X-Request-Id
func RequestIDFrom(ctx context.Context) string
func WithRequestID(ctx context.Context, id string) context.Context // 异步任务沿用同一个 id
func TraceAttrs(ctx context.Context) []slog.Attr                   // 传给 logx.Init
func AccessLog() gin.HandlerFunc                                   // 请求日志走 slog
func Recovery() gin.HandlerFunc                                    // panic → 500 + 统一响应壳

type Response struct {
    Success bool   `json:"success"`
    Ret     int    `json:"ret"`
    Code    string `json:"code"`
    Msg     string `json:"msg"`
    Data    any    `json:"data,omitempty"`
}

func Success(c *gin.Context, data any)
func SuccessWithMsg(c *gin.Context, msg string, data any)
func Created(c *gin.Context, data any)
func Accepted(c *gin.Context, data any)
func Fail(c *gin.Context, code, msg string)
func Error(c *gin.Context, code string, err error)
func BadRequest(c *gin.Context, msg string)
func NotFound(c *gin.Context, msg string)
func InternalError(c *gin.Context, err error)
```

响应壳、响应码常量和函数名都沿用 nebula `handler/response/`，那是公司标准。
名字一模一样，nebula 迁过来只需要替换包名限定符。

业务结果一律 HTTP 200，看 `success`/`code`。**例外是 `Recovery` 捕获的 panic 回 500**
——panic 是服务故障不是业务结果，网关和监控要看得见。nebula 现在用 `gin.Recovery()`
也是回 500（只是响应体为空），所以这不算行为变更。

内置中间件的顺序固定为 **RequestID → AccessLog → Recovery**。AccessLog 必须在
Recovery 之前：panic 会跳过 `c.Next()` 之后的代码，只有让 Recovery 先把 panic 收掉、
正常返回，AccessLog 才记得到那条 500。

关键实现细节：`RequestID` 必须写回 `c.Request = c.Request.WithContext(...)`，
否则 request id 进不了 `context.Context`，`slog` 也就看不到。handler 里要用
`c.Request.Context()` 而不是 `c` 本身。

### 5.4 zenserver

```go
type Server struct {
    Addr           string                // 必填，如 ":8080"
    RegisterRoutes func(r *gin.Engine)   // 必填

    Name            string              // 服务名，出现在启动日志里
    ReleaseMode     bool                // 关掉 gin 的 debug 输出
    Middlewares     []gin.HandlerFunc   // 追加在内置中间件之后
    OnStart         func(ctx context.Context) error  // 监听前初始化
    OnShutdown      func(ctx context.Context) error  // HTTP 关闭后清理
    ShutdownTimeout time.Duration       // 默认 30s
}

func (s *Server) Run() error                       // 监听 SIGINT/SIGTERM
func (s *Server) RunContext(ctx context.Context) error  // ctx 取消即退出
```

`Run()` 是 `RunContext(signal.NotifyContext(...))` 的四行封装。分成两个方法有两个理由：
单测不必给测试进程发信号；把 server 嵌进更大的生命周期时可以自己控制 ctx。

**启动顺序**：校验必填 → 设 gin 模式 → `OnStart` → 同步 `net.Listen` → 装路由 →
`srv.Serve(listener)`。

`OnStart` 放在 bind 之前，DB 连不上就不会有流量打进来。bind 走同步的
`net.Listen` 而不是让 `ListenAndServe` 在 goroutine 里做，这样端口占用之类的错误
立刻返回，不用和退出信号抢 `select`。

**退出顺序**：ctx 取消 → `srv.Shutdown(ctx)` 等在途请求结束 → `OnShutdown` 释放资源。
两步的错误用 `errors.Join` 汇总，不因为第一步失败就跳过第二步。

## 6. 错误处理

原则：**库不终止调用方进程**。没有任何 `os.Exit`、`log.Fatal`、`panic`。

- 必填字段缺失、Level 写错、环境变量解析失败 → 启动时返回 error，快速失败。
- 端口占用 → `ListenAndServe` 的错误经 channel 传回 `RunContext` 返回。
- `OnStart` 返回 error → 不进入监听，直接返回。
- 关闭阶段的多个错误 → `errors.Join` 汇总。
- 业务 handler 里的 panic → `ginx.Recovery` 记录堆栈 + 返回统一错误响应，不打断进程。

### 6.1 已知限制

1. **slice / map 元素里的 `env` tag 不生效。** 反射只递归结构体和非 nil 结构体指针。
   语义上也说不通——slice 的每个元素会拿到同一个环境变量值。需要按元素覆盖时，
   在业务代码里显式处理。
2. **nil 结构体指针不会被分配。** yaml 里没有这一段、字段又是指针时，其中的 `env`
   tag 不生效。想用环境变量配的段落，别用指针。
3. **环境变量为空串等于没设。** 无法用环境变量把一个字段显式置成空串。
4. **`logx.Init` 修改 `slog` 的全局默认 logger**，一个进程只应调用一次。

## 7. 测试

每个包都有单测，验收标准是 `go build ./... && go vet ./... && go test ./...` 全绿。

| 包 | 覆盖的行为 |
|---|---|
| `config` | 默认值→yaml→env 三层覆盖顺序；各类型解析（含 `time.Duration`）；嵌套结构体与结构体指针；解析失败返回 error；非指针入参返回 error |
| `logx` | Level 解析与非法值报错；dev 模式输出到 stdout；prod 模式 app.log 与 error.log 的分流；Extractor 注入的字段出现在日志里；`SplitByHost` 建出 HOSTNAME 子目录 |
| `ginx` | 请求头带 X-Request-Id 时透传、不带时生成；request id 进入 `c.Request.Context()` 并回写响应头；`Recovery` 把 panic 转成统一响应；响应壳的 JSON 字段 |
| `zenserver` | 必填字段校验；`OnStart` 报错则不监听；`RunContext` 在 ctx 取消后返回；在途请求能跑完（优雅退出真的生效）；`OnShutdown` 被调用；端口占用返回 error |

优雅退出那条是重点——nebula 现在这条路是坏的，抽库的主要收益就在这里，必须有测试
钉住。

## 8. 后续（不属于 v1）

有第二个真实 server 落地、看清共性之后再考虑：定时任务抽象、DB 初始化（通用 80% 进库，
Galera params 由项目传入）、Prometheus 指标、HTTP client 助手。

顺手要做的：把 nebula 的 `conf/` 换成 zenserver，顺便修掉它那个假的优雅退出。
