// Package engine 是 Step() 状态机与策略接口。
//
// v0.3 起策略**只出信号不出数量**：`OnBar` 返回 `[]trading.Signal`，
// 由 Sizer 折算成订单、Risk 逐条把关。理由见 DESIGN-v0.3-assembly.md 4.1 ——
// 只要 Qty 还由策略给，Sizer 就没有可以决定的东西，
// 海选也就无法把「信号逻辑」与「仓位方法」当两个独立维度来扫。
package engine

import (
	"encoding/json"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 参数元数据在 v0.3 下沉到了 internal/spec —— Fee / Slippage / Sizer / Risk
// 都要自描述，留在 engine 包会让 trading 反向依赖 engine。
// 这些别名让既有代码与文档里的 engine.ParamSpec 继续可用。
type (
	ParamKind = spec.ParamKind
	ParamSpec = spec.ParamSpec
	Params    = spec.Params
)

const (
	ParamInt    = spec.ParamInt
	ParamFloat  = spec.ParamFloat
	ParamBool   = spec.ParamBool
	ParamString = spec.ParamString
)

// 信号类型同样住在 trading（它是交易概念，且 Sizer 在那儿），这里给别名。
type (
	Signal     = trading.Signal
	SignalKind = trading.SignalKind
)

const (
	SignalEnter  = trading.SignalEnter
	SignalExit   = trading.SignalExit
	SignalTarget = trading.SignalTarget
)

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

	// Ledger 当前账本。**只读** —— 字段已全部私有，
	// 想改账要经撮合，绕过去就没有账目自洽可言了
	Ledger() trading.Ledger
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

	// AdjBar 按指定方式复权后的整根 bar。
	//
	// 规则树的条件里可以写 `close > ma20`，两侧**必须同基准** ——
	// 拿原始价去和均线比，除权日会凭空产生一次穿越，而那不是行情是分红。
	// 只给 AdjClose 不够：条件也能引用 open / high / low。
	AdjBar(id mktdata.InstrumentID, mode mktdata.AdjMode) (mktdata.Bar, bool)
	// AdjClose 按指定方式复权后的收盘价。
	//
	// **拒绝 AdjQFQ**：前复权锚定末日，用于决策即构成未来函数（C1）
	// 且不可复现（C5）。它只允许出现在展示路径上。
	AdjClose(id mktdata.InstrumentID, back int, mode mktdata.AdjMode) (int64, bool)
}

// ConfigurableStrategy 供**配置是结构而非标量**的策略实现。
//
// 普通策略的参数是 `map[string]float64`（由 ParamSpec 描述），
// 规则树的配置是三棵树加一张指标表 —— 那不是标量，塞不进 Params。
// 实现本接口的策略由装配层直接把整段 JSON 交过来自行解析，
// **并且跳过标量参数的校验**（它没有 ParamSpec 可校验）。
type ConfigurableStrategy interface {
	Configure(raw json.RawMessage) error
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
//	「持有哪些标的」 → ctx.Ledger().EachExposure(...)
//	「哪些单在途」   → ctx.Pending()
//	「可卖多少」     → ctx.Available(id)
//
// 从 ctx 导出的状态天然随引擎快照一起恢复，无需额外处理。
// 只有真正的跨步记忆（如「上一步指标是否在均线之上」）才需要本接口。
// ShortSeller 由**会发出开空信号**的策略实现。
//
// 装配时用它挡下「在不允许做空的市场上配了一棵做空树」。
// 不挡的话后果是**静默失效**：开空信号会被 Sizer 当成减仓，
// 而手上没有多头可减，订单被丢掉 —— 一笔成交都不会有，
// 报告上却看不出任何异常，只是策略「什么都没做」。
type ShortSeller interface {
	NeedsShort() bool
}

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
	// OnBar 每个事件时点调用一次，返回本步的交易意图。
	//
	// **返回信号而非订单**：数量由 Sizer 决定，风控由 Risk 链把关。
	// 策略只回答「买什么、卖什么、多大信心」。
	//
	// 信号不会立即成交：Sizer 折算后由 Market 决定最早可执行时点 ——
	// 主板 T+1、创业板/科创板可 T 日盘后、加密零间隔（设计第 1 节）。
	OnBar(StepContext) ([]Signal, error)
}

// Strategies 是策略的注册表。
//
// 工厂**忽略参数 JSON**：策略参数经 Config.Params 走 InitContext.Params()，
// 与 v0.2 的取参方式保持一致，不必让每个策略各写一遍解析。
// 配置层负责把 strategy.params 解成 Params 并按 Specs() 校验。
var Strategies = registry.New[Strategy]("strategy")

// RegisterStrategy 是给策略实现用的便捷注册函数：
// 传一个零参构造器，参数规格从实例上取。
//
// desc 是一句话中文说明，**必填** —— 没有它，用户在下拉框里
// 只看得到一个英文标识符。
func RegisterStrategy(name, desc string, make func() Strategy) {
	Strategies.Register(name, desc, make().Specs(),
		func(json.RawMessage) (Strategy, error) { return make(), nil })
}
