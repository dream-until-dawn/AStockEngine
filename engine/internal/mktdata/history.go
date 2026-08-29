package mktdata

// History 是策略能拿到的**唯一**行情入口。
//
// 它是约束 C1 的落地形式。ROADMAP 原文写的是「用类型系统在编译期禁止越界访问」，
// 但 Go 无法对一个整数偏移做编译期约束 —— `At(-1)` 一样能编译。
// 这里给出的是更强的保证：
//
//	**未来数据物理上不在视图内。**
//
// History 只携带 [start, cur] 这个闭区间，cur 是当前 bar 的行号。
// 未来的行号大于 cur，落在区间之外 —— 不是「拒绝访问」，是「不在里面」。
// 且本类型不提供任何返回底层切片的方法，Columns 的字段也全部非导出，
// 包外代码无从绕过。
//
// History 是值类型，引擎每步更新 cur 后按值传入，无堆分配。
type History struct {
	col   *Columns
	start int32 // 该标的首行（含）
	cur   int32 // 当前 bar 行号（含）；为 start-1 表示该标的尚未有 bar
}

// Valid 报告该标的在当前时点是否已有至少一根 bar。
//
// 上市晚于回测起点的标的，在其上市前所有步骤中 Valid 均为 false ——
// 这正是 C3（幸存者偏差）在引擎侧的体现：标的不是「一直存在」的。
func (h History) Valid() bool { return h.col != nil && h.cur >= h.start }

// Len 返回当前可见的 bar 数量。
func (h History) Len() int {
	if !h.Valid() {
		return 0
	}
	return int(h.cur - h.start + 1)
}

// At 返回向前回溯 back 根的 bar：At(0) 是当前 bar，At(1) 是前一根。
//
// back 为负数返回 false —— 那是调用方的 bug，但即便如此也取不到未来数据，
// 因为负偏移对应的行号超出 cur，函数直接拒绝。
func (h History) At(back int) (Bar, bool) {
	row, ok := h.row(back)
	if !ok {
		return Bar{}, false
	}
	return h.col.barAt(row), true
}

// Close 是取收盘价的快捷方法，避免为读一个字段构造整个 Bar。
// 指标的热路径大量使用，值得单列。
func (h History) Close(back int) (int64, bool) {
	row, ok := h.row(back)
	if !ok {
		return 0, false
	}
	return h.col.close[row], true
}

// High / Low / Open / Volume 同 Close，供指标热路径使用。
func (h History) High(back int) (int64, bool) {
	row, ok := h.row(back)
	if !ok {
		return 0, false
	}
	return h.col.high[row], true
}

func (h History) Low(back int) (int64, bool) {
	row, ok := h.row(back)
	if !ok {
		return 0, false
	}
	return h.col.low[row], true
}

func (h History) Open(back int) (int64, bool) {
	row, ok := h.row(back)
	if !ok {
		return 0, false
	}
	return h.col.open[row], true
}

func (h History) Volume(back int) (int64, bool) {
	row, ok := h.row(back)
	if !ok {
		return 0, false
	}
	return h.col.volume[row], true
}

// TradingDay 返回向前回溯 back 根的交易日。
func (h History) TradingDay(back int) (int32, bool) {
	row, ok := h.row(back)
	if !ok {
		return 0, false
	}
	return h.col.tradingDay[row], true
}

// row 把「回溯 back 根」换算为全局行号，并做边界检查。
//
// 这是整个 C1 保证的收口点：任何越界（含负偏移）都在此拒绝，
// 且返回的行号必然 <= cur。
func (h History) row(back int) (int32, bool) {
	if h.col == nil || back < 0 {
		return 0, false
	}
	row := h.cur - int32(back)
	if row < h.start || row > h.cur {
		return 0, false
	}
	return row, true
}
