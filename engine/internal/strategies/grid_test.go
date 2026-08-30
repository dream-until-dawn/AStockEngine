package strategies

import (
	"testing"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 网格有三种模式：做多、做空、双向。测试按「几何」与「策略」分层 ——
// displacement 只答「站在第几条线上」，legTargets 只答「这条线该持多少」。
//
// **上一个 bug 就是这两件事混在一个函数里造成的**：几何按模式翻符号，
// 策略按另一套符号算权重，两边各自的测试都是绿的。所以下面每一组
// 「模式」测试都从价格一路走到仓位，而不是只测中间某一段。

func gridAt(levels int, stepPct float64, long, short bool) *Grid {
	g := NewGrid()
	g.levels = levels
	g.stepPPM = int64(stepPct * 10_000)
	g.long, g.short = long, short
	return g
}

// ---- 几何：与模式无关 ----

// TestGridDisplacementIsModeFree 位移只认价格，跌为负、涨为正。
//
// 三种模式必须给出**完全相同**的位移 —— 它是几何不是策略。
func TestGridDisplacementIsModeFree(t *testing.T) {
	base := int64(100_000) // 100.000
	cases := []struct {
		price int64
		want  int
	}{
		{130_000, 3}, // 涨 30%
		{120_000, 2}, //
		{110_000, 1}, // 涨 10% —— 第 +1 格
		{105_000, 0}, // 涨 5%，不到一格
		{100_000, 0}, // 0 线
		{95_000, 0},  // 跌 5%，不到一格
		{90_000, -1}, // 跌 10% —— 第 −1 格
		{80_000, -2}, //
		{70_000, -3}, // 跌 30%
		{50_000, -3}, // 再跌也只有 3 格
		{200_000, 3}, // 再涨也只有 3 格
	}
	for _, m := range []struct {
		name        string
		long, short bool
	}{
		{"做多", true, false}, {"做空", false, true}, {"双向", true, true},
	} {
		g := gridAt(3, 10, m.long, m.short)
		for _, c := range cases {
			if got := g.displacement(base, c.price); got != c.want {
				t.Errorf("%s 价格 %d：位移 %+d，想要 %+d", m.name, c.price, got, c.want)
			}
		}
	}
}

// ---- 策略：从价格一路走到仓位 ----

// TestGridLongPriceToPosition 做多网格：**价格越低，多头仓位越重。**
//
// 网格只有这一句话要保证，所以直接测这句话，而不是测中间的档位编号。
func TestGridLongPriceToPosition(t *testing.T) {
	g := gridAt(5, 5, true, false) // 5 格 × 5%
	base := int64(100_000)
	prices := []int64{75_000, 80_000, 85_000, 90_000, 95_000,
		100_000, 105_000, 110_000, 115_000, 120_000, 125_000}
	wantLong := []float64{1.0, 0.9, 0.8, 0.7, 0.6, 0.5, 0.4, 0.3, 0.2, 0.1, 0.0}
	for i, p := range prices {
		lw, sw := g.legTargets(g.displacement(base, p))
		if !near(lw, wantLong[i]) {
			t.Errorf("价格 %d：多头目标 %.2f，想要 %.2f", p, lw, wantLong[i])
		}
		if sw != 0 {
			t.Errorf("价格 %d：做多网格不该有空头目标，得到 %.2f", p, sw)
		}
	}
}

// TestGridShortMirrorsLong 做空网格是做多的镜像：价格越高，空头仓位越重。
func TestGridShortMirrorsLong(t *testing.T) {
	g := gridAt(5, 5, false, true)
	base := int64(100_000)
	prices := []int64{75_000, 85_000, 100_000, 115_000, 125_000}
	wantShort := []float64{0.0, 0.2, 0.5, 0.8, 1.0}
	for i, p := range prices {
		lw, sw := g.legTargets(g.displacement(base, p))
		if !near(sw, wantShort[i]) {
			t.Errorf("价格 %d：空头目标 %.2f，想要 %.2f", p, sw, wantShort[i])
		}
		if lw != 0 {
			t.Errorf("价格 %d：做空网格不该有多头目标，得到 %.2f", p, lw)
		}
	}
}

// TestGridBothSidesIsFlatAtAnchor 双向网格：**0 线空仓**，跌了做多、涨了做空。
//
// 这是它与两个单向模式最实质的差别 —— 单向在 0 线持一半，
// 双向在 0 线一分钱都不占。
func TestGridBothSidesIsFlatAtAnchor(t *testing.T) {
	g := gridAt(5, 5, true, true)
	base := int64(100_000)
	cases := []struct {
		price              int64
		wantLong, wantShrt float64
		what               string
	}{
		{75_000, 1.0, 0.0, "−5 格：满仓多"},
		{85_000, 0.6, 0.0, "−3 格"},
		{95_000, 0.2, 0.0, "−1 格（正好压线：base×0.95）"},
		{96_000, 0.0, 0.0, "不到一格"},
		{100_000, 0.0, 0.0, "**0 线：两边都空**"},
		{104_000, 0.0, 0.0, "不到一格"},
		{105_000, 0.0, 0.2, "+1 格（正好压线）"},
		{115_000, 0.0, 0.6, "+3 格"},
		{125_000, 0.0, 1.0, "+5 格：满仓空"},
	}
	for _, c := range cases {
		lw, sw := g.legTargets(g.displacement(base, c.price))
		if !near(lw, c.wantLong) || !near(sw, c.wantShrt) {
			t.Errorf("双向 %s（价 %d）：多 %.2f / 空 %.2f，想要 %.2f / %.2f",
				c.what, c.price, lw, sw, c.wantLong, c.wantShrt)
		}
	}
}

// TestGridBothSidesEmitsTwoLegs 双向网格每根 bar 要**两条信号**。
//
// Sizer 的 targetOrder 一次只看一条腿（按 side 取 Exposure.Long 或 .Short），
// 只发一条的话穿越 0 线时另一边永远调不到位 —— 平不掉的多头会一直挂着，
// 而这不会报任何错。
func TestGridBothSidesEmitsTwoLegs(t *testing.T) {
	g := gridAt(5, 5, true, true)
	sigs := g.targets(1, -3, "t")
	if len(sigs) != 2 {
		t.Fatalf("双向网格应当发两条信号（多、空各一），得到 %d 条", len(sigs))
	}
	sides := map[trading.Side]float64{}
	for _, s := range sigs {
		if s.Kind != eng.SignalTarget {
			t.Fatalf("应当是 Target 信号，得到 %v", s.Kind)
		}
		sides[s.Side] = s.Weight
	}
	if !near(sides[trading.SideBuy], 0.6) || !near(sides[trading.SideSell], 0) {
		t.Errorf("−3 格：买 %.2f / 卖 %.2f，想要 0.60 / 0.00",
			sides[trading.SideBuy], sides[trading.SideSell])
	}
	// 单向只发一条 —— 多发一条空目标会在单向市场上被当成开空
	if n := len(gridAt(5, 5, true, false).targets(1, -3, "t")); n != 1 {
		t.Errorf("做多网格应当只发一条信号，得到 %d 条", n)
	}
}

// TestGridExitsCoverEveryLeg 止损要把**用到的每条腿**都平掉。
func TestGridExitsCoverEveryLeg(t *testing.T) {
	if n := len(gridAt(5, 5, true, false).exits(1, "grid_stop")); n != 1 {
		t.Errorf("做多网格平一条腿，得到 %d 条", n)
	}
	if n := len(gridAt(5, 5, false, true).exits(1, "grid_stop")); n != 1 {
		t.Errorf("做空网格平一条腿，得到 %d 条", n)
	}
	ex := gridAt(5, 5, true, true).exits(1, "grid_stop")
	if len(ex) != 2 {
		t.Fatalf("双向网格要平两条腿，得到 %d 条", len(ex))
	}
	if ex[0].Side == ex[1].Side {
		t.Error("两条平仓信号方向相同 —— 有一条腿没被平掉")
	}
}

// ---- 重建与止损 ----

// TestGridRebasesOnlyAtEmptyEnd 只有「空仓端」才重建整张网。
//
// 双向两端都是满仓（一端满多、一端满空），没有空仓端 ——
// 它在两端靠止损线兜底，不重建。
func TestGridRebasesOnlyAtEmptyEnd(t *testing.T) {
	long := gridAt(5, 5, true, false)
	if !long.rebases(5) || long.rebases(-5) {
		t.Error("做多网格应当在 +5（空仓端）重建，不在 −5（满仓端）")
	}
	short := gridAt(5, 5, false, true)
	if !short.rebases(-5) || short.rebases(5) {
		t.Error("做空网格应当在 −5（空仓端）重建，不在 +5（满仓端）")
	}
	both := gridAt(5, 5, true, true)
	if both.rebases(5) || both.rebases(-5) {
		t.Error("双向网格两端都是满仓，不该重建 —— 那会把满仓位当成离场位")
	}
}

// TestGridStopIsBeyondFullPosition 止损线在满仓格**之外**，不在满仓那一格。
//
// 满仓格是仓位而不是离场位。在那里止损的话，网格从来没有机会
// 「跌到底再涨回来」—— 而那正是它赚钱的方式。
func TestGridStopIsBeyondFullPosition(t *testing.T) {
	g := gridAt(5, 5, true, false) // 5 格 × 5%，满仓在 −25%
	g.stopLevels = 2               // 止损在 −7 格 = −35%
	base := int64(100_000)

	cases := []struct {
		price int64
		stop  bool
		what  string
	}{
		{80_000, false, "−20%，第 4 格"},
		{75_000, false, "−25%，满仓格 —— **不该止损**"},
		{70_000, false, "−30%，满仓之外但还没到止损线"},
		{65_000, true, "−35%，止损线"},
		{50_000, true, "−50%，早过了"},
		{200_000, false, "涨上去了 —— 做多网格上方没有止损线"},
	}
	for _, c := range cases {
		if got := g.stopped(base, c.price); got != c.stop {
			t.Errorf("%s：止损 = %v，想要 %v", c.what, got, c.stop)
		}
	}
}

// TestGridStopMirrorsForShort 做空的止损在**上方**。
func TestGridStopMirrorsForShort(t *testing.T) {
	g := gridAt(5, 5, false, true)
	g.stopLevels = 2
	base := int64(100_000)
	if g.stopped(base, 125_000) {
		t.Error("+25% 是做空的满仓格，不该止损")
	}
	if !g.stopped(base, 135_000) {
		t.Error("+35% 是做空的止损线，应当触发")
	}
	if g.stopped(base, 50_000) {
		t.Error("做空时价格下跌是盈利，不该止损")
	}
}

// TestGridBothSidesStopsOnEitherEnd 双向网格**两边各有一条止损线**。
//
// 只兜一边的话，另一边穿出去之后满仓腿会一直扛着，
// 而这在报告里看不出来 —— 它既不是强平也不是止损。
func TestGridBothSidesStopsOnEitherEnd(t *testing.T) {
	g := gridAt(5, 5, true, true)
	g.stopLevels = 2 // 两侧止损线在 ±35%
	base := int64(100_000)
	if g.stopped(base, 75_000) || g.stopped(base, 125_000) {
		t.Error("±25% 是两端的满仓格，都不该止损")
	}
	if !g.stopped(base, 65_000) {
		t.Error("−35%：下方止损线应当触发")
	}
	if !g.stopped(base, 135_000) {
		t.Error("+35%：上方止损线应当触发 —— 双向的两端都要兜底")
	}
}

// TestGridStopDisabled stop_levels = 0 表示不设止损。
func TestGridStopDisabled(t *testing.T) {
	g := gridAt(5, 5, true, false)
	g.stopLevels = 0
	if g.stopped(100_000, 1) {
		t.Error("不设止损时永远不该触发")
	}
}

// ---- 市场能力 ----

// TestGridNeedsShort 用到做空腿的网格要向装配器声明它需要做空的市场。
//
// 不声明的话是**静默失效**：开空信号会被 Sizer 当成减仓，
// 而手上没有多头可减，订单被丢掉 —— 一笔成交都不会有，却什么都不报。
func TestGridNeedsShort(t *testing.T) {
	if gridAt(3, 10, true, false).NeedsShort() {
		t.Error("做多网格不需要做空")
	}
	if !gridAt(3, 10, false, true).NeedsShort() {
		t.Error("做空网格必须声明需要做空，否则会在 A 股上静默跑成零成交")
	}
	if !gridAt(3, 10, true, true).NeedsShort() {
		t.Error("双向网格也要做空 —— 漏了这一条它在 A 股上会退化成半个做多网格")
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
