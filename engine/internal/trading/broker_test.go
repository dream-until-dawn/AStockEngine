package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func mkBar(day int32, o, h, l, c, preclose, vol int64) mktdata.Bar {
	return mktdata.Bar{TradingDay: day, Open: o, High: h, Low: l, Close: c,
		PreClose: preclose, Volume: vol, Amount: AmountCents(c, vol),
		TradeStatus: 1}
}

func testBroker(t *testing.T) *Broker {
	t.Helper()
	return NewBroker(NewAShareMarket(), loadDefault(t), NoSlippage{}, DefaultBrokerConfig())
}

func pending(id mktdata.InstrumentID, side Side, qty int64, ref PriceRef) *PendingOrder {
	return &PendingOrder{
		Order:    Order{Instrument: id, Side: side, Qty: qty},
		PriceRef: ref, MaxSteps: 1,
	}
}

func now(day int32) mktdata.TimePoint {
	return mktdata.TimePoint{TsClose: 1_700_000_000_000, TradingDay: day}
}

// 拒单原因必须结构化且**指向最根本的障碍** ——
// 停牌的股票不该报「现金不足」。
func TestRejectReasonsAreSpecific(t *testing.T) {
	br := testBroker(t)
	inst := mainStock()

	cases := []struct {
		name string
		bar  mktdata.Bar
		side Side
		qty  int64
		cash int64
		held int64
		want RejectReason
	}{
		{
			name: "停牌", side: SideBuy, qty: 1000, cash: oneMillionYuan,
			bar: func() mktdata.Bar {
				b := mkBar(day2024, 10000, 10000, 10000, 10000, 10000, 0)
				b.TradeStatus = 0
				return b
			}(),
			want: RejectSuspended,
		},
		{
			name: "一字涨停买不进", side: SideBuy, qty: 1000, cash: oneMillionYuan,
			// 前收 10.000，涨停 11.000；全天一个价且等于涨停
			bar:  mkBar(day2024, 11000, 11000, 11000, 11000, 10000, 1_000_000),
			want: RejectOneWordBoard,
		},
		{
			name: "一字跌停卖不出", side: SideSell, qty: 1000, cash: oneMillionYuan, held: 5000,
			bar:  mkBar(day2024, 9000, 9000, 9000, 9000, 10000, 1_000_000),
			want: RejectOneWordBoard,
		},
		{
			name: "开盘涨停买不进", side: SideBuy, qty: 1000, cash: oneMillionYuan,
			// 开盘即涨停但盘中打开过（high != low），故非一字板
			bar:  mkBar(day2024, 11000, 11000, 10500, 10800, 10000, 1_000_000),
			want: RejectLimitUpNoBuy,
		},
		{
			name: "现金不足", side: SideBuy, qty: 1000, cash: 100,
			bar:  mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 1_000_000),
			want: RejectInsufficientCash,
		},
		{
			name: "无持仓卖出", side: SideSell, qty: 1000, cash: oneMillionYuan, held: 0,
			bar:  mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 1_000_000),
			want: RejectInsufficientPosition,
		},
		{
			name: "申报数量不合规", side: SideBuy, qty: 50, cash: oneMillionYuan,
			bar:  mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 1_000_000),
			want: RejectInvalidQty,
		},
		{
			name: "当日成交量为 0", side: SideBuy, qty: 1000, cash: oneMillionYuan,
			bar:  mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 0),
			want: RejectVolumeCap,
		},
	}

	for _, c := range cases {
		pf := NewPortfolio(c.cash)
		if c.held > 0 {
			pf.ApplyFill(Fill{Order: Order{Instrument: inst.ID, Side: SideBuy, Qty: c.held},
				Price: 10000, Qty: c.held, Fee: FeeBreakdown{Items: map[string]int64{}}}.WithAmount(inst), 0)
			// 买完把现金复位：这些用例要测的是撮合的拒单分支，
			// 不该被建仓花掉的钱干扰
			st := pf.Snapshot()
			st.Cash = c.cash
			pf.Restore(st)
		}
		po := pending(inst.ID, c.side, c.qty, PriceOpen)
		_, rej, ok := br.Match(po, inst, c.bar, now(day2024), pf)
		if ok {
			t.Errorf("%s：本应拒单，却成交了", c.name)
			continue
		}
		if rej.Reason != c.want {
			t.Errorf("%s：拒单原因 = %s，期望 %s（detail: %s）",
				c.name, rej.Reason, c.want, rej.Detail)
		}
		if rej.Detail == "" {
			t.Errorf("%s：拒单缺少 detail —— 单步调试需要看到数值依据", c.name)
		}
	}
}

