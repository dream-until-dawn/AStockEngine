package metrics

import (
	"math"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

const eps = 1e-9

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s：期望 %.10f，得到 %.10f（差 %.3e）", name, want, got, got-want)
	}
}

// ---- 手工核算：一段可以拿计算器验的净值序列 ----
//
// 5 个交易日，初始 100 万元（100,000,000 分）：
//
//	日   权益(分)      日收益
//	1    110,000,000  +10%      （相对初始资金）
//	2    121,000,000  +10%
//	3     96,800,000  -20%
//	4    108,416,000  +12%
//	5    102,995,200   -5%
//
// 全部数字都取得能整除，便于手算。

func handCurve() Curve {
	return Curve{
		Days:   []int32{20240102, 20240103, 20240104, 20240105, 20240108},
		Equity: []int64{110_000_000, 121_000_000, 96_800_000, 108_416_000, 102_995_200},
	}
}

func TestTotalAndAnnualReturn(t *testing.T) {
	r := Compute(Input{
		Curve: handCurve(), InitialCents: 100_000_000, TradingDaysPerYear: 243,
	})
	// 总收益 = 102,995,200 / 100,000,000 − 1 = 0.029952
	near(t, "总收益", r.TotalReturn, 0.029952, eps)

	// 年数 = 5 / 243；年化 = 1.029952^(243/5) − 1
	near(t, "年数", r.Years, 5.0/243.0, eps)
	want := math.Pow(1.029952, 243.0/5.0) - 1
	near(t, "年化收益", r.AnnualReturn, want, 1e-9)

	// 5 个交易日远不足一年，年化必须被标为不可靠
	if r.AnnualReliable {
		t.Error("5 个交易日的年化收益不该标为可靠")
	}
}

// TestAnnualisationUsesCalendarNot252 钉住第一个坑：年化系数不是 252。
func TestAnnualisationUsesCalendarNot252(t *testing.T) {
	in := Input{Curve: handCurve(), InitialCents: 100_000_000}

	in.TradingDaysPerYear = 243
	byCalendar := Compute(in)
	in.TradingDaysPerYear = 252
	by252 := Compute(in)

	if byCalendar.AnnualReturn == by252.AnnualReturn {
		t.Fatal("年化系数换了却算出同一个数 —— 它没被用上")
	}
	// 252 会把年化收益与年化波动同时抬高
	if by252.AnnualReturn <= byCalendar.AnnualReturn {
		t.Errorf("252 应当抬高年化收益：243→%.4f，252→%.4f",
			byCalendar.AnnualReturn, by252.AnnualReturn)
	}
	if by252.AnnualVol <= byCalendar.AnnualVol {
		t.Errorf("252 应当抬高年化波动：243→%.4f，252→%.4f",
			byCalendar.AnnualVol, by252.AnnualVol)
	}
	if r := Compute(in); r.TradingDaysPerYear != 252 {
		t.Error("实际使用的年化系数必须原样报出来")
	}
}

func TestVolatilityAndSharpe(t *testing.T) {
	r := Compute(Input{
		Curve: handCurve(), InitialCents: 100_000_000, TradingDaysPerYear: 243,
	})
	// 日收益：0.10, 0.10, -0.20, 0.12, -0.05
	rets := []float64{0.10, 0.10, -0.20, 0.12, -0.05}
	// 手算：均值 = 0.07/5 = 0.014
	near(t, "日收益均值", mean(rets), 0.014, eps)
	// 样本方差 = Σ(x−μ)²/(n−1)
	//  (0.086)² + (0.086)² + (−0.214)² + (0.106)² + (−0.064)²
	//  = 0.007396 + 0.007396 + 0.045796 + 0.011236 + 0.004096 = 0.07592
	//  / 4 = 0.01898  → σ = 0.137767...
	near(t, "日收益样本方差", variance(rets), 0.01898, 1e-12)
	wantVol := math.Sqrt(0.01898) * math.Sqrt(243)
	near(t, "年化波动", r.AnnualVol, wantVol, 1e-9)

	// 无风险利率为 0 时夏普 = 年化收益 / 年化波动
	near(t, "夏普", r.Sharpe, r.AnnualReturn/wantVol, 1e-9)
}

