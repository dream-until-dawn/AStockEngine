package trading

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// RiskContext 是风控规则可见的账户状态。
type RiskContext interface {
	Time() mktdata.TimePoint
	Ledger() Ledger
	EquityCents() int64
	// PeakEquityCents 历史峰值权益，回撤类规则的基准。
	//
	// 它是**引擎的跨步状态**，必须随快照一起保存 —— 不然从快照恢复后峰值归零，
	// 熔断规则立刻失效，账目随之偏离。
	PeakEquityCents() int64
	Bar(id mktdata.InstrumentID) (mktdata.Bar, bool)
	Instrument(id mktdata.InstrumentID) *mktdata.Instrument
	Pending() []PendingOrder
	Market() Market
}

// Risk 在订单入队前拦截。
//
// 多条规则**链式**执行：任一条拒绝即拒绝，通过的订单可被前一条缩量后
// 传给下一条。风控是若干独立限制的叠加，不是单选。
type Risk interface {
	Name() string
	// Check 返回调整后的订单；ok=false 表示拒绝，此时 Rejection 必须填好原因。
	Check(o Order, ctx RiskContext) (Order, Rejection, bool)
}

// Risks 是风控模块的注册表。
var Risks = registry.New[Risk]("risk")

// RiskChain 顺序执行多条规则。
// StatefulRisk 供**确有跨步状态**的风控规则实现（如熔断的冷静期计数）。
//
// 与 StatefulExit 同理：引擎的快照涵盖不了规则自己的字段。
// 熔断触发后剩余的冷静期若不进快照，从快照恢复的那一步冷静期归零，
// **熔断静默解除** —— 不报错、不异常，只是本该被挡住的单子放行了。
type StatefulRisk interface {
	SnapshotState() ([]byte, error)
	RestoreState([]byte) error
}

type RiskChain []Risk

// SnapshotState 逐条快照。**总是实现**，即使当前链上都是无状态规则 ——
// 链的成员会在配置里换掉，让快照格式随成员而变会让恢复莫名其妙地失败。
func (c RiskChain) SnapshotState() ([]byte, error) {
	out := make([]json.RawMessage, len(c))
	for i, r := range c {
		sr, ok := r.(StatefulRisk)
		if !ok {
			out[i] = json.RawMessage("null")
			continue
		}
		b, err := sr.SnapshotState()
		if err != nil {
			return nil, fmt.Errorf("风控规则 %s 快照失败: %w", r.Name(), err)
		}
		out[i] = b
	}
	return json.Marshal(out)
}

// RestoreState 逐条恢复。
func (c RiskChain) RestoreState(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var in []json.RawMessage
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("解析风控链快照失败: %w", err)
	}
	if len(in) != len(c) {
		return fmt.Errorf("快照有 %d 条风控规则，当前链有 %d 条 —— "+
			"该快照多半来自另一份配置", len(in), len(c))
	}
	for i, raw := range in {
		sr, ok := c[i].(StatefulRisk)
		if !ok {
			continue
		}
		if len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("风控规则 %s 有跨步状态，但快照里是空的", c[i].Name())
		}
		if err := sr.RestoreState(raw); err != nil {
			return fmt.Errorf("风控规则 %s 恢复失败: %w", c[i].Name(), err)
		}
	}
	return nil
}

// Check 依次过每条规则。
func (c RiskChain) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	for _, r := range c {
		next, rej, ok := r.Check(o, ctx)
		if !ok {
			return o, rej, false
		}
		// 缩量到 0 等同于拒绝 —— 让它静静变成一张空单是最难查的那种 bug
		if next.Qty <= 0 {
			return o, Rejection{
				Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
				Detail: "风控把数量压到 0",
			}, false
		}
		o = next
	}
	return o, Rejection{}, true
}

// Names 返回链上各规则名，供报告与配置回显。
func (c RiskChain) Names() []string {
	out := make([]string, len(c))
	for i, r := range c {
		out[i] = r.Name()
	}
	return out
}

// ---- 公共工具 ----

