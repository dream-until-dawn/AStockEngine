package trading

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// 账本全程使用定点整数，**禁止浮点**（C5）：
//
//	现金 / 成本 / 权益   分（1e-2 元）
//	持仓                 股 / 份
//	价格                 与 bar 同 scale（A 股为厘，1e-3 元）
//
// 并发海选下浮点累加顺序不同会导致结果不同，「同配置两次运行逐笔一致」即破。

// lot 是一笔未解冻的买入批次。
//
// 用批次队列而非「今日买入量」计数器，是为了兼容多市场：
// A 股 T+1、跨境/黄金 ETF T+0、加密即时可卖，同一套结构都能表达。
// 批次通常只有 1~2 个，内存代价可忽略。
type lot struct {
	Qty          int64 `json:"qty"`
	SellableFrom int64 `json:"sellable_from"` // UTC 毫秒
}

// Position 是单个标的的持仓。
type Position struct {
	Total int64 `json:"total"` // 总持仓
	// CostCents 是累计买入成本，**含买入时的费用**。
	// 均价由它除以 Total 得出，故无需单独维护均价字段，也就不会出现两者不一致。
	CostCents int64 `json:"cost_cents"`
	Lots      []lot `json:"lots"`
}

// Available 返回在给定时刻可卖出的数量。
func (p *Position) Available(nowMs int64) int64 {
	var n int64
	for _, l := range p.Lots {
		if l.SellableFrom <= nowMs {
			n += l.Qty
		}
	}
	if n > p.Total {
		n = p.Total
	}
	return n
}

// AvgCostCents 返回每股平均成本（分）。向下取整 —— 保守，避免虚增成本。
func (p *Position) AvgCostCents() int64 {
	if p.Total <= 0 {
		return 0
	}
	return p.CostCents / p.Total
}

// Portfolio 是**现货**账本 —— Ledger 接口的唯一实现（A 股）。
//
// 字段全部私有：账本一旦能被外部直接改写，「账目自洽」就不再是一条能保证的性质。
// 从 v0.3 起读账户状态一律经方法（见 Ledger 接口）。
type Portfolio struct {
	initialCash int64
	cash        int64
	positions   map[mktdata.InstrumentID]*Position

	realized int64
	// feeCents 累计费用（分），按 kind 拆分便于归因。
	// **只含付给第三方的真金白银**（佣金 / 印花税 / 过户费），
	// 这样它才能与券商对账单对得上。
	feeCents map[string]int64
	// slippage 累计滑点（分）。它是执行质量的损耗，不是费用，
	// 故与 feeCents 分开记 —— 混在一起会让上面那句对账保证失效。
	slippage int64

	// 公司行动的告警。有因子事件却无 corporate_action 记录时按因子推算入账，
	// 属有损近似，必须留痕（设计 4.3）。
	warnings []string
}

// NewPortfolio 创建现货账本。initialCents 为初始资金（分）。
func NewPortfolio(initialCents int64) *Portfolio {
	return &Portfolio{
		initialCash: initialCents,
		cash:        initialCents,
		positions:   make(map[mktdata.InstrumentID]*Position, 64),
		feeCents:    make(map[string]int64, 4),
	}
}

func (pf *Portfolio) Name() string { return "spot" }

// InitialCashCents 初始资金。
func (pf *Portfolio) InitialCashCents() int64 { return pf.initialCash }

// CashCents 账面现金。
func (pf *Portfolio) CashCents() int64 { return pf.cash }

// BuyingPowerCents 现货账户的可用资金就是现金 —— 没有杠杆，没有可用保证金。
func (pf *Portfolio) BuyingPowerCents() int64 { return pf.cash }

// RealizedCents 累计已实现盈亏。
func (pf *Portfolio) RealizedCents() int64 { return pf.realized }

// FeeCents 返回累计费用的**拷贝**，避免外部改动账本。
func (pf *Portfolio) FeeCents() map[string]int64 {
	out := make(map[string]int64, len(pf.feeCents))
	for k, v := range pf.feeCents {
		out[k] = v
	}
	return out
}

// SlippageCents 累计滑点。
func (pf *Portfolio) SlippageCents() int64 { return pf.slippage }

// Warnings 返回告警的拷贝。
func (pf *Portfolio) Warnings() []string {
	return append([]string(nil), pf.warnings...)
}

// Exposure 单标的敞口。现货账户 Short 恒为 0。
func (pf *Portfolio) Exposure(id mktdata.InstrumentID) Exposure {
	p := pf.positions[id]
	if p == nil {
		return Exposure{}
	}
	return Exposure{Long: p.Total, LongCost: p.CostCents}
}