// TestRiskFreeEntersSharpe 无风险利率必须真的进夏普，且被原样报出。
func TestRiskFreeEntersSharpe(t *testing.T) {
	in := Input{Curve: handCurve(), InitialCents: 100_000_000, TradingDaysPerYear: 243}
	zero := Compute(in)
	in.RiskFreePPM = 30_000 // 3%
	three := Compute(in)

	near(t, "夏普差", zero.Sharpe-three.Sharpe, 0.03/zero.AnnualVol, 1e-9)
	if three.RiskFreePPM != 30_000 {
		t.Error("无风险利率必须原样报出 —— 夏普最容易被悄悄美化")
	}
}

func TestMaxDrawdown(t *testing.T) {
	r := Compute(Input{
		Curve: handCurve(), InitialCents: 100_000_000, TradingDaysPerYear: 243,
	})
	// 峰值 121,000,000（第 2 日），谷底 96,800,000（第 3 日）
	// 回撤 = 1 − 96,800,000/121,000,000 = 0.2
	near(t, "最大回撤", r.MaxDrawdown.Ratio, 0.20, 1e-12)
	if r.MaxDrawdown.PeakDay != 20240103 || r.MaxDrawdown.TroughDay != 20240104 {
		t.Errorf("回撤区间：期望 20240103→20240104，得到 %d→%d",
			r.MaxDrawdown.PeakDay, r.MaxDrawdown.TroughDay)
	}
	// 样本末仍未回到 121,000,000，恢复日应为 0
	if r.MaxDrawdown.RecoveryDay != 0 {
		t.Errorf("尚未恢复时 RecoveryDay 应为 0，得到 %d", r.MaxDrawdown.RecoveryDay)
	}
	near(t, "卡玛", r.Calmar, r.AnnualReturn/0.20, 1e-9)
}

func TestDrawdownRecovery(t *testing.T) {
	r := Compute(Input{
		Curve: Curve{
			Days:   []int32{20240102, 20240103, 20240104, 20240105},
			Equity: []int64{100, 80, 90, 110},
		},
		InitialCents: 100, TradingDaysPerYear: 243,
	})
	near(t, "最大回撤", r.MaxDrawdown.Ratio, 0.20, 1e-12)
	if r.MaxDrawdown.RecoveryDay != 20240105 {
		t.Errorf("恢复日：期望 20240105，得到 %d", r.MaxDrawdown.RecoveryDay)
	}
	if r.MaxDrawdown.RecoverySteps != 2 {
		t.Errorf("恢复步数：期望 2，得到 %d", r.MaxDrawdown.RecoverySteps)
	}
}

// ---- 逐轮交易 ----

func fill(day int32, id int, side trading.Side, price, qty, fee, slip int64) trading.Fill {
	return trading.Fill{
		Order: trading.Order{Instrument: mktdata.InstrumentID(id), Side: side, Qty: qty},
		At:    mktdata.TimePoint{TradingDay: day},
		Price: price, Qty: qty,
		// 名义额在生产路径上由撮合器按标的的 scale 与合约乘数算好。
		// 这里手工造 Fill，用 A 股口径补上 —— 漏填就是 0，
		// 而 0 不报错，只会让这一笔从成交额与逐轮统计里安静地消失
		AmountCents:   trading.AmountCents(price, qty),
		Fee:           trading.FeeBreakdown{Items: map[string]int64{"commission": fee}, Total: fee},
		SlippageCents: slip,
	}
}