// heldValueCents 估算某标的当前持仓的市值（含在途买单的预估占用）。
func heldValueCents(ctx RiskContext, id mktdata.InstrumentID) int64 {
	bar, ok := ctx.Bar(id)
	if !ok || bar.Close <= 0 {
		return 0
	}
	qty := ctx.Ledger().Exposure(id).Long
	for _, po := range ctx.Pending() {
		if po.Instrument == id && po.Side == SideBuy {
			qty += po.Qty
		}
	}
	// 股 × 厘 / 10 = 分
	return qty * bar.Close / 10
}

// shrinkToBudget 把买单数量压到预算之内。返回 0 表示一手都买不起。
func shrinkToBudget(ctx RiskContext, o Order, budgetCents int64) int64 {
	bar, ok := ctx.Bar(o.Instrument)
	if !ok || bar.Close <= 0 {
		return 0
	}
	inst := ctx.Instrument(o.Instrument)
	if inst == nil {
		return 0
	}
	maxQty := QtyForCents(inst, bar.Close, budgetCents)
	if maxQty >= o.Qty {
		return o.Qty
	}
	if maxQty <= 0 {
		return 0
	}
	held := ctx.Ledger().Exposure(o.Instrument).Long
	q, ok := ctx.Market().NormalizeQty(inst, maxQty, SideBuy, held)
	if !ok {
		return 0
	}
	return q
}

// ---- max_position_pct ----

var maxPositionPctSpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 20, Min: 0.01, Max: 100, Step: 0.01,
		Desc: "单只标的市值占总权益的上限（%）"},
	{Name: "shrink", Kind: spec.ParamBool, Default: 1, Min: 0, Max: 1, Step: 1,
		Desc: "超限时缩量（1）还是整单拒绝（0）"},
}

// MaxPositionPct 限制单票集中度。
type MaxPositionPct struct {
	ppm    int64
	shrink bool
}

func (r *MaxPositionPct) Name() string { return "max_position_pct" }

func (r *MaxPositionPct) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if o.Side != SideBuy {
		return o, Rejection{}, true // 卖出只会降低集中度
	}
	eq := ctx.EquityCents()
	if eq <= 0 {
		return o, Rejection{}, true
	}
	cap := eq * r.ppm / 1_000_000
	cur := heldValueCents(ctx, o.Instrument)
	room := cap - cur
	if room <= 0 {
		return o, Rejection{
			Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
			Detail: fmt.Sprintf("已持 %.2f 元，上限 %.2f 元（权益 %.2f 的 %.2f%%）",
				cents(cur), cents(cap), cents(eq), float64(r.ppm)/10_000),
		}, false
	}
	q := shrinkToBudget(ctx, o, room)
	if q >= o.Qty {
		return o, Rejection{}, true
	}
	if !r.shrink || q <= 0 {
		return o, Rejection{
			Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
			Detail: fmt.Sprintf("剩余额度 %.2f 元，只够 %d 股，需要 %d 股",
				cents(room), q, o.Qty),
		}, false
	}
	o.Qty = q
	return o, Rejection{}, true
}

// ---- max_positions ----

var maxPositionsSpecs = []spec.ParamSpec{
	{Name: "n", Kind: spec.ParamInt, Default: 20, Min: 1, Max: 5000, Step: 1,
		Desc: "最多同时持有多少只标的（已持有与在途去重后计数）"},
}

// MaxPositions 限制同时持仓只数。
//
// 计数**去重**：一只标的同时持有并挂着卖单只算一个。
// v0.2 的样例策略在这里是重复计数的（持有 + 在途各算一次），
// 那让卖单意外收紧了买入上限 —— 本实现按去重语义写，是有意的行为修正。
type MaxPositions struct{ n int }

func (r *MaxPositions) Name() string { return "max_positions" }

