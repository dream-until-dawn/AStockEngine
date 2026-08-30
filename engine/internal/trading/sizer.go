package trading

import (
	"encoding/json"
	"fmt"
	"sort"

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
	Ledger() Ledger
	EquityCents() int64
	// InitialCashCents 初始资金。定额下注型 Sizer 需要一个不随盈亏漂移的基准
	InitialCashCents() int64
	Available(id mktdata.InstrumentID) int64
	Bar(id mktdata.InstrumentID) (mktdata.Bar, bool)
	Instrument(id mktdata.InstrumentID) *mktdata.Instrument
	Pending() []PendingOrder
	Market() Market
	// FrictionCents 这笔成交要付出的摩擦（费用 + 滑点）。
	//
	// **定量时必须先把它留出来**：不留的话算出的数量刚好花光可用资金，
	// 撮合时 `金额 + 费用 + 滑点 > 购买力`，整单被拒 ——
	// 表现为「按 100% 权益下注反而一笔都开不出来」，而且报的是
	// 「现金不足」，看不出差的其实只是手续费那一点。
	FrictionCents(inst *mktdata.Instrument, side Side, qty, amountCents int64) int64
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

// slotKey 一个仓位槽：标的 + 方向。
//
// **单向市场里 short 恒为 false**，键退化成标的本身，行为与从前一致。
type slotKey struct {
	id    mktdata.InstrumentID
	short bool
}

// occupied 记录已占用的仓位槽。
//
// 已持有与在途都算占用：同一只标的既不该重复建仓，也不该在挂单未成交时
// 再占一个槽位。**去重**——一只标的同时持有并挂着卖单只算一个占用。
type occupied struct {
	// hedge 双向市场。为真时多空各占一个槽 ——
	// 「持有 BTC 多头」不该挡住「开 BTC 空头」，那正是双向的意义；
	// 为假时全部归到多头槽，与单向市场的语义一致
	hedge bool
	slots map[slotKey]bool
}

func occupancy(ctx SizeContext) *occupied {
	o := &occupied{
		hedge: ctx.Market().AllowsShort(),
		slots: make(map[slotKey]bool, 64),
	}
	ctx.Ledger().EachExposure(func(id mktdata.InstrumentID, e Exposure) bool {
		if e.Long > 0 || !o.hedge {
			o.take(id, SideBuy)
		}
		if e.Short > 0 {
			o.take(id, SideSell)
		}
		return true
	})
	for _, po := range ctx.Pending() {
		// 在途的**平仓**单不占新槽：它释放的正是已经算过的那个槽
		if po.Reduce {
			continue
		}
		o.take(po.Instrument, po.Side)
	}
	return o
}

func (o *occupied) key(id mktdata.InstrumentID, side Side) slotKey {
	return slotKey{id: id, short: o.hedge && side == SideSell}
}

func (o *occupied) has(id mktdata.InstrumentID, side Side) bool {
	return o.slots[o.key(id, side)]
}

func (o *occupied) take(id mktdata.InstrumentID, side Side) {
	o.slots[o.key(id, side)] = true
}

func (o *occupied) len() int { return len(o.slots) }

// entrySide 建仓单的买卖方向。
//
// **单向市场里建仓只能是买**：A 股的策略即便发出 Enter + 卖，
// 也只能理解成减仓（由 Exit 那条路处理），绝不能变成一张卖出开仓单。
// 双向市场里 Enter + 卖就是开空 —— 这个区别由 Market 解释，
// 与 MarginLedger.posFor、MatchRoundTrips 用的是同一张表。
func entrySide(sig Signal, ctx SizeContext) Side {
	if LegOf(sig.Side, false, ctx.Market().AllowsShort()).Short {
		return SideSell
	}
	return SideBuy
}

// sizeBase 按基准口径取出可分配的资金总量。
//
//	cost     已投入本金（现金 + 持仓成本）= 权益 − 未实现盈亏
//	notional 本金 × 杠杆 —— **要让杠杆真的放大敞口，只有这一个选项**。
//	         其余三个都是本金口径，杠杆只会让保证金占用变少、
//	         闲置现金变多，敞口一分不变
//	equity   当前权益（浮盈立刻用于加仓）
//	initial  初始资金（定额下注，不复利）
//
// **默认是 cost**：10000 切 10 份、每份 1000，先买 5 只；这 5 只涨到
// 11000 之后再买 5 只，仍然是每份 1000 而不是 1100 ——
// 浮盈还没落袋，拿它加仓等于用没到手的钱下注。
// 已实现的盈亏照常滚进去（它已经在现金里了）。
func sizeBase(ctx SizeContext, base string) int64 {
	switch base {
	case "notional":
		return ctx.Ledger().NotionalCapacityCents()
	case "equity":
		return ctx.EquityCents()
	case "initial":
		return ctx.InitialCashCents()
	default:
		return ctx.Ledger().CostBasisCents()
	}
}

// rankEntries 把建仓候选按指定口径排序。
//
// # 为什么需要它
//
// 同一天有 20 只标的发出买入信号，而只剩 3 个空位时，
// 从前拿到的是**标的 ID 最小的 3 只** —— 那是数据的顺序，不是任何策略。
//
//	amount  当日成交额大的优先（默认）
//	volume  当日成交量大的优先
//	signal  策略给出的顺序，不重排
//
// **默认按成交额而不是成交量**：成交量是「多少股 / 多少张」，
// 跨标的不可比 —— 100 股 300 元的股票与 100 股 3 元的股票是同一个成交量，
// 而流动性差 100 倍。成交额已经把价格乘进去了，是同一量纲。
// 想按成交量排就把 order_by 设成 volume。
//
// 排序**必须稳定**，且同值时按 ID 兜底 —— 否则同一份配置两次跑出的
// 顺序不同，C5 就在这里失守。
func rankEntries(sigs []Signal, ctx SizeContext, orderBy string) []Signal {
	if orderBy == "signal" || len(sigs) < 2 {
		return sigs
	}
	key := func(sig Signal) int64 {
		b, ok := ctx.Bar(sig.Instrument)
		if !ok {
			return 0
		}
		if orderBy == "volume" {
			return b.Volume
		}
		return b.Amount
	}
	out := make([]Signal, len(sigs))
	copy(out, sigs)
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := key(out[i]), key(out[j])
		if ki != kj {
			return ki > kj
		}
		return out[i].Instrument < out[j].Instrument
	})
	return out
}

