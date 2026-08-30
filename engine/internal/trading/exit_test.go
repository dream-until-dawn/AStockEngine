package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// exitCtx 造一个持仓：成本 costCents，现价 close，可卖 avail。
func exitCtx(costCents, close, qty, avail int64) *fakeSizeCtx {
	id := mktdata.InstrumentID(1)
	pf := NewPortfolio(100_000_000)
	if qty > 0 {
		st := pf.Snapshot()
		if st.Positions == nil {
			st.Positions = map[mktdata.InstrumentID]*Position{}
		}
		st.Positions[id] = &Position{
			Total: qty, CostCents: costCents, Lots: []lot{{Qty: qty}},
		}
		pf.Restore(st)
	}
	return &fakeSizeCtx{
		pf: pf, equity: 100_000_000, initial: 100_000_000, mkt: NewAShareMarket(),
		avail: map[mktdata.InstrumentID]int64{id: avail},
		bars: map[mktdata.InstrumentID]mktdata.Bar{
			id: {TradingDay: 20250102, Close: close, PreClose: close,
				Amount: 1_000_000_000, TradeStatus: 1},
		},
		insts: map[mktdata.InstrumentID]*mktdata.Instrument{
			id: {ID: id, Market: mktdata.MarketAShare, Symbol: "600000",
				Board: mktdata.BoardMain, TrackedBoard: mktdata.BoardMain,
				PriceScale: 1000, QtyScale: 1, MinOrderQty: 100, QtyStep: 100},
		},
	}
}

// ---- 止损 ----

func TestStopLossTriggersBelowThreshold(t *testing.T) {
	r := &StopLoss{ratio: 0.9} // 亏 10% 止损
	// 成本 10 万元、1000 股，现价 8.900 元 → 市值 8.9 万 = 亏 11%
	if got := r.OnStep(exitCtx(10_000_00, 8_900, 1000, 1000)); len(got) != 1 {
		t.Errorf("亏 11%% 应触发止损，得到 %d 条信号", len(got))
	} else if got[0].Kind != SignalExit || got[0].Side != SideSell {
		t.Errorf("止损应发出清仓卖出信号，得到 %+v", got[0])
	}
	// 亏 5%，不该触发
	if got := r.OnStep(exitCtx(10_000_00, 9_500, 1000, 1000)); len(got) != 0 {
		t.Errorf("亏 5%% 不该触发，得到 %d 条", len(got))
	}
}

// TestStopLossSkipsUnsellable T+1 锁着时不发信号。
//
// 发了只会得到一条「无券可卖」的拒单，而且**每天都会重发**。
// 买入次日触发止损是真实存在的情形 —— 那一天你确实卖不掉。
func TestStopLossSkipsUnsellable(t *testing.T) {
	r := &StopLoss{ratio: 0.9}
	if got := r.OnStep(exitCtx(10_000_00, 5_000, 1000, 0)); len(got) != 0 {
		t.Errorf("可卖为 0 时不该发信号，得到 %d 条", len(got))
	}
}

// TestStopLossSkipsPending 已有在途单时不重复发。
func TestStopLossSkipsPending(t *testing.T) {
	r := &StopLoss{ratio: 0.9}
	c := exitCtx(10_000_00, 5_000, 1000, 1000)
	c.pending = []PendingOrder{{Order: Order{Instrument: 1, Side: SideSell, Qty: 1000}}}
	if got := r.OnStep(c); len(got) != 0 {
		t.Errorf("已有在途单时不该重复发，得到 %d 条", len(got))
	}
}

// TestStopLossSkipsSuspended 停牌 / 零成交的 bar 撮合不了。
func TestStopLossSkipsSuspended(t *testing.T) {
	r := &StopLoss{ratio: 0.9}
	c := exitCtx(10_000_00, 5_000, 1000, 1000)
	b := c.bars[1]
	b.TradeStatus = 0
	c.bars[1] = b
	if got := r.OnStep(c); len(got) != 0 {
		t.Errorf("不可成交的 bar 上不该发信号，得到 %d 条", len(got))
	}
}

// ---- 止盈 ----

func TestTakeProfit(t *testing.T) {
	r := &TakeProfit{ratio: 1.2} // 赚 20% 止盈
	if got := r.OnStep(exitCtx(10_000_00, 12_500, 1000, 1000)); len(got) != 1 {
		t.Errorf("赚 25%% 应触发止盈，得到 %d 条", len(got))
	}
	if got := r.OnStep(exitCtx(10_000_00, 11_000, 1000, 1000)); len(got) != 0 {
		t.Errorf("赚 10%% 不该触发，得到 %d 条", len(got))
	}
}

// ---- 移动止损 ----

