package trading

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// MarginLedger 是**逐仓 + 双向**的保证金账本，用于加密永续合约。
//
// # 逐仓（isolated）
//
// 每个仓位有自己的一份保证金，**爆仓只吃掉那一份**，不牵连余额与其他仓位。
// 全仓（cross）是另一套：全部权益共同兜底，一个仓位可以把账户拖到零。
// 这里固定为逐仓。
//
// # 双向（hedge）
//
// 同一标的可以**同时**持有多头与空头，各自独立记账、独立爆仓。
// 所以每个标的有两个槽位，而不是一个带正负号的净头寸。
//
// 单向净持仓（one-way）下「买 100 张」在已有 100 张空单时会变成平仓，
// 双向下它就是开多 —— 意图完全不同。这个区别由 `Order.Reduce` 表达。
//
// # 与现货 Portfolio 的关系
//
// 两者都实现 `Ledger`，引擎不知道区别。这正是 v0.3.2 把账本抽成接口的
// 目的：`Exposure` 从一开始就有 `Short` 与 `ShortCost`，
// `Mark()` 从一开始就返回 `[]Liquidation` —— 那时现货实现里它们恒为空，
// 但**签名是按保证金账户设计的**，所以这里不用改接口。
type MarginLedger struct {
	initialCash int64
	// cash 可用余额（**未被保证金占用的部分**）
	cash int64
	// long / short 逐仓仓位。同一标的两个方向各一个槽位
	long  map[mktdata.InstrumentID]*MarginPos
	short map[mktdata.InstrumentID]*MarginPos

	// leverage 杠杆倍数。开仓保证金 = 名义额 / leverage
	leverage int64
	// mmrPPM 维持保证金率（百万分之一）。
	// 仓位权益低于 名义额 × mmr 时强平
	mmrPPM int64

	realized int64
	feeCents map[string]int64
	slippage int64

	liquidated []Liquidation
	warnings   []string

	valuer Valuer
}

// MarginPos 一个方向上的逐仓持仓。
type MarginPos struct {
	Qty int64 `json:"qty"` // 张，恒为正
	// EntryCents 开仓名义额合计（分）。均价由它除以 Qty 得出，
	// 故不单独维护均价字段，也就不会出现两者不一致
	EntryCents int64 `json:"entry_cents"`
	// MarginCents 这个仓位占用的保证金（分）。**逐仓的核心**：
	// 它是这个仓位能亏掉的全部，亏光即强平
	MarginCents int64 `json:"margin_cents"`
}

// NewMarginLedger 创建逐仓双向账本。
//
// leverage < 1 时取 1（不加杠杆但仍是逐仓：亏损以保证金为限）。
// mmrPPM <= 0 时取 5000（0.5%），与主流交易所 BTC 永续的量级一致。
func NewMarginLedger(cash, leverage, mmrPPM int64) *MarginLedger {
	if leverage < 1 {
		leverage = 1
	}
	if mmrPPM <= 0 {
		mmrPPM = 5000
	}
	return &MarginLedger{
		initialCash: cash, cash: cash,
		long:     make(map[mktdata.InstrumentID]*MarginPos, 64),
		short:    make(map[mktdata.InstrumentID]*MarginPos, 64),
		leverage: leverage, mmrPPM: mmrPPM,
		feeCents: make(map[string]int64, 4),
	}
}

func (m *MarginLedger) Name() string { return "margin_isolated" }

// SetValuer 注入估值函数。**引擎必须调用** —— 不注入会退回 A 股口径，
// 在加密上差十几个数量级。
func (m *MarginLedger) SetValuer(v Valuer) { m.valuer = v }

func (m *MarginLedger) value(id mktdata.InstrumentID, price, qty int64) int64 {
	if m.valuer == nil {
		return AmountCents(price, qty)
	}
	return m.valuer(id, price, qty)
}

func (m *MarginLedger) InitialCashCents() int64 { return m.initialCash }
func (m *MarginLedger) CashCents() int64        { return m.cash }
func (m *MarginLedger) RealizedCents() int64    { return m.realized }
func (m *MarginLedger) SlippageCents() int64    { return m.slippage }
func (m *MarginLedger) Warnings() []string      { return m.warnings }

// BuyingPowerCents 可用余额 × 杠杆。
//
// **不是权益 × 杠杆**：逐仓下已开仓位的浮盈不能拿去开新仓 ——
// 那是全仓的行为。
func (m *MarginLedger) BuyingPowerCents() int64 {
	if m.cash <= 0 {
		return 0
	}
	return m.cash * m.leverage
}

