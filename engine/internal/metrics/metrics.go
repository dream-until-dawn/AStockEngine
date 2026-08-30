// Package metrics 由净值序列与成交流算出绩效指标。
//
// 三处刻意与常见实现不同，每一处都有实测支撑：
//
//  1. **年化交易日不写 252。** 252 是美股惯例；本项目日历实测 A 股
//     2005–2025 年均 242.90 天（238–246）。用 252 会让年化收益与年化波动
//     同时偏高，夏普的分子分母各偏一点、不会互相抵消干净。
//     年化系数由 Calendar 数出来传进来（见 mktdata.TradingDaysPerYear）。
//
//  2. **无风险利率默认 0，且必须在报告里印出来。** 写死 3% 会让不同年份的
//     夏普失去可比性 —— 2008 年和 2024 年的无风险利率差得远。
//
//  3. **基准只能是 ETF 代理，且有覆盖区间。** 数据里没有指数（C10 纯技术面，
//     ETL 没拉指数行情），沪深 300 只能用 510300 代理，而它最早到 2012-05-28。
//     覆盖不到的区间**不计超额**，并把覆盖比例报出来 ——
//     当成 0 收益补齐会凭空造出一段超额收益。
package metrics

import (
	"math"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Curve 是一条净值序列。
type Curve struct {
	Days   []int32
	Equity []int64
}

// Len 返回点数。
func (c Curve) Len() int { return len(c.Days) }

// Input 是一次绩效计算的全部输入。
type Input struct {
	Curve        Curve
	InitialCents int64
	Fills        []trading.Fill
	// TradingDaysPerYear 年化系数，**由日历数出来**，不是 252
	TradingDaysPerYear float64
	// RiskFreePPM 年化无风险利率（百万分之一）
	RiskFreePPM int64
	// Benchmark 基准净值序列，可为空。日期需与 Curve 对齐（按交易日取交集）
	Benchmark *Curve
	// BenchmarkName 基准标的代码，供报告标注
	BenchmarkName string
	// Hedge 该市场是否双向持仓。**决定一笔卖出是「平多」还是「开空」**，
	// 判错会把每一次开空都配成一轮凭空的多头轮次。
	// 由 Market.AllowsShort() 给出
	Hedge bool
}

// Drawdown 一次回撤。
type Drawdown struct {
	PeakDay     int32   `json:"peak_day"`
	TroughDay   int32   `json:"trough_day"`
	RecoveryDay int32   `json:"recovery_day"` // 0 表示到样本末仍未恢复
	Ratio       float64 `json:"ratio"`
	PeakCents   int64   `json:"peak_cents"`
	TroughCents int64   `json:"trough_cents"`
	// TroughSteps / RecoverySteps 以交易日计
	TroughSteps   int `json:"trough_steps"`
	RecoverySteps int `json:"recovery_steps"`
}

// TradeStats 是逐轮交易的统计。
type TradeStats struct {
	RoundTrips int     `json:"round_trips"`
	Wins       int     `json:"wins"`
	Losses     int     `json:"losses"`
	Flat       int     `json:"flat"`
	WinRate    float64 `json:"win_rate"`
	// ProfitFactor 盈亏比 = 平均盈利 / 平均亏损（都取绝对值）
	ProfitFactor  float64 `json:"profit_factor"`
	AvgWinCents   int64   `json:"avg_win_cents"`
	AvgLossCents  int64   `json:"avg_loss_cents"`
	GrossWinCents int64   `json:"gross_win_cents"`
	GrossLossCent int64   `json:"gross_loss_cents"`
	AvgHoldDays   float64 `json:"avg_hold_days"`
	// BonusTrips 建仓份额来自送股/转增的轮次数，已计入上面的统计但单独报出
	BonusTrips int `json:"bonus_trips"`
	// OpenPositions 回测结束时仍持有的标的数，**不计入胜率**
	OpenPositions int `json:"open_positions"`
	// OpenQty 未平仓的定点数量之和。
	//
	// **跨标的相加只在同一 scale 下有意义**：A 股全都是 1 股 = 1，
	// 加起来就是股数；加密的 qty_scale 是 1e8，加起来是个没有单位的数。
	// 报告要报的是 OpenCostCents
	OpenQty int64 `json:"open_qty"`
	// OpenCostCents 未平仓位的开仓金额（分）。跨标的可加，
	// 也就是「还有多少钱没结算」的答案
	OpenCostCents int64 `json:"open_cost_cents"`
}

// BenchmarkStats 是相对基准的统计。
type BenchmarkStats struct {
	Name string `json:"name"`
	// Covered / Total 基准覆盖的时点数与样本总时点数
	Covered int     `json:"covered"`
	Total   int     `json:"total"`
	Return  float64 `json:"return"`
	// Excess 超额收益，**只在覆盖区间上计算**
	Excess           float64 `json:"excess"`
	Beta             float64 `json:"beta"`
	Alpha            float64 `json:"alpha"`
	InformationRatio float64 `json:"information_ratio"`
	// StrategyReturn 策略在覆盖区间上的收益，用于与 Return 对照
	StrategyReturn float64 `json:"strategy_return"`
}

// Result 是全部绩效指标。
type Result struct {
	Steps        int     `json:"steps"`
	FromDay      int32   `json:"from_day"`
	ToDay        int32   `json:"to_day"`
	Years        float64 `json:"years"`
	InitialCents int64   `json:"initial_cents"`
	FinalCents   int64   `json:"final_cents"`

	TotalReturn  float64 `json:"total_return"`
	AnnualReturn float64 `json:"annual_return"`
	// AnnualReliable 样本不足一年时年化收益会被外推放大，此时为 false
	AnnualReliable bool `json:"annual_reliable"`

	AnnualVol   float64  `json:"annual_vol"`
	DownsideVol float64  `json:"downside_vol"`
	Sharpe      float64  `json:"sharpe"`
	Sortino     float64  `json:"sortino"`
	Calmar      float64  `json:"calmar"`
	MaxDrawdown Drawdown `json:"max_drawdown"`

	// TurnoverCents 单边成交额合计；Turnover 为年化换手率
	TurnoverCents int64   `json:"turnover_cents"`
	Turnover      float64 `json:"turnover"`

	FeeCents      int64 `json:"fee_cents"`
	SlippageCents int64 `json:"slippage_cents"`

	Trades TradeStats      `json:"trades"`
	Bench  *BenchmarkStats `json:"benchmark,omitempty"`

	// TradingDaysPerYear 实际使用的年化系数，**必须报出来** ——
	// 它是所有年化量的分母，不写明就无法与别处的数字比较
	TradingDaysPerYear float64 `json:"trading_days_per_year"`
	RiskFreePPM        int64   `json:"risk_free_ppm"`
}

// Compute 算出全部指标。
func Compute(in Input) Result {
	r := Result{
		Steps:              in.Curve.Len(),
		InitialCents:       in.InitialCents,
		TradingDaysPerYear: in.TradingDaysPerYear,
		RiskFreePPM:        in.RiskFreePPM,
	}
	if r.Steps == 0 || in.InitialCents <= 0 {
		return r
	}
	r.FromDay = in.Curve.Days[0]
	r.ToDay = in.Curve.Days[r.Steps-1]
	r.FinalCents = in.Curve.Equity[r.Steps-1]

	tpy := in.TradingDaysPerYear
	if tpy <= 0 {
		tpy = 252 // 兜底；调用方应当传日历数出来的值
	}
	r.Years = float64(r.Steps) / tpy
	r.TotalReturn = float64(r.FinalCents)/float64(in.InitialCents) - 1

	// 年化收益。样本不足一年时照算但标记为不可靠 ——
	// 把三个月的收益按 ^4 外推是最常见的自欺
	r.AnnualReliable = r.Years >= 1.0
	if r.Years > 0 && r.FinalCents > 0 {
		r.AnnualReturn = math.Pow(float64(r.FinalCents)/float64(in.InitialCents), 1/r.Years) - 1
	} else if r.FinalCents <= 0 {
		r.AnnualReturn = -1 // 爆仓
	}

	// 日收益序列。第一步的收益相对初始资金，否则会丢掉建仓当天的涨跌
	rets := make([]float64, 0, r.Steps)
	prev := float64(in.InitialCents)
	for _, e := range in.Curve.Equity {
		if prev > 0 {
			rets = append(rets, float64(e)/prev-1)
		} else {
			rets = append(rets, 0)
		}
		prev = float64(e)
	}

	rfAnnual := float64(in.RiskFreePPM) / 1e6
	rfDaily := rfAnnual / tpy

	r.AnnualVol = stdev(rets) * math.Sqrt(tpy)
	r.DownsideVol = downsideDev(rets, rfDaily) * math.Sqrt(tpy)
	if r.AnnualVol > 0 {
		r.Sharpe = (r.AnnualReturn - rfAnnual) / r.AnnualVol
	}
	if r.DownsideVol > 0 {
		r.Sortino = (r.AnnualReturn - rfAnnual) / r.DownsideVol
	}

	r.MaxDrawdown = maxDrawdown(in.Curve)
	if r.MaxDrawdown.Ratio > 0 {
		r.Calmar = r.AnnualReturn / r.MaxDrawdown.Ratio
	}

	// 成交统计
	for _, f := range in.Fills {
		r.TurnoverCents += f.AmountCents
		r.FeeCents += f.Fee.Total
		r.SlippageCents += f.SlippageCents
	}
	if avg := averageEquity(in.Curve); avg > 0 && r.Years > 0 {
		r.Turnover = float64(r.TurnoverCents) / avg / r.Years
	}

	r.Trades = computeTrades(in.Fills, in.Hedge)
	if in.Benchmark != nil && in.Benchmark.Len() > 0 {
		r.Bench = computeBenchmark(in, tpy)
	}
	return r
}

func computeTrades(fills []trading.Fill, hedge bool) TradeStats {
	trips, open := MatchRoundTrips(fills, hedge)
	var st TradeStats
	st.RoundTrips = len(trips)
	var holdSum float64
	for _, t := range trips {
		switch {
		case t.PnLCents > 0:
			st.Wins++
			st.GrossWinCents += t.PnLCents
		case t.PnLCents < 0:
			st.Losses++
			st.GrossLossCent += -t.PnLCents
		default:
			st.Flat++
		}
		if t.FromBonus {
			st.BonusTrips++
		}
		holdSum += float64(t.HoldDays)
	}
	if st.RoundTrips > 0 {
		st.WinRate = float64(st.Wins) / float64(st.RoundTrips)
		st.AvgHoldDays = holdSum / float64(st.RoundTrips)
	}
	if st.Wins > 0 {
		st.AvgWinCents = st.GrossWinCents / int64(st.Wins)
	}
	if st.Losses > 0 {
		st.AvgLossCents = st.GrossLossCent / int64(st.Losses)
	}
	if st.AvgLossCents > 0 {
		st.ProfitFactor = float64(st.AvgWinCents) / float64(st.AvgLossCents)
	} else if st.AvgWinCents > 0 {
		st.ProfitFactor = math.Inf(1) // 从未亏过
	}
	st.OpenPositions = len(open)
	for _, leg := range open {
		st.OpenQty += leg.LongQty + leg.ShortQty
		st.OpenCostCents += leg.CostCents
	}
	return st
}

func computeBenchmark(in Input, tpy float64) *BenchmarkStats {
	b := &BenchmarkStats{Name: in.BenchmarkName, Total: in.Curve.Len()}

	// 按交易日取交集。基准覆盖不到的时点**直接跳过**，
	// 不补 0 —— 补 0 等于凭空造出一段超额收益
	bench := make(map[int32]int64, in.Benchmark.Len())
	for i, d := range in.Benchmark.Days {
		bench[d] = in.Benchmark.Equity[i]
	}

	var sDays []int32
	var sEq, bEq []int64
	for i, d := range in.Curve.Days {
		if v, ok := bench[d]; ok {
			sDays = append(sDays, d)
			sEq = append(sEq, in.Curve.Equity[i])
			bEq = append(bEq, v)
		}
	}
	b.Covered = len(sDays)
	if b.Covered < 2 {
		return b
	}

	b.StrategyReturn = float64(sEq[len(sEq)-1])/float64(sEq[0]) - 1
	b.Return = float64(bEq[len(bEq)-1])/float64(bEq[0]) - 1
	b.Excess = b.StrategyReturn - b.Return

	sr := pctChange(sEq)
	br := pctChange(bEq)
	if len(sr) < 2 {
		return b
	}
	varB := variance(br)
	if varB > 0 {
		b.Beta = covariance(sr, br) / varB
	}
	rfDaily := float64(in.RiskFreePPM) / 1e6 / tpy
	// Alpha 按年化给出：日均超额 × 年交易日
	b.Alpha = (mean(sr) - rfDaily - b.Beta*(mean(br)-rfDaily)) * tpy

	diff := make([]float64, len(sr))
	for i := range sr {
		diff[i] = sr[i] - br[i]
	}
	if sd := stdev(diff); sd > 0 {
		b.InformationRatio = mean(diff) * tpy / (sd * math.Sqrt(tpy))
	}
	return b
}

func maxDrawdown(c Curve) Drawdown {
	var dd Drawdown
	if c.Len() == 0 {
		return dd
	}
	peak := c.Equity[0]
	peakDay := c.Days[0]
	peakIdx := 0
	for i, e := range c.Equity {
		if e > peak {
			peak, peakDay, peakIdx = e, c.Days[i], i
		}
		if peak <= 0 {
			continue
		}
		if ratio := float64(peak-e) / float64(peak); ratio > dd.Ratio {
			dd = Drawdown{
				PeakDay: peakDay, TroughDay: c.Days[i],
				Ratio: ratio, PeakCents: peak, TroughCents: e,
				TroughSteps: i - peakIdx,
			}
		}
	}
	if dd.Ratio == 0 {
		return dd
	}
	// 恢复日：谷底之后第一次重新站上峰值
	troughIdx := -1
	for i, d := range c.Days {
		if d == dd.TroughDay {
			troughIdx = i
			break
		}
	}
	if troughIdx >= 0 {
		for i := troughIdx; i < c.Len(); i++ {
			if c.Equity[i] >= dd.PeakCents {
				dd.RecoveryDay = c.Days[i]
				dd.RecoverySteps = i - troughIdx
				break
			}
		}
	}
	return dd
}

func averageEquity(c Curve) float64 {
	if c.Len() == 0 {
		return 0
	}
	var sum float64
	for _, e := range c.Equity {
		sum += float64(e)
	}
	return sum / float64(c.Len())
}

// ---- 统计工具 ----

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// stdev 样本标准差（n−1）。绩效指标习惯用样本口径。
func stdev(xs []float64) float64 { return math.Sqrt(variance(xs)) }

func variance(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var s float64
	for _, x := range xs {
		d := x - m
		s += d * d
	}
	return s / float64(len(xs)-1)
}

func covariance(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 2 {
		return 0
	}
	ma, mb := mean(a[:n]), mean(b[:n])
	var s float64
	for i := 0; i < n; i++ {
		s += (a[i] - ma) * (b[i] - mb)
	}
	return s / float64(n-1)
}

// downsideDev 下行标准差：只统计低于门槛的偏离，其余按 0 计。
//
// 分母用**全部样本数**而非仅下行样本数 —— 这是索提诺比率的通行口径，
// 只除下行个数会让「很少下跌但跌得狠」的策略显得更好。
func downsideDev(xs []float64, mar float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var s float64
	for _, x := range xs {
		if d := x - mar; d < 0 {
			s += d * d
		}
	}
	return math.Sqrt(s / float64(len(xs)-1))
}

func pctChange(xs []int64) []float64 {
	if len(xs) < 2 {
		return nil
	}
	out := make([]float64, 0, len(xs)-1)
	for i := 1; i < len(xs); i++ {
		if xs[i-1] == 0 {
			out = append(out, 0)
			continue
		}
		out = append(out, float64(xs[i])/float64(xs[i-1])-1)
	}
	return out
}
