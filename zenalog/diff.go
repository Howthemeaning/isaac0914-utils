// Package zenalog 活动日志库：手动埋点写 ES（typeless 线格式，ES 7+），flat / by-trace 双模式查询。
// 设计见 doc/zenalog/design.md。
package zenalog

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"
)

// maxLevenshteinLen slice 编辑距离对齐的长度上限，超过退化为按位比较。
// O(n×m) 的 DP 在配置行数（几十到几百）下无感，防御的是异常超长输入。
const maxLevenshteinLen = 2000

// nilText 空值在 From/To 里的展示
const nilText = "<nil>"

// maskText 脱敏字段在 From/To 里的展示
const maskText = "***"

// Change 结构化 diff 的单条变更。Field 是嵌套路径（如 "spec.bandwidth"，
// slice 元素带索引如 "rows[0]"），From/To 是格式化后的值。
type Change struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

var timeType = reflect.TypeOf(time.Time{})

// DiffStructs 反射对比变更前后两个对象，输出字段级变更集（对应 javers）。
// 规则：字段名取 json tag；嵌套结构体/指针递归；`zenalog:"-"` 忽略；
// `zenalog:"mask"` 脱敏（From/To 记 ***）；slice 用编辑距离对齐（超长退化按位比）；
// 循环引用防御；不可比类型（func/chan）跳过。
func DiffStructs(before, after any) []Change {
	d := differ{visited: map[[2]uintptr]bool{}}
	d.diff("", reflect.ValueOf(before), reflect.ValueOf(after), false)
	return d.out
}

type differ struct {
	out     []Change
	visited map[[2]uintptr]bool // 已走过的 (before,after) 指针对，破环
}

// diff 递归对比。masked 为真时整棵子树只做一次脱敏叶子比较。
func (d *differ) diff(path string, b, a reflect.Value, masked bool) {
	if !b.IsValid() || !a.IsValid() {
		if !b.IsValid() && !a.IsValid() {
			return
		}
		d.leaf(path, b, a, masked)
		return
	}
	if b.Type() != a.Type() {
		d.leaf(path, b, a, masked)
		return
	}
	if masked {
		d.leaf(path, b, a, true)
		return
	}

	switch b.Kind() {
	case reflect.Pointer, reflect.Interface:
		if b.IsNil() && a.IsNil() {
			return
		}
		if b.IsNil() || a.IsNil() {
			d.leaf(path, b, a, false)
			return
		}
		if b.Kind() == reflect.Pointer {
			key := [2]uintptr{b.Pointer(), a.Pointer()}
			if d.visited[key] {
				return
			}
			d.visited[key] = true
		}
		d.diff(path, b.Elem(), a.Elem(), false)
	case reflect.Struct:
		if b.Type() == timeType {
			d.leaf(path, b, a, false)
			return
		}
		d.diffStruct(path, b, a)
	case reflect.Slice, reflect.Array:
		d.diffSlice(path, b, a)
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		// 不可比类型：跳过该字段而不是报错
	default:
		// 基本类型与 map：叶子比较（map 用规范化 JSON 整体比）
		d.leaf(path, b, a, false)
	}
}

// diffStruct 逐字段递归：字段名取 json tag，zenalog:"-" 忽略、"mask" 脱敏
func (d *differ) diffStruct(path string, b, a reflect.Value) {
	t := b.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		zTag := field.Tag.Get("zenalog")
		if zTag == "-" {
			continue
		}
		name := field.Name
		if jTag := field.Tag.Get("json"); jTag != "" {
			jName, _, _ := strings.Cut(jTag, ",")
			if jName == "-" {
				continue
			}
			if jName != "" {
				name = jName
			}
		}
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		d.diff(childPath, b.Field(i), a.Field(i), zTag == "mask")
	}
}

