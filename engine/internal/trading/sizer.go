package trading

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// SignalKind 信号类型。
type SignalKind int8

const (
	// SignalEnter 建仓 / 加仓，**数量由 Sizer 定**
	SignalEnter SignalKind = iota
	// SignalExit 清仓。数量是确定的（当前可卖全部），不交给 Sizer 算 ——
	// 这也是它单独成一类而不是「Side=Sell 的 Enter」的原因。
	SignalExit
	// SignalTarget 调仓到目标权重
	SignalTarget
)

func (k SignalKind) String() string {
	switch k {
	case SignalEnter:
		return "enter"
	case SignalExit:
		return "exit"
	case SignalTarget:
		return "target"
	}
	return "unknown"
}

// Signal 是策略的输出：**交易意图，不含数量**。
//
// 策略不再自己算 Qty（v0.2 是这样做的）。只要 Qty 还由策略给，Sizer 就没有
// 可以决定的东西，而海选真正想扫的是两个正交维度：信号逻辑 × 仓位方法。
// 写在一起，这两个维度就绑死了。
type Signal struct {
	Instrument mktdata.InstrumentID
	Kind       SignalKind
	Side       Side
	// Strength 策略对该信号的信心，取值 [0,1]。等权 Sizer 忽略它。
	Strength float64
	// Weight Kind==SignalTarget 时的目标权重
	Weight float64
	// LimitPrice 0 表示市价
	LimitPrice int64
	// Tag 归因用，会一路带到 Fill
	Tag string

	// Qty 非零表示策略坚持自己定量，绕过 Sizer。
	//
	// 这条口子存在，但每次使用都会被记录。用得多了本身就是设计信号：
	// 说明 Sizer 的抽象没选对，该改抽象而不是继续绕。
	Qty int64
}

// SizeContext 是 Sizer 可见的账户与行情状态。
type SizeContext interface {
	Time() mktdata.TimePoint
	Portfolio() *Portfolio
	EquityCents() int64
	// InitialCashCents 初始资金。定额下注型 Sizer 需要一个不随盈亏漂移的基准
	InitialCashCents() int64
	Available(id mktdata.InstrumentID) int64
	Bar(id mktdata.InstrumentID) (mktdata.Bar, bool)
	Instrument(id mktdata.InstrumentID) *mktdata.Instrument
	Pending() []PendingOrder
	Market() Market
}

// Sizer 把信号转成订单。
type Sizer interface {
	Name() string
	// Size 接收**整批**信号而非逐条。
	//
	// 等权分配、按信心归一化、总仓位上限都需要看到本步的全部信号，
	// 逐条调用做不了这些。
	Size(sigs []Signal, ctx SizeContext) []Order
}

// Sizers 是仓位模块的注册表。
var Sizers = registry.New[Sizer]("sizer")

// ---- 公共工具 ----

// occupancy 统计「已占用的仓位数」与「哪些标的已在场」。
//
// 已持有与在途都算占用：同一只标的既不该重复买入，也不该在挂单未成交时
// 再占一个仓位。**去重**——一只标的同时持有并挂着卖单只算一个占用。
func occupancy(ctx SizeContext) map[mktdata.InstrumentID]bool {
	in := make(map[mktdata.InstrumentID]bool, 64)
	for id, p := range ctx.Portfolio().Positions {
		if p.Total > 0 {
			in[id] = true
		}
	}
	for _, po := range ctx.Pending() {
		in[po.Instrument] = true
	}
	return in
}

// buyQty 按预算折算数量并交给 Market 规整。
//
// 规整必须走 Market：100 股整数倍、零股卖出这些规则属于市场而非仓位方法，
// 不该在每个 Sizer 里各写一遍（远期加密货币的最小单位完全不同）。
func buyQty(ctx SizeContext, id mktdata.InstrumentID, budgetCents int64) (int64, bool) {
	bar, ok := ctx.Bar(id)
	if !ok || bar.Suspended() || bar.Close <= 0 {
		return 0, false
	}
	inst := ctx.Instrument(id)
	if inst == nil {
		return 0, false
	}
	// 预算是分、价格是厘：分 × 10 / 厘 = 股
	raw := budgetCents * 10 / bar.Close
	if raw <= 0 {
		return 0, false
	}
	held := int64(0)
	if p := ctx.Portfolio().Position(id); p != nil {
		held = p.Total
	}
	return ctx.Market().NormalizeQty(inst, raw, SideBuy, held)
}

