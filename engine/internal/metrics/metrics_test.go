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
	trips, open := MatchRoundTrips(fills)
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

	st := computeTrades(fills)
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
	trips, open := MatchRoundTrips(fills)
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
	if open[1] != 500 {
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
	trips, open := MatchRoundTrips(fills)
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
	})
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
	if trips, open := MatchRoundTrips(nil); len(trips) != 0 || len(open) != 0 {
		t.Error("空成交流应当给出空结果")
	}
}