// EachExposure 遍历非空敞口。
//
// **按 ID 升序遍历** —— map 的遍历顺序是随机的，
// 而调用方（Sizer 的槽位计数、指纹的持仓块）需要确定的顺序（C5）。
func (pf *Portfolio) EachExposure(fn func(id mktdata.InstrumentID, e Exposure) bool) {
	ids := make([]mktdata.InstrumentID, 0, len(pf.positions))
	for id, p := range pf.positions {
		if p.Total > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		p := pf.positions[id]
		if !fn(id, Exposure{Long: p.Total, LongCost: p.CostCents}) {
			return
		}
	}
}

// NumPositions 有非空敞口的标的数。
func (pf *Portfolio) NumPositions() int {
	n := 0
	for _, p := range pf.positions {
		if p.Total > 0 {
			n++
		}
	}
	return n
}

// Available 返回可卖数量。
func (pf *Portfolio) Available(id mktdata.InstrumentID, nowMs int64) int64 {
	if p := pf.positions[id]; p != nil {
		return p.Available(nowMs)
	}
	return 0
}

// CanFill 在记账之前判断这笔成交能否落地。
//
// 撮合器问它而不是自己看现金 —— 「买得起吗」的答案取决于账户类型，
// Broker 不该知道现货与保证金的区别。
func (pf *Portfolio) CanFill(f Fill) (RejectReason, string, bool) {
	if f.Side == SideBuy {
		need := AmountCents(f.Price, f.Qty) + f.Fee.Total + f.SlippageCents
		if need > pf.BuyingPowerCents() {
			return RejectInsufficientCash, fmt.Sprintf("需 %.2f 元，可用 %.2f 元",
				float64(need)/100, float64(pf.BuyingPowerCents())/100), false
		}
		return RejectNone, "", true
	}
	// 现货卖出不能超过持仓 —— **这一条正是现货账本的定义性约束**，
	// 保证金账本在这里允许开空
	held := pf.Exposure(f.Instrument).Long
	if f.Qty > held {
		return RejectInsufficientPosition,
			fmt.Sprintf("申报卖出 %d，持仓 %d", f.Qty, held), false
	}
	return RejectNone, "", true
}

// Mark 现货账户不做逐步重估，也没有强平。
//
// 留这个方法是为了让保证金账本有地方判断维持保证金率 ——
// 强平是市场施加的，必须发生在策略之前，而不是被策略「决定」。
func (pf *Portfolio) Mark(map[mktdata.InstrumentID]int64, mktdata.TimePoint) []Liquidation {
	return nil
}

// AmountCents 由定点价格与数量算出成交额（分）。
//
// 价格以厘计（1e-3 元），1 分 = 10 厘，故 amount = price×qty/10，四舍五入。
// A 股股票最小报价单位是分（10 厘），故通常整除；ETF 报价到厘，
// 但最小申报 100 份，乘积同样整除。留下四舍五入是为了远期的小数数量市场。
func AmountCents(priceLi, qty int64) int64 {
	n := priceLi * qty
	if n >= 0 {
		return (n + 5) / 10
	}
	return -((-n + 5) / 10)
}

// ApplyFill 把一笔成交计入账本。
func (pf *Portfolio) ApplyFill(f Fill, sellableFrom int64) error {
	amount := AmountCents(f.Price, f.Qty)
	for k, v := range f.Fee.Items {
		pf.feeCents[k] += v
	}
	pf.slippage += f.SlippageCents
	// 摩擦成本：费用 + 滑点。两者对现金的作用完全一致，
	// 分开记只是为了归因，不是为了区别对待
	friction := f.Fee.Total + f.SlippageCents

	p := pf.positions[f.Instrument]
	if p == nil {
		p = &Position{}
		pf.positions[f.Instrument] = p
	}

	if f.Side == SideBuy {
		cost := amount + friction
		if cost > pf.cash {
			return fmt.Errorf("现金不足：需 %d 分，有 %d 分", cost, pf.cash)
		}
		pf.cash -= cost
		p.Total += f.Qty
		p.CostCents += cost
		p.Lots = append(p.Lots, lot{Qty: f.Qty, SellableFrom: sellableFrom})
		return nil
	}

	// 卖出
	if f.Qty > p.Total {
		return fmt.Errorf("卖出数量 %d 超过持仓 %d", f.Qty, p.Total)
	}
	// 成本按比例结转，保持均价不变；向下取整（保守）
	costOut := p.CostCents * f.Qty / p.Total
	proceeds := amount - friction
	pf.cash += proceeds
	pf.realized += proceeds - costOut
	p.CostCents -= costOut
	p.Total -= f.Qty
	pf.consumeLots(p, f.Qty)
	if p.Total == 0 {
		// 清仓后残留的成本是取整误差，一并计入已实现盈亏，避免账目漂移
		pf.realized -= p.CostCents
		p.CostCents = 0
		p.Lots = nil
	}
	return nil
}

