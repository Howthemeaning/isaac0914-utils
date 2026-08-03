# zenpage 设计文档

- 日期：2026-08-03
- 状态：已定稿
- 模块路径：`github.com/isaac0914/utils/zenpage`

## 1. 背景与目标

分页是每个列表接口都要写的三段活：

1. 从 query 取 `pageNum` / `pageSize`，校验、给缺省值
2. 算 `LIMIT ? OFFSET ?` 的两个占位符
3. 把 `total` 换算成总页数，和 list 一起拼进响应

参考对象是 `nebula-server`，它这三段分在三个地方：

| 段 | nebula 的位置 | 形态 |
|---|---|---|
| 参数校验 | 每个 handler 里 | `if req.PageSize <= 0 { req.PageSize = 20 }`，**6 个 handler 各抄一遍** |
| SQL 偏移 | `internal/entity/condition/condition.go` | `Pagination.Offset()` / `BuildPaginationSuffix` / `PaginationArgs` |
| 响应构造 | `handler/response/page_info.go` | `PageInfo[T]` + `NewPageInfo`，21 个字段 |

目标：把三段收进一个包，新接口一行取参、一行拼响应，且把下面几个坑一次性钉死。

## 2. 从 nebula 借鉴与修正的点

### 借鉴

1. **响应体用泛型**（`PageInfo[T]`）。每个列表接口不用各写一个 wrapper 结构体。
2. **nil list 换成空 slice**。`NewPageInfo` 开头那三行的理由是对的：JSON 里
   `null` 和 `[]` 对前端不是一回事，前端拿 `null` 去 `.length` / `.map` 会直接抛错。
3. **总数由 repo 层 COUNT 填充，分页参数只读**。响应构造不碰数据库。

### 修正

#### 2.1 `lastPage` 的语义（**这条最重要**）

nebula 的 `PageInfo` 兼容 Java PageHelper，那边：

- `pages` = 总页数
- `lastPage` = `navigateLastPage` = **页码导航窗口的末页**（窗口默认 8 格）

所以总页数 ≤ 8 时 `lastPage == pages`，超过 8 就不等了：2000 条 / 每页 20 = 100 页，
PageHelper 的 `lastPage` 会是导航窗口末端而不是 100。

而前端 `nodejsprojects/portal`（zen-router）底座的 `useList` 在**当前页超出范围**时
（`total > 0` 但 `list` 为空，例如翻到第 5 页后把数据删了）会拿 `lastPage`
**重新发一次请求**。给它一个导航窗口末页，用户就被送到另一个空页，然后重试次数用完、
停在空列表上。

**本包的 `LastPage` = 最后一个有效页号 = 总页数（总数为 0 时是 1）。**
名字沿用 `lastPage` 是因为前端字段名已经这样（改名等于白屏），但语义按前端的实际用法定。
从 nebula 迁过来的人要注意这个差异。

#### 2.2 不带页码导航

PageHelper 那套 `navigatePageNums` / `prePage` / `nextPage` / `isFirstPage` /
`hasNextPage` 共 16 个字段，是给「服务端渲染页码条」用的。前端拿
`total` + `pageNum` + `pageSize` 自己就能算出全部，antd 的 `Pagination` 也只要这三个。

所以本包只回 5 个字段。少回字段是可加的，回了又没人用的字段是删不掉的
（公开 API 发布后不改签名）。

顺带一提，nebula 那份 `calcNavigatePageNums` 里 `end > p.Pages` 那个分支
shadow 了外层的 `end`，越界时会从 `p.Pages` 倒着填。既然不要这 16 个字段，
这段逻辑一并不要。

#### 2.3 `pageNum` 必须有上限

nebula 只判 `PageSize <= 0`，`Page` 不设上限。`pageNum` 不设上限的后果是整型溢出：

```
pageNum = 9223372036854775807
(pageNum - 1) * pageSize  →  溢出成负数  →  负的 OFFSET
```

MySQL 拒掉负 OFFSET，错误落到「数据库错误」分支，返回 `DATABASE_ERROR`——
而这本该是参数错误。cmi-server 上实测过这条。

所以 `MaxPageNum` 是缺省 `Limits` 的一部分（10 万），且 `Offset()` 自己也不会返回负数。

#### 2.4 参数问题一次报全

nebula 是「悄悄改成 20」，本包是**报错**。理由：`pageSize=9999` 大概是调用方误解了
接口，静默改成 20 会让它以为拿到的是全部。报错时两个参数的问题一起报，
不让人试一次改一个（库开发规约）。

