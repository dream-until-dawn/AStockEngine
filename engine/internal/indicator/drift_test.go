package indicator

import (
	"math"
	"testing"
)

// 滚动累加（加新减旧）会累积浮点漂移：从不同起点跑到同一天，值会有微小差异。
//
// 这不违反 C5 —— 配置里含起始日期，同配置结果仍逐笔一致。
// 但 Walk-Forward 的不同窗口在同一日会得到不同的指标值，属预期行为。
//
// 本测试把漂移量**测出来**，据此决定是否需要 Kahan 补偿求和（设计 4.3）。
func TestSMADriftAcrossStartPoints(t *testing.T) {
	const (
		total  = 6000 // 约 24 年日线
		period = 20
		offset = 1200 // 晚 1200 根开始，模拟 Walk-Forward 的不同窗口
	)
	bars := synth(total)

	long := NewSMA(period, DefaultPriceScale)
	for _, b := range bars {
		long.Update(b)
	}
	short := NewSMA(period, DefaultPriceScale)
	for _, b := range bars[offset:] {
		short.Update(b)
	}

	a, b := long.Values()[0], short.Values()[0]
	absDiff := math.Abs(a - b)
	relDiff := absDiff / math.Abs(b)

	t.Logf("SMA%d 跑 %d 根 vs 跑 %d 根，终点同一日：", period, total, total-offset)
	t.Logf("  全程 %.15f", a)
	t.Logf("  截断 %.15f", b)
	t.Logf("  绝对差 %.3e  相对差 %.3e", absDiff, relDiff)

	// 相对漂移若超过 1e-12，说明朴素累加不足以支撑跨窗口比较，需上 Kahan。
	if relDiff > 1e-12 {
		t.Errorf("漂移 %.3e 超过 1e-12，应考虑 Kahan 补偿求和", relDiff)
	}
}

// EMA 是递推式（无减法），理论上不累积「加新减旧」那种漂移，
// 但初值不同会导致收敛残差。一并测出量级。
func TestEMAConvergenceAcrossStartPoints(t *testing.T) {
	const (
		total  = 6000
		period = 12
		offset = 1200
	)
	bars := synth(total)

	long := NewEMA(period, DefaultPriceScale)
	for _, b := range bars {
		long.Update(b)
	}
	short := NewEMA(period, DefaultPriceScale)
	for _, b := range bars[offset:] {
		short.Update(b)
	}
	a, b := long.Values()[0], short.Values()[0]
	t.Logf("EMA%d 全程 %.15f vs 截断 %.15f  相对差 %.3e",
		period, a, b, math.Abs(a-b)/math.Abs(b))
}
