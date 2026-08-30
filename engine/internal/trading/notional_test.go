package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func ashare() *mktdata.Instrument {
	return &mktdata.Instrument{
		Market: mktdata.MarketAShare, PriceScale: 1000, QtyScale: 1,
		ContractMult: 100_000_000, // 1e8 = ×1
	}
}

// btc 1 张 = 0.01 BTC；价格与数量都是 ×1e8。
func btc() *mktdata.Instrument {
	return &mktdata.Instrument{
		Market: mktdata.MarketCrypto, PriceScale: 100_000_000,
		QtyScale: 100_000_000, ContractMult: 1_000_000, // 0.01 × 1e8
	}
}

func TestNotionalRatio(t *testing.T) {
	if n, d := ashare().NotionalRatio(); n != 1 || d != 10 {
		t.Errorf("A 股应为 1/10（与旧的 price*qty/10 一致），得到 %d/%d", n, d)
	}
	if n, d := btc().NotionalRatio(); n != 1 || d != 10_000_000_000_000_000 {
		t.Errorf("BTC 永续应为 1/1e16，得到 %d/%d", n, d)
	}
}

// TestNotionalMatchesAShareLegacy A 股上必须与旧口径逐分一致。
//
// 换算方式一变就是全部历史结果一起变，所以这条是硬约束。
func TestNotionalMatchesAShareLegacy(t *testing.T) {
	in := ashare()
	for _, c := range []struct{ price, qty int64 }{
		{10_000, 100}, {1_234, 5_600}, {99_999, 1}, {1, 1},
		{10_400, 8_900}, {3_132, 23_200},
	} {
		want := AmountCents(c.price, c.qty)
		got := NotionalCents(in, c.price, c.qty)
		if got != want {
			t.Errorf("price=%d qty=%d：新口径 %d ≠ 旧口径 %d", c.price, c.qty, got, want)
		}
	}
}

// TestNotionalCrypto BTC 永续：125,000 USDT/币，1 张 = 0.01 BTC → 1,250 USDT。
func TestNotionalCrypto(t *testing.T) {
	in := btc()
	price := int64(125_000) * 100_000_000 // 125,000 USDT
	qty := int64(1) * 100_000_000         // 1 张
	got := NotionalCents(in, price, qty)
	want := int64(125_000) // 1,250 USDT = 125,000 个 0.01 USDT
	if got != want {
		t.Errorf("1 张 BTC @ 125,000 应为 %d（1,250 USDT），得到 %d", want, got)
	}
	// 100 张
	if got := NotionalCents(in, price, qty*100); got != want*100 {
		t.Errorf("100 张应为 %d，得到 %d", want*100, got)
	}
}

// TestNotionalNoOverflow 加密的 价格×数量 实测到 1.25e21，远超 int64。
//
// 直接相乘会**静默回绕成负数** —— 不报错，只是账目从某一笔起全错。
// 这正是 SCHEMA 0.6 记下的那个既有缺陷。
func TestNotionalNoOverflow(t *testing.T) {
	in := btc()
	price := int64(125_000) * 100_000_000 // 1.25e13
	qty := int64(100_000) * 100_000_000   // 1e13 张 —— 乘积 1.25e26
	// 旧口径必然回绕
	if legacy := AmountCents(price, qty); legacy >= 0 {
		t.Logf("旧口径给出 %d（已回绕，仅作对照）", legacy)
	}
	got := NotionalCents(in, price, qty)
	// 100,000 张 × 0.01 BTC × 125,000 = 1.25e8 USDT = 1.25e10 个 0.01 USDT
	want := int64(12_500_000_000)
	if got != want {
		t.Errorf("期望 %d，得到 %d", want, got)
	}
	if got <= 0 {
		t.Error("结果为负 —— 溢出没被挡住")
	}
}

func TestNotionalSign(t *testing.T) {
	in := ashare()
	if got := NotionalCents(in, 10_000, -100); got != -100_000 {
		t.Errorf("负数量应给负金额，得到 %d", got)
	}
	if got := NotionalCents(in, -10_000, -100); got != 100_000 {
		t.Errorf("两个负号应相消，得到 %d", got)
	}
}

// TestNotionalRounding 四舍五入，不偏向任何一方。
func TestNotionalRounding(t *testing.T) {
	in := ashare()
	// price=1 qty=5 → 5/10 = 0.5 → 进位到 1
	if got := NotionalCents(in, 1, 5); got != 1 {
		t.Errorf("0.5 应进位到 1，得到 %d", got)
	}
	if got := NotionalCents(in, 1, 4); got != 0 {
		t.Errorf("0.4 应舍去，得到 %d", got)
	}
}

// TestNotionalNilInstrument 拿不到标的时退回 A 股口径而不是崩掉。
func TestNotionalNilInstrument(t *testing.T) {
	if got := NotionalCents(nil, 10_000, 100); got != AmountCents(10_000, 100) {
		t.Error("nil 标的应退回 A 股口径")
	}
}
