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
type RiskChain []Risk

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
	maxQty := budgetCents * 10 / bar.Close
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
		Desc: "自峰值权益回撤超过此比例后停止开新仓（%）"},
}

// DrawdownHalt 回撤熔断：只禁买、不强平。
//
// **不强平是刻意的。** 熔断规则若自己发卖单，就等于在策略之外新增了一条
// 交易逻辑，回测结果将无法归因到策略本身。风控的职责是拦截，不是决策。
type DrawdownHalt struct{ ppm int64 }

func (r *DrawdownHalt) Name() string { return "drawdown_halt" }

func (r *DrawdownHalt) Check(o Order, ctx RiskContext) (Order, Rejection, bool) {
	if o.Side != SideBuy {
		return o, Rejection{}, true
	}
	peak := ctx.PeakEquityCents()
	if peak <= 0 {
		return o, Rejection{}, true
	}
	eq := ctx.EquityCents()
	ddPPM := (peak - eq) * 1_000_000 / peak
	if ddPPM < r.ppm {
		return o, Rejection{}, true
	}
	return o, Rejection{
		Order: o, At: ctx.Time(), Reason: RejectRisk, Rule: r.Name(),
		Detail: fmt.Sprintf("当前回撤 %.2f%% 已达熔断线 %.2f%%（峰值 %.2f，现 %.2f）",
			float64(ddPPM)/10_000, float64(r.ppm)/10_000, cents(peak), cents(eq)),
	}, false
}

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

func cents(v int64) float64 { return float64(v) / 100 }

// ---- 注册 ----

func init() {
	Risks.Register("max_position_pct", maxPositionPctSpecs,
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

	Risks.Register("max_positions", maxPositionsSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(maxPositionsSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &MaxPositions{n: p.Int("n", 20)}, nil
		})

	Risks.Register("drawdown_halt", drawdownHaltSpecs,
		func(raw json.RawMessage) (Risk, error) {
			p, err := registry.DecodeParams(drawdownHaltSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &DrawdownHalt{ppm: int64(p.Float("pct", 30) * 10_000)}, nil
		})

	Risks.Register("cash_reserve", cashReserveSpecs,
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
