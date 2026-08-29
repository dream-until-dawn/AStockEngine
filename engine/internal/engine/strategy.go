// Package engine 是 Step() 状态机与策略接口。
//
// 第二刀把 Market / Broker / Portfolio / Fee 接入，`OnBar` 由此开始返回订单。
package engine

import (
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// ParamKind 参数类型。v0.3 的 Web 表单与海选参数网格都由此生成。
type ParamKind int8

const (
	ParamInt ParamKind = iota
	ParamFloat
	ParamBool
)

// ParamSpec 是策略的参数自描述。
//
// 策略用 Go 编译进引擎，Web 端只能配参数不能写逻辑，因此参数元数据必须
// 由策略自己声明 —— 它同时喂三处：Web 自动生成表单、海选自动展开参数网格、
// 配置校验（ROADMAP v0.3）。
type ParamSpec struct {
	Name    string    `json:"name"`
	Kind    ParamKind `json:"kind"`
	Default float64   `json:"default"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
	Step    float64   `json:"step"`
	Desc    string    `json:"desc"`
}

// Params 是一次运行的实际参数取值。
type Params map[string]float64

func (p Params) Int(name string, def int) int {
	if v, ok := p[name]; ok {
		return int(v)
	}
	return def
}

func (p Params) Float(name string, def float64) float64 {
	if v, ok := p[name]; ok {
		return v
	}
	return def
}

// IndicatorFactory 为单个标的创建一个指标实例。
//
// 之所以是工厂而非单例：每个标的需要各自独立的指标状态。
type IndicatorFactory func() indicator.Indicator

// InitContext 是策略初始化时可用的能力。
type InitContext interface {
	Params() Params
	// Use 声明需要的指标。**指标由引擎持有**（评审决议 2）：
	// 便于统一快照，也为远期「用户在 Web 端自由配置所需指标」留出位置。
	Use(key string, f IndicatorFactory)
	Universe() []mktdata.InstrumentID
	// Instrument 取标的静态属性，供策略在初始化时按板块/类型分流
	Instrument(id mktdata.InstrumentID) *mktdata.Instrument
}

// StepContext 是策略在每个时点可见的一切。
//
// 这里没有任何方法能返回未来数据：History 只携带到当前 bar 为止的区间，
// Indicator 是增量式的（从未拿到过完整序列）。这是 C1 的落地形式。
type StepContext interface {
	Time() mktdata.TimePoint
	// Universe 本时点**有 bar 的**标的。上市前无行、退市后无行，
	// 故它天然是 point-in-time 的（C3）
	Universe() []mktdata.InstrumentID
	// History 单标的历史视图，**原始价**
	History(id mktdata.InstrumentID) mktdata.History
	// Bar 当前 bar，**原始价** —— 撮合、涨跌停判定用它
	Bar(id mktdata.InstrumentID) (mktdata.Bar, bool)
	// Indicator 取回 Init 中声明的指标。指标喂的是**后复权**价，
	// 因为序列必须连续，否则除权日会产生假信号（设计 6.1）
	Indicator(id mktdata.InstrumentID, key string) (indicator.Indicator, bool)
	// Instrument 标的静态属性
	Instrument(id mktdata.InstrumentID) *mktdata.Instrument

	// Portfolio 当前账本（只读使用；直接改动会绕过撮合，破坏账目自洽）
	Portfolio() *trading.Portfolio
	// Available 该标的当前可卖数量（已考虑 T+1）
	Available(id mktdata.InstrumentID) int64
	// Pending 尚未成交的订单。策略据此避免重复下单
	Pending() []trading.PendingOrder
	// Fills 本时点的成交
	Fills() []trading.Fill
	// Rejections 本时点的拒单。**每条都带结构化原因与数值 detail**
	Rejections() []trading.Rejection
	// EquityCents 当前总权益（分），按本时点收盘价估值
	EquityCents() int64

	// AdjClose 按指定方式复权后的收盘价。
	//
	// **拒绝 AdjQFQ**：前复权锚定末日，用于决策即构成未来函数（C1）
	// 且不可复现（C5）。它只允许出现在展示路径上。
	AdjClose(id mktdata.InstrumentID, back int, mode mktdata.AdjMode) (int64, bool)
}

// StatefulStrategy 供**确有跨步状态**的策略实现。
//
// 引擎的快照涵盖游标、指标与账本，但**涵盖不了策略自己的字段** ——
// 这一点是端到端回测才暴露的：恢复后的策略是全新实例，
// 不知道自己持有什么，于是重复下单，账本随之偏离。
//
// 但在实现本接口之前，先确认状态是否**真的**需要自己存 ——
// 多数看似需要的状态其实可以从 StepContext 导出：
//
//	「持有哪些标的」 → ctx.Portfolio().Position(id).Total > 0
//	「哪些单在途」   → ctx.Pending()
//	「可卖多少」     → ctx.Available(id)
//
// 从 ctx 导出的状态天然随引擎快照一起恢复，无需额外处理。
// 只有真正的跨步记忆（如「上一步指标是否在均线之上」）才需要本接口。
type StatefulStrategy interface {
	Strategy
	SnapshotState() ([]byte, error)
	RestoreState([]byte) error
}

// Strategy 是策略的统一接口。用 Go 实现并编译进引擎。
type Strategy interface {
	Name() string
	Specs() []ParamSpec
	Init(InitContext) error
	// OnBar 每个事件时点调用一次，返回本步要下的订单。
	//
	// 订单不会立即成交：由 Market 决定最早可执行时点 ——
	// 主板 T+1、创业板/科创板可 T 日盘后、加密零间隔（设计第 1 节）。
	OnBar(StepContext) ([]trading.Order, error)
}
