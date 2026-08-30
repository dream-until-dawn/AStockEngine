package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// 这些测试里的估值函数刻意取成 price×qty，让 100 张 @ 1.00 正好是
// 100.00 —— 逐仓与双向的行为本身与 scale 无关，把 scale 的复杂度
// 混进来只会让断言里的数字没法心算，反而看不出规则错在哪。
// scale 的正确性由 TestNotionalMatchesAShareLegacy 一族负责。

const tid = mktdata.InstrumentID(7)

func newTestLedger(cash, lev int64) *MarginLedger {
	m := NewMarginLedger(cash, lev, 5000) // mmr 0.5%
	m.SetValuer(func(_ mktdata.InstrumentID, price, qty int64) int64 {
		return price * qty
	})
	return m
}

func mkFill(side Side, reduce bool, price, qty, fee int64) Fill {
	f := Fill{
		Order:       Order{Instrument: tid, Side: side, Qty: qty, Reduce: reduce},
		At:          mktdata.TimePoint{TradingDay: 20240101},
		Price:       price,
		Qty:         qty,
		AmountCents: price * qty,
	}
	if fee > 0 {
		f.Fee = FeeBreakdown{Total: fee, Items: map[string]int64{"trading_fee": fee}}
	}
	return f
}

func mustApply(t *testing.T, m *MarginLedger, f Fill) {
	t.Helper()
	if _, _, ok := m.CanFill(f); !ok {
		t.Fatalf("CanFill 拒绝了本该通过的成交 %+v", f.Order)
	}
	if err := m.ApplyFill(f, 0); err != nil {
		t.Fatalf("ApplyFill: %v", err)
	}
}

func marks(price int64) map[mktdata.InstrumentID]int64 {
	return map[mktdata.InstrumentID]int64{tid: price}
}

// TestMarginLongRoundTrip 开多 → 涨 → 平多。
func TestMarginLongRoundTrip(t *testing.T) {
	m := newTestLedger(100_000, 2)

	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0)) // 名义 10000，保证金 5000
	if got := m.CashCents(); got != 95_000 {
		t.Fatalf("开仓后可用余额 = %d，想要 95000", got)
	}
	e := m.Exposure(tid)
	if e.Long != 100 || e.LongCost != 10_000 || e.Short != 0 {
		t.Fatalf("开多后敞口 = %+v", e)
	}
	// 保证金被占用，但权益不变 —— 钱只是换了个位置
	if got := m.EquityCents(marks(100)); got != 100_000 {
		t.Fatalf("持平时权益 = %d，想要 100000", got)
	}
	if got := m.EquityCents(marks(110)); got != 101_000 {
		t.Fatalf("涨一成后权益 = %d，想要 101000", got)
	}

	mustApply(t, m, mkFill(SideSell, true, 110, 100, 0))
	if got := m.CashCents(); got != 101_000 {
		t.Fatalf("平仓后可用余额 = %d，想要 101000", got)
	}
	if got := m.RealizedCents(); got != 1_000 {
		t.Fatalf("已实现 = %d，想要 1000", got)
	}
	if m.NumPositions() != 0 {
		t.Fatalf("平光后仍有 %d 个标的有敞口", m.NumPositions())
	}
}

// TestMarginShortRoundTrip 开空 → 跌 → 平空。
//
// 空头的每一步符号都与多头相反，这里逐条对照着写，
// 就是为了让某一处漏了取反时测试挂在那一行上。
func TestMarginShortRoundTrip(t *testing.T) {
	m := newTestLedger(100_000, 2)

	mustApply(t, m, mkFill(SideSell, false, 100, 100, 0)) // 卖 + 开 = 开空
	e := m.Exposure(tid)
	if e.Short != 100 || e.ShortCost != 10_000 || e.Long != 0 {
		t.Fatalf("开空后敞口 = %+v", e)
	}
	if got := m.CashCents(); got != 95_000 {
		t.Fatalf("开空后可用余额 = %d，想要 95000", got)
	}
	// 跌了才是空头的盈利
	if got := m.EquityCents(marks(90)); got != 101_000 {
		t.Fatalf("跌一成后权益 = %d，想要 101000", got)
	}
	if got := m.EquityCents(marks(110)); got != 99_000 {
		t.Fatalf("涨一成后权益 = %d，想要 99000", got)
	}

	mustApply(t, m, mkFill(SideBuy, true, 90, 100, 0)) // 买 + 平 = 平空
	if got := m.CashCents(); got != 101_000 {
		t.Fatalf("平空后可用余额 = %d，想要 101000", got)
	}
	if got := m.RealizedCents(); got != 1_000 {
		t.Fatalf("已实现 = %d，想要 1000", got)
	}
}

