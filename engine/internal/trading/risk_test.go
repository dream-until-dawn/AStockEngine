package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// 流动性门槛是「标的池含退市股」的前提条件：退市前的缩量期必须被挡掉，
// 否则等于假装能在一天成交 3 万元的股票上买进 10 万元。

func turnoverCtx(amountCents int64) *fakeSizeCtx {
	id := mktdata.InstrumentID(1)
	return &fakeSizeCtx{
		pf:      NewPortfolio(100_000_000),
		equity:  100_000_000,
		initial: 100_000_000,
		mkt:     NewAShareMarket(),
		bars: map[mktdata.InstrumentID]mktdata.Bar{
			id: {TradingDay: 20250102, Close: 10_000, PreClose: 10_000,
				Amount: amountCents, TradeStatus: 1},
		},
		insts: map[mktdata.InstrumentID]*mktdata.Instrument{
			id: {ID: id, Market: mktdata.MarketAShare, Symbol: "600000",
				Board: mktdata.BoardMain, TrackedBoard: mktdata.BoardMain,
				PriceScale: 1000, QtyScale: 1, MinOrderQty: 100, QtyStep: 100},
		},
	}
}

func buyOrder(qty int64) Order {
	return Order{Instrument: 1, Side: SideBuy, Qty: qty}
}

func TestMinTurnoverBlocksThinNames(t *testing.T) {
	r := &MinTurnover{minAmountCents: 500 * 1_000_000} // 门槛 500 万元
	// 当日成交额 300 万元 —— 低于门槛
	if _, rej, ok := r.Check(buyOrder(1000), turnoverCtx(300*1_000_000)); ok {
		t.Error("成交额低于门槛时应拒单")
	} else if rej.Rule != "min_turnover" {
		t.Errorf("拒单原因应标明规则名，得到 %q", rej.Rule)
	}
	// 800 万元 —— 放行
	if _, _, ok := r.Check(buyOrder(1000), turnoverCtx(800*1_000_000)); !ok {
		t.Error("成交额高于门槛时应放行")
	}
}

// TestMinTurnoverNeverBlocksSell 卖出永远放行。
//
// 手里已经有的东西，流动性差不是不卖的理由，恰恰是要卖的理由。
// 挡住卖单会把「退市前缩量」变成「永远卖不掉」，账面上凭空多出一笔资产。
func TestMinTurnoverNeverBlocksSell(t *testing.T) {
	r := &MinTurnover{minAmountCents: 500 * 1_000_000}
	o := Order{Instrument: 1, Side: SideSell, Qty: 1000}
	if _, _, ok := r.Check(o, turnoverCtx(1)); !ok {
		t.Error("卖单不该被流动性门槛拦下")
	}
}

// TestMinTurnoverShrinksBySharePPM 单笔占比上限触发缩量而非拒单。
func TestMinTurnoverShrinksBySharePPM(t *testing.T) {
	// 当日成交额 1000 万元 = 1e9 分；上限 1% → 10 万元 = 1e7 分。
	// 价格 10.000 元（10000 厘），10 万元最多 10,000 股
	r := &MinTurnover{maxSharePPM: 10_000}
	out, _, ok := r.Check(buyOrder(50_000), turnoverCtx(1000*1_000_000))
	if !ok {
		t.Fatal("应缩量而不是拒单")
	}
	if out.Qty != 10_000 {
		t.Errorf("应缩到 10000 股（10 万元 ÷ 10 元），得到 %d", out.Qty)
	}
}

// TestMinTurnoverNoOverflow 成交额极大时不得溢出。
//
// 直接算 amount × ppm 在 amount ~1e13 分、ppm 1e6 时是 1e19，超过 int64。
// 这不是假想：加密标的的日成交额定点值实测到 9.15e12。
func TestMinTurnoverNoOverflow(t *testing.T) {
	r := &MinTurnover{maxSharePPM: 1_000_000}
	out, _, ok := r.Check(buyOrder(100), turnoverCtx(9_148_486_623_968))
	if !ok || out.Qty <= 0 {
		t.Fatalf("大成交额下应正常放行，得到 qty=%d ok=%v", out.Qty, ok)
	}
}

// TestMinTurnoverZeroMeansUnlimited 两个参数都为 0 时等于没配。
func TestMinTurnoverZeroMeansUnlimited(t *testing.T) {
	r := &MinTurnover{}
	out, _, ok := r.Check(buyOrder(1000), turnoverCtx(0))
	if !ok || out.Qty != 1000 {
		t.Errorf("阈值为 0 应原样放行，得到 qty=%d ok=%v", out.Qty, ok)
	}
}
