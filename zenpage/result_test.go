package zenpage

import (
	"encoding/json"
	"testing"
)

func TestLastPage(t *testing.T) {
	cases := []struct {
		name     string
		total    int64
		pageSize int
		want     int
	}{
		{"空结果也是第 1 页", 0, 20, 1},
		{"不满一页", 3, 20, 1},
		{"正好整页", 40, 20, 2},
		{"有余数要向上取整", 41, 20, 3},
		{"每页 1 条", 7, 1, 7},
		// PageHelper 的 lastPage 是导航窗口末页（默认窗口 8），这里必须是真正的
		// 末页：前端拿它重试请求，给 8 会把用户送到另一个空页
		{"总页数超过导航窗口", 2000, 20, 100},
		{"pageSize 非法时兜底为 1", 100, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastPage(c.total, c.pageSize); got != c.want {
				t.Fatalf("lastPage(%d, %d): want %d, got %d", c.total, c.pageSize, c.want, got)
			}
		})
	}
}

// nil list 必须序列化成 []，不能是 null——前端拿 null 去 .map 会直接抛错
func TestNewResultNilListMarshalsAsEmptyArray(t *testing.T) {
	r := NewResult[string](nil, 0, Params{PageNum: 1, PageSize: 20})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["list"]) != "[]" {
		t.Fatalf("nil list must marshal as [], got %s", got["list"])
	}
}

// JSON 字段名是和前端的契约，改名等于线上白屏，钉死
func TestResultJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(NewResult([]int{1, 2}, 41, Params{PageNum: 2, PageSize: 20}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"list", "total", "pageNum", "pageSize", "lastPage"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing field %q in %s", k, b)
		}
	}
	if len(got) != 5 {
		t.Fatalf("Result should have exactly 5 fields, got %d: %s", len(got), b)
	}
	if got["total"].(float64) != 41 || got["pageNum"].(float64) != 2 || got["lastPage"].(float64) != 3 {
		t.Fatalf("field values wrong: %s", b)
	}
}

// Params 原样带回响应，前端靠它确认服务端实际用的分页
func TestNewResultEchoesParams(t *testing.T) {
	p := Params{PageNum: 3, PageSize: 50}
	r := NewResult([]string{"a"}, 200, p)
	if r.PageNum != 3 || r.PageSize != 50 {
		t.Fatalf("params not echoed: %+v", r)
	}
	if r.LastPage != 4 {
		t.Fatalf("lastPage for total=200 pageSize=50 should be 4, got %d", r.LastPage)
	}
}