// exitOrder 把清仓信号变成卖单。
func exitOrder(sig Signal, ctx SizeContext) (Order, bool) {
	avail := ctx.Available(sig.Instrument)
	if avail <= 0 {
		return Order{}, false
	}
	inst := ctx.Instrument(sig.Instrument)
	if inst == nil {
		return Order{}, false
	}
	qty, ok := ctx.Market().NormalizeQty(inst, avail, SideSell, avail)
	if !ok || qty <= 0 {
		return Order{}, false
	}
	return Order{
		Instrument: sig.Instrument, Side: SideSell, Qty: qty,
		Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
	}, true
}

func orderType(sig Signal) OrderType {
	if sig.LimitPrice > 0 {
		return OrderLimit
	}
	return OrderMarket
}

// override 处理策略坚持自己定量的情形。
func override(sig Signal, ctx SizeContext) (Order, bool) {
	if sig.Qty <= 0 {
		return Order{}, false
	}
	inst := ctx.Instrument(sig.Instrument)
	if inst == nil {
		return Order{}, false
	}
	held := int64(0)
	if p := ctx.Portfolio().Position(sig.Instrument); p != nil {
		held = p.Total
	}
	qty, ok := ctx.Market().NormalizeQty(inst, sig.Qty, sig.Side, held)
	if !ok || qty <= 0 {
		return Order{}, false
	}
	return Order{
		Instrument: sig.Instrument, Side: sig.Side, Qty: qty,
		Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
	}, true
}

// dispatch 处理所有 Sizer 共有的前置分支：策略定量、清仓。
// 返回 handled=true 表示本信号已处理完，无需再算仓位。
func dispatch(sig Signal, ctx SizeContext) (Order, bool, bool) {
	if sig.Qty > 0 {
		o, ok := override(sig, ctx)
		return o, ok, true
	}
	if sig.Kind == SignalExit || sig.Side == SideSell {
		o, ok := exitOrder(sig, ctx)
		return o, ok, true
	}
	return Order{}, false, false
}

// ---- equal_weight ----

var equalWeightSpecs = []spec.ParamSpec{
	{Name: "slots", Kind: spec.ParamInt, Default: 10, Min: 1, Max: 500, Step: 1,
		Desc: "把资金等分成多少份，同时也是最多持有的标的数"},
	{Name: "base", Kind: spec.ParamString, DefaultStr: "initial",
		Options: []string{"initial", "equity"},
		Desc:    "每份的计算基准：initial=初始资金（定额下注），equity=当前权益（复利）"},
}

// EqualWeight 等权：把基准资金切成 slots 份，每个建仓信号占一份。
//
// base 是**必须显式想清楚**的一件事：
//
//	initial —— 每份大小恒定，赚了不加码、亏了不减码。v0.2 三个样例策略是这个行为
//	equity  —— 每份随权益浮动，天然复利，也天然在回撤中缩仓
//
// 两者都是真实的仓位政策，没有哪个「更正确」，所以做成显式参数而非默认行为。
type EqualWeight struct {
	slots int
	base  string
}

func (s *EqualWeight) Name() string { return "equal_weight" }