// TestRoundTripsHandChecked 手工核算三笔来回。
//
//	标的 1：10.000 元买 1000 股，费 50 分、滑点 50 分 → 成本 1,000,000+100 = 1,000,100
//	        12.000 元卖 1000 股，费 60 分、滑点 60 分 → 收入 1,200,000−120 = 1,199,880
//	        盈亏 +199,780 分  ✅ 赢
//	标的 2：20.000 元买 500 股，费 0、滑点 0 → 成本 1,000,000
//	        18.000 元卖 500 股，费 0、滑点 0 → 收入   900,000
//	        盈亏 −100,000 分  ❌ 输
//	标的 3：5.000 元买 2000 股 → 成本 1,000,000
//	        5.000 元卖 2000 股 → 收入 1,000,000
//	        盈亏 0            ➖ 平
func TestRoundTripsHandChecked(t *testing.T) {
	fills := []trading.Fill{
		fill(20240102, 1, trading.SideBuy, 10_000, 1000, 50, 50),
		fill(20240103, 2, trading.SideBuy, 20_000, 500, 0, 0),
		fill(20240104, 3, trading.SideBuy, 5_000, 2000, 0, 0),
		fill(20240110, 1, trading.SideSell, 12_000, 1000, 60, 60),
		fill(20240111, 2, trading.SideSell, 18_000, 500, 0, 0),
		fill(20240112, 3, trading.SideSell, 5_000, 2000, 0, 0),
	}
	trips, open := MatchRoundTrips(fills, false)
	if len(trips) != 3 {
		t.Fatalf("期望 3 轮，得到 %d", len(trips))
	}
	if len(open) != 0 {
		t.Errorf("全部平仓后不该有未平仓，得到 %v", open)
	}
	want := map[mktdata.InstrumentID]int64{1: 199_780, 2: -100_000, 3: 0}
	for _, tr := range trips {
		if tr.PnLCents != want[tr.Instrument] {
			t.Errorf("标的 %d：期望盈亏 %d 分，得到 %d 分",
				tr.Instrument, want[tr.Instrument], tr.PnLCents)
		}
	}

	st := computeTrades(fills, false)
	if st.RoundTrips != 3 || st.Wins != 1 || st.Losses != 1 || st.Flat != 1 {
		t.Errorf("轮次统计：期望 3/1赢/1输/1平，得到 %d/%d/%d/%d",
			st.RoundTrips, st.Wins, st.Losses, st.Flat)
	}
	// 胜率 = 1/3（平局计入分母 —— 它确实是一轮交易）
	near(t, "胜率", st.WinRate, 1.0/3.0, 1e-12)
	// 盈亏比 = 平均盈利 199,780 / 平均亏损 100,000 = 1.9978
	near(t, "盈亏比", st.ProfitFactor, 1.9978, 1e-12)
	// 标的 1 持有 20240102→20240110 = 8 天，标的 2 与 3 各 8 天
	near(t, "平均持有天数", st.AvgHoldDays, 8, 1e-12)
}

// TestRoundTripPartialFIFO 部分平仓要按 FIFO 逐层配对，成本按份额比例分摊。
func TestRoundTripPartialFIFO(t *testing.T) {
	fills := []trading.Fill{
		fill(20240102, 1, trading.SideBuy, 10_000, 1000, 0, 0),  // 成本 1,000,000
		fill(20240103, 1, trading.SideBuy, 20_000, 1000, 0, 0),  // 成本 2,000,000
		fill(20240110, 1, trading.SideSell, 30_000, 1500, 0, 0), // 收入 4,500,000
	}
	trips, open := MatchRoundTrips(fills, false)
	if len(trips) != 2 {
		t.Fatalf("1500 股跨两层，期望 2 轮，得到 %d", len(trips))
	}
	// 第一层 1000 股：成本 1,000,000，收入 4,500,000×1000/1500 = 3,000,000 → +2,000,000
	if trips[0].PnLCents != 2_000_000 {
		t.Errorf("第一层：期望 +2,000,000，得到 %d", trips[0].PnLCents)
	}
	// 第二层 500 股：成本 2,000,000×500/1000 = 1,000,000，收入 1,500,000 → +500,000
	if trips[1].PnLCents != 500_000 {
		t.Errorf("第二层：期望 +500,000，得到 %d", trips[1].PnLCents)
	}
	if open[1].LongQty != 500 {
		t.Errorf("应余 500 股未平仓，得到 %d", open[1])
	}
}

// TestRoundTripBonusShares 送股份额按零成本入队，并被标出来。
//
// 买 1000 股后 10 送 10 变成 2000 股，全部卖出。多出的 1000 股没有买入记录 ——
// 它们的成本已经付在原有份额上，再计一次会让盈亏比虚高。
func TestRoundTripBonusShares(t *testing.T) {
	fills := []trading.Fill{
		fill(20240102, 1, trading.SideBuy, 20_000, 1000, 0, 0),  // 成本 2,000,000
		fill(20240110, 1, trading.SideSell, 10_000, 2000, 0, 0), // 收入 2,000,000
	}
	trips, open := MatchRoundTrips(fills, false)
	if len(trips) != 2 {
		t.Fatalf("期望 2 轮（买入层 + 送股层），得到 %d", len(trips))
	}
	if len(open) != 0 {
		t.Errorf("不该有未平仓，得到 %v", open)
	}
	var total int64
	bonus := 0
	for _, tr := range trips {
		total += tr.PnLCents
		if tr.FromBonus {
			bonus++
		}
	}
	// 总盈亏必须是 0：付 2,000,000 收 2,000,000，除权前后价值不变
	if total != 0 {
		t.Errorf("送股不改变总价值，期望盈亏 0，得到 %d", total)
	}
	if bonus != 1 {
		t.Errorf("应有 1 轮标为送股来源，得到 %d", bonus)
	}
}

