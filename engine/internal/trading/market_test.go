package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func bar(day int32, preclose int64, st int8) mktdata.Bar {
	return mktdata.Bar{TradingDay: day, PreClose: preclose, Open: preclose,
		High: preclose, Low: preclose, Close: preclose, TradeStatus: 1, IsST: st}
}

// 涨跌停取整必须是**四舍五入**，不是银行家舍入。
// 这两个数在 v0.1 的 ETL 侧就踩过（ETL.md 6.2），此处固定住 Go 侧的实现。
func TestLimitPriceRoundsHalfUp(t *testing.T) {
	m := NewAShareMarket()
	inst := stock(mktdata.BoardMain)
	cases := []struct{ preclose, wantUp int64 }{
		{4350, 4790}, // 4.35 × 1.10 = 4.785 → 4.79（银行家舍入会得 4.78）
		{5350, 5890}, // 5.35 × 1.10 = 5.885 → 5.89（银行家舍入会得 5.88）
		{10000, 11000},
	}
	for _, c := range cases {
		up, _, ok := m.LimitPrices(inst, bar(20240318, c.preclose, 0))
		if !ok || up != c.wantUp {
			t.Errorf("前收 %d 厘：涨停 = %d 厘，期望 %d 厘", c.preclose, up, c.wantUp)
		}
	}
}

// 涨跌幅有时间维度：ST 主板 2026-07-06 起由 5% 放宽至 10%，
// 创业板 2020-08-24 起由 10% 放宽至 20%。
func TestLimitRatioTimeDimension(t *testing.T) {
	m := NewAShareMarket()
	cases := []struct {
		name     string
		inst     *mktdata.Instrument
		day      int32
		st       int8
		preclose int64
		wantUp   int64
	}{
		{"主板 ST 放宽前 5%", stock(mktdata.BoardMain), 20260705, 1, 10000, 10500},
		{"主板 ST 放宽后 10%", stock(mktdata.BoardMain), 20260706, 1, 10000, 11000},
		{"主板非 ST 恒 10%", stock(mktdata.BoardMain), 20260705, 0, 10000, 11000},
		{"创业板放宽前 10%", stock(mktdata.BoardChiNext), 20200821, 0, 10000, 11000},
		{"创业板放宽后 20%", stock(mktdata.BoardChiNext), 20200824, 0, 10000, 12000},
		{"创业板 ST 仍 20%", stock(mktdata.BoardChiNext), 20240318, 1, 10000, 12000},
		{"科创板 20%", stock(mktdata.BoardSTAR), 20240318, 0, 10000, 12000},
		{"北交所 30%", stock(mktdata.BoardBSE), 20240318, 0, 10000, 13000},
	}
	for _, c := range cases {
		up, _, ok := m.LimitPrices(c.inst, bar(c.day, c.preclose, c.st))
		if !ok || up != c.wantUp {
			t.Errorf("%s：涨停 = %d，期望 %d", c.name, up, c.wantUp)
		}
	}
}

// ETF 用 TrackedBoard 而非 Board —— ETF 自身不属任何板块，
// 其涨跌停由跟踪的指数决定。
func TestETFUsesTrackedBoard(t *testing.T) {
	m := NewAShareMarket()
	e := etf()
	e.Board = mktdata.BoardMain
	e.TrackedBoard = mktdata.BoardSTAR // 跟踪科创板指数
	up, _, _ := m.LimitPrices(e, bar(20240318, 10000, 0))
	if up != 12000 {
		t.Errorf("跟踪科创板的 ETF 涨停 = %d，期望 12000（20%%）", up)
	}
}