// TestMarginHedgeBothSides 同一标的同时持多与持空。
//
// 单向净持仓下这是不可能的 —— 第二笔会变成平仓。
// 断言的正是「没有被合并成净头寸」。
func TestMarginHedgeBothSides(t *testing.T) {
	m := newTestLedger(100_000, 2)

	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0))
	mustApply(t, m, mkFill(SideSell, false, 100, 100, 0))

	e := m.Exposure(tid)
	if e.Long != 100 || e.Short != 100 {
		t.Fatalf("双向持仓后敞口 = %+v，想要多空各 100", e)
	}
	// 一个标的两个仓位，仍然只算一只
	if got := m.NumPositions(); got != 1 {
		t.Fatalf("NumPositions = %d，想要 1（问的是标的数不是仓位数）", got)
	}
	// 两份保证金都占上了
	if got := m.CashCents(); got != 90_000 {
		t.Fatalf("双开后可用余额 = %d，想要 90000", got)
	}
	// 对冲住了：涨跌都不影响权益
	for _, p := range []int64{80, 100, 130} {
		if got := m.EquityCents(marks(p)); got != 100_000 {
			t.Fatalf("价格 %d 时权益 = %d，对冲仓位不该有净盈亏", p, got)
		}
	}
}

// TestMarginIsolatedLiquidationCapsLoss 逐仓强平只吃掉那一份保证金。
//
// **这是逐仓与全仓唯一的分界**，也是这个账本存在的理由：
// 浮亏 3000 而保证金只有 2000 时，账户损失必须停在 2000。
func TestMarginIsolatedLiquidationCapsLoss(t *testing.T) {
	m := newTestLedger(100_000, 5)

	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0)) // 名义 10000，保证金 2000
	if got := m.CashCents(); got != 98_000 {
		t.Fatalf("开仓后可用余额 = %d，想要 98000", got)
	}

	// 跌到 70：浮亏 3000，比 2000 的保证金还多
	liq := m.Mark(marks(70), mktdata.TimePoint{TradingDay: 20240102})
	if len(liq) != 1 {
		t.Fatalf("强平了 %d 个仓位，想要 1", len(liq))
	}
	if liq[0].Side != SideBuy || liq[0].Qty != 100 {
		t.Fatalf("强平记录 = %+v", liq[0])
	}
	if liq[0].LostMarginCents != 2_000 || liq[0].NotionalCents != 7_000 {
		t.Fatalf("强平记录的金额 = 损失 %d / 名义 %d，想要 2000 / 7000",
			liq[0].LostMarginCents, liq[0].NotionalCents)
	}
	if m.NumPositions() != 0 {
		t.Fatalf("强平后仓位还在")
	}
	// 余额一分没动：亏的是保证金，不是余额
	if got := m.CashCents(); got != 98_000 {
		t.Fatalf("强平后可用余额 = %d，想要 98000（逐仓不倒扣余额）", got)
	}
	if got := m.RealizedCents(); got != -2_000 {
		t.Fatalf("强平后已实现 = %d，想要 -2000（以保证金为限）", got)
	}
	if got := m.EquityCents(marks(70)); got != 98_000 {
		t.Fatalf("强平后权益 = %d，想要 98000", got)
	}
}

// TestMarginLiquidationSparesOtherLeg 逐仓下爆一条腿不牵连另一条。
func TestMarginLiquidationSparesOtherLeg(t *testing.T) {
	m := newTestLedger(100_000, 5)
	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0))
	mustApply(t, m, mkFill(SideSell, false, 100, 100, 0))

	// 涨到 130：空头浮亏 3000 > 保证金 2000，多头浮盈 3000
	liq := m.Mark(marks(130), mktdata.TimePoint{TradingDay: 20240102})
	if len(liq) != 1 || liq[0].Side != SideSell {
		t.Fatalf("强平结果 = %+v，想要只爆空头", liq)
	}
	e := m.Exposure(tid)
	if e.Long != 100 || e.Short != 0 {
		t.Fatalf("爆空之后敞口 = %+v，多头不该受影响", e)
	}
}