// 成交量上限：默认 10%，超出部分截断而非整单拒绝。
func TestVolumeCapTruncates(t *testing.T) {
	br := testBroker(t)
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan * 100)
	// 当日成交量 10000 股，上限 10% = 1000 股
	b := mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 10_000)
	po := pending(inst.ID, SideBuy, 5000, PriceOpen)

	fill, _, ok := br.Match(po, inst, b, now(day2024), pf)
	if !ok {
		t.Fatal("应部分成交")
	}
	if fill.Qty != 1000 {
		t.Errorf("成交量 = %d，期望截断至上限 1000（当日 10000 × 10%%）", fill.Qty)
	}
}

// 盘后定价按收盘价成交、次日开盘按开盘价成交 —— 价格基准生效。
func TestPriceRefApplied(t *testing.T) {
	br := testBroker(t)
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan * 100)
	b := mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 1_000_000)

	openFill, _, _ := br.Match(pending(inst.ID, SideBuy, 1000, PriceOpen), inst, b, now(day2024), pf)
	if openFill.Price != 10000 {
		t.Errorf("按开盘价成交 = %d，期望 10000", openFill.Price)
	}
	closeFill, _, _ := br.Match(pending(inst.ID, SideBuy, 1000, PriceClose), inst, b, now(day2024), pf)
	if closeFill.Price != 10200 {
		t.Errorf("按收盘价成交 = %d，期望 10200", closeFill.Price)
	}
}

// 滑点不得把成交价推出涨跌停区间 —— 那是不可能发生的成交。
func TestSlippageClampedToLimits(t *testing.T) {
	br := NewBroker(NewAShareMarket(), loadDefault(t),
		BpsSlippage{Bps: 2000}, DefaultBrokerConfig()) // 20% 滑点，刻意夸张
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan * 100)
	// 前收 10.000 → 涨停 11.000；开盘 10.900，加 20% 滑点会到 13.08
	b := mkBar(day2024, 10900, 10950, 10500, 10800, 10000, 1_000_000)

	fill, rej, ok := br.Match(pending(inst.ID, SideBuy, 1000, PriceOpen), inst, b, now(day2024), pf)
	if !ok {
		t.Fatalf("本应成交，却拒单：%s %s", rej.Reason, rej.Detail)
	}
	if fill.Price > 11000 {
		t.Errorf("成交价 %d 超出涨停 11000 —— 滑点未被限制", fill.Price)
	}
}

// 限价单：当日价格未触及限价则不成交。
func TestLimitOrderNotReached(t *testing.T) {
	br := testBroker(t)
	inst := mainStock()
	pf := NewPortfolio(oneMillionYuan * 100)
	b := mkBar(day2024, 10000, 10500, 9800, 10200, 10000, 1_000_000)

	po := pending(inst.ID, SideBuy, 1000, PriceOpen)
	po.Type, po.LimitPrice = OrderLimit, 9000 // 限价 9.000，当日最低 9.800
	_, rej, ok := br.Match(po, inst, b, now(day2024), pf)
	if ok {
		t.Error("限价未触及本应拒单")
	}
	if rej.Reason != RejectLimitPriceNotReached {
		t.Errorf("拒单原因 = %s，期望限价未触及", rej.Reason)
	}

	// 限价 10.000 且当日最低 9.800，应按限价成交
	po2 := pending(inst.ID, SideBuy, 1000, PriceOpen)
	po2.Type, po2.LimitPrice = OrderLimit, 10_000
	fill, _, ok := br.Match(po2, inst, b, now(day2024), pf)
	if !ok || fill.Price > 10_000 {
		t.Errorf("限价内应成交且价格不高于限价，得到 ok=%v price=%d", ok, fill.Price)
	}
}

