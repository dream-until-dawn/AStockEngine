package trading

import (
	"encoding/json"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// ---- 测试替身 ----

type fakeSizeCtx struct {
	pf      *Portfolio
	equity  int64
	initial int64
	bars    map[mktdata.InstrumentID]mktdata.Bar
	insts   map[mktdata.InstrumentID]*mktdata.Instrument
	pending []PendingOrder
	avail   map[mktdata.InstrumentID]int64
	mkt     Market
	peak    int64
}

func (c *fakeSizeCtx) Ledger() Ledger          { return c.pf }
func (c *fakeSizeCtx) EquityCents() int64      { return c.equity }
func (c *fakeSizeCtx) InitialCashCents() int64 { return c.initial }
func (c *fakeSizeCtx) PeakEquityCents() int64  { return c.peak }
func (c *fakeSizeCtx) Pending() []PendingOrder { return c.pending }
func (c *fakeSizeCtx) Market() Market          { return c.mkt }

func (c *fakeSizeCtx) Time() mktdata.TimePoint {
	return mktdata.TimePoint{TradingDay: 20250102}
}

func (c *fakeSizeCtx) Available(id mktdata.InstrumentID) int64 { return c.avail[id] }

func (c *fakeSizeCtx) Bar(id mktdata.InstrumentID) (mktdata.Bar, bool) {
	b, ok := c.bars[id]
	return b, ok
}

func (c *fakeSizeCtx) Instrument(id mktdata.InstrumentID) *mktdata.Instrument {
	return c.insts[id]
}

var (
	_ SizeContext = (*fakeSizeCtx)(nil)
	_ RiskContext = (*fakeSizeCtx)(nil)
)

// seedPosition 直接种一笔持仓。
//
// 账本字段在 v0.3 之后全部私有 —— 测试也不例外，只能经快照进出。
// 这不是麻烦，是同一条保证：没有任何路径能绕过撮合改账。
func seedPosition(pf *Portfolio, id mktdata.InstrumentID, qty int64) {
	st := pf.Snapshot()
	if st.Positions == nil {
		st.Positions = map[mktdata.InstrumentID]*Position{}
	}
	st.Positions[id] = &Position{Total: qty, Lots: []lot{{Qty: qty}}}
	pf.Restore(st)
}

// newCtx 造一个有 n 只标的的场景，每只收盘价 10.000 元。
func newCtx(n int, cash int64) *fakeSizeCtx {
	c := &fakeSizeCtx{
		pf:      NewPortfolio(cash),
		equity:  cash,
		initial: cash,
		peak:    cash,
		bars:    map[mktdata.InstrumentID]mktdata.Bar{},
		insts:   map[mktdata.InstrumentID]*mktdata.Instrument{},
		avail:   map[mktdata.InstrumentID]int64{},
		mkt:     NewAShareMarket(),
	}
	for i := 1; i <= n; i++ {
		id := mktdata.InstrumentID(i)
		c.bars[id] = mktdata.Bar{TradingDay: 20250102, Close: 10_000, Open: 10_000,
			High: 10_000, Low: 10_000, PreClose: 10_000, Volume: 1e8, TradeStatus: 1}
		c.insts[id] = &mktdata.Instrument{
			ID: id, Symbol: "T", Type: mktdata.TypeStock, Board: mktdata.BoardMain,
			PriceScale: 1000, QtyScale: 1, MinOrderQty: 100, QtyStep: 100,
			Status: mktdata.StatusListed,
		}
	}
	return c
}

func enters(ids ...int) []Signal {
	out := make([]Signal, 0, len(ids))
	for _, i := range ids {
		out = append(out, Signal{
			Instrument: mktdata.InstrumentID(i), Kind: SignalEnter, Side: SideBuy,
		})
	}
	return out
}

func mustSizer(t *testing.T, name, params string) Sizer {
	t.Helper()
	s, err := Sizers.Build(name, json.RawMessage(params))
	if err != nil {
		t.Fatalf("构造 %s 失败: %v", name, err)
	}
	return s
}

// ---- equal_weight ----

func TestEqualWeightSlotBudget(t *testing.T) {
	// 100 万元切 10 份 = 10 万元一份；10.000 元一股 → 10000 股
	ctx := newCtx(3, 100_000_000)
	orders := mustSizer(t, "equal_weight", `{"slots":10}`).Size(enters(1, 2, 3), ctx)
	if len(orders) != 3 {
		t.Fatalf("期望 3 张单，得到 %d", len(orders))
	}
	for _, o := range orders {
		if o.Qty != 10_000 {
			t.Errorf("标的 %d：期望 10000 股，得到 %d", o.Instrument, o.Qty)
		}
		if o.Side != SideBuy {
			t.Errorf("标的 %d：期望买单", o.Instrument)
		}
	}
}

func TestEqualWeightBaseInitialVsEquity(t *testing.T) {
	// 权益翻倍后：base=initial 仍按初始资金切份，base=equity 跟着涨
	ctx := newCtx(1, 100_000_000)
	ctx.equity = 200_000_000

	byInitial := mustSizer(t, "equal_weight", `{"slots":10,"base":"initial"}`).Size(enters(1), ctx)
	byEquity := mustSizer(t, "equal_weight", `{"slots":10,"base":"equity"}`).Size(enters(1), ctx)

	if len(byInitial) != 1 || len(byEquity) != 1 {
		t.Fatalf("各期望 1 张单，得到 %d / %d", len(byInitial), len(byEquity))
	}
	if byInitial[0].Qty != 10_000 {
		t.Errorf("base=initial：期望 10000 股，得到 %d", byInitial[0].Qty)
	}
	if byEquity[0].Qty != 20_000 {
		t.Errorf("base=equity：期望 20000 股，得到 %d", byEquity[0].Qty)
	}
}

// TestEqualWeightSlotsCountDedup 钉住 v0.3 的槽位计数语义。
//
// v0.2 的样例策略把「已持有」与「在途」分别计数不去重，于是同一只标的
// 挂着卖单时会占掉两个槽 —— 卖单意外收紧了买入上限。
// 这里明确要求**去重**：持有 + 挂单的同一只标的只算一个占用。
func TestEqualWeightSlotsCountDedup(t *testing.T) {
	ctx := newCtx(5, 100_000_000)
	// 标的 1 持有中，且挂着一张卖单 —— 按 v0.2 会占 2 个槽
	seedPosition(ctx.pf, 1, 10_000)
	ctx.pending = []PendingOrder{{
		Order: Order{Instrument: 1, Side: SideSell, Qty: 10_000},
	}}
	ctx.avail[1] = 10_000

	// slots=2：去重后占用 1 个，还剩 1 个槽，标的 2 应当能买
	orders := mustSizer(t, "equal_weight", `{"slots":2}`).Size(enters(2, 3), ctx)
	if len(orders) != 1 {
		t.Fatalf("期望 1 张单（剩 1 个槽），得到 %d", len(orders))
	}
	if orders[0].Instrument != 2 {
		t.Errorf("期望买入标的 2（信号顺序在前），得到 %d", orders[0].Instrument)
	}
}

func TestEqualWeightSkipsHeldAndPending(t *testing.T) {
	ctx := newCtx(3, 100_000_000)
	seedPosition(ctx.pf, 1, 100)
	ctx.pending = []PendingOrder{{Order: Order{Instrument: 2, Side: SideBuy, Qty: 100}}}

	orders := mustSizer(t, "equal_weight", `{"slots":10}`).Size(enters(1, 2, 3), ctx)
	if len(orders) != 1 || orders[0].Instrument != 3 {
		t.Fatalf("已持有与在途的标的都不该再下单，得到 %+v", orders)
	}
}

func TestSizerExitUsesAvailable(t *testing.T) {
	// 清仓卖出的数量是**可卖数量**（已考虑 T+1），不是持仓总量
	ctx := newCtx(1, 0)
	seedPosition(ctx.pf, 1, 1_000)
	ctx.avail[1] = 600

	orders := mustSizer(t, "equal_weight", `{"slots":10}`).Size([]Signal{
		{Instrument: 1, Kind: SignalExit, Side: SideSell, Tag: "exit"},
	}, ctx)
	if len(orders) != 1 {
		t.Fatalf("期望 1 张卖单，得到 %d", len(orders))
	}
	if orders[0].Qty != 600 {
		t.Errorf("期望卖 600（可卖量），得到 %d", orders[0].Qty)
	}
	if orders[0].Side != SideSell {
		t.Error("期望卖单")
	}
}

// TestSizerQtyOverride 策略坚持自己定量时绕过仓位计算，但仍要过申报单位规整。
func TestSizerQtyOverride(t *testing.T) {
	ctx := newCtx(1, 100_000_000)
	orders := mustSizer(t, "equal_weight", `{"slots":10}`).Size([]Signal{
		{Instrument: 1, Kind: SignalEnter, Side: SideBuy, Qty: 250},
	}, ctx)
	if len(orders) != 1 {
		t.Fatalf("期望 1 张单，得到 %d", len(orders))
	}
	// 250 股不是 100 的整数倍，A 股买入要向下取整到 200
	if orders[0].Qty != 200 {
		t.Errorf("期望规整到 200 股，得到 %d", orders[0].Qty)
	}
}

// ---- 其他 Sizer ----

func TestFixedCashAndPctEquity(t *testing.T) {
	ctx := newCtx(2, 100_000_000)

	fc := mustSizer(t, "fixed_cash", `{"cents":5000000}`).Size(enters(1), ctx)
	if len(fc) != 1 || fc[0].Qty != 5_000 { // 5 万元 / 10 元 = 5000 股
		t.Errorf("fixed_cash: 期望 5000 股，得到 %+v", fc)
	}

	pe := mustSizer(t, "pct_equity", `{"pct":25}`).Size(enters(1), ctx)
	if len(pe) != 1 || pe[0].Qty != 25_000 { // 25 万元 / 10 元 = 25000 股
		t.Errorf("pct_equity: 期望 25000 股，得到 %+v", pe)
	}
}

func TestStrengthWeightedNormalises(t *testing.T) {
	ctx := newCtx(3, 100_000_000)
	sz := mustSizer(t, "strength_weighted", `{"total_pct":100}`)
	orders := sz.Size([]Signal{
		{Instrument: 1, Kind: SignalEnter, Side: SideBuy, Strength: 0.5},
		{Instrument: 2, Kind: SignalEnter, Side: SideBuy, Strength: 0.3},
		{Instrument: 3, Kind: SignalEnter, Side: SideBuy, Strength: 0.2},
	}, ctx)
	if len(orders) != 3 {
		t.Fatalf("期望 3 张单，得到 %d", len(orders))
	}
	// 信心之和 1.0，总预算 100 万 → 50 万 / 30 万 / 20 万 → 50000 / 30000 / 20000 股
	want := map[mktdata.InstrumentID]int64{1: 50_000, 2: 30_000, 3: 20_000}
	for _, o := range orders {
		if o.Qty != want[o.Instrument] {
			t.Errorf("标的 %d：期望 %d 股，得到 %d", o.Instrument, want[o.Instrument], o.Qty)
		}
	}
}

// TestStrengthWeightedZeroStrength 策略没给信心时按等权处理，
// 而不是当成 0 把信号全部丢掉。
func TestStrengthWeightedZeroStrength(t *testing.T) {
	ctx := newCtx(2, 100_000_000)
	orders := mustSizer(t, "strength_weighted", `{"total_pct":100}`).Size(enters(1, 2), ctx)
	if len(orders) != 2 {
		t.Fatalf("期望 2 张单，得到 %d", len(orders))
	}
	for _, o := range orders {
		if o.Qty != 50_000 {
			t.Errorf("标的 %d：期望等分 50000 股，得到 %d", o.Instrument, o.Qty)
		}
	}
}

func TestSizerRejectsUnknownParam(t *testing.T) {
	if _, err := Sizers.Build("equal_weight", json.RawMessage(`{"slot":10}`)); err == nil {
		t.Fatal("把 slots 写成 slot 应当报错而不是静默用默认值")
	}
	if _, err := Sizers.Build("equal_weight", json.RawMessage(`{"base":"random"}`)); err == nil {
		t.Fatal("base 取值不在 Options 中应当报错")
	}
	if _, err := Sizers.Build("no_such_sizer", nil); err == nil {
		t.Fatal("未知 sizer 应当报错")
	}
}