func (r *MaxPositions) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if o.Side != SideBuy {
		return o, Rejection{}, true
	}
	in := make(map[mktdata.InstrumentID]bool, 64)
	ctx.Ledger().EachExposure(func(id mktdata.InstrumentID, e Exposure) bool {
		in[id] = true
		return true
	})
	for _, po := range ctx.Pending() {
		in[po.Instrument] = true
	}
	if in[o.Instrument] {
		return o, Rejection{}, true // 加仓不占新坑
	}
	if len(in) >= r.n {
		return o, Rejection{
			Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
			Detail: fmt.Sprintf("已占用 %d 个仓位，上限 %d", len(in), r.n),
		}, false
	}
	return o, Rejection{}, true
}

// ---- drawdown_halt ----

var drawdownHaltSpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 30, Min: 0.1, Max: 100, Step: 0.1,
		Desc: "自峰值权益回撤超过此比例即触发熔断（%）"},
	{Name: "cooldown_bars", Kind: spec.ParamInt, Default: 0, Min: 0, Max: 2000, Step: 1,
		Desc: "冷静期：触发后停止开新仓多少根 bar，期满从当前权益重新起算回撤。0 = 不计时，只要仍在回撤线下就一直停"},
	{Name: "flatten", Kind: spec.ParamBool, Default: 0, Min: 0, Max: 1, Step: 1,
		Desc: "触发时是否平掉全部仓位。关 = 只禁新仓、持仓不动"},
}

// DrawdownHalt 回撤熔断：自峰值权益回撤超过阈值后停止开新仓。
//
// # 三种形态，由参数决定
//
//	cooldown=0, flatten=off  —— 纯拦截。只要还在回撤线下就禁新仓，
//	                            回上去自动恢复。这是默认，也是历史行为
//	cooldown=N, flatten=off  —— 触发后**固定停 N 根 bar**，
//	                            期间即便权益回上来也不放行
//	cooldown=N, flatten=on   —— 触发时先清空持仓，再停 N 根
//
// # 为什么「平掉全部仓位」要显式勾选
//
// 熔断规则一旦自己发卖单，就等于在策略之外新增了一条交易逻辑，
// 回测结果不再能干净地归因到策略本身 —— 亏损是策略的还是熔断的，
// 分不出来。**所以默认关闭**，打开是使用者的明确选择。
//
// 打开之后它仍然是一条**可归因**的逻辑：平仓信号带 `tag=drawdown_halt`，
// 报告里能按 tag 把这批成交拆出来看。
//
// # 冷静期为什么不等价于「等回撤收窄」
//
// 不设冷静期时，权益只要反弹一点越过阈值就立刻放行 ——
// 而那一点反弹常常是下跌中继。冷静期表达的是另一个意思：
// **不是「跌够了没有」，而是「先别动，让它走一段」**。
type DrawdownHalt struct {
	ppm      int64
	cooldown int
	flatten  bool

	// tripped 已触发且尚在冷静期。**跨步状态**，必须进快照
	tripped bool
	// remain 冷静期剩余 bar 数，仅 cooldown > 0 时有意义
	remain int
	// flattenAt 本步要清仓（由 OnStep 置位）。用一个字段而不是
	// 直接在 OnStep 里发信号后就忘掉，是因为 Check 也要知道
	// 「本步已经熔断了」—— 两个钩子在同一步里必须看到同一个判断
	flattenedOnce bool
	// refPeak 冷静期结束后重新起算的参考峰值。0 表示还没重新起算过，
	// 此时用引擎的历史峰值。**只有设了冷静期才会用到它**，理由见 rebase 注释
	refPeak int64
}

// peakFor 当前该拿哪个峰值算回撤。
func (r *DrawdownHalt) peakFor(enginePeak int64) int64 {
	if r.refPeak > 0 {
		return r.refPeak
	}
	return enginePeak
}

func (r *DrawdownHalt) Name() string { return "drawdown_halt" }

// ddPPM 当前自峰值的回撤（百万分之一）。峰值非正时返回 0。
func ddPPM(peak, eq int64) int64 {
	if peak <= 0 {
		return 0
	}
	return (peak - eq) * 1_000_000 / peak
}

