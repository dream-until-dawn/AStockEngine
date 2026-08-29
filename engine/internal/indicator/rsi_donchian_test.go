package indicator

import (
	"math"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func bar(high, low, close int64) mktdata.Bar {
	return mktdata.Bar{High: high, Low: low, Close: close, PreClose: close,
		Open: close, Volume: 100, Amount: 100 * close, TradeStatus: 1}
}

// ---- RSI ----

// TestRSIAllUpIs100 单边上涨时 RSI = 100。
func TestRSIAllUpIs100(t *testing.T) {
	r := NewRSI(14)
	for i := 0; i < 30; i++ {
		p := int64(10_000 + i*100)
		r.Update(bar(p, p, p))
	}
	if !r.Ready() {
		t.Fatal("30 根后应已就绪")
	}
	if v := r.Values()[0]; math.Abs(v-100) > 1e-9 {
		t.Errorf("单边上涨 RSI 应为 100，得到 %v", v)
	}
}

// TestRSIAllDownIs0 单边下跌时 RSI = 0。
func TestRSIAllDownIs0(t *testing.T) {
	r := NewRSI(14)
	for i := 0; i < 30; i++ {
		p := int64(20_000 - i*100)
		r.Update(bar(p, p, p))
	}
	if v := r.Values()[0]; math.Abs(v) > 1e-9 {
		t.Errorf("单边下跌 RSI 应为 0，得到 %v", v)
	}
}

// TestRSIFlatIsNeutral 完全无波动时返回 50，而不是 NaN 或 0/100。
//
// 这不是假想情形：停牌行的收盘价等于前收（SCHEMA.md 1.3），
// 连续停牌满 period 根后分母就是 0。返回 0 会让均值回归策略疯狂买入停牌股。
func TestRSIFlatIsNeutral(t *testing.T) {
	r := NewRSI(14)
	for i := 0; i < 30; i++ {
		r.Update(bar(10_000, 10_000, 10_000))
	}
	if v := r.Values()[0]; v != 50 {
		t.Errorf("无波动时应返回中性值 50，得到 %v", v)
	}
}

// TestRSINotReadyBeforeWarmup 预热期内不得声称就绪。
//
// 需要 period+1 根 bar：首根没有前收，产生不了涨跌幅。
func TestRSINotReadyBeforeWarmup(t *testing.T) {
	r := NewRSI(14)
	for i := 0; i < 14; i++ {
		r.Update(bar(10_000, 10_000, int64(10_000+i*10)))
	}
	if r.Ready() {
		t.Error("14 根 bar 只产生 13 个涨跌幅，不该就绪")
	}
	r.Update(bar(10_000, 10_000, 10_200))
	if !r.Ready() {
		t.Error("第 15 根后应就绪")
	}
}

// ---- Donchian ----

// TestDonchianExcludesCurrentBar 通道**不含当前这根**。
//
// 这是整个指标最关键的一条：含今日的话，「今日最高价突破上轨」
// 会退化成「today.High >= today.High」恒真，突破策略每天都开仓。
func TestDonchianExcludesCurrentBar(t *testing.T) {
	d := NewDonchian(5, DefaultPriceScale)
	for i := 0; i < 6; i++ {
		d.Update(bar(10_000, 9_000, 9_500)) // 前 6 根都在 [9.0, 10.0]
	}
	// 第 7 根冲到 20.000 —— 通道必须仍是旧窗口的 10.000
	d.Update(bar(20_000, 19_000, 19_500))
	up := d.Values()[0]
	if math.Abs(up-10.0) > 1e-9 {
		t.Errorf("上轨应是旧窗口的 10.000（不含当前根的 20.000），得到 %v", up)
	}
	// 再来一根，此时 20.000 才应进入窗口
	d.Update(bar(10_000, 9_000, 9_500))
	if up2 := d.Values()[0]; math.Abs(up2-20.0) > 1e-9 {
		t.Errorf("下一根时上轨应更新为 20.000，得到 %v", up2)
	}
}

// TestDonchianNotReadyBeforeWarmup 需要 period+1 根。
func TestDonchianNotReadyBeforeWarmup(t *testing.T) {
	d := NewDonchian(20, DefaultPriceScale)
	for i := 0; i < 20; i++ {
		d.Update(bar(10_000, 9_000, 9_500))
	}
	if d.Ready() {
		t.Error("20 根时窗口刚满，还没有「不含自己」的通道可比，不该就绪")
	}
	d.Update(bar(10_000, 9_000, 9_500))
	if !d.Ready() {
		t.Error("第 21 根后应就绪")
	}
}

// TestDonchianMid 中轨是上下轨的均值。
func TestDonchianMid(t *testing.T) {
	d := NewDonchian(3, DefaultPriceScale)
	for i := 0; i < 4; i++ {
		d.Update(bar(12_000, 8_000, 10_000))
	}
	v := d.Values()
	if math.Abs(v[2]-(v[0]+v[1])/2) > 1e-9 {
		t.Errorf("中轨应为 (上+下)/2，得到 %v（上 %v 下 %v）", v[2], v[0], v[1])
	}
}