// consumeLots 按**先可卖先出**消耗批次。
//
// 不是简单的 FIFO：卖出只能动用已解冻的批次，故先按可卖时刻排序。
// 这样当日买入的批次不会被误消耗掉。
func (pf *Portfolio) consumeLots(p *Position, qty int64) {
	sort.SliceStable(p.Lots, func(i, j int) bool {
		return p.Lots[i].SellableFrom < p.Lots[j].SellableFrom
	})
	rest := qty
	out := p.Lots[:0]
	for _, l := range p.Lots {
		if rest <= 0 {
			out = append(out, l)
			continue
		}
		if l.Qty <= rest {
			rest -= l.Qty
			continue
		}
		l.Qty -= rest
		rest = 0
		out = append(out, l)
	}
	p.Lots = out
}

// CorporateAction 是除权日的公司行动，字段与 SCHEMA.md 第 5 节一致。
type CorporateAction struct {
	Instrument mktdata.InstrumentID
	ExDate     int32
	// 均为**每股**值，定点 scale 1e6
	CashBeforeTax int64
	StockDividend int64
	StockTransfer int64
	RightsRatio   int64
	RightsPrice   int64 // 配股价，定点，与价格同 scale
}

// DividendTaxPPM 红利税率（百万分之一）。
//
// **税率属规则不属数据**（SCHEMA.md 5.3）：实际随持股期限分档
// （持股超 1 年免征、1 个月至 1 年 10%、1 个月以内 20%）。
// 本刀先用固定税率，分档留待后续 —— 分档需要逐笔持股期限，
// 而现有的批次队列已经记录了买入时刻，具备实现条件。
type DividendTaxPPM int64

// ApplyCorporateAction 把公司行动计入账本。
//
// **只调价格不调账户是回测的常见错误，会系统性低估收益**（C2）。
//
// 配股默认**不参与**（评审决议 1）：最保守，且不参与才是散户常态。
// 参与需要策略显式表态，那是后续版本的事。
func (pf *Portfolio) ApplyCorporateAction(ca CorporateAction, taxPPM int64,
	nowMs int64) {
	p := pf.positions[ca.Instrument]
	if p == nil || p.Total <= 0 {
		return
	}

	// 现金分红：每股税前红利 scale 1e6（元）→ 分需再 ×100，即 /1e4
	if ca.CashBeforeTax > 0 {
		gross := p.Total * ca.CashBeforeTax / 10_000
		tax := gross * taxPPM / 1_000_000
		pf.cash += gross - tax
	}

	// 送股 + 转增：持仓按比例增加，**总成本不变**（均价因此自然摊薄）
	add := ca.StockDividend + ca.StockTransfer
	if add > 0 {
		extra := p.Total * add / 1_000_000
		if extra > 0 {
			p.Total += extra
			// 送转股按比例分摊到各批次，保持可卖时刻的对应关系
			for i := range p.Lots {
				p.Lots[i].Qty += p.Lots[i].Qty * add / 1_000_000
			}
			// 分摊后的取整残差补到最后一个批次，保证批次总量等于持仓
			pf.reconcileLots(p, nowMs)
		}
	}
}

// reconcileLots 让批次总量与持仓总量一致。
//
// 送转按比例分摊必然产生取整残差，若不校正，可卖数量会与实际持仓脱节 ——
// 那会在后续卖出时表现为「明明有仓位却卖不出」这种极难排查的现象。
func (pf *Portfolio) reconcileLots(p *Position, nowMs int64) {
	var sum int64
	for _, l := range p.Lots {
		sum += l.Qty
	}
	diff := p.Total - sum
	if diff == 0 {
		return
	}
	if len(p.Lots) == 0 {
		p.Lots = append(p.Lots, lot{Qty: diff, SellableFrom: nowMs})
		return
	}
	p.Lots[len(p.Lots)-1].Qty += diff
	if p.Lots[len(p.Lots)-1].Qty < 0 {
		// 理论上不会发生；真发生了说明有更严重的账目错误，留痕而非掩盖
		pf.warnings = append(pf.warnings,
			fmt.Sprintf("批次校正后出现负数量，持仓 %d，批次和 %d", p.Total, sum))
		p.Lots[len(p.Lots)-1].Qty = 0
	}
}