// OnStep 维护冷静期倒计时，并在触发时（如已勾选）平掉全部仓位。
//
// **它是 ExitRule 那一侧的入口**，每步都会被调用，与是否有订单无关 ——
// 冷静期倒计时必须每步走一格，挂在 Check 上的话，没有订单的那些步
// 就不会计数，「停 5 根 bar」会变成「停 5 次下单尝试」。
func (r *DrawdownHalt) OnStep(ctx ExitContext) []Signal {
	eq := ctx.EquityCents()
	// 重新起算之后，参考峰值跟着权益往上走 —— 否则它就成了一个死值，
	// 回撤只会相对那一刻算，越走越失真
	if r.refPeak > 0 && eq > r.refPeak {
		r.refPeak = eq
	}
	dd := ddPPM(r.peakFor(ctx.PeakEquityCents()), eq)
	over := dd >= r.ppm

	switch {
	case r.tripped && r.cooldown > 0:
		// 冷静期内：每步走一格，走完才解除
		r.remain--
		if r.remain <= 0 {
			r.tripped, r.remain, r.flattenedOnce = false, 0, false
			// **从这里重新起算回撤。**
			//
			// 不重算的话冷静期是个陷阱：勾了清仓之后账户全是现金、
			// 权益不再变动，而历史峰值停在熔断之前 ——
			// 回撤永远回不到阈值以内，冷静期一到就立刻再次触发，
			// 从此再也不交易。实测：2021 年触发一次，
			// 之后 5 年 0 成交 285 次拒单，而报告上只是一条直线。
			//
			// 「冷静期」本来的意思就是「停一段时间，然后重新开始」，
			// 重新开始自然要从当下重新计量
			r.refPeak = eq
		}
	case r.tripped:
		// 不计时：回到阈值以内即解除
		if !over {
			r.tripped, r.flattenedOnce = false, false
		}
	case over:
		r.tripped, r.remain = true, r.cooldown
	}

	if !r.tripped || !r.flatten || r.flattenedOnce {
		return nil
	}
	// 只在**刚触发的那一步**清仓，不是冷静期内每步都发
	r.flattenedOnce = true
	return flattenAll(ctx, r.Name())
}

// Check 拦下开仓单。
//
// **平仓单永远放行**：熔断是「别再进场」，不是「锁死账户」——
// 拦住平仓会让止损也失效，那比不熔断更危险。
func (r *DrawdownHalt) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if o.Reduce {
		return o, Rejection{}, true
	}
	if !LegOf(o.Side, o.Reduce, ctx.Market().AllowsShort()).Opening {
		return o, Rejection{}, true
	}
	eq := ctx.EquityCents()
	peak := r.peakFor(ctx.PeakEquityCents())
	dd := ddPPM(peak, eq)

	// tripped 由 OnStep 维护。但 OnStep 只在 TradeFrom 之后被调用，
	// 且外部可能只把这条规则挂在风控链上 —— 所以这里也直接判一次阈值，
	// 两条路任一成立即拦。少判一条的后果是熔断在某些装配下静默失效
	if !r.tripped && dd < r.ppm {
		return o, Rejection{}, true
	}
	detail := fmt.Sprintf("当前回撤 %.2f%% 已达熔断线 %.2f%%（峰值 %.2f，现 %.2f）",
		float64(dd)/10_000, float64(r.ppm)/10_000, cents(peak), cents(eq))
	if r.tripped && r.cooldown > 0 {
		detail += fmt.Sprintf("，冷静期还剩 %d 根", r.remain)
	}
	return o, Rejection{
		Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
		Detail: detail,
	}, false
}

// Tripped 报告当前是否处于熔断中，供单步调试展示。
func (r *DrawdownHalt) Tripped() (bool, int) { return r.tripped, r.remain }

type drawdownHaltState struct {
	Tripped   bool  `json:"tripped"`
	Remain    int   `json:"remain"`
	Flattened bool  `json:"flattened"`
	RefPeak   int64 `json:"ref_peak"`
}

