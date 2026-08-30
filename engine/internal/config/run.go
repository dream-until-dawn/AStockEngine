package config

import (
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/metrics"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/record"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// RunStats 是一次运行的计数汇总。
type RunStats struct {
	Steps   int
	Signals int
	Fills   int
	Rejects int
}

// ComputeMetrics 把记录喂给绩效模块。
//
// **抽出来是因为它有两处容易各写各的**：
//
//  1. 年化系数要问 Market，不能写死 252 —— 252 是美股惯例，
//     A 股由日历实测约 242.90 天，24×7 的加密是 365。
//  2. 基准曲线要从 DataSet 取，取不到时**不能静默当成没有基准** ——
//     v0.3.1 踩过一次：裁子集把基准裁掉了，报告里的对标区块凭空消失，
//     不报错、不异常，只是少了一块。
//
// 命令行、服务端、海选三个入口都走这里，就不会有一个漏掉。
func ComputeMetrics(
	cfg *Config, ds *DataSet, rec record.Recorder, mkt trading.Market,
) metrics.Result {
	var days []int32
	var eq []int64
	if m, ok := rec.(*record.Memory); ok {
		days, eq = m.Curve()
	}
	// **绩效只算在实际交易的区间上。**
	//
	// Walk-Forward 的每个窗口都多裁一段预热前缀（TradeFrom 之前只喂指标
	// 不交易），那一段的权益恒等于初始资金。把它算进曲线的后果不是
	// 「多几个点」这么轻：4 年的窗口里有 3 年是零收益，
	// 年化系数按 970 步算 → 年化收益被摊薄 4 倍、年化波动被压低、
	// **夏普被系统性抬高**。而这些数字看上去完全正常。
	days, eq = trimToTradeFrom(days, eq, cfg.Engine.TradeFrom)
	in := metrics.Input{
		Curve:        metrics.Curve{Days: days, Equity: eq},
		InitialCents: cfg.Portfolio.InitialCashCents,
		Fills:        rec.Fills(),
		RiskFreePPM:  cfg.Metrics.RiskFreePPM,
	}
	var from, to int32
	if len(days) > 0 {
		from, to = days[0], days[len(days)-1]
	}
	in.TradingDaysPerYear = mkt.AnnualDays(ds.Calendar, from, to)

	if bd, be, ok := ds.BenchmarkCurve(); ok {
		in.Benchmark = &metrics.Curve{Days: bd, Equity: be}
		in.BenchmarkName = cfg.Metrics.Benchmark
	}
	return metrics.Compute(in)
}

// RunToEnd 装配并跑完一次回测，返回引擎、绩效与计数。
//
// 海选每秒要跑几十次，这条路径上**不打印任何东西**、不落盘 ——
// 记录级别由配置决定（summary 足够算全部指标）。
func RunToEnd(cfg *Config, ds *DataSet) (
	*eng.Engine, metrics.Result, RunStats, error,
) {
	e, err := cfg.Assemble(ds)
	if err != nil {
		return nil, metrics.Result{}, RunStats{}, err
	}
	var st RunStats
	for !e.Done() {
		if _, err := e.Step(); err != nil {
			return nil, metrics.Result{}, st, fmt.Errorf("步进失败: %w", err)
		}
		nf, nr := e.LastCounts()
		st.Signals += len(e.Signals())
		st.Fills += nf
		st.Rejects += nr
	}
	st.Steps = e.Steps()
	return e, ComputeMetrics(cfg, ds, e.Recorder(), e.Market()), st, nil
}

// trimToTradeFrom 砍掉预热段。
//
// 保留 TradeFrom **前一个**点作为基准起点 —— 收益是相对起点算的，
// 直接从 TradeFrom 那天开始会把当天的涨跌算丢。
func trimToTradeFrom(days []int32, eq []int64, tradeFrom int32) ([]int32, []int64) {
	if tradeFrom == 0 || len(days) == 0 {
		return days, eq
	}
	i := 0
	for i < len(days) && days[i] < tradeFrom {
		i++
	}
	if i == 0 || i >= len(days) {
		return days, eq // 全在交易区间内，或全在预热区间内（后者由调用方发现）
	}
	return days[i-1:], eq[i-1:]
}
