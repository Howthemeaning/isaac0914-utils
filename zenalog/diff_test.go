package zenalog

import (
	"bytes"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"
)

type diffInner struct {
	Bandwidth int    `json:"bandwidth"`
	Note      string `json:"note,omitempty"`
}

type diffOuter struct {
	Name   string     `json:"name"`
	Spec   diffInner  `json:"spec"`
	Secret string     `json:"secret" zenalog:"mask"`
	Skip   string     `json:"skip" zenalog:"-"`
	NoJSON string     `json:"-"`
	Ptr    *diffInner `json:"ptr"`
}

type diffRow struct {
	ID string `json:"id"`
	V  int    `json:"v"`
}

type diffList struct {
	Rows []diffRow `json:"rows"`
}

// sortChanges 按 Field+From 排序，断言不依赖实现的输出顺序
func sortChanges(cs []Change) []Change {
	out := append([]Change(nil), cs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].From < out[j].From
	})
	return out
}

func assertChanges(t *testing.T, got, want []Change) {
	t.Helper()
	got, want = sortChanges(got), sortChanges(want)
	if len(got) != len(want) {
		t.Fatalf("changes count = %d, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDiffStructs(t *testing.T) {
	tests := []struct {
		name          string
		before, after any
		want          []Change
	}{
		{
			name:   "no change",
			before: diffOuter{Name: "a"},
			after:  diffOuter{Name: "a"},
			want:   nil,
		},
		{
			name:   "basic field change",
			before: diffOuter{Name: "a"},
			after:  diffOuter{Name: "b"},
			want:   []Change{{Field: "name", From: "a", To: "b"}},
		},
		{
			name:   "nested struct path",
			before: diffOuter{Spec: diffInner{Bandwidth: 100}},
			after:  diffOuter{Spec: diffInner{Bandwidth: 200}},
			want:   []Change{{Field: "spec.bandwidth", From: "100", To: "200"}},
		},
		{
			name:   "pointer nil to value",
			before: diffOuter{},
			after:  diffOuter{Ptr: &diffInner{Bandwidth: 1}},
			want:   []Change{{Field: "ptr", From: "<nil>", To: `{"bandwidth":1}`}},
		},
		{
			name:   "pointer value to nil",
			before: diffOuter{Ptr: &diffInner{Bandwidth: 1}},
			after:  diffOuter{},
			want:   []Change{{Field: "ptr", From: `{"bandwidth":1}`, To: "<nil>"}},
		},
		{
			name:   "pointer both set recurses",
			before: diffOuter{Ptr: &diffInner{Bandwidth: 1}},
			after:  diffOuter{Ptr: &diffInner{Bandwidth: 2}},
			want:   []Change{{Field: "ptr.bandwidth", From: "1", To: "2"}},
		},
		{
			name:   "top-level pointers are dereferenced",
			before: &diffOuter{Name: "a"},
			after:  &diffOuter{Name: "b"},
			want:   []Change{{Field: "name", From: "a", To: "b"}},
		},
		{
			name:   "mask redacts values",
			before: diffOuter{Secret: "old-password"},
			after:  diffOuter{Secret: "new-password"},
			want:   []Change{{Field: "secret", From: "***", To: "***"}},
		},
		{
			name:   "mask unchanged reports nothing",
			before: diffOuter{Secret: "same"},
			after:  diffOuter{Secret: "same"},
			want:   nil,
		},
		{
			name:   "zenalog dash skips field",
			before: diffOuter{Skip: "a"},
			after:  diffOuter{Skip: "b"},
			want:   nil,
		},
		{
			name:   "json dash skips field",
			before: diffOuter{NoJSON: "a"},
			after:  diffOuter{NoJSON: "b"},
			want:   nil,
		},
		{
			name:   "slice insert at head reports one addition",
			before: diffList{Rows: []diffRow{{ID: "a", V: 1}, {ID: "b", V: 2}, {ID: "c", V: 3}}},
			after:  diffList{Rows: []diffRow{{ID: "x", V: 9}, {ID: "a", V: 1}, {ID: "b", V: 2}, {ID: "c", V: 3}}},
			want:   []Change{{Field: "rows[0]", From: "<nil>", To: `{"id":"x","v":9}`}},
		},
		{
			name:   "slice delete in middle reports one removal",
			before: diffList{Rows: []diffRow{{ID: "a", V: 1}, {ID: "b", V: 2}, {ID: "c", V: 3}}},
			after:  diffList{Rows: []diffRow{{ID: "a", V: 1}, {ID: "c", V: 3}}},
			want:   []Change{{Field: "rows[1]", From: `{"id":"b","v":2}`, To: "<nil>"}},
		},
		{
			name:   "slice element modify recurses with index",
			before: diffList{Rows: []diffRow{{ID: "a", V: 1}, {ID: "b", V: 2}}},
			after:  diffList{Rows: []diffRow{{ID: "a", V: 1}, {ID: "b", V: 5}}},
			want:   []Change{{Field: "rows[1].v", From: "2", To: "5"}},
		},
		{
			name:   "slice reorder pairs by position",
			before: diffList{Rows: []diffRow{{ID: "a", V: 1}, {ID: "b", V: 2}}},
			after:  diffList{Rows: []diffRow{{ID: "b", V: 2}, {ID: "a", V: 1}}},
			want: []Change{
				{Field: "rows[0].id", From: "a", To: "b"},
				{Field: "rows[0].v", From: "1", To: "2"},
				{Field: "rows[1].id", From: "b", To: "a"},
				{Field: "rows[1].v", From: "2", To: "1"},
			},
		},
		{
			name: "primitive slice",
			before: struct {
				Tags []string `json:"tags"`
			}{Tags: []string{"x", "y"}},
			after: struct {
				Tags []string `json:"tags"`
			}{Tags: []string{"x", "z"}},
			want: []Change{{Field: "tags[1]", From: "y", To: "z"}},
		},
		{
			name: "time.Time is a leaf",
			before: struct {
				T time.Time `json:"t"`
			}{T: time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)},
			after: struct {
				T time.Time `json:"t"`
			}{T: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)},
			want: []Change{{Field: "t", From: "2026-07-30T00:00:00Z", To: "2026-07-31T00:00:00Z"}},
		},
		{
			name: "map compared as canonical leaf",
			before: struct {
				M map[string]int `json:"m"`
			}{M: map[string]int{"a": 1}},
			after: struct {
				M map[string]int `json:"m"`
			}{M: map[string]int{"a": 2}},
			want: []Change{{Field: "m", From: `{"a":1}`, To: `{"a":2}`}},
		},
		{
			name: "incomparable func field skipped",
			before: struct {
				Fn   func() `json:"fn"`
				Name string `json:"name"`
			}{Fn: func() {}, Name: "a"},
			after: struct {
				Fn   func() `json:"fn"`
				Name string `json:"name"`
			}{Fn: func() {}, Name: "b"},
			want: []Change{{Field: "name", From: "a", To: "b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertChanges(t, DiffStructs(tt.before, tt.after), tt.want)
		})
	}
}

type diffNode struct {
	Name string    `json:"name"`
	Next *diffNode `json:"next"`
}

func TestDiffStructsCycle(t *testing.T) {
	a := &diffNode{Name: "a"}
	a.Next = a
	b := &diffNode{Name: "b"}
	b.Next = b

	// 不炸不挂即可；顶层 name 的变更要在
	got := DiffStructs(a, b)
	found := false
	for _, c := range got {
		if c.Field == "name" && c.From == "a" && c.To == "b" {
			found = true
		}
	}
	if !found {
		t.Errorf("cycle diff should still report name change, got %+v", got)
	}
}

func TestDiffStructsLongSliceFallsBack(t *testing.T) {
	// 超过 2000 退化为按位比较：顶部插入会报出大量变更（Levenshtein 只会报 1 条），
	// 并记一条 debug 日志
	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	long := make([]int, 2001)
	for i := range long {
		long[i] = i
	}
	shifted := append([]int{-1}, long...)

	type holder struct {
		Xs []int `json:"xs"`
	}
	got := DiffStructs(holder{Xs: long}, holder{Xs: shifted})
	if len(got) <= 1 {
		t.Errorf("positional fallback should report many changes, got %d", len(got))
	}
	if !strings.Contains(logBuf.String(), "positional") {
		t.Errorf("fallback should log a debug line, got: %s", logBuf.String())
	}
}

func TestDiffStructsLevenshteinWithinLimit(t *testing.T) {
	// 2000 以内走编辑距离：顶部插入只报 1 条
	long := make([]int, 1999)
	for i := range long {
		long[i] = i
	}
	shifted := append([]int{-1}, long...)

	type holder struct {
		Xs []int `json:"xs"`
	}
	got := DiffStructs(holder{Xs: long}, holder{Xs: shifted})
	want := []Change{{Field: "xs[0]", From: "<nil>", To: "-1"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %d changes %v, want %v", len(got), got, want)
	}
}

func TestDiffStructsAcceptsNilRoots(t *testing.T) {
	if got := DiffStructs(nil, nil); len(got) != 0 {
		t.Errorf("nil roots should diff to nothing, got %+v", got)
	}
	got := DiffStructs(nil, diffOuter{Name: "a"})
	if len(got) == 0 {
		t.Error("nil -> value should report a change")
	}
	// 顶层出现，具体形状不苛求，但不能 panic
	_ = fmt.Sprintf("%v", got)
}