func (m *MarginLedger) FeeCents() map[string]int64 {
	out := make(map[string]int64, len(m.feeCents))
	for k, v := range m.feeCents {
		out[k] = v
	}
	return out
}

func (m *MarginLedger) TotalFeeCents() int64 {
	var t int64
	for _, v := range m.feeCents {
		t += v
	}
	return t
}

// Exposure 返回两个方向的敞口。
func (m *MarginLedger) Exposure(id mktdata.InstrumentID) Exposure {
	var e Exposure
	if p := m.long[id]; p != nil {
		e.Long, e.LongCost = p.Qty, p.EntryCents
	}
	if p := m.short[id]; p != nil {
		e.Short, e.ShortCost = p.Qty, p.EntryCents
	}
	return e
}

// EachExposure 按 ID 升序遍历有敞口的标的。
//
// **必须定序**：map 遍历顺序随机，而遍历顺序会影响强平的先后，
// 进而影响结果 —— C5 就是在这类地方悄悄失守的。
func (m *MarginLedger) EachExposure(fn func(id mktdata.InstrumentID, e Exposure) bool) {
	ids := m.activeIDs()
	for _, id := range ids {
		if !fn(id, m.Exposure(id)) {
			return
		}
	}
}

func (m *MarginLedger) activeIDs() []mktdata.InstrumentID {
	seen := make(map[mktdata.InstrumentID]bool, len(m.long)+len(m.short))
	for id, p := range m.long {
		if p.Qty > 0 {
			seen[id] = true
		}
	}
	for id, p := range m.short {
		if p.Qty > 0 {
			seen[id] = true
		}
	}
	ids := make([]mktdata.InstrumentID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// NumPositions 有敞口的**标的**数。同一标的多空各一个仓位仍算一个标的 ——
// 「持有多少只」问的是标的数，不是仓位数。
func (m *MarginLedger) NumPositions() int { return len(m.activeIDs()) }

// Available T+0：多头持仓随时可平。
//
// 返回多头数量是为了兼容那些只认多头的调用方（Sizer 的清仓路径、
// 止损规则）。空头的可平数量由 CanFill 按 Reduce 判断。
func (m *MarginLedger) Available(id mktdata.InstrumentID, _ int64) int64 {
	if p := m.long[id]; p != nil {
		return p.Qty
	}
	return 0
}

// EquityCents 权益 = 可用余额 + Σ(仓位保证金 + 未实现盈亏)。
func (m *MarginLedger) EquityCents(marks map[mktdata.InstrumentID]int64) int64 {
	v := m.cash
	for _, id := range m.activeIDs() {
		price := marks[id]
		if p := m.long[id]; p != nil && p.Qty > 0 {
			v += p.MarginCents + m.unrealized(id, p, price, SideBuy)
		}
		if p := m.short[id]; p != nil && p.Qty > 0 {
			v += p.MarginCents + m.unrealized(id, p, price, SideSell)
		}
	}
	return v
}

// unrealized 未实现盈亏（分）。
//
//	多头：现值 − 开仓额
//	空头：开仓额 − 现值
//
// price 为 0（没有报价）时返回 0 —— 拿一个不存在的价去估值，
// 会让权益凭空跳到「全部亏光」。
func (m *MarginLedger) unrealized(
	id mktdata.InstrumentID, p *MarginPos, price int64, side Side,
) int64 {
	if price <= 0 || p.Qty <= 0 {
		return 0
	}
	now := m.value(id, price, p.Qty)
	if side == SideBuy {
		return now - p.EntryCents
	}
	return p.EntryCents - now
}

// ---- 成交 ----

// posFor 取某个订单作用的仓位槽。
//
//	买 + 开 = 开多      买 + 平 = 平空
//	卖 + 开 = 开空      卖 + 平 = 平多
func (m *MarginLedger) posFor(o Order) (side Side, opening bool) {
	leg := LegOf(o.Side, o.Reduce, true) // 保证金账本只用于双向市场
	if leg.Short {
		return SideSell, leg.Opening
	}
	return SideBuy, leg.Opening
}

func (m *MarginLedger) slot(side Side) map[mktdata.InstrumentID]*MarginPos {
	if side == SideBuy {
		return m.long
	}
	return m.short
}

// CanFill 判断一笔成交是否可行。
func (m *MarginLedger) CanFill(f Fill) (RejectReason, string, bool) {
	side, opening := m.posFor(f.Order)
	friction := f.Fee.Total + f.SlippageCents
	if opening {
		margin := m.marginFor(f.AmountCents)
		need := margin + friction
		if need > m.cash {
			return RejectInsufficientCash, fmt.Sprintf(
				"开%s需保证金 %.2f + 摩擦 %.2f，可用余额 %.2f",
				sideWord(side), cents(margin), cents(friction), cents(m.cash)), false
		}
		return RejectNone, "", true
	}
	p := m.slot(side)[f.Instrument]
	if p == nil || p.Qty < f.Qty {
		have := int64(0)
		if p != nil {
			have = p.Qty
		}
		return RejectInsufficientPosition, fmt.Sprintf(
			"平%s %d 张，实际持有 %d 张", sideWord(side), f.Qty, have), false
	}
	return RejectNone, "", true
}

// marginFor 开仓保证金 = 名义额 / 杠杆，向上取整。
//
// **向上取整**：保证金要求不能因为取整而偏松 ——
// 那会让账户在边界上开出一张本不该开的仓。
func (m *MarginLedger) marginFor(notional int64) int64 {
	if notional <= 0 {
		return 0
	}
	return ceilDiv(notional, m.leverage)
}

// ApplyFill 把一笔成交计入账本。
func (m *MarginLedger) ApplyFill(f Fill, _ int64) error {
	for k, v := range f.Fee.Items {
		m.feeCents[k] += v
	}
	m.slippage += f.SlippageCents
	friction := f.Fee.Total + f.SlippageCents

	side, opening := m.posFor(f.Order)
	slot := m.slot(side)
	p := slot[f.Instrument]

	if opening {
		margin := m.marginFor(f.AmountCents)
		if margin+friction > m.cash {
			return fmt.Errorf("开%s保证金 %.2f + 摩擦 %.2f 超过可用余额 %.2f",
				sideWord(side), cents(margin), cents(friction), cents(m.cash))
		}
		m.cash -= margin + friction
		// **摩擦立刻计入已实现**：它是真实付出的钱，不该等到平仓才认
		m.realized -= friction
		if p == nil {
			p = &MarginPos{}
			slot[f.Instrument] = p
		}
		p.Qty += f.Qty
		p.EntryCents += f.AmountCents
		p.MarginCents += margin
		return nil
	}

	if p == nil || p.Qty < f.Qty {
		have := int64(0)
		if p != nil {
			have = p.Qty
		}
		return fmt.Errorf("平%s %d 张但只有 %d 张", sideWord(side), f.Qty, have)
	}
	// 按比例释放保证金与开仓成本 —— 部分平仓时两者必须同比例，
	// 否则剩下的仓位会带着一个不成比例的保证金
	part := ratioPart(p.EntryCents, f.Qty, p.Qty)
	relMargin := ratioPart(p.MarginCents, f.Qty, p.Qty)

	var pnl int64
	if side == SideBuy {
		pnl = f.AmountCents - part // 多头：卖出所得 − 开仓成本
	} else {
		pnl = part - f.AmountCents // 空头：开仓所得 − 买回成本
	}
	m.cash += relMargin + pnl - friction
	m.realized += pnl - friction

	p.Qty -= f.Qty
	p.EntryCents -= part
	p.MarginCents -= relMargin
	if p.Qty <= 0 {
		delete(slot, f.Instrument)
	}
	return nil
}

// ratioPart 按 qty/total 的比例取 whole 的一部分，四舍五入。
func ratioPart(whole, qty, total int64) int64 {
	if total <= 0 {
		return whole
	}
	if qty >= total {
		return whole
	}
	return roundHalfUp(whole*qty, total)
}

// ---- 公司行动 ----

// ApplyCorporateAction 永续合约没有分红送配。
//
// 空实现而不是 panic：引擎对所有账本一视同仁地调用它，
// 而「加密没有这回事」是一个有意义的答案。
func (m *MarginLedger) ApplyCorporateAction(CorporateAction, int64, int64) {}

// ApplyImpliedSplit 同上，永续合约没有除权。
func (m *MarginLedger) ApplyImpliedSplit(mktdata.InstrumentID, int32, float64, int64) {}

// ---- 强平 ----

// Mark 按市价重估，并对**保证金亏光的仓位**强平。
//
// 逐仓的强平判据：仓位权益（保证金 + 未实现盈亏）跌到
// 名义额 × 维持保证金率 以下。
//
// 强平后**保证金全部损失**，不倒扣余额 —— 这正是逐仓的意义：
// 一个仓位最多亏掉它自己的那份保证金。
func (m *MarginLedger) Mark(
	marks map[mktdata.InstrumentID]int64, now mktdata.TimePoint,
) []Liquidation {
	var out []Liquidation
	for _, id := range m.activeIDs() {
		price := marks[id]
		if price <= 0 {
			continue // 没有报价就不判强平 —— 猜一个价去爆仓是最坏的做法
		}
		for _, side := range [...]Side{SideBuy, SideSell} {
			slot := m.slot(side)
			p := slot[id]
			if p == nil || p.Qty <= 0 {
				continue
			}
			pnl := m.unrealized(id, p, price, side)
			equity := p.MarginCents + pnl
			notional := m.value(id, price, p.Qty)
			maint := roundHalfUp(notional*m.mmrPPM, 1_000_000)
			if equity > maint {
				continue
			}
			liq := Liquidation{
				Instrument: id, At: now, Side: side, Qty: p.Qty, Price: price,
				NotionalCents: notional, LostMarginCents: p.MarginCents,
				Reason: fmt.Sprintf("逐仓权益 %.2f 低于维持保证金 %.2f（%.2f%%）",
					cents(equity), cents(maint), float64(m.mmrPPM)/10_000),
			}
			// 保证金全损，余额不动 —— 逐仓不倒扣
			m.realized -= p.MarginCents
			delete(slot, id)
			m.warnings = append(m.warnings, liq.String())
			out = append(out, liq)
		}
	}
	m.liquidated = append(m.liquidated, out...)
	return out
}

// Liquidations 返回累计强平记录。
func (m *MarginLedger) Liquidations() []Liquidation { return m.liquidated }

// ---- 快照 ----

type marginState struct {
	InitialCash int64                 `json:"initial_cash"`
	Cash        int64                 `json:"cash"`
	Long        map[string]*MarginPos `json:"long"`
	Short       map[string]*MarginPos `json:"short"`
	Leverage    int64                 `json:"leverage"`
	MMRPPM      int64                 `json:"mmr_ppm"`
	Realized    int64                 `json:"realized"`
	FeeCents    map[string]int64      `json:"fee_cents"`
	Slippage    int64                 `json:"slippage"`
	Warnings    []string              `json:"warnings"`
}

func (m *MarginLedger) SnapshotLedger() ([]byte, error) {
	st := marginState{
		InitialCash: m.initialCash, Cash: m.cash,
		Long:     make(map[string]*MarginPos, len(m.long)),
		Short:    make(map[string]*MarginPos, len(m.short)),
		Leverage: m.leverage, MMRPPM: m.mmrPPM, Realized: m.realized,
		FeeCents: m.feeCents, Slippage: m.slippage, Warnings: m.warnings,
	}
	for id, p := range m.long {
		st.Long[fmt.Sprint(int32(id))] = p
	}
	for id, p := range m.short {
		st.Short[fmt.Sprint(int32(id))] = p
	}
	return json.Marshal(st)
}

func (m *MarginLedger) RestoreLedger(b []byte) error {
	var st marginState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("解析保证金账本快照失败: %w", err)
	}
	m.initialCash, m.cash = st.InitialCash, st.Cash
	m.leverage, m.mmrPPM = st.Leverage, st.MMRPPM
	m.realized, m.slippage = st.Realized, st.Slippage
	m.feeCents = st.FeeCents
	if m.feeCents == nil {
		m.feeCents = map[string]int64{}
	}
	m.warnings = st.Warnings
	m.long = make(map[mktdata.InstrumentID]*MarginPos, len(st.Long))
	m.short = make(map[mktdata.InstrumentID]*MarginPos, len(st.Short))
	for k, v := range st.Long {
		id, err := parseID(k)
		if err != nil {
			return err
		}
		m.long[id] = v
	}
	for k, v := range st.Short {
		id, err := parseID(k)
		if err != nil {
			return err
		}
		m.short[id] = v
	}
	return nil
}

func parseID(k string) (mktdata.InstrumentID, error) {
	var id int32
	if _, err := fmt.Sscan(k, &id); err != nil {
		return 0, fmt.Errorf("快照中的标的 ID %q 无法解析: %w", k, err)
	}
	return mktdata.InstrumentID(id), nil
}

func sideWord(s Side) string {
	if s == SideBuy {
		return "多"
	}
	return "空"
}

var _ Ledger = (*MarginLedger)(nil)