// TestOpenPositionsExcluded 未平仓不计入胜率，但数量要报出来。
func TestOpenPositionsExcluded(t *testing.T) {
	st := computeTrades([]trading.Fill{
		fill(20240102, 1, trading.SideBuy, 10_000, 1000, 0, 0),
		fill(20240103, 2, trading.SideBuy, 10_000, 500, 0, 0),
		fill(20240110, 1, trading.SideSell, 12_000, 1000, 0, 0),
	}, false)
	if st.RoundTrips != 1 || st.Wins != 1 {
		t.Errorf("只有 1 轮平仓，期望 1 轮 1 赢，得到 %d 轮 %d 赢", st.RoundTrips, st.Wins)
	}
	if st.WinRate != 1.0 {
		t.Errorf("胜率应为 100%%（未平仓不参与），得到 %.4f", st.WinRate)
	}
	if st.OpenPositions != 1 || st.OpenQty != 500 {
		t.Errorf("未平仓：期望 1 只 500 股，得到 %d 只 %d 股",
			st.OpenPositions, st.OpenQty)
	}
}

// ---- 基准 ----

// TestBenchmarkPartialCoverage 基准覆盖不到的区间不计超额，覆盖比例要报出来。
//
// 这是第三个坑：数据里没有指数，只能用 ETF 代理，而 510300 最早到 2012。
// 把覆盖不到的区间按 0 收益补齐会凭空造出一段超额收益。
func TestBenchmarkPartialCoverage(t *testing.T) {
	// 策略 4 天，基准只覆盖后 2 天
	bench := Curve{Days: []int32{20240104, 20240105}, Equity: []int64{100, 110}}
	r := Compute(Input{
		Curve: Curve{
			Days:   []int32{20240102, 20240103, 20240104, 20240105},
			Equity: []int64{100, 150, 200, 220},
		},
		InitialCents: 100, TradingDaysPerYear: 243,
		Benchmark: &bench, BenchmarkName: "510300",
	})
	if r.Bench == nil {
		t.Fatal("应当有基准统计")
	}
	if r.Bench.Covered != 2 || r.Bench.Total != 4 {
		t.Errorf("覆盖：期望 2/4，得到 %d/%d", r.Bench.Covered, r.Bench.Total)
	}
	// 覆盖区间上：策略 200→220 = +10%，基准 100→110 = +10%，超额 0
	near(t, "基准收益", r.Bench.Return, 0.10, 1e-12)
	near(t, "策略（覆盖区间）", r.Bench.StrategyReturn, 0.10, 1e-12)
	near(t, "超额", r.Bench.Excess, 0, 1e-12)
	// 若错误地把前两天按 0 补齐，策略收益会变成 +120%，超额凭空多出一大截
	if math.Abs(r.Bench.Excess-1.10) < 0.01 {
		t.Error("超额被算成了全区间收益 —— 覆盖不到的区间被补齐了")
	}
}

func TestEmptyInputs(t *testing.T) {
	if r := Compute(Input{}); r.Steps != 0 {
		t.Error("空输入不该崩，也不该编出数字")
	}
	if trips, open := MatchRoundTrips(nil, false); len(trips) != 0 || len(open) != 0 {
		t.Error("空成交流应当给出空结果")
	}
}

// ---- 双向（合约）配对 ----

// hedgeFill 造一笔带开平标记的成交。
func hedgeFill(day int32, id int, side trading.Side, reduce bool,
	price, qty, fee int64) trading.Fill {

	f := fill(day, id, side, price, qty, fee, 0)
	f.Reduce = reduce
	return f
}

