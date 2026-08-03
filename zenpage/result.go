package zenpage

// Result 分页响应体，放进响应壳的 data 里。
//
// 字段名对齐前端 `nodejsprojects/portal`（zen-router）底座 useList 的既有约定，
// 别再发明 page / current / totalPages 之类的同义词。
type Result[T any] struct {
	List     []T   `json:"list"`
	Total    int64 `json:"total"`
	PageNum  int   `json:"pageNum"`
	PageSize int   `json:"pageSize"`
	// LastPage 最后一个有效页号，即总页数（总数为 0 时是 1）。
	//
	// ⚠️ 与 Java PageHelper 的同名字段不是一个意思：那边的 lastPage 是
	// navigateLastPage（页码导航窗口的末页），总页数超过窗口大小时两者不等。
	// 前端在「当前页超出范围」（total > 0 但 list 为空，例如翻到第 5 页后把
	// 数据删了）时会拿这个值重新发一次请求，所以它必须是真正的最后一页，
	// 否则会把用户送到另一个空页。
	LastPage int `json:"lastPage"`
}

// NewResult 构造分页响应。
//
// list 为 nil 时换成空 slice：JSON 里 null 和 [] 对前端不是一回事，
// 前端拿 null 去 .length / .map 会直接抛错。
func NewResult[T any](list []T, total int64, p Params) *Result[T] {
	if list == nil {
		list = make([]T, 0)
	}
	return &Result[T]{
		List:     list,
		Total:    total,
		PageNum:  p.PageNum,
		PageSize: p.PageSize,
		LastPage: lastPage(total, p.PageSize),
	}
}

// lastPage 总页数，向上取整，至少 1。
// PageSize 非正时不做除法——Params 只能由 Parse 产出所以不会发生，
// 但库里不能因为调用方手搓了一个零值结构体就 panic。
func lastPage(total int64, pageSize int) int {
	if pageSize < 1 || total <= 0 {
		return 1
	}
	pages := (total + int64(pageSize) - 1) / int64(pageSize)
	return int(pages)
}