// ApplyImpliedSplit 处理「有复权因子事件但无 corporate_action 记录」的情形。
//
// ETL 侧约有 6,770 个因子事件缺少分红送配记录（ETL.md 6.11），
// 其中 1,270 个是 2005-2007 股改对价送股。若完全不处理，
// 这些日期会出现「价格跳变但账户没变」的失真。
//
// 按因子比例反推送转并入账（评审决议 2）。这是**有损近似** ——
// 分不清是送转还是分红，一律当作送转 —— 但优于完全不处理，且必须留痕。
func (pf *Portfolio) ApplyImpliedSplit(id mktdata.InstrumentID, exDate int32,
	factorRatio float64, nowMs int64) {
	p := pf.positions[id]
	if p == nil || p.Total <= 0 || factorRatio <= 1.0 {
		return
	}
	// factorRatio = 因子(当日)/因子(前一事件)，即每股变成 ratio 股
	newTotal := int64(float64(p.Total)*factorRatio + 0.5)
	if newTotal <= p.Total {
		return
	}
	extra := newTotal - p.Total
	for i := range p.Lots {
		p.Lots[i].Qty = int64(float64(p.Lots[i].Qty)*factorRatio + 0.5)
	}
	p.Total = newTotal
	pf.reconcileLots(p, nowMs)
	pf.warnings = append(pf.warnings, fmt.Sprintf(
		"标的 %d 于 %d 有因子事件但无分红送配记录，按因子比例 %.6f 推算送转 %d 股（有损近似）",
		id, exDate, factorRatio, extra))
}

// MarketValueCents 按给定价格计算持仓市值（分）。
func (pf *Portfolio) MarketValueCents(prices map[mktdata.InstrumentID]int64) int64 {
	var v int64
	for id, p := range pf.positions {
		if p.Total <= 0 {
			continue
		}
		v += AmountCents(prices[id], p.Total)
	}
	return v
}

// EquityCents 返回总权益 = 现金 + 持仓市值。
func (pf *Portfolio) EquityCents(prices map[mktdata.InstrumentID]int64) int64 {
	return pf.cash + pf.MarketValueCents(prices)
}

// TotalFeeCents 返回累计费用。
func (pf *Portfolio) TotalFeeCents() int64 {
	var v int64
	for _, c := range pf.feeCents {
		v += c
	}
	return v
}

// PortfolioState 是账本的可序列化状态（C6）。
type PortfolioState struct {
	InitialCash   int64                              `json:"initial_cash"`
	Cash          int64                              `json:"cash"`
	RealizedCents int64                              `json:"realized_cents"`
	FeeCents      map[string]int64                   `json:"fee_cents"`
	SlippageCents int64                              `json:"slippage_cents"`
	Positions     map[mktdata.InstrumentID]*Position `json:"positions"`
	Warnings      []string                           `json:"warnings,omitempty"`
}

func (pf *Portfolio) Snapshot() PortfolioState {
	ps := make(map[mktdata.InstrumentID]*Position, len(pf.positions))
	for id, p := range pf.positions {
		cp := *p
		cp.Lots = append([]lot(nil), p.Lots...)
		ps[id] = &cp
	}
	fees := make(map[string]int64, len(pf.feeCents))
	for k, v := range pf.feeCents {
		fees[k] = v
	}
	return PortfolioState{
		InitialCash: pf.initialCash, Cash: pf.cash,
		RealizedCents: pf.realized, FeeCents: fees,
		SlippageCents: pf.slippage, Positions: ps,
		Warnings: append([]string(nil), pf.warnings...),
	}
}

func (pf *Portfolio) Restore(s PortfolioState) {
	pf.initialCash, pf.cash, pf.realized = s.InitialCash, s.Cash, s.RealizedCents
	pf.slippage = s.SlippageCents
	pf.feeCents = make(map[string]int64, len(s.FeeCents))
	for k, v := range s.FeeCents {
		pf.feeCents[k] = v
	}
	pf.positions = make(map[mktdata.InstrumentID]*Position, len(s.Positions))
	for id, p := range s.Positions {
		cp := *p
		cp.Lots = append([]lot(nil), p.Lots...)
		pf.positions[id] = &cp
	}
	pf.warnings = append([]string(nil), s.Warnings...)
}

// ---- Ledger 的快照实现 ----
//
// 用 JSON 而不是让引擎直接持有 PortfolioState：换一个账本实现（保证金账户）
// 时，它的状态形状与现货完全不同，引擎不该知道那是什么。

// SnapshotLedger 序列化账本状态。
func (pf *Portfolio) SnapshotLedger() ([]byte, error) {
	return json.Marshal(pf.Snapshot())
}

// RestoreLedger 从快照恢复账本状态。
func (pf *Portfolio) RestoreLedger(b []byte) error {
	var st PortfolioState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("解析账本快照失败: %w", err)
	}
	pf.Restore(st)
	return nil
}