// TestHedgeShortRoundTrip 做空一轮：高开低平算赢。
//
// 现货口径下这一对成交会被读成「卖掉一个不存在的多头，再买回来」，
// 盈亏符号正好相反 —— 断言的就是符号。
func TestHedgeShortRoundTrip(t *testing.T) {
	fills := []trading.Fill{
		hedgeFill(20240102, 1, trading.SideSell, false, 12_000, 1000, 100), // 开空
		hedgeFill(20240110, 1, trading.SideBuy, true, 10_000, 1000, 100),   // 平空
	}
	trips, open := MatchRoundTrips(fills, true)
	if len(trips) != 1 {
		t.Fatalf("配出 %d 轮，想要 1 轮", len(trips))
	}
	tr := trips[0]
	if !tr.Short {
		t.Errorf("这一轮应标为做空")
	}
	// 开仓收 1,200,000 − 100 摩擦；平仓付 1,000,000 + 100 摩擦
	if tr.ProceedCents != 1_200_000-100 || tr.CostCents != 1_000_000+100 {
		t.Errorf("金额 = 收 %d / 付 %d，想要 1199900 / 1000100",
			tr.ProceedCents, tr.CostCents)
	}
	if tr.PnLCents != 199_800 {
		t.Errorf("做空下跌应盈利 199800，得到 %d", tr.PnLCents)
	}
	if tr.HoldDays != 8 {
		t.Errorf("持有 %d 天，想要 8", tr.HoldDays)
	}
	if len(open) != 0 {
		t.Errorf("平光了却报 %d 只未平仓", len(open))
	}
}

// TestHedgeShortLosesWhenPriceRises 做空上涨算输 —— 符号的另一半。
func TestHedgeShortLosesWhenPriceRises(t *testing.T) {
	trips, _ := MatchRoundTrips([]trading.Fill{
		hedgeFill(20240102, 1, trading.SideSell, false, 10_000, 1000, 0),
		hedgeFill(20240110, 1, trading.SideBuy, true, 12_000, 1000, 0),
	}, true)
	if len(trips) != 1 || trips[0].PnLCents != -200_000 {
		t.Fatalf("做空上涨应亏 200000，得到 %+v", trips)
	}
}

// TestHedgeDoesNotCrossMatch 多空是两个独立队列，不许互相配对。
//
// **这是双向配对唯一真正的难点**。合用一个队列时，「开空」会去
// 平掉手里的多头，于是一笔开仓凭空变成一轮平仓 ——
// 实测中 336 笔成交配出了 314 轮（上限本是 168）。
func TestHedgeDoesNotCrossMatch(t *testing.T) {
	fills := []trading.Fill{
		hedgeFill(20240102, 1, trading.SideBuy, false, 10_000, 1000, 0),  // 开多
		hedgeFill(20240103, 1, trading.SideSell, false, 11_000, 1000, 0), // 开空，不是平多
	}
	trips, open := MatchRoundTrips(fills, true)
	if len(trips) != 0 {
		t.Fatalf("两笔都是开仓，不该配出任何轮次，得到 %d 轮：%+v", len(trips), trips)
	}
	leg := open[1]
	if leg.LongQty != 1000 || leg.ShortQty != 1000 {
		t.Fatalf("未平仓敞口 = 多 %d / 空 %d，想要各 1000", leg.LongQty, leg.ShortQty)
	}
	// 一个标的两个方向仍然只算一只
	if len(open) != 1 {
		t.Fatalf("未平仓标的数 = %d，想要 1", len(open))
	}
}