// 可执行时点：主板 T+1，创业板/科创板可 T 日盘后成交。
// 这是第一刀评审推翻「全市场 T+1」后的核心行为。
func TestNextExecutableDiffersByBoard(t *testing.T) {
	m := NewAShareMarket()
	sig := mktdata.TimePoint{TsClose: 1_700_000_000_000, TradingDay: 20240318}

	main, _ := m.NextExecutable(stock(mktdata.BoardMain), sig)
	if main.NotBefore <= sig.TsClose {
		t.Errorf("主板应严格晚于信号时点，NotBefore=%d signal=%d", main.NotBefore, sig.TsClose)
	}
	if main.PriceRef != PriceOpen {
		t.Errorf("主板应按开盘价成交，实为 %s", main.PriceRef)
	}

	for _, b := range []mktdata.Board{mktdata.BoardChiNext, mktdata.BoardSTAR} {
		w, _ := m.NextExecutable(stock(b), sig)
		if w.NotBefore > sig.TsClose {
			t.Errorf("%s 支持盘后定价，应可在同一时点成交", b)
		}
		if w.PriceRef != PriceClose {
			t.Errorf("%s 盘后定价应按收盘价成交，实为 %s", b, w.PriceRef)
		}
	}
}

// 盘后定价买入的股份同样 T+1，不因成交时段不同而改变。
func TestSellableAlwaysNextStep(t *testing.T) {
	m := NewAShareMarket()
	filled := mktdata.TimePoint{TsClose: 1_700_000_000_000, TradingDay: 20240318}
	for _, b := range []mktdata.Board{mktdata.BoardMain, mktdata.BoardChiNext, mktdata.BoardSTAR} {
		if got := m.SellableFrom(stock(b), filled); got <= filled.TsClose {
			t.Errorf("%s：可卖时刻 %d 未晚于成交时刻 %d", b, got, filled.TsClose)
		}
	}
}

// 申报数量：买入按 qty_step 向下取整并不得低于 min_order_qty；
// 卖出允许零股，但仅限一次性全部卖出。
func TestNormalizeQty(t *testing.T) {
	m := NewAShareMarket()
	main := stock(mktdata.BoardMain)
	main.MinOrderQty, main.QtyStep = 100, 100
	star := stock(mktdata.BoardSTAR)
	star.MinOrderQty, star.QtyStep = 200, 1 // 科创板 200 股起、1 股递增

	cases := []struct {
		name string
		inst *mktdata.Instrument
		qty  int64
		side Side
		held int64
		want int64
		ok   bool
	}{
		{"主板买 150 取整到 100", main, 150, SideBuy, 0, 100, true},
		{"主板买 50 低于最小量", main, 50, SideBuy, 0, 0, false},
		{"科创板买 250 允许 1 股递增", star, 250, SideBuy, 0, 250, true},
		{"科创板买 150 低于 200", star, 150, SideBuy, 0, 0, false},
		{"卖出零股需全部卖出", main, 50, SideSell, 50, 50, true},
		{"卖出部分需按步长", main, 150, SideSell, 1000, 100, true},
		{"卖出超过持仓则截断为全部", main, 2000, SideSell, 50, 50, true},
		{"无持仓卖出失败", main, 100, SideSell, 0, 0, false},
	}
	for _, c := range cases {
		got, ok := m.NormalizeQty(c.inst, c.qty, c.side, c.held)
		if got != c.want || ok != c.ok {
			t.Errorf("%s：得到 (%d,%v)，期望 (%d,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// 停牌行的 OHLC 全等于停牌前收盘价，若不拦截会「成交」在不存在的价位上。
func TestSuspendedNotTradable(t *testing.T) {
	m := NewAShareMarket()
	inst := stock(mktdata.BoardMain)
	inst.ListDate = 20200101
	b := bar(20240318, 10000, 0)
	b.TradeStatus = 0
	if m.Tradable(inst, b) {
		t.Error("停牌日不应可交易")
	}
	b.TradeStatus = 1
	if !m.Tradable(inst, b) {
		t.Error("正常交易日应可交易")
	}
	// 上市前
	if m.Tradable(inst, bar(20190101, 10000, 0)) {
		t.Error("上市前不应可交易")
	}
}
