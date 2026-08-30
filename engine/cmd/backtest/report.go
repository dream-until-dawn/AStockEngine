package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/metrics"
)

// printReport 打印绩效报告。
//
// 每个年化量都跟着把**分母**印出来。夏普是最容易被悄悄美化的数字 ——
// 换个年化系数、换个无风险利率就能变好看，而这两个数通常不写在报告里。
func printReport(r metrics.Result) {
	fmt.Println("=== 绩效 ===")
	fmt.Printf("  区间 %d ~ %d  %d 个交易日 ≈ %.2f 年\n",
		r.FromDay, r.ToDay, r.Steps, r.Years)
	fmt.Printf("  年化系数 %.2f 交易日/年（**由日历数出，不是 252**）  无风险利率 %.2f%%\n",
		r.TradingDaysPerYear, float64(r.RiskFreePPM)/10_000)
	fmt.Println()

	fmt.Printf("  总收益        %s\n", pct(r.TotalReturn))
	ann := pct(r.AnnualReturn)
	if !r.AnnualReliable {
		ann += "  ⚠ 样本不足一年，年化是外推出来的"
	}
	fmt.Printf("  年化收益      %s\n", ann)
	fmt.Printf("  年化波动      %s\n", pctAbs(r.AnnualVol))
	fmt.Printf("  下行波动      %s\n", pctAbs(r.DownsideVol))
	fmt.Printf("  夏普          %s\n", ratio(r.Sharpe))
	fmt.Printf("  索提诺        %s\n", ratio(r.Sortino))
	fmt.Printf("  卡玛          %s\n", ratio(r.Calmar))
	fmt.Println()

	d := r.MaxDrawdown
	fmt.Printf("  最大回撤      %s   %d → %d（%d 个交易日）\n",
		pctAbs(d.Ratio), d.PeakDay, d.TroughDay, d.TroughSteps)
	fmt.Printf("                %.2f %s → %.2f %s",
		cents(d.PeakCents), money, cents(d.TroughCents), money)
	if d.RecoveryDay != 0 {
		fmt.Printf("，%d 恢复（%d 个交易日）\n", d.RecoveryDay, d.RecoverySteps)
	} else if d.Ratio > 0 {
		fmt.Println("，**至样本末仍未恢复**")
	} else {
		fmt.Println()
	}
	fmt.Println()

	t := r.Trades
	fmt.Printf("  完整轮次      %d 轮（%d 赢 / %d 输 / %d 平）\n",
		t.RoundTrips, t.Wins, t.Losses, t.Flat)
	fmt.Printf("  胜率          %s      盈亏比 %s\n", pctAbs(t.WinRate), ratio(t.ProfitFactor))
	fmt.Printf("  平均盈利      %.2f %s    平均亏损 %.2f %s    平均持有 %.1f 天\n",
		cents(t.AvgWinCents), money, cents(t.AvgLossCents), money, t.AvgHoldDays)
	if t.BonusTrips > 0 {
		fmt.Printf("                其中 %d 轮的份额来自送股/转增（零成本入队）\n", t.BonusTrips)
	}
	if t.OpenPositions > 0 {
		// 未平仓不计入胜率，但必须报出来 —— 藏起来会让胜率失真
		// 数量按定点值跨标的相加没有意义（加密的 scale 是 1e8），
		// 报开仓金额 —— 它才是跨标的可加的量
		fmt.Printf("  未平仓        %d 只，开仓金额 %.2f %s，不计入胜率\n",
			t.OpenPositions, cents(t.OpenCostCents), money)
	}
	fmt.Println()

	fmt.Printf("  单边成交额    %.2f %s    年化换手 %.2f 倍\n",
		cents(r.TurnoverCents), money, r.Turnover)
	fmt.Printf("  费用 %.2f %s + 滑点 %.2f %s = 摩擦 %.2f %s（占成交额 %s）\n",
		cents(r.FeeCents), money, cents(r.SlippageCents), money,
		cents(r.FeeCents+r.SlippageCents), money,
		pctOf(r.FeeCents+r.SlippageCents, r.TurnoverCents))

	if r.Bench != nil {
		printBenchmark(r)
	}
	fmt.Println()
}

func printBenchmark(r metrics.Result) {
	b := r.Bench
	fmt.Println()
	fmt.Printf("  === 对标 %s ===\n", b.Name)
	if b.Covered < 2 {
		fmt.Printf("    基准在本区间内无数据（覆盖 %d / %d 个时点），不计超额\n",
			b.Covered, b.Total)
		return
	}
	cover := float64(b.Covered) / float64(b.Total)
	fmt.Printf("    覆盖 %d / %d 个时点（%s）",
		b.Covered, b.Total, strings.TrimSpace(pctAbs(cover)))
	if b.Covered < b.Total {
		// 覆盖不到的区间**不计超额**。当成 0 收益补齐会凭空造出超额收益 ——
		// 数据里没有指数，宽基 ETF 最早到 2012，这不是偶发情况
		fmt.Printf("  ⚠ 未覆盖区间不计超额\n")
	} else {
		fmt.Println()
	}
	fmt.Printf("    策略 %s   基准 %s   超额 %s\n",
		pct(b.StrategyReturn), pct(b.Return), pct(b.Excess))
	fmt.Printf("    Beta %s   Alpha %s（年化）   信息比率 %s\n",
		ratio(b.Beta), pct(b.Alpha), ratio(b.InformationRatio))
}

// pctAbs 用于没有方向的量：波动、回撤、胜率、覆盖率。
// 给它们加正负号只会让人多看一眼才反应过来。
func pctAbs(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "     —   "
	}
	return fmt.Sprintf("%8.2f%%", v*100)
}

func pct(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "     —   "
	}
	return fmt.Sprintf("%+8.2f%%", v*100)
}

func pctOf(a, b int64) string {
	if b == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", float64(a)/float64(b)*100)
}

func ratio(v float64) string {
	if math.IsNaN(v) {
		return "     —  "
	}
	if math.IsInf(v, 1) {
		return "      ∞ "
	}
	if math.IsInf(v, -1) {
		return "     -∞ "
	}
	return fmt.Sprintf("%8.3f", v)
}

// warnLine 把告警排成一段，便于在报告末尾统一呈现。
func warnLine(ws []string) string {
	if len(ws) == 0 {
		return ""
	}
	return "  ⚠ " + strings.Join(ws, "\n  ⚠ ")
}
