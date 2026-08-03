# isaac0914-utils

Isaac 的个人工具库。

```
go get github.com/isaac0914/utils
```

| 目录 | 内容 |
|---|---|
| [zenserver](zenserver) | Go HTTP server 骨架：配置加载、日志、gin 装配、优雅退出 |
| [zenpage](zenpage) | 列表分页：参数校验、LIMIT/OFFSET 计算、响应构造 |

---

## zenserver

把每次新建 server 都要重写一遍的那层骨架抽出来。设计文档见
[doc/zenserver/design.md](doc/zenserver/design.md)。

不包含数据库、定时任务、Prometheus、gRPC——这些最依赖具体部署环境，等有第二个真实
server 落地、看清哪些是真共性再抽。

### 快速开始

```go
package main

import (
    "context"
    "log"

    "github.com/isaac0914/utils/zenserver"
    "github.com/isaac0914/utils/zenserver/config"
    "github.com/isaac0914/utils/zenserver/ginx"
    "github.com/isaac0914/utils/zenserver/logx"
    "github.com/gin-gonic/gin"
)

type Config struct {
    Addr string      `yaml:"addr" env:"HTTP_ADDR"`
    Log  logx.Config `yaml:"log"`
}

func main() {
    // 默认值写在这里，yaml 只做覆盖，配置文件缺项时行为确定
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
        ReleaseMode:    true,
        RegisterRoutes: registerRoutes,
        OnStart:        func(ctx context.Context) error { return nil }, // 连 DB 之类
        OnShutdown:     func(ctx context.Context) error { return nil }, // 释放资源
    }
    if err := srv.Run(); err != nil {
        log.Fatal(err)
    }
}

func registerRoutes(r *gin.Engine) {
    r.GET("/vms/:id", func(c *gin.Context) {
        // 注意用 c.Request.Context()，request id 在那上面
        vm, err := findVM(c.Request.Context(), c.Param("id"))
        if err != nil {
            ginx.InternalError(c, err)
            return
        }
        ginx.Success(c, vm)
    })
}
```

配置文件：

```yaml
addr: ":8080"
log:
  level: info      # debug|info|warn|error
  mode: prod       # dev 输出纯文本到 stdout；prod 输出 JSON 到文件
  dir: logs
  max_size: 1024   # 单文件 MB，0 = lumberjack 默认的 100
  max_age: 0       # 保留天数，0 = 不按时间删除
  max_backups: 0   # 保留个数，0 = 全部保留
  compress: false
  split_by_host: true   # 按 HOSTNAME 建子目录，k8s 多 pod 共享卷时开
```

轮转那三项原样交给 lumberjack，零值用它自己的语义，本库不加工 —— `0` 在那边是有意义的
取值（不删旧日志），不能当成「没配」处理。

`Run()` 监听 SIGINT/SIGTERM，收到信号后先等在途请求跑完再执行 `OnShutdown`，
默认最长等 30s。要自己控制生命周期就用 `RunContext(ctx)`。

### 四个包

**`zenserver`** —— `Server` 结构体 + `Run()`。8 个字段，`Addr` 和 `RegisterRoutes`
必填，缺了在启动时一次报全。状态全挂实例上，一个进程可以起多个。

**`zenserver/config`** —— `LoadInto(path, out)` 读 yaml 再用环境变量覆盖。
覆盖顺序是 **结构体默认值 → yaml → 环境变量**。环境变量用 `env` tag 声明：

```go
type DB struct {
    Host    string        `yaml:"host" env:"DB_HOST"`
    Timeout time.Duration `yaml:"timeout" env:"DB_TIMEOUT"`  // 支持 "30s" 写法
}
```

解析失败会汇总成一个 error 返回，不会静默变成零值。

**`zenserver/logx`** —— `Init(cfg, extractors...)` 配置 `slog` 全局 logger。
dev 模式纯文本到 stdout；prod 模式 JSON 到文件，`app.log` 收全量、`error.log` 与
stdout 只收 ERROR。

`Extractor` 是往每条日志注入追踪字段的口子，库本身不认识任何具体 key：

```go
// request id 由 ginx 提供，项目自己的 task id 自己提供
logx.Init(cfg.Log, ginx.TraceAttrs, mysaga.TaskAttrs)
```

**`zenserver/ginx`** —— 中间件和统一响应壳。

中间件：`RequestID()`（读或生成 `X-Request-Id`）、`AccessLog()`、`Recovery()`。
这三个由 `zenserver` 自动装上，顺序固定为 RequestID → AccessLog → Recovery——
AccessLog 必须在 Recovery 之前，否则 panic 会跳过它，那条 500 就记不到。