`pageSize` **没传**才走缺省值，传了但不合法就是错。

## 3. 接口

```go
// 缺省值与上限
const (
    DefaultPageSize = 20
    MaxPageSize     = 200
    MaxPageNum      = 100000
)

type Limits struct {
    DefaultPageSize int
    MaxPageSize     int
    MaxPageNum      int
}
func Default() Limits

type Params struct {
    PageNum  int
    PageSize int
}
func Parse(pageNum, pageSize string, lim Limits) (Params, error)
func (p Params) Offset() int
func (p Params) Limit() int

type Result[T any] struct {
    List     []T   `json:"list"`
    Total    int64 `json:"total"`
    PageNum  int   `json:"pageNum"`
    PageSize int   `json:"pageSize"`
    LastPage int   `json:"lastPage"`
}
func NewResult[T any](list []T, total int64, p Params) *Result[T]
```

### 用法

```go
func (h *Handler) List(c *gin.Context) {
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

repo 层：

```go
q := "SELECT ... WHERE " + cond + " ORDER BY created_time ASC, id ASC LIMIT ? OFFSET ?"
err := db.SelectContext(ctx, &items, q, append(args, p.Limit(), p.Offset())...)
```

## 4. 设计取舍

### 4.1 为什么入口收字符串而不是 int

`Parse` 收的是 query 里的原始字符串，好处是能区分「没传」和「传了 0」，
并且能对 `pageNum=abc` 给出人能看懂的错误。收 int 就得靠框架的 binder 先转一次，
`abc` 会变成 gin 的 `strconv.ParseInt: parsing "abc": invalid syntax`。

**只有一个入口。** 不为「我手上已经是 int 了」再开一个 `Clamp(int, int)`——
库开发规约里明确禁止因为参数类型不同而分裂方法。手上是 int 的调用方
`strconv.Itoa` 一下即可。

### 4.2 为什么不依赖 gin

`Parse` 收字符串，`Result` 是纯结构体，所以 **zenpage 没有任何依赖**，
`import` 列表里只有 `fmt` / `strconv` / `strings`。

不做 `zenpage.Bind(c *gin.Context)` 这种便利函数：省下的是一次 `c.Query`，
换来的是整个包绑死 gin。调用方那行 `zenpage.Parse(c.Query("pageNum"), ...)`
已经足够短。

### 4.3 为什么 `Limits` 是值而不是包级变量

库开发规约：不引入包级可变全局状态。`Default()` 返回一份拷贝，
调用方改自己那份不影响别人。缺省值本身是 `const`，改不了。

### 4.4 `Limits` 配错要报错

`Limits{}` 零值会让 `pageSize` 缺省值变成 0、上限变成 0，任何请求都过不了。
静默用零值是最坏的选择——配错的时候正是最需要知道的时候，所以
`Parse` 第一件事就是校验 `Limits` 本身。

## 5. 不包含

| 不做 | 理由 |
|---|---|
| 游标 / keyset 分页 | 目前没有真实需求。等有了再加新类型，不改 `Params` 的签名 |
| 自动执行 COUNT 查询 | 那需要认识 sqlx 和 SQL 方言，库要保持零依赖。COUNT 留在 repo 层 |
| `pageNum` 超出范围时服务端自动夹到末页 | 前端已经靠 `lastPage` 重试，服务端再夹一次两边都在猜。 |
| 页码导航字段 | 见 2.2 |
| `allTotal`（筛选前总数） | zen-router 的 `ListResp` 里是可选字段，目前没有使用方 |

## 6. 已知限制

1. **`Params` 是值类型且没有校验标记**，手搓 `Params{}` 传给 `NewResult` 不会报错，
   只会得到 `LastPage: 1`。库不 panic 是硬要求，所以这里选择兜底而不是报错。
   正常路径永远从 `Parse` 拿 `Params`。
2. **`Total` 是 `int64`，`LastPage` 是 `int`。** 总数用 int64 是因为 `COUNT(*)`
   返回 int64；页数用 int 是因为它必然远小于总数。32 位平台上总页数超过 21 亿才会溢出，
   那之前 `MaxPageNum` 早就拦住了。
3. **错误信息里的字段名固定是 `pageNum` / `pageSize`**，与 `Result` 的 JSON 字段名一致。
   query 参数叫别的名字（例如 nebula 的 `page` / `page_size`）时，调用方自己取值传进来，
   但错误信息里显示的还是规范名。