// splitEntries 先把清仓 / 策略定量的信号处理掉，再把剩下的建仓候选排好序。
//
// **清仓排在建仓之前**：卖出释放的资金正是买入要用的。
// 反过来的话，同一天里「卖 A 买 B」会因为 A 的钱还没回来而买不进 B。
func splitEntries(
	sigs []Signal, ctx SizeContext, orderBy string,
) (done []Order, entries []Signal) {

	done = make([]Order, 0, len(sigs))
	entries = make([]Signal, 0, len(sigs))
	for _, sig := range sigs {
		if o, ok, handled := dispatch(sig, ctx); handled {
			if ok {
				done = append(done, o)
			}
			continue
		}
		entries = append(entries, sig)
	}
	return done, rankEntries(entries, ctx, orderBy)
}

// buyQty 按预算折算数量并交给 Market 规整。
//
// 规整必须走 Market：100 股整数倍、零股卖出这些规则属于市场而非仓位方法，
// 不该在每个 Sizer 里各写一遍（远期加密货币的最小单位完全不同）。
//
// # 预算要覆盖「金额 + 摩擦」，不只是金额
//
// 撮合时校验的是 `金额 + 费用 + 滑点 ≤ 购买力`。若定量只按金额算，
// 预算等于全部权益时（`pct_equity` 设 100%）算出的数量刚好花光资金，
// 撮合必然因为那点手续费而整单被拒，报「现金不足」——
// 看不出差的只是手续费。
//
// 摩擦几乎都与金额成正比，所以按比例缩一次就很接近；
// 再逐次退让几档兜住印花税那种有下限的规则。
func buyQty(ctx SizeContext, id mktdata.InstrumentID, budgetCents int64) (int64, bool) {
	return buyQtyFit(ctx, id, budgetCents, false)
}