// TestMarginNoLiquidationWithoutQuote 没有报价时不判强平。
func TestMarginNoLiquidationWithoutQuote(t *testing.T) {
	m := newTestLedger(100_000, 5)
	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0))

	if liq := m.Mark(map[mktdata.InstrumentID]int64{}, mktdata.TimePoint{}); len(liq) != 0 {
		t.Fatalf("缺报价时强平了 %d 个仓位 —— 猜价爆仓是最坏的做法", len(liq))
	}
	if m.NumPositions() != 1 {
		t.Fatalf("仓位被误删了")
	}
}

// TestMarginPartialClose 部分平仓时保证金与开仓成本必须同比例释放。
func TestMarginPartialClose(t *testing.T) {
	m := newTestLedger(100_000, 2)
	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0)) // 成本 10000，保证金 5000

	mustApply(t, m, mkFill(SideSell, true, 150, 40, 0)) // 平掉四成
	e := m.Exposure(tid)
	if e.Long != 60 || e.LongCost != 6_000 {
		t.Fatalf("部分平仓后敞口 = %+v，想要 60 张 / 成本 6000", e)
	}
	if got := m.long[tid].MarginCents; got != 3_000 {
		t.Fatalf("剩余保证金 = %d，想要 3000（与剩余仓位同比例）", got)
	}
	// 平掉的四成：卖 6000 − 成本 4000 = 2000
	if got := m.RealizedCents(); got != 2_000 {
		t.Fatalf("已实现 = %d，想要 2000", got)
	}
	// 余额 95000 + 释放保证金 2000 + 盈利 2000
	if got := m.CashCents(); got != 99_000 {
		t.Fatalf("可用余额 = %d，想要 99000", got)
	}
}

// TestMarginFeesChargedAtOpen 开仓的摩擦立刻计入已实现。
func TestMarginFeesChargedAtOpen(t *testing.T) {
	m := newTestLedger(100_000, 2)
	f := mkFill(SideBuy, false, 100, 100, 30)
	f.SlippageCents = 20
	mustApply(t, m, f)

	if got := m.CashCents(); got != 94_950 {
		t.Fatalf("可用余额 = %d，想要 94950（保证金 5000 + 摩擦 50）", got)
	}
	if got := m.RealizedCents(); got != -50 {
		t.Fatalf("已实现 = %d，想要 -50（摩擦是真实付出的钱，不等平仓才认）", got)
	}
	if got := m.TotalFeeCents(); got != 30 {
		t.Fatalf("费用合计 = %d，想要 30（滑点不算费用）", got)
	}
	if got := m.SlippageCents(); got != 20 {
		t.Fatalf("滑点 = %d，想要 20", got)
	}
}

// TestMarginCanFillRejects 两类拒单。
func TestMarginCanFillRejects(t *testing.T) {
	m := newTestLedger(10_000, 2)

	// 开仓：保证金不够
	big := mkFill(SideBuy, false, 100, 1_000, 0) // 名义 100000，保证金 50000
	reason, msg, ok := m.CanFill(big)
	if ok || reason != RejectInsufficientCash {
		t.Fatalf("超额开仓竟然通过了：ok=%v reason=%v msg=%q", ok, reason, msg)
	}

	// 平仓：没有那么多可平
	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0))
	reason, msg, ok = m.CanFill(mkFill(SideSell, true, 100, 200, 0))
	if ok || reason != RejectInsufficientPosition {
		t.Fatalf("超额平仓竟然通过了：ok=%v reason=%v msg=%q", ok, reason, msg)
	}
	// 平空更是无仓可平 —— 手上只有多头
	if _, _, ok := m.CanFill(mkFill(SideBuy, true, 100, 1, 0)); ok {
		t.Fatalf("在没有空头时平空竟然通过了")
	}
}

// TestMarginBuyingPowerIsCashNotEquity 逐仓的购买力只看余额。
//
// 全仓才是「权益 × 杠杆」；逐仓下已开仓位的浮盈是那个仓位的，
// 拿去开新仓等于把逐仓变回了全仓。
func TestMarginBuyingPowerIsCashNotEquity(t *testing.T) {
	m := newTestLedger(100_000, 3)
	if got := m.BuyingPowerCents(); got != 300_000 {
		t.Fatalf("购买力 = %d，想要 300000", got)
	}
	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0))
	cash := m.CashCents()
	if got := m.BuyingPowerCents(); got != cash*3 {
		t.Fatalf("购买力 = %d，想要 %d（余额 × 杠杆，不是权益 × 杠杆）", got, cash*3)
	}
}

