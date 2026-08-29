package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

const (
	oneMillionYuan = int64(100_000_000) // 100 万元，单位分
	day2024        = int32(20240318)
)

func mainStock() *mktdata.Instrument {
	s := stock(mktdata.BoardMain)
	s.MinOrderQty, s.QtyStep, s.ListDate = 100, 100, 20000101
	return s
}

func mainETF() *mktdata.Instrument {
	e := etf()
	e.MinOrderQty, e.QtyStep, e.ListDate = 100, 100, 20000101
	return e
}

// 手工核算一笔个股的完整买卖，与引擎输出**逐分位**比对。
// 这是本刀最硬的验收标准。
func TestManualCheckStockRoundTrip(t *testing.T) {
	f := loadDefault(t)
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan * 10) // 1000 万元

	// ---- 买入 1000 股 @ 1700.000 元 ----
	buyPrice := int64(1_700_000) // 厘
	buyQty := int64(1000)
	buyAmount := AmountCents(buyPrice, buyQty)
	if buyAmount != 170_000_000 {
		t.Fatalf("成交额 = %d 分，手工核算 170000000 分", buyAmount)
	}
	buyFee := f.Calc(FeeRequest{Instrument: inst, Side: SideBuy,
		Qty: buyQty, AmountCents: buyAmount, TradingDay: day2024})
	// 手工：佣金 42500 + 过户费 1700 = 44200，买入不收印花税
	if buyFee.Total != 44_200 {
		t.Fatalf("买入费用 = %d 分，手工核算 44200 分（明细 %v）", buyFee.Total, buyFee.Items)
	}

	fill := Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: buyQty},
		Price: buyPrice, Qty: buyQty, Fee: buyFee}
	if err := pf.ApplyFill(fill, 1_000_000_000); err != nil {
		t.Fatal(err)
	}

	wantCash := oneMillionYuan*10 - 170_000_000 - 44_200
	if pf.Cash != wantCash {
		t.Errorf("买入后现金 = %d 分，手工核算 %d 分", pf.Cash, wantCash)
	}
	p := pf.Position(inst.ID)
	if p.Total != 1000 {
		t.Errorf("持仓 = %d，期望 1000", p.Total)
	}
	// 成本含买入费用：170044200 分 / 1000 股 = 170044 分/股 = 1700.44 元
	if p.CostCents != 170_044_200 {
		t.Errorf("持仓成本 = %d 分，手工核算 170044200 分", p.CostCents)
	}
	if p.AvgCostCents() != 170_044 {
		t.Errorf("每股成本 = %d 分，手工核算 170044 分（1700.44 元）", p.AvgCostCents())
	}

	// ---- 卖出 1000 股 @ 1800.000 元 ----
	sellPrice := int64(1_800_000)
	sellAmount := AmountCents(sellPrice, buyQty)
	sellFee := f.Calc(FeeRequest{Instrument: inst, Side: SideSell,
		Qty: buyQty, AmountCents: sellAmount, TradingDay: day2024})
	// 手工：佣金 45000 + 印花税 90000（0.5‰）+ 过户费 1800 = 136800
	if sellFee.Total != 136_800 {
		t.Fatalf("卖出费用 = %d 分，手工核算 136800 分（明细 %v）", sellFee.Total, sellFee.Items)
	}

	sf := Fill{Order: Order{Instrument: inst.ID, Side: SideSell, Qty: buyQty},
		Price: sellPrice, Qty: buyQty, Fee: sellFee}
	if err := pf.ApplyFill(sf, 0); err != nil {
		t.Fatal(err)
	}

	// 手工：卖出净得 180000000 - 136800 = 179863200
	//       已实现 = 179863200 - 170044200 = 9819000 分 = 98190 元
	if pf.RealizedCents != 9_819_000 {
		t.Errorf("已实现盈亏 = %d 分，手工核算 9819000 分", pf.RealizedCents)
	}
	wantFinal := oneMillionYuan*10 + 9_819_000
	if pf.Cash != wantFinal {
		t.Errorf("最终现金 = %d 分，手工核算 %d 分", pf.Cash, wantFinal)
	}
	if pf.Position(inst.ID).Total != 0 {
		t.Error("清仓后持仓应为 0")
	}
	// 累计费用应等于两次之和
	if got := pf.TotalFeeCents(); got != 44_200+136_800 {
		t.Errorf("累计费用 = %d 分，期望 181000 分", got)
	}
}

// ETF 免印花税与过户费，同一笔交易的成本明显低于个股 ——
// 这是 C8「按标的类型分流」的直接后果，也是为什么同一策略
// 跑个股与跑 ETF 会得出不同结论。
func TestManualCheckETFCheaperThanStock(t *testing.T) {
	f := loadDefault(t)
	amount := int64(40_000_000) // 40 万元
	qty := int64(10_000)

	st := f.Calc(FeeRequest{Instrument: mainStock(), Side: SideSell,
		Qty: qty, AmountCents: amount, TradingDay: day2024})
	ef := f.Calc(FeeRequest{Instrument: mainETF(), Side: SideSell,
		Qty: qty, AmountCents: amount, TradingDay: day2024})

	// 个股：佣金 10000 + 印花税 20000 + 过户费 400 = 30400
	// ETF ：佣金 10000
	if st.Total != 30_400 {
		t.Errorf("个股卖出费用 = %d 分，手工核算 30400 分（%v）", st.Total, st.Items)
	}
	if ef.Total != 10_000 {
		t.Errorf("ETF 卖出费用 = %d 分，手工核算 10000 分（%v）", ef.Total, ef.Items)
	}
}