// diffSlice 编辑距离对齐：配对元素递归（路径带索引），未配对报增删。
// 超过 maxLevenshteinLen 退化为按位比较并记 debug 日志。
func (d *differ) diffSlice(path string, b, a reflect.Value) {
	n, m := b.Len(), a.Len()
	if n > maxLevenshteinLen || m > maxLevenshteinLen {
		slog.Debug("zenalog: slice too long, fall back to positional diff",
			"path", path, "before_len", n, "after_len", m, "limit", maxLevenshteinLen)
		d.diffSlicePositional(path, b, a)
		return
	}

	// 规范化串做相等判定
	bc := make([]string, n)
	for i := range n {
		bc[i] = canonical(b.Index(i))
	}
	ac := make([]string, m)
	for j := range m {
		ac[j] = canonical(a.Index(j))
	}

	// 标准编辑距离 DP
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
		dp[i][0] = i
	}
	for j := 1; j <= m; j++ {
		dp[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := 1
			if bc[i-1] == ac[j-1] {
				cost = 0
			}
			dp[i][j] = min(dp[i-1][j-1]+cost, dp[i-1][j]+1, dp[i][j-1]+1)
		}
	}

	// 回溯，平局偏好：相等对角 > 替换 > 删除 > 插入
	type op struct{ kind, bi, ai int } // kind: 0 配对递归 1 删除 2 新增
	var ops []op
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && bc[i-1] == ac[j-1] && dp[i][j] == dp[i-1][j-1]:
			i, j = i-1, j-1
		case i > 0 && j > 0 && dp[i][j] == dp[i-1][j-1]+1:
			ops = append(ops, op{0, i - 1, j - 1})
			i, j = i-1, j-1
		case i > 0 && dp[i][j] == dp[i-1][j]+1:
			ops = append(ops, op{1, i - 1, 0})
			i--
		default:
			ops = append(ops, op{2, 0, j - 1})
			j--
		}
	}
	// 回溯是倒序收集的，翻回正序输出
	for k := len(ops) - 1; k >= 0; k-- {
		o := ops[k]
		switch o.kind {
		case 0:
			d.diff(fmt.Sprintf("%s[%d]", path, o.ai), b.Index(o.bi), a.Index(o.ai), false)
		case 1:
			d.out = append(d.out, Change{
				Field: fmt.Sprintf("%s[%d]", path, o.bi),
				From:  formatValue(b.Index(o.bi)),
				To:    nilText,
			})
		case 2:
			d.out = append(d.out, Change{
				Field: fmt.Sprintf("%s[%d]", path, o.ai),
				From:  nilText,
				To:    formatValue(a.Index(o.ai)),
			})
		}
	}
}

// diffSlicePositional 按位比较的退化路径
func (d *differ) diffSlicePositional(path string, b, a reflect.Value) {
	n, m := b.Len(), a.Len()
	for i := 0; i < max(n, m); i++ {
		elemPath := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case i < n && i < m:
			d.diff(elemPath, b.Index(i), a.Index(i), false)
		case i < n:
			d.out = append(d.out, Change{Field: elemPath, From: formatValue(b.Index(i)), To: nilText})
		default:
			d.out = append(d.out, Change{Field: elemPath, From: nilText, To: formatValue(a.Index(i))})
		}
	}
}

// leaf 叶子比较：规范化串相等即无变更；masked 时值一律记 ***
func (d *differ) leaf(path string, b, a reflect.Value, masked bool) {
	if canonical(b) == canonical(a) {
		return
	}
	if masked {
		d.out = append(d.out, Change{Field: path, From: maskText, To: maskText})
		return
	}
	d.out = append(d.out, Change{Field: path, From: formatValue(b), To: formatValue(a)})
}

// canonical 相等判定用的规范化串：JSON 序列化（map 键有序），失败退回 %v
func canonical(v reflect.Value) string {
	if !v.IsValid() {
		return "null"
	}
	if !v.CanInterface() {
		return fmt.Sprintf("%v", v)
	}
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return fmt.Sprintf("%v", v.Interface())
	}
	return string(b)
}

// formatValue From/To 的展示值：字符串原样、时间 RFC3339、空指针 <nil>、
// 复合类型用规范化 JSON
func formatValue(v reflect.Value) string {
	if !v.IsValid() {
		return nilText
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nilText
		}
		return formatValue(v.Elem())
	case reflect.String:
		return v.String()
	case reflect.Struct:
		if v.Type() == timeType && v.CanInterface() {
			return v.Interface().(time.Time).UTC().Format(time.RFC3339)
		}
	}
	switch v.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return fmt.Sprintf("%v", v.Interface())
	}
	return canonical(v)
}