func (s *EqualWeight) Size(sigs []Signal, ctx SizeContext) []Order {
	budget := ctx.InitialCashCents()
	if s.base == "equity" {
		budget = ctx.EquityCents()
	}
	// **预算不足只挡建仓，不挡清仓。** 早期版本在这里直接 return nil，
	// 结果权益归零的账户连卖都卖不出去 —— 由 TestSizerExitUsesAvailable 发现。
	// 卖出从来不需要预算。
	var slotCents int64
	if s.slots > 0 && budget > 0 {
		slotCents = budget / int64(s.slots)
	}

	occupied := occupancy(ctx)
	used := len(occupied)
	out := make([]Order, 0, len(sigs))

	for _, sig := range sigs {
		if o, ok, handled := dispatch(sig, ctx); handled {
			if ok {
				out = append(out, o)
			}
			continue
		}
		// 建仓：预算不足、已在场、仓位已满，三者任一即跳过
		if slotCents <= 0 || occupied[sig.Instrument] || used >= s.slots {
			continue
		}
		qty, ok := buyQty(ctx, sig.Instrument, slotCents)
		if !ok {
			continue
		}
		out = append(out, Order{
			Instrument: sig.Instrument, Side: SideBuy, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
		occupied[sig.Instrument] = true
		used++
	}
	return out
}

// ---- fixed_cash ----

var fixedCashSpecs = []spec.ParamSpec{
	{Name: "cents", Kind: spec.ParamFloat, Default: 10_000_000, Min: 1, Max: 1e15, Step: 1,
		Desc: "每笔投入的金额（分）"},
	{Name: "max_positions", Kind: spec.ParamInt, Default: 0, Min: 0, Max: 5000, Step: 1,
		Desc: "最多同时持有多少只，0 表示不限"},
}

// FixedCash 每笔固定金额。
type FixedCash struct {
	cents        int64
	maxPositions int
}

func (s *FixedCash) Name() string { return "fixed_cash" }

func (s *FixedCash) Size(sigs []Signal, ctx SizeContext) []Order {
	occupied := occupancy(ctx)
	used := len(occupied)
	out := make([]Order, 0, len(sigs))
	for _, sig := range sigs {
		if o, ok, handled := dispatch(sig, ctx); handled {
			if ok {
				out = append(out, o)
			}
			continue
		}
		if occupied[sig.Instrument] {
			continue
		}
		if s.maxPositions > 0 && used >= s.maxPositions {
			continue
		}
		qty, ok := buyQty(ctx, sig.Instrument, s.cents)
		if !ok {
			continue
		}
		out = append(out, Order{
			Instrument: sig.Instrument, Side: SideBuy, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
		occupied[sig.Instrument] = true
		used++
	}
	return out
}

// ---- fixed_qty ----

var fixedQtySpecs = []spec.ParamSpec{
	{Name: "qty", Kind: spec.ParamInt, Default: 100, Min: 1, Max: 1e9, Step: 1,
		Desc: "每笔固定股数 / 份数（仍会过最小申报单位规整）"},
	{Name: "max_positions", Kind: spec.ParamInt, Default: 0, Min: 0, Max: 5000, Step: 1,
		Desc: "最多同时持有多少只，0 表示不限"},
}

// FixedQty 每笔固定数量。
type FixedQty struct {
	qty          int64
	maxPositions int
}

func (s *FixedQty) Name() string { return "fixed_qty" }

func (s *FixedQty) Size(sigs []Signal, ctx SizeContext) []Order {
	occupied := occupancy(ctx)
	used := len(occupied)
	out := make([]Order, 0, len(sigs))
	for _, sig := range sigs {
		if o, ok, handled := dispatch(sig, ctx); handled {
			if ok {
				out = append(out, o)
			}
			continue
		}
		if occupied[sig.Instrument] {
			continue
		}
		if s.maxPositions > 0 && used >= s.maxPositions {
			continue
		}
		inst := ctx.Instrument(sig.Instrument)
		bar, hasBar := ctx.Bar(sig.Instrument)
		if inst == nil || !hasBar || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		qty, ok := ctx.Market().NormalizeQty(inst, s.qty, SideBuy, 0)
		if !ok || qty <= 0 {
			continue
		}
		out = append(out, Order{
			Instrument: sig.Instrument, Side: SideBuy, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
		occupied[sig.Instrument] = true
		used++
	}
	return out
}

// ---- pct_equity ----

var pctEquitySpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 10, Min: 0.01, Max: 100, Step: 0.01,
		Desc: "每笔占当前权益的百分比"},
	{Name: "max_positions", Kind: spec.ParamInt, Default: 0, Min: 0, Max: 5000, Step: 1,
		Desc: "最多同时持有多少只，0 表示不限"},
}

// PctEquity 每笔占当前权益的固定比例。与 equal_weight base=equity 的区别在于
// 它不受「份数」约束 —— 可以叠到超过 100%（会被现金不足自然挡住）。
type PctEquity struct {
	ppm          int64 // pct 转成百万分之一，避免浮点参与金额计算
	maxPositions int
}

func (s *PctEquity) Name() string { return "pct_equity" }

func (s *PctEquity) Size(sigs []Signal, ctx SizeContext) []Order {
	// 同 EqualWeight：权益为零也必须能卖
	budget := int64(0)
	if eq := ctx.EquityCents(); eq > 0 {
		budget = eq * s.ppm / 1_000_000
	}
	occupied := occupancy(ctx)
	used := len(occupied)
	out := make([]Order, 0, len(sigs))
	for _, sig := range sigs {
		if o, ok, handled := dispatch(sig, ctx); handled {
			if ok {
				out = append(out, o)
			}
			continue
		}
		if budget <= 0 || occupied[sig.Instrument] {
			continue
		}
		if s.maxPositions > 0 && used >= s.maxPositions {
			continue
		}
		qty, ok := buyQty(ctx, sig.Instrument, budget)
		if !ok {
			continue
		}
		out = append(out, Order{
			Instrument: sig.Instrument, Side: SideBuy, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
		occupied[sig.Instrument] = true
		used++
	}
	return out
}

// ---- strength_weighted ----

var strengthWeightedSpecs = []spec.ParamSpec{
	{Name: "total_pct", Kind: spec.ParamFloat, Default: 100, Min: 0.01, Max: 100, Step: 0.01,
		Desc: "本步全部建仓信号合计投入权益的百分比"},
	{Name: "min_strength", Kind: spec.ParamFloat, Default: 0, Min: 0, Max: 1, Step: 0.01,
		Desc: "低于此信心的信号直接丢弃"},
}

// StrengthWeighted 按信心分配：本步的建仓信号按 Strength 归一化瓜分总预算。
//
// 这是唯一真正用到「整批信号」的 Sizer —— 归一化的分母是本步所有信号的
// Strength 之和，逐条调用根本算不出来。
type StrengthWeighted struct {
	totalPPM    int64
	minStrength float64
}

func (s *StrengthWeighted) Name() string { return "strength_weighted" }

func (s *StrengthWeighted) Size(sigs []Signal, ctx SizeContext) []Order {
	eq := ctx.EquityCents()
	occupied := occupancy(ctx)
	out := make([]Order, 0, len(sigs))

	// 先把非建仓的处理掉，同时统计建仓信号的信心之和
	var sum float64
	enters := make([]Signal, 0, len(sigs))
	for _, sig := range sigs {
		if o, ok, handled := dispatch(sig, ctx); handled {
			if ok {
				out = append(out, o)
			}
			continue
		}
		if occupied[sig.Instrument] || sig.Strength < s.minStrength {
			continue
		}
		w := sig.Strength
		if w <= 0 {
			w = 1 // 策略没给信心就按等权处理，而不是当作 0 全部丢弃
		}
		sum += w
		enters = append(enters, Signal{
			Instrument: sig.Instrument, Kind: sig.Kind, Side: sig.Side,
			Strength: w, LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
	}
	if sum <= 0 || eq <= 0 {
		return out
	}

	total := eq * s.totalPPM / 1_000_000
	for _, sig := range enters {
		budget := int64(float64(total) * (sig.Strength / sum))
		qty, ok := buyQty(ctx, sig.Instrument, budget)
		if !ok {
			continue
		}
		out = append(out, Order{
			Instrument: sig.Instrument, Side: SideBuy, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
	}
	return out
}

// ---- 注册 ----

func init() {
	Sizers.Register("equal_weight", equalWeightSpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(equalWeightSpecs, raw)
			if err != nil {
				return nil, err
			}
			base, err := registry.DecodeString(equalWeightSpecs, raw, "base")
			if err != nil {
				return nil, err
			}
			return &EqualWeight{slots: p.Int("slots", 10), base: base}, nil
		})

	Sizers.Register("fixed_cash", fixedCashSpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(fixedCashSpecs, raw)
			if err != nil {
				return nil, err
			}
			c := int64(p.Float("cents", 10_000_000))
			if c <= 0 {
				return nil, fmt.Errorf("cents 必须为正")
			}
			return &FixedCash{cents: c, maxPositions: p.Int("max_positions", 0)}, nil
		})

	Sizers.Register("fixed_qty", fixedQtySpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(fixedQtySpecs, raw)
			if err != nil {
				return nil, err
			}
			return &FixedQty{
				qty: int64(p.Int("qty", 100)), maxPositions: p.Int("max_positions", 0),
			}, nil
		})

	Sizers.Register("pct_equity", pctEquitySpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(pctEquitySpecs, raw)
			if err != nil {
				return nil, err
			}
			return &PctEquity{
				ppm:          int64(p.Float("pct", 10) * 10_000),
				maxPositions: p.Int("max_positions", 0),
			}, nil
		})

	Sizers.Register("strength_weighted", strengthWeightedSpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(strengthWeightedSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &StrengthWeighted{
				totalPPM:    int64(p.Float("total_pct", 100) * 10_000),
				minStrength: p.Float("min_strength", 0),
			}, nil
		})
}
