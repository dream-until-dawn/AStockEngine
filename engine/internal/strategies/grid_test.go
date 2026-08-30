package strategies

import (
	"testing"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 网格的两个方向是镜像的：做多越跌越买、涨回来平；做空越涨越空、跌回来平。
// 档位判定是纯函数，直接测它 —— 走完整引擎只会把「哪一格」淹没在别的因素里。

func gridAt(levels int, stepPct float64, short bool) *Grid {
	g := NewGrid()
	g.levels = levels
	g.stepPPM = int64(stepPct * 10_000)
	g.short = short
	return g
}

// TestGridLongLevels 做多：跌是**加仓**方向，档位取负；涨是减仓方向，档位取正。
func TestGridLongLevels(t *testing.T) {
	g := gridAt(3, 10, false) // 3 格，每格 10%
	base := int64(100_000)    // 100.000
	cases := []struct {
		price int64
		want  int
	}{
		{130_000, 3}, // 涨 30% —— +3 格，空仓
		{120_000, 2}, // 涨 20%
		{110_000, 1}, // 涨 10% —— 第 +1 格
		{105_000, 0}, // 涨 5%，不到一格
		{100_000, 0}, // 平 —— 0 线，半仓
		{95_000, 0},  // 跌 5%，不到一格
		{90_000, -1}, // 跌 10% —— 第 −1 格
		{85_000, -1}, //
		{80_000, -2}, // 跌 20%
		{70_000, -3}, // 跌 30% —— 满仓格
		{50_000, -3}, // 再跌也只有 3 格
		{200_000, 3}, // 再涨也只有 3 格
	}
	for _, c := range cases {
		if got := g.targetLevel(base, c.price); got != c.want {
			t.Errorf("做多 价格 %d：档位 %+d，想要 %+d", c.price, got, c.want)
		}
	}
}

// TestGridShortLevelsMirror 做空：整个镜像 —— **涨**才是加仓方向，档位取负。
func TestGridShortLevelsMirror(t *testing.T) {
	g := gridAt(3, 10, true)
	base := int64(100_000)
	cases := []struct {
		price int64
		want  int
	}{
		{70_000, 3},   // 跌 30% —— 做空网格在这里空仓
		{90_000, 1},   // 跌 10%
		{95_000, 0},   //
		{100_000, 0},  // 平
		{105_000, 0},  // 涨 5%，不到一格
		{110_000, -1}, // 涨 10% —— 开第 1 份空
		{115_000, -1}, //
		{120_000, -2}, //
		{130_000, -3}, // 满仓格
		{200_000, -3}, //
	}
	for _, c := range cases {
		if got := g.targetLevel(base, c.price); got != c.want {
			t.Errorf("做空 价格 %d：档位 %+d，想要 %+d", c.price, got, c.want)
		}
	}
}

// TestGridFallingPriceBuysMore 把 targetLevel 与 target **串起来**测。
//
// 这是这个文件里唯一一个跨函数的测试，也是唯一一个能抓住那个 bug 的测试：
// 曾经 targetLevel 对做多把「跌 3 格」返回 +3，而 target 认为 +3 是
// 「涨上去该减仓」，于是整张网跑成了越跌卖得越多的追涨杀跌。
// 两个函数各自的单元测试当时全是绿的 —— 因为它们各自用的是**相反**的符号约定，
// 而没有任何一个测试同时看见两边。
//
// 网格只有一句话要保证：**价格越低，仓位越重。** 直接测这句话。
func TestGridFallingPriceBuysMore(t *testing.T) {
	g := gridAt(5, 5, false) // 5 格 × 5%
	base := int64(100_000)
	// 从满仓格一路涨到空仓格，目标比例必须**单调下降**
	prices := []int64{75_000, 80_000, 85_000, 90_000, 95_000,
		100_000, 105_000, 110_000, 115_000, 120_000, 125_000}
	want := []float64{1.0, 0.9, 0.8, 0.7, 0.6, 0.5, 0.4, 0.3, 0.2, 0.1, 0.0}
	for i, p := range prices {
		w := g.target(1, g.targetLevel(base, p), "t").Weight
		if diff := w - want[i]; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("价格 %d（基准 %d）：目标仓位 %.2f，想要 %.2f",
				p, base, w, want[i])
		}
	}

	// 做空镜像：价格越**高**仓位越重
	sg := gridAt(5, 5, true)
	for i, p := range prices {
		w := sg.target(1, sg.targetLevel(base, p), "t").Weight
		if diff := w - want[len(want)-1-i]; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("做空 价格 %d：目标仓位 %.2f，想要 %.2f",
				p, w, want[len(want)-1-i])
		}
	}
}