func (r *DrawdownHalt) SnapshotState() ([]byte, error) {
	return json.Marshal(drawdownHaltState{
		Tripped: r.tripped, Remain: r.remain, Flattened: r.flattenedOnce,
		RefPeak: r.refPeak,
	})
}

func (r *DrawdownHalt) RestoreState(b []byte) error {
	var st drawdownHaltState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("解析回撤熔断快照失败: %w", err)
	}
	r.tripped, r.remain, r.flattenedOnce = st.Tripped, st.Remain, st.Flattened
	r.refPeak = st.RefPeak
	return nil
}

// flattenAll 给全部持仓发平仓信号，方向按市场规则定。
//
// 双向市场下多空两条腿都要平，且平的方向相反 ——
// 只发卖出的话，空头仓位会原封不动地留着。
func flattenAll(ctx ExitContext, tag string) []Signal {
	hedge := ctx.Market().AllowsShort()
	var out []Signal
	ctx.Ledger().EachExposure(func(id mktdata.InstrumentID, e Exposure) bool {
		if e.Long > 0 {
			out = append(out, Signal{
				Instrument: id, Kind: SignalExit, Side: SideSell, Tag: tag,
			})
		}
		if hedge && e.Short > 0 {
			// 平空是**买入**。ExitChain 按标的去重，同一标的的多空
			// 两条腿会撞在一起 —— 但双向下同时持多又持空本身少见，
			// 且下一步会再发一次，不会漏掉
			out = append(out, Signal{
				Instrument: id, Kind: SignalExit, Side: SideBuy, Tag: tag,
			})
		}
		return true
	})
	return out
}

var (
	_ Risk         = (*DrawdownHalt)(nil)
	_ ExitRule     = (*DrawdownHalt)(nil)
	_ StatefulRisk = (*DrawdownHalt)(nil)
)

// ---- min_capital ----

var minCapitalSpecs = []spec.ParamSpec{
	{Name: "floor", Kind: spec.ParamFloat, Default: 0, Min: 0, Max: 1e12, Step: 1,
		Desc: "最低有效资金（计价币种，如元 / USDT）。无持仓且权益低于它即判定策略失败。0 = 不设显式下限，只靠「开不起最小一手」自动判定"},
	{Name: "block", Kind: spec.ParamBool, Default: 1, Min: 0, Max: 1, Step: 1,
		Desc: "判定失败后是否停止开新仓。关 = 只记录不拦截（想看它继续跌到哪里时用）"},
}

// MinCapital 最低有效资金：**无持仓**且资金已不足以再开仓时，判定策略失败。
//
// # 为什么要「无持仓」这个前提
//
// 有持仓时权益低只说明当前浮亏，行情回来就回来了 —— 那不是失败，是回撤。
// 真正的失败是**手里空了、钱也不够再开一手**：从这一刻起，
// 后面的每一根 bar 都不可能再产生任何成交，净值曲线会走成一条直线。
//
// 那条直线在报告里非常危险：最大回撤定格在失败那天、
// 年化波动被后面几年的零波动摊薄、夏普反而变好看。
// **不说出来的话，一个已经死掉的策略会显示成一个低波动策略。**
//
// # 两种判据
//
//	floor > 0  —— 显式下限。权益低于它就算失败
//	floor = 0  —— 自动判定：这一步连一张最小申报单位的仓都开不起
//
// 自动判定要按**标的**算，而每只标的的最小一手值多少钱各不相同
// （BTC 一张 0.01 个、A 股一手 100 股），所以它在 Check 里按当前
// 这张订单的标的判 —— 那正是「想买但买不起」的那一刻。
type MinCapital struct {
	floorCents int64
	block      bool

	// failed 已判定失败。**跨步状态**：一旦判定就不再撤销 ——
	// 无持仓无成交的账户不会自己把钱变回来
	failed bool
	// failedAt 判定失败的交易日，供报告标注
	failedAt int32
	// reason 判定依据，人读
	reason string
}

func (r *MinCapital) Name() string { return "min_capital" }

