package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// ---- 定量基准：cost / equity / initial ----
//
// 三个基准的差别只在**浮盈算不算数**：
//
//	cost    不算。10000 切 10 份永远是每份 1000，哪怕持仓已经浮盈到 11000
//	equity  算。浮盈立刻放大后续仓位
//	initial 连已实现盈亏都不算，永远按初始资金切（定额下注）
//
// 差别不会报错，只会让「同一个策略」在两种口径下跑出完全不同的曲线。

// TestCostBasisIgnoresUnrealisedGain 已投入本金 = 权益 − 未实现盈亏。
func TestCostBasisIgnoresUnrealisedGain(t *testing.T) {
	pf := NewPortfolio(1_000_000) // 10,000.00
	// 花 500,000 买了一笔，现在市值 600,000（浮盈 100,000）
	st := pf.Snapshot()
	st.Cash = 500_000
	st.Positions = map[mktdata.InstrumentID]*Position{
		1: {Total: 1000, CostCents: 500_000, Lots: []lot{{Qty: 1000}}},
	}
	pf.Restore(st)

	if got := pf.CostBasisCents(); got != 1_000_000 {
		t.Fatalf("已投入本金 = %d，想要 1000000（现金 500000 + 成本 500000）", got)
	}
	// 市值涨到 60 万时权益是 110 万，但本金仍是 100 万
	eq := pf.EquityCents(map[mktdata.InstrumentID]int64{1: 6_000})
	if eq != 1_100_000 {
		t.Fatalf("权益 = %d，想要 1100000", eq)
	}
	if pf.CostBasisCents() != eq-100_000 {
		t.Errorf("已投入本金应当正好是权益减去未实现盈亏")
	}
}

// TestEqualWeightCostBaseDoesNotCompoundUnrealised 需求方点名的那个场景。
//
// 10000 切 10 份 → 每份 1000，先买 5 只；这 5 只涨到总权益 11000 之后
// 再买 5 只，**仍然是每份 1000 而不是 1100** ——
// 浮盈还没落袋，拿它加仓等于用没到手的钱下注。
func TestEqualWeightCostBaseDoesNotCompoundUnrealised(t *testing.T) {
	ctx := newCtx(10, 1_000_000) // 10,000.00 元，10 只标的
	// 价格取 1.000 元：A 股一手 100 股，10 元的股票上「每份 1000 还是 1100」
	// 规整之后都是 100 股，差别会被申报单位吞掉，测不出东西
	for i := 1; i <= 10; i++ {
		b := ctx.bars[mktdata.InstrumentID(i)]
		b.Close, b.Open, b.High, b.Low, b.PreClose = 1_000, 1_000, 1_000, 1_000, 1_000
		ctx.bars[mktdata.InstrumentID(i)] = b
	}
	// 已买 5 只，各花 100,000（1000.00 元）→ 现金 500,000
	st := ctx.pf.Snapshot()
	st.Cash = 500_000
	st.Positions = map[mktdata.InstrumentID]*Position{}
	for i := 1; i <= 5; i++ {
		st.Positions[mktdata.InstrumentID(i)] = &Position{
			Total: 1000, CostCents: 100_000, Lots: []lot{{Qty: 1000}},
		}
	}
	ctx.pf.Restore(st)
	// 这 5 只都涨了两成 → 权益 1,100,000
	for i := 1; i <= 5; i++ {
		b := ctx.bars[mktdata.InstrumentID(i)]
		b.Close = 1_200
		ctx.bars[mktdata.InstrumentID(i)] = b
	}
	ctx.equity = 1_100_000

	got := mustSizer(t, "equal_weight",
		`{"slots":10,"base":"cost","order_by":"signal"}`).Size(enters(6, 7, 8, 9, 10), ctx)
	if len(got) != 5 {
		t.Fatalf("应当给 5 只都下单，得到 %d 笔", len(got))
	}
	// 每份 1,000,000 分 / 10 = 100,000 分 = 1000.00 元；
	// 1.000 元一股 → 1000 股
	for _, o := range got {
		if o.Qty != 1000 {
			t.Fatalf("每份应仍是 1000.00 元（1000 股），得到 %d 股 —— "+
				"浮盈被算进基准了", o.Qty)
		}
	}

	// 对照：base=equity 把浮盈算进去，每份变成 1100.00 元 → 1100 股
	eqGot := mustSizer(t, "equal_weight",
		`{"slots":10,"base":"equity","order_by":"signal"}`).Size(enters(6), ctx)
	if len(eqGot) != 1 || eqGot[0].Qty != 1100 {
		t.Fatalf("base=equity 应当按 1100.00 元买 1100 股，得到 %+v", eqGot)
	}
}

