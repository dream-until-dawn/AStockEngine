package trading

import (
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

// Portfolio 是账本。
type Portfolio struct {
	// InitialCash 初始资金（分），用于计算总收益
	InitialCash int64
	Cash        int64
	Positions   map[mktdata.InstrumentID]*Position

	// RealizedCents 累计已实现盈亏（分）
	RealizedCents int64
	// FeeCents 累计费用（分），按 kind 拆分便于归因
	FeeCents map[string]int64

	// 公司行动的告警。有因子事件却无 corporate_action 记录时按因子推算入账，
	// 属有损近似，必须留痕（设计 4.3）。
	Warnings []string
}

// NewPortfolio 创建账本。initialCents 为初始资金（分）。
func NewPortfolio(initialCents int64) *Portfolio {
	return &Portfolio{
		InitialCash: initialCents,
		Cash:        initialCents,
		Positions:   make(map[mktdata.InstrumentID]*Position, 64),
		FeeCents:    make(map[string]int64, 4),
	}
}

// Position 返回持仓，不存在时返回 nil。
func (pf *Portfolio) Position(id mktdata.InstrumentID) *Position { return pf.Positions[id] }

// Available 返回可卖数量。
func (pf *Portfolio) Available(id mktdata.InstrumentID, nowMs int64) int64 {
	if p := pf.Positions[id]; p != nil {
		return p.Available(nowMs)
	}
	return 0
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
		pf.FeeCents[k] += v
	}

	p := pf.Positions[f.Instrument]
	if p == nil {
		p = &Position{}
		pf.Positions[f.Instrument] = p
	}

	if f.Side == SideBuy {
		cost := amount + f.Fee.Total
		if cost > pf.Cash {
			return fmt.Errorf("现金不足：需 %d 分，有 %d 分", cost, pf.Cash)
		}
		pf.Cash -= cost
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
	proceeds := amount - f.Fee.Total
	pf.Cash += proceeds
	pf.RealizedCents += proceeds - costOut
	p.CostCents -= costOut
	p.Total -= f.Qty
	pf.consumeLots(p, f.Qty)
	if p.Total == 0 {
		// 清仓后残留的成本是取整误差，一并计入已实现盈亏，避免账目漂移
		pf.RealizedCents -= p.CostCents
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
	p := pf.Positions[ca.Instrument]
	if p == nil || p.Total <= 0 {
		return
	}

	// 现金分红：每股税前红利 scale 1e6（元）→ 分需再 ×100，即 /1e4
	if ca.CashBeforeTax > 0 {
		gross := p.Total * ca.CashBeforeTax / 10_000
		tax := gross * taxPPM / 1_000_000
		pf.Cash += gross - tax
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
		pf.Warnings = append(pf.Warnings,
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
	p := pf.Positions[id]
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
	pf.Warnings = append(pf.Warnings, fmt.Sprintf(
		"标的 %d 于 %d 有因子事件但无分红送配记录，按因子比例 %.6f 推算送转 %d 股（有损近似）",
		id, exDate, factorRatio, extra))
}

// MarketValueCents 按给定价格计算持仓市值（分）。
func (pf *Portfolio) MarketValueCents(prices map[mktdata.InstrumentID]int64) int64 {
	var v int64
	for id, p := range pf.Positions {
		if p.Total <= 0 {
			continue
		}
		v += AmountCents(prices[id], p.Total)
	}
	return v
}

// EquityCents 返回总权益 = 现金 + 持仓市值。
func (pf *Portfolio) EquityCents(prices map[mktdata.InstrumentID]int64) int64 {
	return pf.Cash + pf.MarketValueCents(prices)
}

// TotalFeeCents 返回累计费用。
func (pf *Portfolio) TotalFeeCents() int64 {
	var v int64
	for _, c := range pf.FeeCents {
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
	Positions     map[mktdata.InstrumentID]*Position `json:"positions"`
	Warnings      []string                           `json:"warnings,omitempty"`
}

func (pf *Portfolio) Snapshot() PortfolioState {
	ps := make(map[mktdata.InstrumentID]*Position, len(pf.Positions))
	for id, p := range pf.Positions {
		cp := *p
		cp.Lots = append([]lot(nil), p.Lots...)
		ps[id] = &cp
	}
	fees := make(map[string]int64, len(pf.FeeCents))
	for k, v := range pf.FeeCents {
		fees[k] = v
	}
	return PortfolioState{
		InitialCash: pf.InitialCash, Cash: pf.Cash,
		RealizedCents: pf.RealizedCents, FeeCents: fees, Positions: ps,
		Warnings: append([]string(nil), pf.Warnings...),
	}
}

func (pf *Portfolio) Restore(s PortfolioState) {
	pf.InitialCash, pf.Cash, pf.RealizedCents = s.InitialCash, s.Cash, s.RealizedCents
	pf.FeeCents = make(map[string]int64, len(s.FeeCents))
	for k, v := range s.FeeCents {
		pf.FeeCents[k] = v
	}
	pf.Positions = make(map[mktdata.InstrumentID]*Position, len(s.Positions))
	for id, p := range s.Positions {
		cp := *p
		cp.Lots = append([]lot(nil), p.Lots...)
		pf.Positions[id] = &cp
	}
	pf.Warnings = append([]string(nil), s.Warnings...)
}