// TestGridOpenCloseSides 开平方向：做多买开卖平，做空卖开买平。
func TestGridOpenCloseSides(t *testing.T) {
	long := gridAt(3, 10, false)
	if long.openSide() != trading.SideBuy || long.closeSide() != trading.SideSell {
		t.Errorf("做多应当买开卖平，得到 %v / %v", long.openSide(), long.closeSide())
	}
	short := gridAt(3, 10, true)
	if short.openSide() != trading.SideSell || short.closeSide() != trading.SideBuy {
		t.Errorf("做空应当卖开买平，得到 %v / %v", short.openSide(), short.closeSide())
	}
}

// TestGridNeedsShort 做空网格要向装配器声明它需要做空的市场。
//
// 不声明的话是**静默失效**：开空信号会被 Sizer 当成减仓，
// 而手上没有多头可减，订单被丢掉 —— 一笔成交都不会有，却什么都不报。
func TestGridNeedsShort(t *testing.T) {
	if gridAt(3, 10, false).NeedsShort() {
		t.Error("做多网格不需要做空")
	}
	if !gridAt(3, 10, true).NeedsShort() {
		t.Error("做空网格必须声明需要做空，否则会在 A 股上静默跑成零成交")
	}
}

// ---- 目标比例 ----
//
// 单边 levels 格 → 上下共 2×levels 格，资金分 2×levels 份，0 线持一半。
// 分一半而不是全仓建仓，是为了在跌到底之前每一格都有份可加、
// 涨上去之前每一格都有份可减 —— 否则「网格」就只是一次买入加一次卖出。

func TestGridTargetWeights(t *testing.T) {
	g := gridAt(5, 5, false)
	cases := []struct {
		level int
		want  float64 // 目标持仓比例
	}{
		{-5, 1.0}, // 满仓 10/10
		{-3, 0.8}, // 8/10
		{-1, 0.6}, // 6/10
		{0, 0.5},  // 半仓 5/10 —— 建仓就在这里
		{1, 0.4},
		{3, 0.2},
		{5, 0.0}, // 空仓 0/10
	}
	for _, c := range cases {
		sig := g.target(1, c.level, "t")
		if sig.Kind != eng.SignalTarget {
			t.Fatalf("档位 %+d：应当发 Target 信号，得到 %v", c.level, sig.Kind)
		}
		if diff := sig.Weight - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("档位 %+d：目标比例 %.4f，想要 %.4f", c.level, sig.Weight, c.want)
		}
	}
}

// TestGridTargetWeightsShort 做空方向的比例与做多一致 —— 差别在开平方向。
func TestGridTargetWeightsShort(t *testing.T) {
	g := gridAt(5, 5, true)
	if got := g.target(1, -5, "t"); got.Weight != 1.0 || got.Side != trading.SideSell {
		t.Errorf("做空满仓格：比例 %.2f 方向 %v，想要 1.00 / 卖", got.Weight, got.Side)
	}
	if got := g.target(1, 5, "t"); got.Weight != 0 {
		t.Errorf("做空空仓格：比例 %.2f，想要 0", got.Weight)
	}
}

// ---- 止损线 ----

// TestGridStopIsBelowFullPosition 止损线在满仓格**之下**，不在满仓那一格。
//
// −levels 是满仓位而不是离场位。在那里止损的话，网格从来没有机会
// 「跌到底再涨回来」—— 而那正是它赚钱的方式。
func TestGridStopIsBelowFullPosition(t *testing.T) {
	g := gridAt(5, 5, false) // 5 格 × 5%，满仓在 −25%
	g.stopLevels = 2         // 止损在 −7 格 = −35%
	base := int64(100_000)

	cases := []struct {
		price int64
		stop  bool
		what  string
	}{
		{80_000, false, "−20%，第 4 格"},
		{75_000, false, "−25%，满仓格 —— **不该止损**"},
		{70_000, false, "−30%，满仓之下但还没到止损线"},
		{65_000, true, "−35%，止损线"},
		{50_000, true, "−50%，早过了"},
	}
	for _, c := range cases {
		if got := g.stopped(base, c.price); got != c.stop {
			t.Errorf("%s：止损 = %v，想要 %v", c.what, got, c.stop)
		}
	}
}

// TestGridStopDisabled stop_levels = 0 表示不设止损。
func TestGridStopDisabled(t *testing.T) {
	g := gridAt(5, 5, false)
	g.stopLevels = 0
	if g.stopped(100_000, 1) {
		t.Error("不设止损时永远不该触发")
	}
}

// TestGridStopMirrorsForShort 做空的止损在**上方**。
func TestGridStopMirrorsForShort(t *testing.T) {
	g := gridAt(5, 5, true)
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
