package strategies

import (
	"testing"

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

// TestGridLongLevels 做多：价格跌破第 n 格才算第 n 档。
func TestGridLongLevels(t *testing.T) {
	g := gridAt(3, 10, false) // 3 格，每格 10%
	base := int64(100_000)    // 100.000
	cases := []struct {
		price int64
		want  int
	}{
		{110_000, 0}, // 涨了 —— 空仓
		{100_000, 0}, // 平 —— 空仓
		{95_000, 0},  // 跌 5%，不到一格
		{90_000, 1},  // 跌 10% —— 第 1 格
		{85_000, 1},
		{80_000, 2}, // 跌 20% —— 第 2 格
		{70_000, 3}, // 跌 30% —— 第 3 格（满仓）
		{50_000, 3}, // 再跌也只有 3 格
	}
	for _, c := range cases {
		if got := g.targetLevel(base, c.price); got != c.want {
			t.Errorf("做多 价格 %d：档位 %d，想要 %d", c.price, got, c.want)
		}
	}
}

// TestGridShortLevelsMirror 做空：整个反过来，价格**涨破**第 n 格才开第 n 份空。
func TestGridShortLevelsMirror(t *testing.T) {
	g := gridAt(3, 10, true)
	base := int64(100_000)
	cases := []struct {
		price int64
		want  int
	}{
		{90_000, 0},  // 跌了 —— 空仓（做空网格在跌的时候不持仓）
		{100_000, 0}, // 平
		{105_000, 0}, // 涨 5%，不到一格
		{110_000, 1}, // 涨 10% —— 第 1 格
		{115_000, 1},
		{120_000, 2},
		{130_000, 3},
		{200_000, 3},
	}
	for _, c := range cases {
		if got := g.targetLevel(base, c.price); got != c.want {
			t.Errorf("做空 价格 %d：档位 %d，想要 %d", c.price, got, c.want)
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