响应壳沿用 `success/ret/code/msg/data`，业务结果一律 HTTP 200，看 `success` 和
`code`。例外是 `Recovery` 捕获的 panic 回 500——那是服务故障不是业务结果，
网关和监控要看得见。

```go
ginx.Success(c, data)
ginx.Created(c, data)
ginx.Accepted(c, data)
ginx.Fail(c, ginx.CodeConflict, "already exists")
ginx.BadRequest(c, "invalid name")
ginx.NotFound(c, "no such vm")
ginx.InternalError(c, err)
```

### 已知限制

1. **slice / map 元素里的 `env` tag 不生效。** 反射只递归结构体和非 nil 结构体指针。
   语义上也说不通——slice 每个元素会拿到同一个环境变量值
2. **nil 结构体指针不会被分配。** 想用环境变量配的段落别用指针
3. **环境变量为空串等于没设**，无法用环境变量把字段显式置成空串
4. **`logx.Init` 改的是 `slog` 全局默认 logger**，一个进程只应调用一次
5. **`ReleaseMode` 改的是 gin 的全局模式**，这是 gin 自己的设计

### 依赖

只有三个：`gin`、`lumberjack`、`yaml.v3`。

不用 viper（`yaml.v3` 两行够了，环境变量由 `env` tag 接管），不用
`samber/slog-multi`（级别分流的 fanout handler 手写四十行），不用 `google/uuid`
（request id 用 `crypto/rand.Text()`）。库的依赖会传染给每个使用方，所以吝啬。

`go` 指令钉在 `1.24.0`，gin 钉在 v1.11.0，对齐 nebula-server 的工具链。

---

## zenpage

列表分页的三段活：取参校验、算 `LIMIT/OFFSET`、拼响应。设计文档见
[doc/zenpage/design.md](doc/zenpage/design.md)。

**零依赖**——`Parse` 收字符串、`Result` 是纯结构体，不认识 gin 也不认识数据库。

### 快速开始

```go
func (h *Handler) List(c *gin.Context) {
    // 空串 = 没传，走缺省值；传了但不合法就报错，两个参数的问题一次报全
    p, err := zenpage.Parse(c.Query("pageNum"), c.Query("pageSize"), zenpage.Default())
    if err != nil {
        ginx.Fail(c, ginx.CodeInvalidParam, err.Error())
        return
    }
    items, total, err := h.svc.List(c.Request.Context(), p)
    if err != nil {
        ginx.InternalError(c, err)
        return
    }
    ginx.Success(c, zenpage.NewResult(items, total, p))
}
```

repo 层两个占位符都从 `Params` 上来：

```go
q := "SELECT ... WHERE " + cond + " ORDER BY created_time ASC, id ASC LIMIT ? OFFSET ?"
err := db.SelectContext(ctx, &items, q, append(args, p.Limit(), p.Offset())...)
```

排序必须**稳定**（带上唯一列做末位排序键），否则翻页时行会重复或漏掉——这一条
库管不了，写 SQL 的时候自己注意。

### 响应形状

```json
{ "list": [], "total": 41, "pageNum": 2, "pageSize": 20, "lastPage": 3 }
```

字段名对齐前端 `nodejsprojects/portal`（zen-router）底座 `useList` 的既有约定。

- `list` 为 nil 时序列化成 `[]` 而不是 `null`（前端拿 `null` 去 `.map` 会抛错）
- **`lastPage` = 最后一个有效页号 = 总页数**（总数为 0 时是 1）。
  ⚠️ 和 Java PageHelper 的同名字段不是一个意思，那边是 `navigateLastPage`
  （页码导航窗口末页），总页数超过 8 时两者不等。前端在「当前页超出范围」时会拿这个值
  重发请求，给错了会把用户送到另一个空页
- 只有这 5 个字段。PageHelper 那 16 个页码导航字段前端用 `total`/`pageNum`/`pageSize`
  自己就能算，不回

### 缺省值与上限

```go
zenpage.Default()   // DefaultPageSize 20, MaxPageSize 200, MaxPageNum 100000
```

要改就取一份自己改：

```go
lim := zenpage.Default()
lim.MaxPageSize = 500
```

`MaxPageNum` 不是凑数的：`pageNum` 不设上限时，`pageNum=9223372036854775807` 会让
`(pageNum-1)*pageSize` 溢出成负数 → 负 OFFSET → 被数据库拒掉 → 报成「数据库错误」，
掩盖了真正的参数错误。这条在 cmi-server 上实测过。

`Limits{}` 零值会被 `Parse` 拒掉，不静默降级。

### 开发

```bash
GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go test -race ./...
```