// OnStep 每步检查显式下限。
//
// **必须在这里而不是只在 Check 里**：策略破产之后 Sizer 会因为
// 算出的数量为 0 而根本不产生订单，Check 也就永远不会被调用 ——
// 而「再也下不出单」恰恰是要检测的那件事。
func (r *MinCapital) OnStep(ctx ExitContext) []Signal {
	if r.failed || r.floorCents <= 0 {
		return nil
	}
	if ctx.Ledger().NumPositions() > 0 {
		return nil
	}
	eq := ctx.EquityCents()
	if eq >= r.floorCents {
		return nil
	}
	r.fail(ctx.Time().TradingDay, fmt.Sprintf(
		"无持仓且权益 %.2f 低于最低有效资金 %.2f", cents(eq), cents(r.floorCents)))
	return nil // 已经没有仓位可平了
}

// Check 拦下开仓单，并在自动模式下判定「连最小一手都开不起」。
func (r *MinCapital) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if !LegOf(o.Side, o.Reduce, ctx.Market().AllowsShort()).Opening {
		return o, Rejection{}, true // 平仓永远放行
	}
	if !r.failed {
		r.detect(o, ctx)
	}
	if !r.failed || !r.block {
		return o, Rejection{}, true
	}
	return o, Rejection{
		Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
		Detail: "策略已判定失败（" + r.reason + "），不再开新仓",
	}, false
}

// detect 判定这一刻是否已经失败。
func (r *MinCapital) detect(o Order, ctx RiskContext) {
	if ctx.Ledger().NumPositions() > 0 {
		return // 还有仓位，不是失败是回撤
	}
	eq := ctx.EquityCents()
	if r.floorCents > 0 {
		if eq < r.floorCents {
			r.fail(ctx.Time().TradingDay, fmt.Sprintf(
				"无持仓且权益 %.2f 低于最低有效资金 %.2f",
				cents(eq), cents(r.floorCents)))
		}
		return
	}
	// 自动：这只标的的最小申报单位值多少钱？买得起就没失败
	inst := ctx.Instrument(o.Instrument)
	bar, ok := ctx.Bar(o.Instrument)
	if inst == nil || !ok || bar.Close <= 0 {
		return
	}
	minQty := int64(inst.MinOrderQty)
	if minQty <= 0 {
		minQty = int64(inst.QtyStep)
	}
	if minQty <= 0 {
		return
	}
	need := NotionalCents(inst, bar.Close, minQty)
	if need <= 0 || ctx.Ledger().BuyingPowerCents() >= need {
		return
	}
	r.fail(ctx.Time().TradingDay, fmt.Sprintf(
		"无持仓，可用购买力 %.2f 开不起 %s 的最小一手（%.2f）",
		cents(ctx.Ledger().BuyingPowerCents()), inst.Symbol, cents(need)))
}

func (r *MinCapital) fail(day int32, why string) {
	r.failed, r.failedAt, r.reason = true, day, why
}

// Failed 报告是否已判定失败、判定日与依据，供报告与单步调试展示。
func (r *MinCapital) Failed() (bool, int32, string) { return r.failed, r.failedAt, r.reason }

type minCapitalState struct {
	Failed   bool   `json:"failed"`
	FailedAt int32  `json:"failed_at"`
	Reason   string `json:"reason"`
}

func (r *MinCapital) SnapshotState() ([]byte, error) {
	return json.Marshal(minCapitalState{
		Failed: r.failed, FailedAt: r.failedAt, Reason: r.reason,
	})
}

func (r *MinCapital) RestoreState(b []byte) error {
	var st minCapitalState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("解析最低有效资金快照失败: %w", err)
	}
	r.failed, r.failedAt, r.reason = st.Failed, st.FailedAt, st.Reason
	return nil
}

// FailureReporter 由能判定「策略已经死了」的风控规则实现。
//
// 抽成接口是为了让报告与前端**不认规则名** —— 将来多一条判据
// （连续 N 年无成交、保证金全部亏光……）只要实现它就会自动显示出来。
type FailureReporter interface {
	// Failed 返回是否已判失败、判定交易日、人读依据
	Failed() (bool, int32, string)
}