// buyQtyFit 同上，fitCash 为真时**再按账户实际拿得出的钱缩一次**。
//
// 两件事必须分开，它们的性质完全不同：
//
//	预算覆盖摩擦   —— 修的是「算漏了手续费」，本来就该这样
//	按可用资金缩量 —— 把「钱不够就不买」改成「钱不够就少买」，
//	                 这是一条**仓位政策**，不是 bug
//
// 后者影响很大：实测 macd_cross 从 +17.85% 变成 −17.01%（成交 1833 → 1929）——
// 因为本来会被拒掉的单子现在变成了小一号的成交。所以它默认关闭，
// 由 `fit_cash` 显式打开。
func buyQtyFit(
	ctx SizeContext, id mktdata.InstrumentID, budgetCents int64, fitCash bool,
) (int64, bool) {
	bar, ok := ctx.Bar(id)
	if !ok || bar.Suspended() || bar.Close <= 0 {
		return 0, false
	}
	inst := ctx.Instrument(id)
	if inst == nil {
		return 0, false
	}
	if budgetCents <= 0 {
		return 0, false
	}
	held := ctx.Ledger().Exposure(id).Long

	// 先按不含摩擦的预算试一版，再往下收敛到「含摩擦也放得下」
	target := budgetCents
	for i := 0; i < 6; i++ {
		// 按**标的自己的 scale 与合约乘数**折算，且走 128 位 ——
		// 写死 ×10 是 A 股口径，在加密上算出来的数量差十几个数量级，
		// 然后被 NormalizeQty 判成 0，信号就这么静默消失了
		raw := QtyForCents(inst, bar.Close, target)
		if raw <= 0 {
			return 0, false
		}
		qty, ok := ctx.Market().NormalizeQty(inst, raw, SideBuy, held)
		if !ok || qty <= 0 {
			return 0, false
		}
		amount := NotionalCents(inst, bar.Close, qty)
		friction := ctx.FrictionCents(inst, SideBuy, qty, amount)
		fits := amount+friction <= budgetCents
		if fits && (!fitCash || ctx.Ledger().AffordOpen(amount, friction)) {
			return qty, true
		}
		// 放不下：把目标按「预算 ÷ 实际要花的钱」缩一次。
		// 比例缩不动时（规整到同一档）再硬退一格，避免原地打转
		limit := budgetCents
		if fitCash {
			if bp := ctx.Ledger().BuyingPowerCents(); bp < limit {
				limit = bp
			}
		}
		next := MulDiv(target, limit, amount+friction)
		if next >= target {
			next = target - target/64 - 1
		}
		target = next
		if target <= 0 {
			return 0, false
		}
	}
	return 0, false
}

// entryOrder 按预算生成一张建仓单。
//
// `SignalTarget` 走另一条路：它说的不是「买一份」而是**「持到这个比例」**，
// 于是要先看现在持了多少，再补差额 —— 可能是买、可能是卖、也可能不动。
// 这是网格那类策略唯一表达得出「加一格 / 减一格」的方式。
func entryOrder(
	sig Signal, ctx SizeContext, side Side, budgetCents int64, fitCash bool,
) (Order, bool) {

	if sig.Kind == SignalTarget {
		return targetOrder(sig, ctx, side, budgetCents, fitCash)
	}
	qty, ok := buyQtyFit(ctx, sig.Instrument, budgetCents, fitCash)
	if !ok {
		return Order{}, false
	}
	return Order{
		Instrument: sig.Instrument, Side: side, Qty: qty,
		Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
	}, true
}

