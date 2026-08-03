package zenpage

import (
	"strconv"
	"strings"
	"testing"
)

// 空串走缺省值：没传分页参数的请求应该照常返回第一页
func TestParseEmptyUsesDefaults(t *testing.T) {
	p, err := Parse("", "", Default())
	if err != nil {
		t.Fatalf("empty params should be valid: %v", err)
	}
	if p.PageNum != 1 || p.PageSize != DefaultPageSize {
		t.Fatalf("want pageNum=1 pageSize=%d, got %+v", DefaultPageSize, p)
	}
}

func TestParseValid(t *testing.T) {
	p, err := Parse("3", "50", Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.PageNum != 3 || p.PageSize != 50 {
		t.Fatalf("want 3/50, got %+v", p)
	}
}

// 两个参数都错时必须一次报全，不能让调用方试一次改一个
func TestParseReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Parse("abc", "9999", Default())
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pageNum") || !strings.Contains(msg, "pageSize") {
		t.Fatalf("error must name both bad fields, got %q", msg)
	}
	// 原始值要回显，否则调用方不知道自己传了什么
	if !strings.Contains(msg, `"abc"`) || !strings.Contains(msg, `"9999"`) {
		t.Fatalf("error must echo the offending values, got %q", msg)
	}
}

func TestParseRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name             string
		pageNum, pageSie string
	}{
		{"pageNum 为 0", "0", ""},
		{"pageNum 为负", "-1", ""},
		{"pageNum 超上限", strconv.Itoa(MaxPageNum + 1), ""},
		{"pageSize 为 0", "", "0"},
		{"pageSize 超上限", "", strconv.Itoa(MaxPageSize + 1)},
		{"pageNum 不是整数", "1.5", ""},
		{"pageSize 是空格", "", " "},
		// 整型溢出：不设上限时 (pageNum-1)*pageSize 会溢出成负 OFFSET，
		// 被数据库拒掉后报成「数据库错误」，掩盖了真正的参数错误
		{"pageNum 是 int64 上限", "9223372036854775807", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(c.pageNum, c.pageSie, Default()); err == nil {
				t.Fatalf("Parse(%q, %q) should fail", c.pageNum, c.pageSie)
			}
		})
	}
}

// Limits 配错时报错，不静默降级成零值——配错的时候正是最需要知道的时候
func TestParseRejectsZeroLimits(t *testing.T) {
	if _, err := Parse("1", "20", Limits{}); err == nil {
		t.Fatal("zero Limits should be rejected")
	}
	if _, err := Parse("1", "20", Limits{DefaultPageSize: 20, MaxPageSize: 200}); err == nil {
		t.Fatal("Limits with MaxPageNum=0 should be rejected")
	}
}

// 自定义 Limits 要真的生效
func TestParseHonoursCustomLimits(t *testing.T) {
	lim := Limits{DefaultPageSize: 5, MaxPageSize: 10, MaxPageNum: 3}
	p, err := Parse("", "", lim)
	if err != nil || p.PageSize != 5 {
		t.Fatalf("custom default not applied: %+v err=%v", p, err)
	}
	if _, err := Parse("", "11", lim); err == nil {
		t.Fatal("pageSize above custom max should fail")
	}
	if _, err := Parse("4", "", lim); err == nil {
		t.Fatal("pageNum above custom max should fail")
	}
}

func TestOffsetAndLimit(t *testing.T) {
	cases := []struct {
		pageNum, pageSize, wantOffset int
	}{
		{1, 20, 0},
		{2, 20, 20},
		{3, 50, 100},
		{MaxPageNum, MaxPageSize, (MaxPageNum - 1) * MaxPageSize},
	}
	for _, c := range cases {
		p := Params{PageNum: c.pageNum, PageSize: c.pageSize}
		if got := p.Offset(); got != c.wantOffset {
			t.Fatalf("Offset() for %+v: want %d, got %d", p, c.wantOffset, got)
		}
		if got := p.Limit(); got != c.pageSize {
			t.Fatalf("Limit() for %+v: want %d, got %d", p, c.pageSize, got)
		}
		// OFFSET 永远不能是负数，负值会被数据库当语法错误拒掉
		if p.Offset() < 0 {
			t.Fatalf("Offset() must never be negative, got %d for %+v", p.Offset(), p)
		}
	}
}

// 手搓的零值 Params 不能让 Offset 算出负数或 panic
func TestOffsetOnZeroParams(t *testing.T) {
	if got := (Params{}).Offset(); got != 0 {
		t.Fatalf("zero Params Offset() should be 0, got %d", got)
	}
}