// TestBpsSlippageCostOnNotional 钉住滑点的施加位置。
//
// 早期版本把滑点加在**单价**上（厘），5 bp 在 20 元以下的标的上不足一厘，
// 于是三种取整方向实测分别给出 3.67 / 5.34 / 5.63 bp（下取整 / 四舍五入 /
// 上取整），而配置写的是 5.00 —— 问题不在取整方向，在于施加的位置。
// 现在按成交总额算（分），相对误差可忽略。
func TestBpsSlippageCostOnNotional(t *testing.T) {
	s := BpsSlippage{Bps: 5}
	var bar mktdata.Bar

	cases := []struct {
		price, qty int64
		want       int64 // 期望滑点成本（分）
		note       string
	}{
		// 成交额 = price×qty/10 分；5 bp = ×5/10000
		{594, 100_000, 2_970, "0.594 元 × 10 万股 = 5.94 万元 → 29.70 元"},
		{1_071, 100_000, 5_355, "1.071 元 × 10 万股 = 10.71 万元 → 53.55 元"},
		{9_000, 10_000, 4_500, "9.000 元 × 1 万股 = 9 万元 → 45.00 元"},
		{100_000, 1_000, 5_000, "100 元 × 1000 股 = 10 万元 → 50.00 元"},
		{1_297_400, 100, 6_487, "1297.4 元 × 100 股 = 12.974 万元 → 64.87 元"},
	}
	for _, c := range cases {
		amt := AmountCents(c.price, c.qty)
		got := s.CostCents(SideBuy, amt, bar)
		if got != c.want {
			t.Errorf("%s：期望 %d 分，得到 %d 分", c.note, c.want, got)
		}
		// 买卖两侧成本相同 —— 方向由账本决定（买加、卖减），不由滑点模型决定
		if sell := s.CostCents(SideSell, amt, bar); sell != got {
			t.Errorf("%s：买卖两侧滑点应相同，得到 %d / %d", c.note, got, sell)
		}
	}
}

// TestBpsSlippageAccuracy 无论价位高低，实际生效的 bp 都应贴近配置值。
// 这正是旧实现做不到的（0.594 元的标的误差 −100% ~ +237%）。
func TestBpsSlippageAccuracy(t *testing.T) {
	s := BpsSlippage{Bps: 5}
	var bar mktdata.Bar
	for _, price := range []int64{594, 1_071, 1_228, 1_936, 3_132, 5_675, 9_000, 100_000} {
		qty := int64(100_000)
		amount := AmountCents(price, qty)
		got := s.CostCents(SideBuy, AmountCents(price, qty), bar)
		effBP := float64(got) / float64(amount) * 10_000
		if effBP < 4.99 || effBP > 5.01 {
			t.Errorf("价 %.3f 元：实际生效 %.4f bp，偏离配置的 5 bp", float64(price)/1000, effBP)
		}
	}
}

// TestBpsSlippageZero 0 bp 必须真的是 0。
func TestBpsSlippageZero(t *testing.T) {
	s := BpsSlippage{Bps: 0}
	var bar mktdata.Bar
	for _, ref := range []int64{100, 9_000, 1_297_400} {
		if got := s.CostCents(SideBuy, AmountCents(ref, 10_000), bar); got != 0 {
			t.Errorf("0 bp：期望 0，得到 %d", got)
		}
	}
	if got := (NoSlippage{}).CostCents(SideBuy, 9_000_000, bar); got != 0 {
		t.Errorf("NoSlippage：期望 0，得到 %d", got)
	}
}