// TestSizeBaseInitialStillWorks initial 保留给旧配置，行为不变。
func TestSizeBaseInitialStillWorks(t *testing.T) {
	ctx := newCtx(3, 1_000_000)
	ctx.equity = 2_000_000 // 权益翻倍
	ctx.initial = 1_000_000

	got := mustSizer(t, "equal_weight",
		`{"slots":10,"base":"initial","order_by":"signal"}`).Size(enters(1), ctx)
	// 初始 10,000.00 元 / 10 份 = 1000.00 元 → 100 股（权益翻倍不影响）
	if len(got) != 1 || got[0].Qty != 100 {
		t.Fatalf("base=initial 应始终按初始资金切，得到 %+v", got)
	}
}

// ---- 候选排序 ----

// TestOrderByAmountPrefersLiquid 空位不够时，成交额大的先拿到。
//
// 从前拿到的是**标的 ID 最小的那几只** —— 那是数据的顺序，不是任何策略。
func TestOrderByAmountPrefersLiquid(t *testing.T) {
	ctx := newCtx(5, 1_000_000)
	// 成交额与 ID 反着来：5 号最活跃，1 号最冷清
	for i := 1; i <= 5; i++ {
		b := ctx.bars[mktdata.InstrumentID(i)]
		b.Amount = int64(i) * 1_000_000_000
		b.Volume = int64(6-i) * 1_000_000 // 成交量刻意与成交额反向
		ctx.bars[mktdata.InstrumentID(i)] = b
	}

	// 只有 2 个空位，5 只标的都发信号
	got := mustSizer(t, "equal_weight",
		`{"slots":2,"base":"cost","order_by":"amount"}`).Size(enters(1, 2, 3, 4, 5), ctx)
	if len(got) != 2 {
		t.Fatalf("2 个空位应当只下 2 笔，得到 %d 笔", len(got))
	}
	if got[0].Instrument != 5 || got[1].Instrument != 4 {
		t.Errorf("应当按成交额取 5、4，得到 %d、%d", got[0].Instrument, got[1].Instrument)
	}
}

// TestOrderByVolumeUsesVolume order_by=volume 时按成交量排。
//
// **成交量跨标的不可比**（100 股 300 元的与 100 股 3 元的是同一个成交量，
// 而流动性差 100 倍），所以默认是成交额。但需求方要按成交量时给得出来。
func TestOrderByVolumeUsesVolume(t *testing.T) {
	ctx := newCtx(5, 1_000_000)
	for i := 1; i <= 5; i++ {
		b := ctx.bars[mktdata.InstrumentID(i)]
		b.Amount = int64(i) * 1_000_000_000
		b.Volume = int64(6-i) * 1_000_000
		ctx.bars[mktdata.InstrumentID(i)] = b
	}
	got := mustSizer(t, "equal_weight",
		`{"slots":2,"base":"cost","order_by":"volume"}`).Size(enters(1, 2, 3, 4, 5), ctx)
	if len(got) != 2 {
		t.Fatalf("2 个空位应当只下 2 笔，得到 %d 笔", len(got))
	}
	if got[0].Instrument != 1 || got[1].Instrument != 2 {
		t.Errorf("应当按成交量取 1、2，得到 %d、%d", got[0].Instrument, got[1].Instrument)
	}
}

// TestOrderBySignalKeepsOriginalOrder order_by=signal 不重排。
func TestOrderBySignalKeepsOriginalOrder(t *testing.T) {
	ctx := newCtx(5, 1_000_000)
	for i := 1; i <= 5; i++ {
		b := ctx.bars[mktdata.InstrumentID(i)]
		b.Amount = int64(6-i) * 1_000_000_000 // 1 号成交额最大
		ctx.bars[mktdata.InstrumentID(i)] = b
	}
	got := mustSizer(t, "equal_weight",
		`{"slots":2,"base":"cost","order_by":"signal"}`).Size(enters(3, 1, 2), ctx)
	if len(got) != 2 || got[0].Instrument != 3 || got[1].Instrument != 1 {
		t.Fatalf("应当保持策略给的顺序 3、1，得到 %+v", got)
	}
}

// TestRankIsStableOnTies 成交额相同时按 ID 兜底定序。
//
// **不定序的话同一份配置两次跑出的顺序不同**，C5 就在这里失守。
func TestRankIsStableOnTies(t *testing.T) {
	ctx := newCtx(5, 1_000_000)
	for i := 1; i <= 5; i++ {
		b := ctx.bars[mktdata.InstrumentID(i)]
		b.Amount = 1_000_000_000 // 全都一样
		ctx.bars[mktdata.InstrumentID(i)] = b
	}
	for round := 0; round < 20; round++ {
		got := mustSizer(t, "equal_weight",
			`{"slots":2,"base":"cost"}`).Size(enters(5, 3, 1, 4, 2), ctx)
		if len(got) != 2 || got[0].Instrument != 1 || got[1].Instrument != 2 {
			t.Fatalf("第 %d 次：成交额相同应按 ID 升序兜底，得到 %+v", round, got)
		}
	}
}