// targetOrder 把「持到预算的 Weight 比例」折算成补差额的一张单。
//
// # 为什么必须有它
//
// 信号模型原本只表达得出「买一份」（Enter）与「全平」（Exit）。
// 「卖掉十分之一」两者都不是 —— 网格每涨一格减一份，正是这个形状。
//
// 从前网格用 `Enter + 卖 + Strength` 凑：单向市场里被 dispatch 当成清仓
// （全平，Strength 根本没参与），双向市场里更糟，会被当成**反向开仓**。
// 两个都不是「减一点」。
//
// # Weight 是相对**这个 Sizer 给这只标的的预算**
//
// 不是相对总权益。`pct_equity pct=95` 下 Weight=0.5 就是 47.5% 的本金；
// `equal_weight slots=10` 下 Weight=0.5 就是半个槽。
// 这样同一棵策略换个 Sizer 不必改 Weight 的含义。
func targetOrder(
	sig Signal, ctx SizeContext, side Side, budgetCents int64, fitCash bool,
) (Order, bool) {

	inst := ctx.Instrument(sig.Instrument)
	bar, ok := ctx.Bar(sig.Instrument)
	if inst == nil || !ok || bar.Suspended() || bar.Close <= 0 {
		return Order{}, false
	}
	w := sig.Weight
	if w < 0 {
		w = 0
	}
	if w > 1 {
		w = 1
	}
	want := MulDiv(budgetCents, int64(w*1_000_000), 1_000_000)

	// 现在这条腿上持了多少（按当前价估值）
	ex := ctx.Ledger().Exposure(sig.Instrument)
	held := ex.Long
	if side == SideSell {
		held = ex.Short
	}
	have := NotionalCents(inst, bar.Close, held)

	switch {
	case want > have:
		// 补仓：差多少买多少
		qty, ok := buyQtyFit(ctx, sig.Instrument, want-have, fitCash)
		if !ok {
			return Order{}, false
		}
		return Order{
			Instrument: sig.Instrument, Side: side, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		}, true

	case want < have:
		// 减仓：**这才是 Target 存在的理由**。方向与开仓相反，且带 Reduce ——
		// 少了 Reduce，双向账本会把这张单当成反向开仓
		closeSide := SideSell
		if side == SideSell {
			closeSide = SideBuy
		}
		raw := QtyForCents(inst, bar.Close, have-want)
		if raw <= 0 {
			return Order{}, false
		}
		if raw > held {
			raw = held
		}
		avail := held
		if side == SideBuy {
			// 现货 T+1：今天买的今天卖不掉
			if a := ctx.Available(sig.Instrument); a < avail {
				avail = a
			}
		}
		if raw > avail {
			raw = avail
		}
		qty, ok := ctx.Market().NormalizeQty(inst, raw, closeSide, avail)
		if !ok || qty <= 0 {
			return Order{}, false
		}
		return Order{
			Instrument: sig.Instrument, Side: closeSide, Qty: qty, Reduce: true,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		}, true
	}
	return Order{}, false // 已经在目标上，不动
}

