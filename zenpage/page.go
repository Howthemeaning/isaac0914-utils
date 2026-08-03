// Package zenpage 提供列表分页的参数校验、SQL 偏移量计算和响应构造。
//
// 分页这件事每个 server 都要写一遍，重复的部分是三段：从 query 里取
// pageNum/pageSize 并校验、算 LIMIT/OFFSET、把总数换算成总页数拼进响应壳。
// 三段都有各自的坑（见 doc/zenpage/design.md），所以收在一个包里。
//
// 本包没有任何第三方依赖，也不认识 gin：入口收字符串，由调用方从自己的
// 框架里取。
package zenpage

import (
	"fmt"
	"strconv"
	"strings"
)

// 缺省值与上限。三个都能按需覆盖，这里只是「不想操心时」的取值。
//
// MaxPageNum 存在的原因是防整型溢出：pageNum 不设上限时，
// pageNum=9223372036854775807 会让 (pageNum-1)*pageSize 溢出成负数，
// 变成负的 OFFSET 被数据库拒掉，最后报成「数据库错误」——而这本该是参数错误。
const (
	DefaultPageSize = 20
	MaxPageSize     = 200
	MaxPageNum      = 100000
)

// Limits 分页参数的取值范围与缺省值。零值不可用，用 Default() 取一份再改。
type Limits struct {
	DefaultPageSize int // pageSize 没传时用它
	MaxPageSize     int // pageSize 上限，挡「一次拉全库」
	MaxPageNum      int // pageNum 上限，挡整型溢出
}

// Default 返回一份缺省 Limits。返回值而不是包级变量，避免调用方互相踩。
func Default() Limits {
	return Limits{
		DefaultPageSize: DefaultPageSize,
		MaxPageSize:     MaxPageSize,
		MaxPageNum:      MaxPageNum,
	}
}

// Params 校验过的分页参数。只能由 Parse 产出，字段值一定在 Limits 范围内。
type Params struct {
	PageNum  int
	PageSize int
}

// Parse 校验分页参数。pageNum / pageSize 收的是 query 里的原始字符串
// （如 c.Query("pageNum")），空串表示没传、走缺省值。
//
// 两个参数的问题会一次报全，不让调用方试一次改一个。错误信息里的字段名固定叫
// pageNum / pageSize，与 Result 的 JSON 字段名一致；query 参数名叫别的名字时，
// 调用方自己取值传进来即可。
func Parse(pageNum, pageSize string, lim Limits) (Params, error) {
	if lim.DefaultPageSize <= 0 || lim.MaxPageSize <= 0 || lim.MaxPageNum <= 0 {
		return Params{}, fmt.Errorf("zenpage: invalid Limits %+v, use zenpage.Default()", lim)
	}

	var problems []string
	num := parseOne("pageNum", pageNum, 1, 1, lim.MaxPageNum, &problems)
	size := parseOne("pageSize", pageSize, lim.DefaultPageSize, 1, lim.MaxPageSize, &problems)
	if len(problems) > 0 {
		return Params{}, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return Params{PageNum: num, PageSize: size}, nil
}

// parseOne 空串取 def，否则必须是 [min, max] 内的整数。不合法时往 problems 追加。
func parseOne(name, raw string, def, min, max int, problems *[]string) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		*problems = append(*problems,
			fmt.Sprintf("%s must be an integer in [%d, %d], got %q", name, min, max, raw))
		return def
	}
	return v
}

// Offset SQL 的 OFFSET。PageNum 从 1 开始，所以第一页是 0。
func (p Params) Offset() int {
	if p.PageNum < 1 || p.PageSize < 1 {
		return 0
	}
	return (p.PageNum - 1) * p.PageSize
}

// Limit SQL 的 LIMIT，等于 PageSize。单独给一个方法是为了让
// `LIMIT ? OFFSET ?` 两个占位符的取值都从 Params 上来，不用调用方拆字段。
func (p Params) Limit() int {
	return p.PageSize
}