// TestMarginForRoundsUp 保证金向上取整 —— 取整不能让要求变松。
func TestMarginForRoundsUp(t *testing.T) {
	m := newTestLedger(100_000, 3)
	if got := m.marginFor(10); got != 4 { // 10/3 = 3.33 → 4
		t.Fatalf("marginFor(10) = %d，想要 4", got)
	}
	if got := m.marginFor(9); got != 3 {
		t.Fatalf("marginFor(9) = %d，想要 3", got)
	}
	if got := m.marginFor(0); got != 0 {
		t.Fatalf("marginFor(0) = %d，想要 0", got)
	}
}

// TestMarginPosFor 开平方向映射表。四个格子都要在。
func TestMarginPosFor(t *testing.T) {
	m := newTestLedger(100_000, 2)
	cases := []struct {
		side    Side
		reduce  bool
		want    Side
		opening bool
		what    string
	}{
		{SideBuy, false, SideBuy, true, "买 + 开 = 开多"},
		{SideSell, false, SideSell, true, "卖 + 开 = 开空"},
		{SideSell, true, SideBuy, false, "卖 + 平 = 平多"},
		{SideBuy, true, SideSell, false, "买 + 平 = 平空"},
	}
	for _, c := range cases {
		got, opening := m.posFor(Order{Side: c.side, Reduce: c.reduce})
		if got != c.want || opening != c.opening {
			t.Errorf("%s：得到 side=%v opening=%v", c.what, got, opening)
		}
	}
}

// TestMarginSnapshotRoundTrip 快照必须完整还原，双向仓位一个不落。
func TestMarginSnapshotRoundTrip(t *testing.T) {
	m := newTestLedger(100_000, 4)
	f := mkFill(SideBuy, false, 100, 100, 25)
	f.SlippageCents = 15
	mustApply(t, m, f)
	mustApply(t, m, mkFill(SideSell, false, 120, 50, 0))
	m.Mark(marks(1), mktdata.TimePoint{TradingDay: 20240103}) // 制造一条强平告警

	b, err := m.SnapshotLedger()
	if err != nil {
		t.Fatalf("SnapshotLedger: %v", err)
	}
	got := NewMarginLedger(1, 1, 1)
	got.SetValuer(func(_ mktdata.InstrumentID, price, qty int64) int64 { return price * qty })
	if err := got.RestoreLedger(b); err != nil {
		t.Fatalf("RestoreLedger: %v", err)
	}

	if got.CashCents() != m.CashCents() || got.InitialCashCents() != m.InitialCashCents() {
		t.Fatalf("余额没还原：%d/%d 想要 %d/%d",
			got.CashCents(), got.InitialCashCents(), m.CashCents(), m.InitialCashCents())
	}
	if got.RealizedCents() != m.RealizedCents() || got.SlippageCents() != m.SlippageCents() {
		t.Fatalf("盈亏/滑点没还原")
	}
	if got.TotalFeeCents() != m.TotalFeeCents() {
		t.Fatalf("费用没还原：%d 想要 %d", got.TotalFeeCents(), m.TotalFeeCents())
	}
	if len(got.Warnings()) != len(m.Warnings()) {
		t.Fatalf("告警没还原：%d 条 想要 %d 条", len(got.Warnings()), len(m.Warnings()))
	}
	// 杠杆与维持保证金率也要还原 —— 它们参与后续的开仓与强平判据，
	// 漏了会让恢复出来的账本用另一套规则继续跑
	if got.leverage != m.leverage || got.mmrPPM != m.mmrPPM {
		t.Fatalf("杠杆/维持保证金率没还原：%d/%d 想要 %d/%d",
			got.leverage, got.mmrPPM, m.leverage, m.mmrPPM)
	}
	if got.Exposure(tid) != m.Exposure(tid) {
		t.Fatalf("敞口没还原：%+v 想要 %+v", got.Exposure(tid), m.Exposure(tid))
	}
	if got.EquityCents(marks(100)) != m.EquityCents(marks(100)) {
		t.Fatalf("权益对不上")
	}
}

// TestMarginNoCorporateActions 永续合约没有分红送配，调了也不该动账。
func TestMarginNoCorporateActions(t *testing.T) {
	m := newTestLedger(100_000, 2)
	mustApply(t, m, mkFill(SideBuy, false, 100, 100, 0))
	before := m.Exposure(tid)
	cash := m.CashCents()

	m.ApplyCorporateAction(CorporateAction{}, 0, 0)
	m.ApplyImpliedSplit(tid, 20240102, 2.0, 0)

	if m.Exposure(tid) != before || m.CashCents() != cash {
		t.Fatalf("公司行动动了加密的账：敞口 %+v 余额 %d", m.Exposure(tid), m.CashCents())
	}
}
