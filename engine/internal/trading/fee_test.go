package trading

import (
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func stock(board mktdata.Board) *mktdata.Instrument {
	return &mktdata.Instrument{
		ID: 1, Symbol: "600519", Type: mktdata.TypeStock,
		Board: board, TrackedBoard: board, PriceScale: 1000, QtyScale: 1,
	}
}

func etf() *mktdata.Instrument {
	return &mktdata.Instrument{
		ID: 2, Symbol: "510300", Type: mktdata.TypeETF,
		Board: mktdata.BoardMain, TrackedBoard: mktdata.BoardMain,
		PriceScale: 1000, QtyScale: 1,
	}
}

func loadDefault(t *testing.T) *ConfigFee {
	t.Helper()
	f, err := LoadFee(filepath.Join("..", "..", "..", "configs", "fee", "ashare_default.json"))
	if err != nil {
		t.Fatalf("加载默认费率失败: %v", err)
	}
	return f
}

// 手工核算一笔个股买入，与引擎输出逐分位比对 —— 这是本刀最硬的验收标准。
func TestStockBuyManualCheck(t *testing.T) {
	f := loadDefault(t)
	// 1000 股 @ 1700.00 元 = 1,700,000 元 = 170,000,000 分
	req := FeeRequest{
		Instrument: stock(mktdata.BoardMain), Side: SideBuy,
		Qty: 1000, AmountCents: 170_000_000, TradingDay: 20240318,
	}
	got := f.Calc(req)

	// 手工：佣金 170000000 × 250 / 1e6 = 42500 分 = 425 元（高于最低 500 分）
	//       印花税 买入不收（2008-09-19 起单边）
	//       过户费 170000000 × 10 / 1e6 = 1700 分 = 17 元
	//       合计 44200 分 = 442 元
	want := map[string]int64{"commission": 42500, "transfer_fee": 1700}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %d 分，手工核算 %d 分", k, got.Get(k), v)
		}
	}
	if got.Get("stamp_duty") != 0 {
		t.Errorf("买入不应收印花税，实收 %d 分", got.Get("stamp_duty"))
	}
	if got.Total != 44200 {
		t.Errorf("合计 = %d 分，手工核算 44200 分", got.Total)
	}
}

// 卖出应含印花税，且 2023-08-28 前后税率不同 —— 固定住时间维度。
func TestStampDutyTimeDimension(t *testing.T) {
	f := loadDefault(t)
	base := FeeRequest{
		Instrument: stock(mktdata.BoardMain), Side: SideSell,
		Qty: 1000, AmountCents: 170_000_000,
	}
	cases := []struct {
		day  int32
		want int64
		desc string
	}{
		{20230825, 170_000, "减半前 1‰"},
		{20230828, 85_000, "减半后 0.5‰"},
		{20080918, 170_000, "改单边前 1‰（双边，卖出侧同样收）"},
		{20070601, 510_000, "2007-05-30 上调后 3‰"},
	}
	for _, c := range cases {
		req := base
		req.TradingDay = c.day
		if got := f.Calc(req).Get("stamp_duty"); got != c.want {
			t.Errorf("%s：%d 日印花税 = %d 分，期望 %d 分", c.desc, c.day, got, c.want)
		}
	}
	// 改单边之后，买入不再收
	buy := base
	buy.Side, buy.TradingDay = SideBuy, 20240318
	if got := f.Calc(buy).Get("stamp_duty"); got != 0 {
		t.Errorf("单边征收后买入仍收了 %d 分印花税", got)
	}
	// 改单边之前，买入要收
	buyOld := base
	buyOld.Side, buyOld.TradingDay = SideBuy, 20080918
	if got := f.Calc(buyOld).Get("stamp_duty"); got != 170_000 {
		t.Errorf("双边征收期买入印花税 = %d 分，期望 170000 分", got)
	}
}