// T+1：当日买入的股份当日不可卖。
func TestT1Settlement(t *testing.T) {
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan)
	now := int64(1_700_000_000_000)

	fill := Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: 1000},
		Price: 10_000, Qty: 1000, Fee: FeeBreakdown{Items: map[string]int64{}}}
	if err := pf.ApplyFill(fill, now+1); err != nil { // T+1 才可卖
		t.Fatal(err)
	}
	if got := pf.Available(inst.ID, now); got != 0 {
		t.Errorf("当日可卖 = %d，期望 0（T+1 未解冻）", got)
	}
	if got := pf.Available(inst.ID, now+1); got != 1000 {
		t.Errorf("次日可卖 = %d，期望 1000", got)
	}
	if got := pf.Position(inst.ID).Total; got != 1000 {
		t.Errorf("总持仓 = %d，期望 1000（可卖受限但持仓存在）", got)
	}
}

// 现金分红入账 —— 只调价格不调账户会系统性低估收益（C2）。
func TestCashDividendCredited(t *testing.T) {
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan)
	fill := Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: 1000},
		Price: 10_000, Qty: 1000, Fee: FeeBreakdown{Items: map[string]int64{}}}
	pf.ApplyFill(fill, 0)
	before := pf.Cash

	// 每股税前 0.5 元 = 500000（scale 1e6）；1000 股 → 500 元 = 50000 分
	pf.ApplyCorporateAction(CorporateAction{
		Instrument: inst.ID, ExDate: day2024, CashBeforeTax: 500_000,
	}, 0, 0)
	if got := pf.Cash - before; got != 50_000 {
		t.Errorf("分红入账 = %d 分，手工核算 50000 分（1000 股 × 0.5 元）", got)
	}

	// 带 10% 红利税
	before = pf.Cash
	pf.ApplyCorporateAction(CorporateAction{
		Instrument: inst.ID, ExDate: day2024, CashBeforeTax: 500_000,
	}, 100_000, 0)
	if got := pf.Cash - before; got != 45_000 {
		t.Errorf("税后分红 = %d 分，手工核算 45000 分（税前 500 元 × 90%%）", got)
	}
}

// 送转：持仓按比例增加，**总成本不变**，均价因此摊薄。
func TestStockDividendDilutesCost(t *testing.T) {
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan)
	fill := Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: 1000},
		Price: 10_000, Qty: 1000, Fee: FeeBreakdown{Items: map[string]int64{}}}
	pf.ApplyFill(fill, 0)
	p := pf.Position(inst.ID)
	costBefore := p.CostCents
	avgBefore := p.AvgCostCents()

	// 每 10 股送 8 转 12 = 每股送 0.8 转 1.2，合计每股 +2 股（比亚迪 2025-07-29 的实例）
	pf.ApplyCorporateAction(CorporateAction{
		Instrument: inst.ID, ExDate: day2024,
		StockDividend: 800_000, StockTransfer: 1_200_000,
	}, 0, 0)

	if p.Total != 3000 {
		t.Errorf("送转后持仓 = %d，手工核算 3000（1000 × 3）", p.Total)
	}
	if p.CostCents != costBefore {
		t.Errorf("送转不应改变总成本：%d → %d", costBefore, p.CostCents)
	}
	if got, want := p.AvgCostCents(), avgBefore/3; got != want {
		t.Errorf("均价 = %d 分，期望摊薄至 %d 分", got, want)
	}
	// 批次总量必须与持仓一致，否则可卖数量会与实际脱节
	var sum int64
	for _, l := range p.Lots {
		sum += l.Qty
	}
	if sum != p.Total {
		t.Errorf("批次总量 %d 与持仓 %d 不一致", sum, p.Total)
	}
}

// 有因子事件但无分红记录时按因子推算入账，并留痕（评审决议 2）。
func TestImpliedSplitWarns(t *testing.T) {
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan)
	fill := Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: 1000},
		Price: 10_000, Qty: 1000, Fee: FeeBreakdown{Items: map[string]int64{}}}
	pf.ApplyFill(fill, 0)

	pf.ApplyImpliedSplit(inst.ID, day2024, 2.0, 0)
	if got := pf.Position(inst.ID).Total; got != 2000 {
		t.Errorf("按因子 2.0 推算后持仓 = %d，期望 2000", got)
	}
	if len(pf.Warnings) == 0 {
		t.Error("有损近似必须留痕，未产生告警")
	}
}

// 账本快照往返 —— C6 实盘增量的前提。
func TestPortfolioSnapshotRoundTrip(t *testing.T) {
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan)
	fill := Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: 1000},
		Price: 10_000, Qty: 1000,
		Fee: FeeBreakdown{Items: map[string]int64{"commission": 500}, Total: 500}}
	pf.ApplyFill(fill, 12345)

	snap := pf.Snapshot()
	restored := NewPortfolio(0)
	restored.Restore(snap)

	if restored.Cash != pf.Cash || restored.RealizedCents != pf.RealizedCents {
		t.Error("现金或已实现盈亏未正确恢复")
	}
	rp := restored.Position(inst.ID)
	if rp == nil || rp.Total != 1000 || rp.CostCents != pf.Position(inst.ID).CostCents {
		t.Error("持仓未正确恢复")
	}
	if got := restored.Available(inst.ID, 12345); got != 1000 {
		t.Errorf("恢复后可卖 = %d，期望 1000（批次的可卖时刻应一并恢复）", got)
	}
	if got := restored.Available(inst.ID, 12344); got != 0 {
		t.Errorf("恢复后解冻前可卖 = %d，期望 0", got)
	}
	// 快照必须是深拷贝，改动恢复后的账本不得影响原账本
	restored.Cash = 0
	if pf.Cash == 0 {
		t.Error("快照不是深拷贝")
	}
}
