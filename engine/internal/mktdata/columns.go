// Package mktdata 持有行情的列式内存表示，并提供受控的访问视图。
//
// **本包是约束 C1（未来函数防护）的实现基础**：Columns 的字段全部非导出，
// 且不提供任何返回底层切片的方法。包外代码只能通过 History 视图访问数据，
// 而 History 持有的区间末端就是当前 bar —— 未来不是「不可访问」，
// 是物理上不在视图内。
//
// 布局决策见 docs/DESIGN-v0.2-dataflow.md 第 2 节：标的主序 + 时点索引。
package mktdata

// PriceScale 与 SCHEMA.md 0.2 一致：A 股价格以 1/1000 元为单位。
// 该值实际由 instruments.price_scale 逐标的给出，此处仅作默认。
const PriceScale = 1000

// AmountScale 金额以分为单位。
const AmountScale = 100

// RatioScale 比率（换手率等）固定 1e6。
const RatioScale = 1_000_000

// FactorScale 复权因子定点 scale，与 SCHEMA.md 4 一致。
const FactorScale = 1_000_000_000_000

// InstrumentID 引擎内部标的 ID，与 instruments.instrument_id 一致。
type InstrumentID int32

// TimePoint 是引擎的事件时点。
//
// TsClose 是时间游标的唯一依据 —— 它才是「该 bar 信息可得」的时刻。
// TradingDay 仅用于业务语义与展示。ts_open 不进内存（见设计 1.1）。
type TimePoint struct {
	TsClose    int64
	TradingDay int32
}

// Bar 是单根 K 线的值拷贝。价格与数量均为定点整数。
type Bar struct {
	TradingDay  int32
	Open        int64
	High        int64
	Low         int64
	Close       int64
	Volume      int64
	Amount      int64
	PreClose    int64
	TradeStatus int8
	IsST        int8
}

// Suspended 报告该 bar 是否为停牌行。
//
// 停牌行的 OHLC 全等于停牌前收盘价、量额为 0（SCHEMA.md 1.3），
// 因此价格序列连续、指标不会出现空洞，但**不得据此成交**。
func (b Bar) Suspended() bool { return b.TradeStatus == 0 }

// span 描述某标的在全局列数组中的连续区间。
type span struct {
	start int32 // 起始行（含）
	n     int32 // 行数
}

// Columns 是全部行情的列式内存表示。
//
// 所有字段非导出且无 getter 返回切片 —— 这是 C1 的结构性保证：
// 包外代码无法取得底层数组，也就无法绕过 History 的时点边界。
type Columns struct {
	// 与 SCHEMA.md 1 的列一一对应。ts_open 与 turn 不加载：
	// 前者引擎不需要（游标用 ts_close），后者 ETF 恒为 0 且策略罕用。
	tradingDay  []int32
	tsClose     []int64
	open        []int64
	high        []int64
	low         []int64
	close       []int64
	volume      []int64
	amount      []int64
	preClose    []int64
	tradeStatus []int8
	isST        []int8

	// 标的主序：每个标的的行在 [start, start+n) 内连续且按 ts_close 升序
	spans map[InstrumentID]span
	ids   []InstrumentID // 稳定顺序，便于确定性遍历

	// 时点索引：steps[i] 对应第 i 个事件时点，stepRows[i] 是该时点的行号列表
	steps    []TimePoint
	stepRows [][]int32
}

// Rows 返回加载的总行数。
func (c *Columns) Rows() int { return len(c.close) }

// NumInstruments 返回标的数。
func (c *Columns) NumInstruments() int { return len(c.spans) }

// NumSteps 返回事件时点数。
func (c *Columns) NumSteps() int { return len(c.steps) }

// Instruments 返回全部标的 ID 的拷贝，顺序稳定。
func (c *Columns) Instruments() []InstrumentID {
	out := make([]InstrumentID, len(c.ids))
	copy(out, c.ids)
	return out
}

// StepAt 返回第 i 个事件时点。
func (c *Columns) StepAt(i int) TimePoint { return c.steps[i] }

// barAt 按全局行号取出一根 bar。仅供包内使用。
func (c *Columns) barAt(row int32) Bar {
	return Bar{
		TradingDay:  c.tradingDay[row],
		Open:        c.open[row],
		High:        c.high[row],
		Low:         c.low[row],
		Close:       c.close[row],
		Volume:      c.volume[row],
		Amount:      c.amount[row],
		PreClose:    c.preClose[row],
		TradeStatus: c.tradeStatus[row],
		IsST:        c.isST[row],
	}
}

// MemoryBytes 估算已占用的内存字节数，用于基准与容量规划。
func (c *Columns) MemoryBytes() int64 {
	n := int64(len(c.close))
	var b int64
	b += n * 4                     // tradingDay
	b += n * 8 * 8                 // tsClose + 7 个 int64 价量列
	b += n * 2                     // tradeStatus + isST
	for _, rows := range c.stepRows {
		b += int64(len(rows)) * 4 // stepRows
	}
	b += int64(len(c.steps)) * 12 // steps
	b += int64(len(c.spans)) * 16 // spans（粗估，未计 map 开销）
	return b
}