// ETF 免印花税与过户费 —— C8 的按标的类型分流。
func TestETFExemptions(t *testing.T) {
	f := loadDefault(t)
	req := FeeRequest{
		Instrument: etf(), Side: SideSell,
		Qty: 10000, AmountCents: 40_000_000, TradingDay: 20240318,
	}
	got := f.Calc(req)
	if got.Get("stamp_duty") != 0 {
		t.Errorf("ETF 不应收印花税，实收 %d 分", got.Get("stamp_duty"))
	}
	if got.Get("transfer_fee") != 0 {
		t.Errorf("ETF 不应收过户费，实收 %d 分", got.Get("transfer_fee"))
	}
	// 佣金 40000000 × 250 / 1e6 = 10000 分 = 100 元
	if got.Get("commission") != 10_000 {
		t.Errorf("ETF 佣金 = %d 分，期望 10000 分", got.Get("commission"))
	}
}

// 小额交易触发佣金最低 5 元。
func TestCommissionMinimum(t *testing.T) {
	f := loadDefault(t)
	// 100 股 @ 10.00 元 = 1000 元 = 100000 分
	// 按费率 100000 × 250 / 1e6 = 25 分，远低于最低 500 分
	req := FeeRequest{
		Instrument: stock(mktdata.BoardMain), Side: SideBuy,
		Qty: 100, AmountCents: 100_000, TradingDay: 20240318,
	}
	if got := f.Calc(req).Get("commission"); got != 500 {
		t.Errorf("佣金 = %d 分，期望触发最低 500 分", got)
	}
}

// 按费率算出的部分向上取整到分 —— 券商按此收取，不足一分按一分计。
func TestFeeRoundsUp(t *testing.T) {
	cfg := FeeConfig{Name: "t", Rules: []FeeRule{
		{Kind: "commission", Side: "both", RatePPM: 250},
	}}
	f, err := NewFee(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 1 分 × 250/1e6 = 0.00025 分 → 向上取整为 1 分
	req := FeeRequest{Instrument: stock(mktdata.BoardMain), Side: SideBuy,
		Qty: 1, AmountCents: 1, TradingDay: 20240318}
	if got := f.Calc(req).Get("commission"); got != 1 {
		t.Errorf("向上取整后应为 1 分，实为 %d 分", got)
	}
}

// 加密货币的 maker/taker 分档 —— 验证配置模型足够通用，
// 不是只为 A 股量身定做的。
func TestCryptoMakerTaker(t *testing.T) {
	cfg := FeeConfig{Name: "crypto", Rules: []FeeRule{
		{Kind: "trading_fee", Side: "both", Liquidity: "maker", RatePPM: 200},
		{Kind: "trading_fee", Side: "both", Liquidity: "taker", RatePPM: 500},
	}}
	f, err := NewFee(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inst := &mktdata.Instrument{ID: 9, Type: mktdata.TypeStock, PriceScale: 100000000}
	base := FeeRequest{Instrument: inst, Side: SideBuy,
		Qty: 1, AmountCents: 100_000_000, TradingDay: 20240318}

	mk, tk := base, base
	mk.Liquidity, tk.Liquidity = LiquidityMaker, LiquidityTaker
	if got := f.Calc(mk).Get("trading_fee"); got != 20_000 {
		t.Errorf("maker 费 = %d 分，期望 20000 分", got)
	}
	if got := f.Calc(tk).Get("trading_fee"); got != 50_000 {
		t.Errorf("taker 费 = %d 分，期望 50000 分", got)
	}
}

// 配置错误必须在加载期失败，而不是在回测中途悄悄多收费。
func TestInvalidConfigRejected(t *testing.T) {
	bad := []FeeConfig{
		{Name: "empty"},
		{Name: "no-kind", Rules: []FeeRule{{Side: "both", RatePPM: 1}}},
		{Name: "bad-side", Rules: []FeeRule{{Kind: "c", Side: "long", RatePPM: 1}}},
		{Name: "range", Rules: []FeeRule{{Kind: "c", Side: "both", From: 20240301, To: 20240101}}},
		{Name: "negative", Rules: []FeeRule{{Kind: "c", Side: "both", RatePPM: -1}}},
	}
	for _, c := range bad {
		if _, err := NewFee(c); err == nil {
			t.Errorf("配置 %q 本应被拒绝", c.Name)
		}
	}
}
