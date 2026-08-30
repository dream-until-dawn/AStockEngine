package indicator

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// RSI 相对强弱指标（Wilder 原版平滑）。
//
//	上涨幅度 U = max(C - C_prev, 0)，下跌幅度 D = max(C_prev - C, 0)
//	avgU = (avgU×(n-1) + U) / n      Wilder 平滑，等价于 α = 1/n 的 EMA
//	avgD = (avgD×(n-1) + D) / n
//	RSI  = 100 × avgU / (avgU + avgD)
//
// 加它是为了给策略池补一个**与均线正交**的维度。
// MACD 与双均线本质是同一个想法（两条 EMA 的关系）的两种写法，
// 只扫这两个，海选能得出的结论止于「均线类不行」——
// 而那不等于「技术指标不行」。RSI 是均值回归，方向与趋势跟随相反。
//
// 三个必须写明的约定（各平台在此处不一致，是数值对不上的常见原因）：
//
//  1. **首根不产生 U/D**（没有前收可比），从第二根开始累计
//  2. **前 n 根用简单平均做初值**，此后才转 Wilder 平滑 ——
//     直接从 0 开始平滑会让前几十根系统性偏低
//  3. **avgU + avgD 为 0 时返回 50**。这在本项目**必然发生**：
//     停牌行的收盘价等于前收（SCHEMA.md 1.3），连续停牌满 n 根后
//     窗口内涨跌幅全为 0。返回 100 或 0 都会让策略在停牌股上乱下单
//
// RSI 是比值，与价格 scale 无关，故不需要 priceScale。
type RSI struct {
	period int

	prev    float64 // 上一根收盘价
	hasPrev bool
	sumU    float64 // 初始化阶段的累计
	sumD    float64
	avgU    float64
	avgD    float64
	warm    int // 已产生的 U/D 个数
}

// NewRSI 创建 RSI。常用周期 6 / 12 / 14 / 24。
func NewRSI(period int) *RSI {
	if period < 1 {
		period = 14
	}
	return &RSI{period: period}
}

func (r *RSI) Update(b mktdata.Bar) {
	c := float64(b.Close)
	if !r.hasPrev {
		r.prev, r.hasPrev = c, true
		return // 首根没有前收，不产生 U/D
	}
	u, d := 0.0, 0.0
	if diff := c - r.prev; diff > 0 {
		u = diff
	} else {
		d = -diff
	}
	r.prev = c
	r.warm++

	switch {
	case r.warm <= r.period:
		// 前 n 根攒简单平均做初值
		r.sumU += u
		r.sumD += d
		r.avgU = r.sumU / float64(r.warm)
		r.avgD = r.sumD / float64(r.warm)
	default:
		n := float64(r.period)
		r.avgU = (r.avgU*(n-1) + u) / n
		r.avgD = (r.avgD*(n-1) + d) / n
	}
}

// Ready 在攒满 period 个涨跌幅后为真 —— 需要 period+1 根 bar。
func (r *RSI) Ready() bool     { return r.warm >= r.period }
func (r *RSI) Names() []string { return []string{"RSI"} }

func (r *RSI) Values() []float64 {
	if !r.Ready() {
		return []float64{0}
	}
	den := r.avgU + r.avgD
	if den <= 0 {
		// 窗口内完全没有波动 —— 停牌行会造成这种情况。
		// 中性值 50 是唯一不会诱发交易的答案
		return []float64{50}
	}
	return []float64{100 * r.avgU / den}
}

func (r *RSI) Snapshot() State {
	warmFlag := 0.0
	if r.hasPrev {
		warmFlag = 1
	}
	return State{
		Kind: "RSI", Warm: r.warm,
		Floats: []float64{r.prev, warmFlag, r.sumU, r.sumD, r.avgU, r.avgD},
	}
}

func (r *RSI) Restore(st State) error {
	if st.Kind != "RSI" || len(st.Floats) != 6 {
		return fmt.Errorf("RSI 快照不匹配：kind=%s floats=%d", st.Kind, len(st.Floats))
	}
	r.prev = st.Floats[0]
	r.hasPrev = st.Floats[1] != 0
	r.sumU, r.sumD = st.Floats[2], st.Floats[3]
	r.avgU, r.avgD = st.Floats[4], st.Floats[5]
	r.warm = st.Warm
	return nil
}

var _ Indicator = (*RSI)(nil)