// TestHedgeClosesMatchOwnSide 平多只吃多头队列，平空只吃空头队列。
func TestHedgeClosesMatchOwnSide(t *testing.T) {
	fills := []trading.Fill{
		hedgeFill(20240102, 1, trading.SideBuy, false, 10_000, 1000, 0),  // 开多 @1.00
		hedgeFill(20240103, 1, trading.SideSell, false, 20_000, 1000, 0), // 开空 @2.00
		hedgeFill(20240104, 1, trading.SideSell, true, 15_000, 1000, 0),  // 平多 @1.50
		hedgeFill(20240105, 1, trading.SideBuy, true, 15_000, 1000, 0),   // 平空 @1.50
	}
	trips, open := MatchRoundTrips(fills, true)
	if len(trips) != 2 {
		t.Fatalf("配出 %d 轮，想要 2 轮", len(trips))
	}
	if len(open) != 0 {
		t.Fatalf("都平光了却报 %d 只未平仓", len(open))
	}
	var long, short *RoundTrip
	for i := range trips {
		if trips[i].Short {
			short = &trips[i]
		} else {
			long = &trips[i]
		}
	}
	if long == nil || short == nil {
		t.Fatalf("应当一多一空，得到 %+v", trips)
	}
	// 多头 1.00 → 1.50 赚 500,000；空头 2.00 → 1.50 也赚 500,000
	if long.PnLCents != 500_000 {
		t.Errorf("多头轮盈亏 = %d，想要 500000", long.PnLCents)
	}
	if short.PnLCents != 500_000 {
		t.Errorf("空头轮盈亏 = %d，想要 500000", short.PnLCents)
	}
	// 平仓日必须各归各的
	if long.CloseDay != 20240104 || short.CloseDay != 20240105 {
		t.Errorf("平仓日错配：多 %d 空 %d", long.CloseDay, short.CloseDay)
	}
}

// TestSpotIgnoresReduce 现货口径不看 Reduce —— 买永远是开、卖永远是平。
//
// A 股的减仓卖出与清仓卖出都是平多，没有「卖出开仓」这回事。
// 若现货也去读 Reduce，一笔没标 Reduce 的减仓就会被当成开空。
func TestSpotIgnoresReduce(t *testing.T) {
	for _, reduce := range []bool{false, true} {
		trips, open := MatchRoundTrips([]trading.Fill{
			hedgeFill(20240102, 1, trading.SideBuy, reduce, 10_000, 1000, 0),
			hedgeFill(20240110, 1, trading.SideSell, reduce, 12_000, 1000, 0),
		}, false)
		if len(trips) != 1 || trips[0].Short || trips[0].PnLCents != 200_000 {
			t.Fatalf("reduce=%v：现货应配出 1 轮做多 +200000，得到 %+v", reduce, trips)
		}
		if len(open) != 0 {
			t.Fatalf("reduce=%v：不该有未平仓", reduce)
		}
	}
}

// TestHedgePartialClose 部分平空按比例分摊开仓所得。
func TestHedgePartialClose(t *testing.T) {
	trips, open := MatchRoundTrips([]trading.Fill{
		hedgeFill(20240102, 1, trading.SideSell, false, 20_000, 1000, 0), // 开空 2,000,000
		hedgeFill(20240110, 1, trading.SideBuy, true, 15_000, 400, 0),    // 平掉四成
	}, true)
	if len(trips) != 1 {
		t.Fatalf("配出 %d 轮，想要 1 轮", len(trips))
	}
	// 开仓所得的四成 = 800,000；买回花费 = 1.50 × 400 = 600,000
	if trips[0].ProceedCents != 800_000 || trips[0].CostCents != 600_000 {
		t.Errorf("按比例分摊错了：收 %d / 付 %d，想要 800000 / 600000",
			trips[0].ProceedCents, trips[0].CostCents)
	}
	if trips[0].PnLCents != 200_000 {
		t.Errorf("盈亏 = %d，想要 200000", trips[0].PnLCents)
	}
	if open[1].ShortQty != 600 {
		t.Errorf("应余 600 张空头，得到 %d", open[1].ShortQty)
	}
	// 剩下六成的开仓所得也要留在队列里
	if open[1].CostCents != 1_200_000 {
		t.Errorf("未平仓金额 = %d，想要 1200000", open[1].CostCents)
	}
}

// TestOpenCostIsCrossInstrumentAddable 未平仓金额跨标的可加，数量不可加。
func TestOpenCostIsCrossInstrumentAddable(t *testing.T) {
	st := computeTrades([]trading.Fill{
		fill(20240102, 1, trading.SideBuy, 10_000, 1000, 0, 0), // 1,000,000
		fill(20240103, 2, trading.SideBuy, 20_000, 500, 0, 0),  //   1,000,000
	}, false)
	if st.OpenPositions != 2 {
		t.Fatalf("未平仓标的数 = %d，想要 2", st.OpenPositions)
	}
	if st.OpenCostCents != 2_000_000 {
		t.Errorf("未平仓金额 = %d，想要 2000000", st.OpenCostCents)
	}
}
