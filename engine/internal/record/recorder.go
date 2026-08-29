// Package record 是回测过程的记录层。
//
// 三档级别对应三种用途：
//
//	None     海选内层。几百组配置并发跑，每组只关心最终权益，
//	         逐步留存会把内存吃光
//	Summary  常规回测。净值序列 + 全部成交 —— 绩效指标要的就这些
//	Full     单步调试（v0.4）。每步的信号 / 定量 / 成交 / 拒单全留，
//	         回答「为什么没买」这类问题
//
// **Summary 必须留全部成交**：胜率、盈亏比、换手率都从成交流算出来，
// 只留每步汇总数就算不了。成交笔数比时点数少一个量级（实测 1,787 vs 1,614），
// 留下来不构成负担。
package record

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Level 记录级别。
type Level int8

const (
	None Level = iota
	Summary
	Full
)

func (l Level) String() string {
	switch l {
	case None:
		return "none"
	case Summary:
		return "summary"
	case Full:
		return "full"
	}
	return "unknown"
}

// ParseLevel 解析级别名。
func ParseLevel(s string) (Level, error) {
	switch s {
	case "none":
		return None, nil
	case "summary", "":
		return Summary, nil
	case "full":
		return Full, nil
	}
	return 0, fmt.Errorf("未知的记录级别 %q，可选：none / summary / full", s)
}

// Step 是一个时点的记录。
type Step struct {
	Time        mktdata.TimePoint `json:"time"`
	EquityCents int64             `json:"equity_cents"`
	CashCents   int64             `json:"cash_cents"`
	Positions   int               `json:"positions"`
	NumSignals  int               `json:"num_signals"`
	NumFills    int               `json:"num_fills"`
	NumRejects  int               `json:"num_rejects"`

	// 以下仅 Full 级填充
	Signals    []trading.Signal    `json:"signals,omitempty"`
	Sized      []trading.Order     `json:"sized,omitempty"`
	Fills      []trading.Fill      `json:"fills,omitempty"`
	Rejections []trading.Rejection `json:"rejections,omitempty"`
}

// Recorder 接收每步的记录。
type Recorder interface {
	Level() Level
	OnStep(Step)
	// Steps 返回逐步记录。None 级返回 nil。
	Steps() []Step
	// Fills 返回全部成交，按时间顺序。None 级返回 nil。
	Fills() []trading.Fill
	// Rejections 返回全部拒单。仅 Full 级有内容。
	Rejections() []trading.Rejection
}

// Memory 是内存记录器。
//
// v0.3 只做内存版：流式落盘的格式该由 v0.4 的 API 消费方式决定，
// 现在定会白定。超过阈值时报警而不是静默吃内存 ——
// 全市场 7,175 只 × 5,260 步的 Full 级可能上 GB。
type Memory struct {
	level Level
	steps []Step
	fills []trading.Fill
	rejs  []trading.Rejection

	// finalEquity 即使在 None 级也要保留 —— 那是海选唯一关心的数
	finalEquity int64
	numSteps    int

	// warnAt 超过多少条记录就报警一次，0 表示不报警
	warnAt int
	warned bool
	// Warnings 记录溢出告警，由调用方决定怎么呈现
	Warnings []string
}

// NewMemory 创建内存记录器。warnAt 为 0 时用默认阈值 200 万条。
func NewMemory(level Level, warnAt int) *Memory {
	if warnAt <= 0 {
		warnAt = 2_000_000
	}
	m := &Memory{level: level, warnAt: warnAt}
	if level != None {
		m.steps = make([]Step, 0, 4096)
		m.fills = make([]trading.Fill, 0, 4096)
	}
	return m
}

func (m *Memory) Level() Level { return m.level }

func (m *Memory) OnStep(s Step) {
	m.numSteps++
	m.finalEquity = s.EquityCents
	if m.level == None {
		return
	}

	// 成交在 Summary 级也要留 —— 绩效指标的来源
	m.fills = append(m.fills, s.Fills...)

	if m.level == Summary {
		// 明细清空，只留计数。**必须清空而不是留着不看** ——
		// 引擎给的切片会在下一步被复用，留着等于留下一堆会变的引用
		s.Signals, s.Sized, s.Fills, s.Rejections = nil, nil, nil, nil
	} else {
		s.Signals = append([]trading.Signal(nil), s.Signals...)
		s.Sized = append([]trading.Order(nil), s.Sized...)
		s.Fills = append([]trading.Fill(nil), s.Fills...)
		s.Rejections = append([]trading.Rejection(nil), s.Rejections...)
		m.rejs = append(m.rejs, s.Rejections...)
	}
	m.steps = append(m.steps, s)
	m.checkSize()
}

func (m *Memory) checkSize() {
	if m.warned || m.warnAt <= 0 {
		return
	}
	n := len(m.steps) + len(m.fills) + len(m.rejs)
	if n < m.warnAt {
		return
	}
	m.warned = true
	m.Warnings = append(m.Warnings, fmt.Sprintf(
		"记录条数已达 %d（%s 级）。内存记录器没有上限，"+
			"再跑下去可能吃满内存 —— 考虑降到 summary 级或收窄标的池 / 区间",
		n, m.level))
}

func (m *Memory) Steps() []Step                   { return m.steps }
func (m *Memory) Fills() []trading.Fill           { return m.fills }
func (m *Memory) Rejections() []trading.Rejection { return m.rejs }

// FinalEquityCents 最终权益。None 级也有。
func (m *Memory) FinalEquityCents() int64 { return m.finalEquity }

// NumSteps 步数。None 级也有。
func (m *Memory) NumSteps() int { return m.numSteps }

// Curve 抽出净值序列，供绩效计算使用。
func (m *Memory) Curve() ([]int32, []int64) {
	days := make([]int32, len(m.steps))
	eq := make([]int64, len(m.steps))
	for i, s := range m.steps {
		days[i], eq[i] = s.Time.TradingDay, s.EquityCents
	}
	return days, eq
}
