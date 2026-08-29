// Package engine 是 Step() 状态机与策略接口。
//
// 本刀（v0.2 第一刀）只到「按事件时点推进、每步把当前可见数据交给策略」为止。
// Market / Broker / Portfolio / Fee 不在范围内，因此 OnBar 尚不返回订单。
package engine

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
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

// Int 取整数参数，缺失时返回 def。
func (p Params) Int(name string, def int) int {
	if v, ok := p[name]; ok {
		return int(v)
	}
	return def
}

// Float 取浮点参数，缺失时返回 def。
func (p Params) Float(name string, def float64) float64 {
	if v, ok := p[name]; ok {
		return v
	}
	return def
}

// IndicatorFactory 为单个标的创建一个指标实例。
//
// 之所以是工厂而非单例：每个标的需要各自独立的指标状态
// （5000 只标的 × K 个指标），engine 按需为每个标的创建。
type IndicatorFactory func() indicator.Indicator

// InitContext 是策略初始化时可用的能力。
type InitContext interface {
	// Params 返回本次运行的参数取值
	Params() Params
	// Use 声明需要的指标。引擎负责为每个标的创建实例并逐步喂 bar，
	// 策略在 OnBar 中通过同一个 key 取回。
	//
	// **指标由引擎持有**（评审决议 2）：便于统一快照，也为远期
	// 「用户在 Web 端自由配置所需指标」留出位置。
	Use(key string, f IndicatorFactory)
	// Universe 返回本次运行涉及的全部标的
	Universe() []mktdata.InstrumentID
}

// StepContext 是策略在每个时点可见的一切。
//
// 这里没有任何方法能返回未来数据：History 只携带到当前 bar 为止的区间，
// Indicator 是增量式的（从未拿到过完整序列）。这是 C1 的落地形式。
type StepContext interface {
	// Time 当前事件时点
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
	// AdjClose 按指定方式复权后的收盘价。
	//
	// 刻意不接受 AdjQFQ —— 前复权锚定末日，用于决策即构成未来函数。
	// 它只存在于展示层的独立接口上（设计 6.1）。
	AdjClose(id mktdata.InstrumentID, back int, mode mktdata.AdjMode) (int64, bool)
}

// Strategy 是策略的统一接口。用 Go 实现并编译进引擎。
type Strategy interface {
	// Name 用于结果标识与日志
	Name() string
	// Specs 声明参数元数据
	Specs() []ParamSpec
	// Init 在回测开始前调用一次，用于声明指标
	Init(InitContext) error
	// OnBar 每个事件时点调用一次。
	//
	// 本刀尚不返回订单 —— Broker 不在范围内。下一刀会改为
	// `OnBar(StepContext) ([]Order, error)`。
	OnBar(StepContext) error
}

// ErrQFQInDecision 在策略试图在决策路径上使用前复权时返回。
var ErrQFQInDecision = fmt.Errorf(
	"前复权锚定末日，用于决策即构成未来函数（C1）且不可复现（C5）；" +
		"它只允许出现在展示路径上")