// TestTrailingStopTracksPeak 峰值要跨步累积，回落到阈值才触发。
func TestTrailingStopTracksPeak(t *testing.T) {
	r := &TrailingStop{drop: 0.9, arm: 1, peak: map[mktdata.InstrumentID]float64{}}
	// 涨到 1.5 倍
	if got := r.OnStep(exitCtx(10_000_00, 15_000, 1000, 1000)); len(got) != 0 {
		t.Fatalf("上涨途中不该触发，得到 %d 条", len(got))
	}
	// 回落到 1.4 倍：自峰值回落 6.7%，未到 10%
	if got := r.OnStep(exitCtx(10_000_00, 14_000, 1000, 1000)); len(got) != 0 {
		t.Errorf("回落 6.7%% 不该触发，得到 %d 条", len(got))
	}
	// 回落到 1.3 倍：自峰值 1.5 回落 13.3% > 10%
	if got := r.OnStep(exitCtx(10_000_00, 13_000, 1000, 1000)); len(got) != 1 {
		t.Errorf("自峰值回落 13.3%% 应触发，得到 %d 条", len(got))
	}
}

// TestTrailingStopArm 未达启用阈值前不触发。
//
// 没有它，刚建仓就被一点回撤扫出去，移动止损成了「买入即卖出」。
func TestTrailingStopArm(t *testing.T) {
	r := &TrailingStop{drop: 0.9, arm: 1.2, peak: map[mktdata.InstrumentID]float64{}}
	r.OnStep(exitCtx(10_000_00, 10_500, 1000, 1000)) // 峰值 1.05
	if got := r.OnStep(exitCtx(10_000_00, 9_000, 1000, 1000)); len(got) != 0 {
		t.Errorf("盈利未达 20%% 时不该启用，得到 %d 条", len(got))
	}
}

// TestTrailingStopSnapshotRoundTrip 峰值必须能往返。
//
// 不存的话，从快照恢复后峰值归零，移动止损**静默失效** ——
// 不报错、不异常，只是再也不会触发。C6 的实盘每天都从快照恢复。
func TestTrailingStopSnapshotRoundTrip(t *testing.T) {
	a := &TrailingStop{drop: 0.9, arm: 1, peak: map[mktdata.InstrumentID]float64{}}
	a.OnStep(exitCtx(10_000_00, 15_000, 1000, 1000)) // 峰值 1.5

	b := &TrailingStop{drop: 0.9, arm: 1, peak: map[mktdata.InstrumentID]float64{}}
	snap, err := a.SnapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreState(snap); err != nil {
		t.Fatal(err)
	}
	// 恢复后直接喂 1.3 倍：若峰值丢了，1.3 会成为新峰值而不触发
	if got := b.OnStep(exitCtx(10_000_00, 13_000, 1000, 1000)); len(got) != 1 {
		t.Errorf("恢复后应保留峰值 1.5 并触发，得到 %d 条", len(got))
	}
}

// TestTrailingStopForgetsClosedPositions 清仓后峰值要清掉。
//
// 不清的话，重新买回同一只标的会沿用上一轮的峰值 ——
// 一买进来就「自峰值回落 40%」，当天被扫出去。
func TestTrailingStopForgetsClosedPositions(t *testing.T) {
	r := &TrailingStop{drop: 0.9, arm: 1, peak: map[mktdata.InstrumentID]float64{}}
	r.OnStep(exitCtx(10_000_00, 15_000, 1000, 1000))
	if r.peak[1] == 0 {
		t.Fatal("峰值没记上")
	}
	// 空仓的上下文
	empty := exitCtx(0, 15_000, 0, 0)
	r.OnStep(empty)
	if _, ok := r.peak[1]; ok {
		t.Error("已清仓的标的峰值应被清掉")
	}
}

// ---- 链 ----

// TestExitChainDedupes 同一标的只发一次，靠前的规则赢。
//
// 重复的离场信号会让 Sizer 算出两张卖单，第二张必然因无券可卖被拒。
func TestExitChainDedupes(t *testing.T) {
	c := ExitChain{&StopLoss{ratio: 0.9}, &TakeProfit{ratio: 0.5}}
	// 造一个同时满足两条的情形：亏 50%（止损触发）且 ratio 0.5 ≥ 0.5（止盈也触发）
	got := c.OnStep(exitCtx(10_000_00, 5_000, 1000, 1000))
	if len(got) != 1 {
		t.Fatalf("同一标的应只发一次，得到 %d 条", len(got))
	}
	if got[0].Tag != "stop_loss" {
		t.Errorf("应由靠前的规则赢，得到 %q", got[0].Tag)
	}
}

func TestExitChainEmpty(t *testing.T) {
	if got := (ExitChain{}).OnStep(exitCtx(10_000_00, 5_000, 1000, 1000)); got != nil {
		t.Errorf("空链不该发信号，得到 %v", got)
	}
}

// TestExitChainSnapshotRejectsWrongLength 快照条数对不上要报错。
func TestExitChainSnapshotRejectsWrongLength(t *testing.T) {
	a := ExitChain{&StopLoss{ratio: 0.9}}
	snap, err := a.SnapshotState()
	if err != nil {
		t.Fatal(err)
	}
	b := ExitChain{&StopLoss{ratio: 0.9}, &TakeProfit{ratio: 1.2}}
	if err := b.RestoreState(snap); err == nil {
		t.Error("链长度不一致时应报错 —— 该快照多半来自另一份配置")
	}
}