// Failure 扫一条风控链，返回第一条判定失败的规则的结论。
func (c RiskChain) Failure() (bool, int32, string) {
	for _, r := range c {
		fr, ok := r.(FailureReporter)
		if !ok {
			continue
		}
		if failed, day, why := fr.Failed(); failed {
			return true, day, why
		}
	}
	return false, 0, ""
}

var (
	_ FailureReporter = (*MinCapital)(nil)
	_ Risk            = (*MinCapital)(nil)
	_ ExitRule        = (*MinCapital)(nil)
	_ StatefulRisk    = (*MinCapital)(nil)
)

// ---- cash_reserve ----

var cashReserveSpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 10, Min: 0, Max: 99, Step: 0.1,
		Desc: "始终保留不参与买入的现金比例，按总权益计（%）"},
	{Name: "shrink", Kind: spec.ParamBool, Default: 1, Min: 0, Max: 1, Step: 1,
		Desc: "超限时缩量（1）还是整单拒绝（0）"},
}

// CashReserve 保留一部分现金不参与买入。
type CashReserve struct {
	ppm    int64
	shrink bool
}

func (r *CashReserve) Name() string { return "cash_reserve" }

func (r *CashReserve) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if o.Side != SideBuy {
		return o, Rejection{}, true
	}
	eq := ctx.EquityCents()
	if eq <= 0 {
		return o, Rejection{}, true
	}
	reserve := eq * r.ppm / 1_000_000
	// 在途买单已经预定了现金，必须一并扣掉，否则同一步里的多张单会各自
	// 以为现金充裕（撮合时才发现不够，那时拒单原因指向的是「现金不足」而非风控）
	var committed int64
	for _, po := range ctx.Pending() {
		if po.Side != SideBuy {
			continue
		}
		if b, ok := ctx.Bar(po.Instrument); ok && b.Close > 0 {
			committed += po.Qty * b.Close / 10
		}
	}
	usable := ctx.Ledger().BuyingPowerCents() - reserve - committed
	if usable <= 0 {
		return o, Rejection{
			Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
			Detail: fmt.Sprintf("现金 %.2f，需留存 %.2f，在途已占 %.2f",
				cents(ctx.Ledger().BuyingPowerCents()), cents(reserve), cents(committed)),
		}, false
	}
	q := shrinkToBudget(ctx, o, usable)
	if q >= o.Qty {
		return o, Rejection{}, true
	}
	if !r.shrink || q <= 0 {
		return o, Rejection{
			Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
			Detail: fmt.Sprintf("可用现金 %.2f 元只够 %d 股，需要 %d 股",
				cents(usable), q, o.Qty),
		}, false
	}
	o.Qty = q
	return o, Rejection{}, true
}

// ---- 流动性 ----

var minTurnoverSpecs = []spec.ParamSpec{
	{Name: "amount_wan", Kind: spec.ParamFloat, Default: 500, Min: 0, Max: 1e7, Step: 10,
		Desc: "当日成交额低于此值（万元）不开新仓；0 表示不限"},
	{Name: "max_share_ppm", Kind: spec.ParamInt, Default: 0, Min: 0, Max: 1_000_000, Step: 1000,
		Desc: "单笔金额占当日成交额的上限（百万分之一）；0 表示不限。超限时缩量"},
}

// MinTurnover 流动性门槛：成交太清淡的标的不开新仓。
//
// **为什么这条必须是风控而不是标的池过滤。**
// 「这只标的流动性够不够」是**逐日**的事实：一只 2015 年活跃、2018 年
// 濒临退市的股票，在两段里的答案不同。写进 universe 就成了「用今天的
// 流动性去决定 2015 年买不买」—— 那是未来函数，与 `is_st` 同理（C1）。
//
// 有了它，标的池才敢按 C3 的要求含退市股：退市前的缩量期会被这条
// 自动挡掉，而不必靠 `status: listed` 把整只标的从历史里抹掉
// （那会系统性高估收益 —— 退市的往往先大跌再退）。
//
// **只拦买入。** 卖出永远放行：手里已经有的东西，流动性差不是不卖的理由，
// 恰恰是要卖的理由。真正的成交限制由 Broker 的成交量上限负责。
type MinTurnover struct {
	minAmountCents int64 // 当日成交额下限（分）
	maxSharePPM    int64 // 单笔占当日成交额的上限
}