// TestExitsComeBeforeEntries 清仓排在建仓之前 —— 卖出释放的钱正是买入要用的。
func TestExitsComeBeforeEntries(t *testing.T) {
	ctx := newCtx(3, 1_000_000)
	seedPosition(ctx.pf, 1, 1000)
	ctx.avail[1] = 1000

	sigs := []Signal{
		{Instrument: 2, Kind: SignalEnter, Side: SideBuy},
		{Instrument: 1, Kind: SignalExit, Side: SideSell},
	}
	got := mustSizer(t, "equal_weight", `{"slots":10,"base":"cost"}`).Size(sigs, ctx)
	if len(got) != 2 {
		t.Fatalf("应当两笔都出，得到 %d 笔", len(got))
	}
	if got[0].Side != SideSell || got[0].Instrument != 1 {
		t.Errorf("清仓应排在最前，得到 %+v", got[0])
	}
}

// ---- 预算要覆盖摩擦 ----

// TestBudgetCoversFriction 定量必须把手续费与滑点留出来。
//
// 撮合校验的是「金额 + 费用 + 滑点 ≤ 购买力」。只按金额定量的话，
// 按 100% 权益下注算出的数量刚好花光资金，撮合必然因为那点手续费
// **整单被拒**，报「现金不足」—— 看不出差的只是手续费。
// 实测：加密 100% 下注从 126 次「现金不足」降到 4 次。
func TestBudgetCoversFriction(t *testing.T) {
	ctx := newCtx(1, 1_000_000) // 10,000.00 元
	ctx.frictionPPM = 10_000    // 1% 摩擦，够粗，规整到 100 股也看得出来

	got := mustSizer(t, "pct_equity", `{"pct":100,"base":"cost"}`).Size(enters(1), ctx)
	if len(got) != 1 {
		t.Fatalf("应当下出一笔，得到 %d 笔", len(got))
	}
	inst := ctx.insts[1]
	amount := NotionalCents(inst, ctx.bars[1].Close, got[0].Qty)
	friction := ctx.FrictionCents(inst, SideBuy, got[0].Qty, amount)
	if amount+friction > 1_000_000 {
		t.Fatalf("金额 %d + 摩擦 %d 超出了 1000000 —— 撮合必然拒单",
			amount, friction)
	}
	// 但也不该缩得太狠：留 1% 摩擦，990 股是那个刚好放得下的档
	if got[0].Qty != 900 && got[0].Qty != 1000 {
		t.Logf("买入 %d 股（金额 %d + 摩擦 %d）", got[0].Qty, amount, friction)
	}
	if got[0].Qty < 900 {
		t.Errorf("缩得过头了：只买了 %d 股", got[0].Qty)
	}
}

// TestBudgetCappedByBuyingPower 打开 fit_cash 后，预算再大也不超过账户拿得出的钱。
//
// 按 100% **权益**下注时，能动用的其实只有**现金** —— 持仓那部分不是流动资金。
//
// **默认关闭**：它把「钱不够就不买」改成「钱不够就少买」，
// 那是一条仓位政策而不是 bug 修复。实测 macd_cross 打开后
// 从 +17.85% 变成 −17.01%（成交 1833 → 1929）。
func TestBudgetCappedByBuyingPower(t *testing.T) {
	ctx := newCtx(2, 1_000_000)
	// 现金只剩 20 万，其余都在持仓里
	st := ctx.pf.Snapshot()
	st.Cash = 200_000
	st.Positions = map[mktdata.InstrumentID]*Position{
		1: {Total: 8000, CostCents: 800_000, Lots: []lot{{Qty: 8000}}},
	}
	ctx.pf.Restore(st)
	ctx.equity = 1_000_000

	got := mustSizer(t, "pct_equity",
		`{"pct":100,"base":"equity","fit_cash":true}`).Size(enters(2), ctx)
	if len(got) != 1 {
		t.Fatalf("应当下出一笔，得到 %d 笔", len(got))
	}
	amount := NotionalCents(ctx.insts[2], ctx.bars[2].Close, got[0].Qty)
	if !ctx.pf.AffordOpen(amount, 0) {
		t.Fatalf("算出的数量买不起：金额 %d，可用 %d",
			amount, ctx.pf.BuyingPowerCents())
	}
}
