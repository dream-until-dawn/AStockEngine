package indicator

import (
	"math"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// 构造一段确定性的行情，含一段停牌（OHLC 打平、量为 0），
// 因为停牌行会让 KDJ 的窗口极差归零 —— 那是必须覆盖的分支。
func synth(n int) []mktdata.Bar {
	out := make([]mktdata.Bar, n)
	p := int64(10_000) // 10.000 元
	for i := 0; i < n; i++ {
		if i >= 40 && i < 55 { // 停牌段
			out[i] = mktdata.Bar{
				TradingDay: int32(20200101 + i), Open: p, High: p, Low: p, Close: p,
				PreClose: p, Volume: 0, Amount: 0, TradeStatus: 0,
			}
			continue
		}
		d := int64((i*37)%201) - 100 // 确定性伪随机
		np := p + d
		if np < 1000 {
			np = 1000
		}
		out[i] = mktdata.Bar{
			TradingDay: int32(20200101 + i),
			Open:       p, High: maxI(p, np) + 50, Low: minI(p, np) - 50,
			Close: np, PreClose: p, Volume: 1000, Amount: 1000 * np,
			TradeStatus: 1,
		}
		p = np
	}
	return out
}

func maxI(a, b int64) int64 { if a > b { return a }; return b }
func minI(a, b int64) int64 { if a < b { return a }; return b }

// 快照必须能往返：这是 C6 实盘增量的前提 ——
// 每天从昨日快照恢复后继续步进，结果必须与从头跑到今天完全一致。
func TestSnapshotRoundTrip(t *testing.T) {
	bars := synth(120)
	cases := []struct {
		name string
		mk   func() Indicator
	}{
		{"SMA20", func() Indicator { return NewSMA(20, DefaultPriceScale) }},
		{"EMA12", func() Indicator { return NewEMA(12, DefaultPriceScale) }},
		{"MACD", func() Indicator { return NewMACD(12, 26, 9, DefaultPriceScale) }},
		{"KDJ", func() Indicator { return NewKDJ(9, 3, 3) }},
		{"RSI14", func() Indicator { return NewRSI(14) }},
		{"Donchian20", func() Indicator { return NewDonchian(20, DefaultPriceScale) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			full := c.mk()
			for _, b := range bars {
				full.Update(b)
			}

			// 跑到一半做快照，恢复到新实例后继续
			half := c.mk()
			for _, b := range bars[:60] {
				half.Update(b)
			}
			snap := half.Snapshot()

			restored := c.mk()
			if err := restored.Restore(snap); err != nil {
				t.Fatalf("恢复失败: %v", err)
			}
			for _, b := range bars[60:] {
				restored.Update(b)
			}

			if full.Ready() != restored.Ready() {
				t.Fatalf("Ready 不一致: %v vs %v", full.Ready(), restored.Ready())
			}
			a, b := full.Values(), restored.Values()
			for i := range a {
				if math.Abs(a[i]-b[i]) > 1e-9 {
					t.Errorf("%s[%d] 不一致: 全程 %.10f vs 快照恢复 %.10f",
						full.Names()[i], i, a[i], b[i])
				}
			}
		})
	}
}

// 停牌段会让 KDJ 窗口极差为 0，必须不产生 NaN/Inf。
func TestKDJSuspendedNoNaN(t *testing.T) {
	kd := NewKDJ(9, 3, 3)
	for _, b := range synth(120) {
		kd.Update(b)
		for i, v := range kd.Values() {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("第 %d 日 %s 出现 %v", b.TradingDay, kd.Names()[i], v)
			}
		}
	}
}

// Ready 之前的值不得被使用 —— 此测试固定住 Ready 的口径，
// 避免日后有人无意中改动预热长度。
func TestReadyThresholds(t *testing.T) {
	bars := synth(60)
	sma, macd, kdj := NewSMA(20, DefaultPriceScale), NewMACD(12, 26, 9, DefaultPriceScale), NewKDJ(9, 3, 3)
	for i, b := range bars {
		sma.Update(b)
		macd.Update(b)
		kdj.Update(b)
		n := i + 1
		if got, want := sma.Ready(), n >= 20; got != want {
			t.Fatalf("SMA20 第 %d 根 Ready=%v，期望 %v", n, got, want)
		}
		if got, want := macd.Ready(), n >= 35; got != want {
			t.Fatalf("MACD 第 %d 根 Ready=%v，期望 %v（long+signal=35）", n, got, want)
		}
		if got, want := kdj.Ready(), n >= 9; got != want {
			t.Fatalf("KDJ 第 %d 根 Ready=%v，期望 %v", n, got, want)
		}
	}
}