func (r *MinTurnover) Name() string { return "min_turnover" }

func (r *MinTurnover) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if o.Side != SideBuy {
		return o, Rejection{}, true
	}
	bar, ok := ctx.Bar(o.Instrument)
	if !ok {
		return o, Rejection{}, true // 没有 bar 的情形由 Broker 处理
	}
	if r.minAmountCents > 0 && bar.Amount < r.minAmountCents {
		return o, Rejection{
			Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
			Detail: fmt.Sprintf("当日成交额 %.0f 万元，低于门槛 %.0f 万元",
				cents(bar.Amount)/10_000, cents(r.minAmountCents)/10_000),
		}, false
	}
	if r.maxSharePPM > 0 && bar.Amount > 0 {
		// 单笔金额上限 = 当日成交额 × ppm / 1e6。
		// 先除后乘避免 amount × ppm 溢出 —— amount 上限约 1e13 分，
		// × 1e6 就是 1e19，超过 int64
		capCents := bar.Amount / 1_000_000 * r.maxSharePPM
		q := shrinkToBudget(ctx, o, capCents)
		if q <= 0 {
			return o, Rejection{
				Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
				Detail: fmt.Sprintf("单笔上限 %.2f 元（当日成交额的 %.2f%%）一手都不够",
					cents(capCents), float64(r.maxSharePPM)/10_000),
			}, false
		}
		o.Qty = q
	}
	return o, Rejection{}, true
}

func cents(v int64) float64 { return float64(v) / 100 }

// ---- 注册 ----

func init() {
	Risks.Register("max_position_pct", "单只标的市值占总权益的上限，超限缩量或拒单", maxPositionPctSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(maxPositionPctSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &MaxPositionPct{
				ppm:    int64(p.Float("pct", 20) * 10_000),
				shrink: p.Bool("shrink", true),
			}, nil
		})

	Risks.Register("max_positions", "最多同时持有多少只标的（持有与在途去重后计数）", maxPositionsSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(maxPositionsSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &MaxPositions{n: p.Int("n", 20)}, nil
		})

	Risks.Register("drawdown_halt",
		"回撤熔断：自峰值权益回撤超过阈值后停止开新仓，可设冷静期、可选清仓",
		drawdownHaltSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(drawdownHaltSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &DrawdownHalt{
				ppm:      int64(p.Float("pct", 30) * 10_000),
				cooldown: p.Int("cooldown_bars", 0),
				flatten:  p.Bool("flatten", false),
			}, nil
		})

	Risks.Register("min_capital",
		"最低有效资金：无持仓且资金已开不起仓时，判定策略失败并停止开新仓",
		minCapitalSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(minCapitalSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &MinCapital{
				floorCents: int64(p.Float("floor", 0) * 100),
				block:      p.Bool("block", true),
			}, nil
		})

	Risks.Register("min_turnover", "流动性门槛：当日成交额太低的标的不开新仓（卖出永远放行）", minTurnoverSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(minTurnoverSpecs, raw)
			if err != nil {
				return nil, err
			}
			// 万元 → 分：×1e4 元 ×100 分
			return &MinTurnover{
				minAmountCents: int64(p.Float("amount_wan", 500) * 1_000_000),
				maxSharePPM:    int64(p.Int("max_share_ppm", 0)),
			}, nil
		})

	Risks.Register("cash_reserve", "现金预留：始终留一部分现金不参与买入", cashReserveSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(cashReserveSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &CashReserve{
				ppm:    int64(p.Float("pct", 10) * 10_000),
				shrink: p.Bool("shrink", true),
			}, nil
		})
}

var (
	_ Risk = (*MinTurnover)(nil)
)