// exitOrder 把清仓信号变成平仓单。
//
// **方向由信号的 Side 决定平哪一边**：
//
//	Exit + 卖 = 平多（数量取可卖多头）
//	Exit + 买 = 平空（数量取空头持仓）
//
// 单向市场里只有前者。所有平仓单都带 `Reduce`，
// 否则双向账本会把「卖」当成开空。
func exitOrder(sig Signal, ctx SizeContext) (Order, bool) {
	inst := ctx.Instrument(sig.Instrument)
	if inst == nil {
		return Order{}, false
	}
	side := SideSell
	avail := ctx.Available(sig.Instrument)
	if sig.Side == SideBuy && ctx.Market().AllowsShort() {
		side = SideBuy
		avail = ctx.Ledger().Exposure(sig.Instrument).Short
	}
	if avail <= 0 {
		return Order{}, false
	}
	qty, ok := ctx.Market().NormalizeQty(inst, avail, side, avail)
	if !ok || qty <= 0 {
		return Order{}, false
	}
	return Order{
		Instrument: sig.Instrument, Side: side, Qty: qty, Reduce: true,
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
	ex := ctx.Ledger().Exposure(sig.Instrument)
	held := ex.Long
	// 平仓单：Exit 一律是平；单向市场里「卖」也只能是平
	reduce := sig.Kind == SignalExit || !ctx.Market().AllowsShort()
	if sig.Side == SideBuy && ctx.Market().AllowsShort() && sig.Kind == SignalExit {
		held = ex.Short
	}
	qty, ok := ctx.Market().NormalizeQty(inst, sig.Qty, sig.Side, held)
	if !ok || qty <= 0 {
		return Order{}, false
	}
	return Order{
		Instrument: sig.Instrument, Side: sig.Side, Qty: qty,
		Reduce: reduce && sig.Side == SideSell || sig.Kind == SignalExit,
		Type:   orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
	}, true
}

// dispatch 处理所有 Sizer 共有的前置分支：策略定量、清仓。
// 返回 handled=true 表示本信号已处理完，无需再算仓位。
func dispatch(sig Signal, ctx SizeContext) (Order, bool, bool) {
	if sig.Qty > 0 {
		o, ok := override(sig, ctx)
		return o, ok, true
	}
	if sig.Kind == SignalExit {
		o, ok := exitOrder(sig, ctx)
		return o, ok, true
	}
	// **`Enter + 卖` 的含义由市场决定**：
	// 不能做空时那是减仓（A 股的网格就这么用），能做空时那是开空。
	// 后者要走定量路径，不能当成平仓
	if sig.Side == SideSell && !ctx.Market().AllowsShort() {
		o, ok := exitOrder(sig, ctx)
		return o, ok, true
	}
	return Order{}, false, false
}

// ---- equal_weight ----

var equalWeightSpecs = []spec.ParamSpec{
	{Name: "slots", Kind: spec.ParamInt, Default: 10, Min: 1, Max: 500, Step: 1,
		Desc: "把资金等分成多少份，同时也是最多持有的标的数"},
	{Name: "base", Kind: spec.ParamString, DefaultStr: "cost",
		Options: []string{"cost", "notional", "equity", "initial"},
		Desc: "每份的计算基准：" +
			"cost=已投入本金（现金 + 持仓成本，浮盈不放大后续仓位）；" +
			"notional=本金 × 杠杆（要让杠杆真的放大敞口就选它，仅保证金账本有别）；" +
			"equity=当前权益（浮盈立刻用于加仓）；" +
			"initial=初始资金（定额下注，不复利 —— 旧配置在用，不建议新用）"},
	{Name: "order_by", Kind: spec.ParamString, DefaultStr: "amount",
		Options: []string{"amount", "volume", "signal"},
		Desc: "候选多于空位时先给谁：" +
			"amount=当日成交额大的优先（默认，流动性口径）；" +
			"volume=当日成交量大的优先；" +
			"signal=按策略给出的顺序"},
	{Name: "fit_cash", Kind: spec.ParamBool, Default: 0, Min: 0, Max: 1, Step: 1,
		Desc: "资金不够时缩量买入（开）还是整单放弃（关，默认）。" +
			"打开后本会被「现金不足」拒掉的单子会变成小一号的成交，成交数与结果都会明显变化"},
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
	slots   int
	base    string
	orderBy string
	fitCash bool
}

func (s *EqualWeight) Name() string { return "equal_weight" }

func (s *EqualWeight) Size(sigs []Signal, ctx SizeContext) []Order {
	budget := sizeBase(ctx, s.base)
	// **预算不足只挡建仓，不挡清仓。** 早期版本在这里直接 return nil，
	// 结果权益归零的账户连卖都卖不出去 —— 由 TestSizerExitUsesAvailable 发现。
	// 卖出从来不需要预算。
	var slotCents int64
	if s.slots > 0 && budget > 0 {
		slotCents = budget / int64(s.slots)
	}

	out, entries := splitEntries(sigs, ctx, s.orderBy)
	occ := occupancy(ctx)
	used := occ.len()

	for _, sig := range entries {
		// 建仓方向：单向市场恒为买，双向市场跟随信号（Enter + 卖 = 开空）
		side := entrySide(sig, ctx)
		// 建仓：预算不足、已在场、仓位已满，三者任一即跳过。
		//
		// **调仓（Target）例外**：它是把已有仓位补到某个比例，
		// 不是开一个新槽。用「已在场」把它挡掉的话，
		// 网格从第二格起就再也动不了 —— 而它每一格都在调仓
		if slotCents <= 0 {
			continue
		}
		occupiedHere := occ.has(sig.Instrument, side)
		if occupiedHere && sig.Kind != SignalTarget {
			continue
		}
		if !occupiedHere && used >= s.slots {
			continue
		}
		o, ok := entryOrder(sig, ctx, side, slotCents, s.fitCash)
		if !ok {
			continue
		}
		out = append(out, o)
		occ.take(sig.Instrument, side)
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
	out, entries := splitEntries(sigs, ctx, "amount")
	occ := occupancy(ctx)
	used := occ.len()
	for _, sig := range entries {
		// 建仓方向：单向市场恒为买，双向市场跟随信号（Enter + 卖 = 开空）
		side := entrySide(sig, ctx)
		if occ.has(sig.Instrument, side) {
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
			Instrument: sig.Instrument, Side: side, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
		occ.take(sig.Instrument, side)
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
	out, entries := splitEntries(sigs, ctx, "amount")
	occ := occupancy(ctx)
	used := occ.len()
	for _, sig := range entries {
		// 建仓方向：单向市场恒为买，双向市场跟随信号（Enter + 卖 = 开空）
		side := entrySide(sig, ctx)
		if occ.has(sig.Instrument, side) {
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
			Instrument: sig.Instrument, Side: side, Qty: qty,
			Type: orderType(sig), LimitPrice: sig.LimitPrice, Tag: sig.Tag,
		})
		occ.take(sig.Instrument, side)
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
	{Name: "base", Kind: spec.ParamString, DefaultStr: "cost",
		Options: []string{"cost", "notional", "equity"},
		Desc: "百分比的基准：cost=已投入本金（浮盈不放大后续仓位）；" +
			"notional=本金 × 杠杆（杠杆放大敞口）；equity=当前权益（浮盈立刻用于加仓）"},
	{Name: "order_by", Kind: spec.ParamString, DefaultStr: "amount",
		Options: []string{"amount", "volume", "signal"},
		Desc: "候选多于空位时先给谁：amount=当日成交额大的优先（默认）；" +
			"volume=当日成交量大的优先；signal=按策略给出的顺序"},
	{Name: "fit_cash", Kind: spec.ParamBool, Default: 0, Min: 0, Max: 1, Step: 1,
		Desc: "资金不够时缩量买入（开）还是整单放弃（关，默认）。" +
			"打开后本会被「现金不足」拒掉的单子会变成小一号的成交，成交数与结果都会明显变化"},
}

// PctEquity 每笔占基准资金的固定比例。与 equal_weight 的区别在于
// 它不受「份数」约束 —— 可以叠到超过 100%（会被资金不足自然挡住）。
type PctEquity struct {
	ppm          int64 // pct 转成百万分之一，避免浮点参与金额计算
	maxPositions int
	base         string
	orderBy      string
	fitCash      bool
}

func (s *PctEquity) Name() string { return "pct_equity" }

func (s *PctEquity) Size(sigs []Signal, ctx SizeContext) []Order {
	// 同 EqualWeight：资金为零也必须能卖
	budget := int64(0)
	if b := sizeBase(ctx, s.base); b > 0 {
		budget = MulDiv(b, s.ppm, 1_000_000)
	}
	out, entries := splitEntries(sigs, ctx, s.orderBy)
	occ := occupancy(ctx)
	used := occ.len()
	for _, sig := range entries {
		// 建仓方向：单向市场恒为买，双向市场跟随信号（Enter + 卖 = 开空）
		side := entrySide(sig, ctx)
		// 调仓（Target）例外，理由同 EqualWeight
		if budget <= 0 {
			continue
		}
		occupiedHere := occ.has(sig.Instrument, side)
		if occupiedHere && sig.Kind != SignalTarget {
			continue
		}
		if !occupiedHere && s.maxPositions > 0 && used >= s.maxPositions {
			continue
		}
		o, ok := entryOrder(sig, ctx, side, budget, s.fitCash)
		if !ok {
			continue
		}
		out = append(out, o)
		occ.take(sig.Instrument, side)
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
	{Name: "base", Kind: spec.ParamString, DefaultStr: "cost",
		Options: []string{"cost", "notional", "equity"},
		Desc: "总预算的基准：cost=已投入本金（浮盈不放大后续仓位）；" +
			"notional=本金 × 杠杆（杠杆放大敞口）；equity=当前权益（浮盈立刻用于加仓）"},
}

// StrengthWeighted 按信心分配：本步的建仓信号按 Strength 归一化瓜分总预算。
//
// 这是唯一真正用到「整批信号」的 Sizer —— 归一化的分母是本步所有信号的
// Strength 之和，逐条调用根本算不出来。
type StrengthWeighted struct {
	totalPPM    int64
	minStrength float64
	base        string
}

func (s *StrengthWeighted) Name() string { return "strength_weighted" }

func (s *StrengthWeighted) Size(sigs []Signal, ctx SizeContext) []Order {
	eq := sizeBase(ctx, s.base)
	// 按信心瓜分总预算，与「谁排前面」无关 —— 所以不排序
	out, candidates := splitEntries(sigs, ctx, "signal")
	occ := occupancy(ctx)

	// 统计建仓信号的信心之和
	var sum float64
	enters := make([]Signal, 0, len(candidates))
	for _, sig := range candidates {
		// 建仓方向：单向市场恒为买，双向市场跟随信号（Enter + 卖 = 开空）
		side := entrySide(sig, ctx)
		if occ.has(sig.Instrument, side) || sig.Strength < s.minStrength {
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
		// 走 entryOrder 而不是直接 buyQty —— 否则 Target 信号会被当成
		// 普通建仓：**只在空仓时买得进一次，之后每一格都变成空操作**。
		// 实测网格因此 22 条信号只成交 1 笔
		o, ok := entryOrder(sig, ctx, entrySide(sig, ctx), budget, false)
		if !ok {
			continue
		}
		out = append(out, o)
	}
	return out
}

// ---- 注册 ----

func init() {
	Sizers.Register("equal_weight", "等权：把资金等分成 N 份，每个信号占一份", equalWeightSpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(equalWeightSpecs, raw)
			if err != nil {
				return nil, err
			}
			base, err := registry.DecodeString(equalWeightSpecs, raw, "base")
			if err != nil {
				return nil, err
			}
			ob, err := registry.DecodeString(equalWeightSpecs, raw, "order_by")
			if err != nil {
				return nil, err
			}
			return &EqualWeight{
				slots: p.Int("slots", 10), base: base, orderBy: ob,
				fitCash: p.Bool("fit_cash", false),
			}, nil
		})

	// fixed_cash / fixed_qty 是**定额下注**，不复利。
	// 装配器里已经不再提供（见 modules.go 的 legacy 标记）——
	// 留在注册表里只为让既有配置还能跑
	Sizers.Register("fixed_cash", "每笔投入固定金额（定额下注，不复利；旧配置在用）", fixedCashSpecs,
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

	Sizers.Register("fixed_qty", "每笔固定股数 / 份数（定额下注，不复利；旧配置在用）", fixedQtySpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(fixedQtySpecs, raw)
			if err != nil {
				return nil, err
			}
			return &FixedQty{
				qty: int64(p.Int("qty", 100)), maxPositions: p.Int("max_positions", 0),
			}, nil
		})

	Sizers.Register("pct_equity", "每笔投入基准资金的固定百分比（复利）", pctEquitySpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(pctEquitySpecs, raw)
			if err != nil {
				return nil, err
			}
			base, err := registry.DecodeString(pctEquitySpecs, raw, "base")
			if err != nil {
				return nil, err
			}
			ob, err := registry.DecodeString(pctEquitySpecs, raw, "order_by")
			if err != nil {
				return nil, err
			}
			return &PctEquity{
				base: base, orderBy: ob, fitCash: p.Bool("fit_cash", false),
				ppm:          int64(p.Float("pct", 10) * 10_000),
				maxPositions: p.Int("max_positions", 0),
			}, nil
		})

	Sizers.Register("strength_weighted", "按信号强度分配：策略给的 Strength 越高分到越多", strengthWeightedSpecs,
		func(raw json.RawMessage) (Sizer, error) {
			p, err := registry.DecodeParams(strengthWeightedSpecs, raw)
			if err != nil {
				return nil, err
			}
			base, err := registry.DecodeString(strengthWeightedSpecs, raw, "base")
			if err != nil {
				return nil, err
			}
			return &StrengthWeighted{
				base:        base,
				totalPPM:    int64(p.Float("total_pct", 100) * 10_000),
				minStrength: p.Float("min_strength", 0),
			}, nil
		})
}
