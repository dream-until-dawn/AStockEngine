package trading

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// 回撤熔断有三种形态，参数决定用哪一种：
//
//	cooldown=0, flatten=off  纯拦截，回上去自动恢复（历史行为，默认）
//	cooldown=N, flatten=off  触发后固定停 N 根，期间回上来也不放行
//	cooldown=N, flatten=on   触发时先清仓，再停 N 根
//
// 三种都要测，因为它们的差别正是「什么时候恢复」——
// 而恢复早了不会报错，只会在回撤中继里再买一次。

// ddCtx 造一个指定峰值与当前权益的上下文。
func ddCtx(peak, equity int64) *fakeSizeCtx {
	id := mktdata.InstrumentID(1)
	return &fakeSizeCtx{
		pf:      NewPortfolio(peak),
		equity:  equity,
		initial: peak,
		peak:    peak,
		mkt:     NewAShareMarket(),
		bars: map[mktdata.InstrumentID]mktdata.Bar{
			id: {TradingDay: 20250102, Close: 10_000, PreClose: 10_000,
				Amount: 100_000_000_000, TradeStatus: 1},
		},
		insts: map[mktdata.InstrumentID]*mktdata.Instrument{
			id: {ID: id, Market: mktdata.MarketAShare, Symbol: "600000",
				PriceScale: 1000, QtyScale: 1, MinOrderQty: 100, QtyStep: 100},
		},
	}
}

func passes(t *testing.T, r *DrawdownHalt, ctx *fakeSizeCtx) bool {
	t.Helper()
	_, _, ok := r.Check(buyOrder(100), ctx)
	return ok
}

// TestDrawdownHaltDefaultIsPureBlock 默认形态：只拦截，回上去就恢复。
//
// 这是历史行为，**必须一个字不变** —— 既有配置都在用它。
func TestDrawdownHaltDefaultIsPureBlock(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000} // 30%

	if !passes(t, r, ddCtx(1000, 800)) { // 回撤 20%
		t.Error("回撤 20% 未达 30% 线，应放行")
	}
	if passes(t, r, ddCtx(1000, 650)) { // 回撤 35%
		t.Error("回撤 35% 已过线，应拦下")
	}
	// 回上去立刻恢复
	if !passes(t, r, ddCtx(1000, 900)) {
		t.Error("回撤收窄后应立刻恢复")
	}
}

// TestDrawdownHaltNeverBlocksExits 平仓单永远放行。
//
// 熔断是「别再进场」，不是「锁死账户」。拦住平仓会让止损也失效，
// 那比不熔断更危险。
func TestDrawdownHaltNeverBlocksExits(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000}
	ctx := ddCtx(1000, 500) // 回撤 50%，肯定触发

	sell := Order{Instrument: 1, Side: SideSell, Qty: 100, Reduce: true}
	if _, _, ok := r.Check(sell, ctx); !ok {
		t.Error("平仓单不该被熔断拦下")
	}
	// A 股里没标 Reduce 的卖出也是平多
	if _, _, ok := r.Check(Order{Instrument: 1, Side: SideSell, Qty: 100}, ctx); !ok {
		t.Error("单向市场的卖出就是平仓，不该被拦")
	}
}

// TestDrawdownHaltCooldownHoldsThroughRecovery 冷静期内即使权益回来也不放行。
//
// **这正是冷静期与「等回撤收窄」的区别**：不设冷静期时，
// 权益反弹一点越过阈值就立刻放行，而那一点反弹常常是下跌中继。
func TestDrawdownHaltCooldownHoldsThroughRecovery(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000, cooldown: 3}

	// 第 1 步：触发
	r.OnStep(ddCtx(1000, 600))
	if tripped, remain := r.Tripped(); !tripped || remain != 3 {
		t.Fatalf("触发后应进入 3 根冷静期，得到 tripped=%v remain=%d", tripped, remain)
	}
	if passes(t, r, ddCtx(1000, 600)) {
		t.Error("熔断中应拦下开仓")
	}

	// 第 2、3 步：权益已经完全恢复，但冷静期没走完
	for i := 2; i <= 3; i++ {
		full := ddCtx(1000, 1000) // 回撤 0%
		r.OnStep(full)
		if passes(t, r, full) {
			t.Fatalf("第 %d 步：冷静期未走完，即便权益已恢复也不该放行", i)
		}
	}

	// 第 4 步：冷静期走完
	full := ddCtx(1000, 1000)
	r.OnStep(full)
	if tripped, _ := r.Tripped(); tripped {
		t.Error("冷静期走完后应解除")
	}
	if !passes(t, r, full) {
		t.Error("冷静期走完后应放行")
	}
}

// TestDrawdownHaltCooldownCountsBarsNotOrders 冷静期按 bar 走，不按下单次数走。
//
// 倒计时若挂在 Check 上，没有订单的那些步就不计数，
// 「停 5 根」会变成「停 5 次下单尝试」—— 而后者可能横跨几个月。
func TestDrawdownHaltCooldownCountsBarsNotOrders(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000, cooldown: 2}
	r.OnStep(ddCtx(1000, 600)) // 触发

	// 一整步都不下单，只走 OnStep
	r.OnStep(ddCtx(1000, 1000))
	if tripped, remain := r.Tripped(); !tripped || remain != 1 {
		t.Fatalf("没有订单的那一步也要计数，得到 tripped=%v remain=%d", tripped, remain)
	}
	r.OnStep(ddCtx(1000, 1000))
	if tripped, _ := r.Tripped(); tripped {
		t.Error("两根走完应解除")
	}
}

// TestDrawdownHaltFlattenOffKeepsPositions 不勾选清仓时，持仓不动。
//
// **默认关闭是刻意的**：熔断一旦自己发卖单，就等于在策略之外
// 新增了一条交易逻辑，亏损是策略的还是熔断的就分不出来了。
func TestDrawdownHaltFlattenOffKeepsPositions(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000, cooldown: 5}
	ctx := ddCtx(1000, 500)
	ctx.pf.positions[1] = &Position{Total: 1000, CostCents: 500}

	if sigs := r.OnStep(ctx); len(sigs) != 0 {
		t.Fatalf("未勾选清仓时不该发离场信号，得到 %+v", sigs)
	}
}

// TestDrawdownHaltFlattenEmitsExitsOnce 勾选清仓：只在触发那一步发，不是每步都发。
func TestDrawdownHaltFlattenEmitsExitsOnce(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000, cooldown: 5, flatten: true}
	ctx := ddCtx(1000, 500)
	ctx.pf.positions[1] = &Position{Total: 1000, CostCents: 500}

	sigs := r.OnStep(ctx)
	if len(sigs) != 1 {
		t.Fatalf("触发那一步应发 1 条离场信号，得到 %d 条", len(sigs))
	}
	if sigs[0].Kind != SignalExit || sigs[0].Side != SideSell {
		t.Errorf("单向市场的离场应是卖出平多，得到 %+v", sigs[0])
	}
	if sigs[0].Tag != "drawdown_halt" {
		t.Errorf("离场信号要带规则名以便归因，得到 tag=%q", sigs[0].Tag)
	}

	// 冷静期内的后续步不该反复发 —— 仓位已经在平的路上了
	if again := r.OnStep(ctx); len(again) != 0 {
		t.Fatalf("冷静期内不该重复发离场信号，得到 %+v", again)
	}
}

// TestDrawdownHaltFlattenBothLegsInHedge 双向市场下多空两条腿都要平。
//
// 只发卖出的话，空头仓位会原封不动地留着 —— 而熔断的意思是「清空」。
func TestDrawdownHaltFlattenBothLegsInHedge(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000, cooldown: 5, flatten: true}
	ctx := ddCtx(1000, 500)
	ctx.mkt = NewCryptoMarket()

	led := newTestLedger(100_000, 2)
	mustApply(t, led, mkFill(SideBuy, false, 100, 100, 0))  // 开多
	mustApply(t, led, mkFill(SideSell, false, 100, 100, 0)) // 开空
	ctx.led = led

	sigs := r.OnStep(ctx)
	if len(sigs) != 2 {
		t.Fatalf("多空各一条腿，应发 2 条离场信号，得到 %d 条：%+v", len(sigs), sigs)
	}
	var sawSell, sawBuy bool
	for _, s := range sigs {
		if s.Kind != SignalExit {
			t.Errorf("应是离场信号，得到 %+v", s)
		}
		if s.Side == SideSell {
			sawSell = true // 平多
		} else {
			sawBuy = true // 平空
		}
	}
	if !sawSell || !sawBuy {
		t.Errorf("平多要卖、平空要买，两者都要有：卖=%v 买=%v", sawSell, sawBuy)
	}
}

// TestDrawdownHaltSnapshotRoundTrip 冷静期必须进快照。
//
// 不存的话，从快照恢复的那一步冷静期归零，**熔断静默解除** ——
// 不报错、不异常，只是本该被挡住的单子放行了。
func TestDrawdownHaltSnapshotRoundTrip(t *testing.T) {
	r := &DrawdownHalt{ppm: 300_000, cooldown: 4, flatten: true}
	ctx := ddCtx(1000, 500)
	ctx.pf.positions[1] = &Position{Total: 1000, CostCents: 500}
	r.OnStep(ctx) // 触发并清仓
	r.OnStep(ctx) // 走一格

	b, err := r.SnapshotState()
	if err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	got := &DrawdownHalt{ppm: 300_000, cooldown: 4, flatten: true}
	if err := got.RestoreState(b); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}

	wt, wr := r.Tripped()
	gt, gr := got.Tripped()
	if gt != wt || gr != wr {
		t.Fatalf("冷静期没还原：tripped=%v remain=%d，想要 %v/%d", gt, gr, wt, wr)
	}
	// 「已经清过仓」也要还原 —— 否则恢复后会再清一次
	if sigs := got.OnStep(ctx); len(sigs) != 0 {
		t.Fatalf("恢复后不该重复清仓，得到 %+v", sigs)
	}
}

// TestDrawdownHaltChainSnapshot 风控链的快照/恢复。
func TestDrawdownHaltChainSnapshot(t *testing.T) {
	mk := func() RiskChain {
		return RiskChain{
			&MaxPositions{n: 5}, // 无状态
			&DrawdownHalt{ppm: 300_000, cooldown: 3},
		}
	}
	src := mk()
	src[1].(*DrawdownHalt).OnStep(ddCtx(1000, 500))

	b, err := src.SnapshotState()
	if err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	dst := mk()
	if err := dst.RestoreState(b); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}
	tripped, remain := dst[1].(*DrawdownHalt).Tripped()
	if !tripped || remain != 3 {
		t.Fatalf("链恢复后 tripped=%v remain=%d，想要 true/3", tripped, remain)
	}
}

// TestDrawdownHaltChainSnapshotRejectsWrongLength 成员数对不上要报错，不能静默跳过。
func TestDrawdownHaltChainSnapshotRejectsWrongLength(t *testing.T) {
	src := RiskChain{&DrawdownHalt{ppm: 300_000}}
	b, _ := src.SnapshotState()

	dst := RiskChain{&DrawdownHalt{ppm: 300_000}, &MaxPositions{n: 5}}
	if err := dst.RestoreState(b); err == nil {
		t.Error("成员数不同的快照应当被拒绝 —— 它多半来自另一份配置")
	}
}

// ---- min_capital ----
//
// 破产的账户在报告里非常危险：净值走成一条直线，最大回撤定格在
// 失败那天，年化波动被后面几年的零波动摊薄，**夏普反而变好看**。
// 不说出来的话，一个已经死掉的策略会显示成一个低波动策略。

// TestMinCapitalFloorNeedsFlat 有持仓时权益低只是回撤，不是失败。
func TestMinCapitalFloorNeedsFlat(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: true} // 下限 1000.00
	ctx := ddCtx(1_000_000, 50_000)                    // 权益 500.00，远低于下限
	ctx.pf.positions[1] = &Position{Total: 1000, CostCents: 500}

	if _, _, ok := r.Check(buyOrder(100), ctx); !ok {
		t.Error("还有持仓时不该判失败 —— 那是回撤，不是失败")
	}
	if failed, _, _ := r.Failed(); failed {
		t.Error("有持仓却被判成失败")
	}
}

// TestMinCapitalFloorTripsWhenFlat 无持仓 + 权益低于下限 = 失败。
func TestMinCapitalFloorTripsWhenFlat(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: true}
	ctx := ddCtx(1_000_000, 50_000)

	if _, rej, ok := r.Check(buyOrder(100), ctx); ok {
		t.Fatal("无持仓且权益低于下限，应判失败并拦单")
	} else if rej.Rule != "min_capital" {
		t.Errorf("拒单应标明规则名，得到 %q", rej.Rule)
	}
	failed, day, why := r.Failed()
	if !failed || day != 20250102 || why == "" {
		t.Errorf("失败状态没记全：failed=%v day=%d why=%q", failed, day, why)
	}
}

// TestMinCapitalFloorDetectedWithoutOrders 破产之后根本下不出单，
// 所以判定不能只挂在 Check 上。
//
// **这是这条规则真正要解决的问题**：Sizer 算出的数量为 0 时不产生订单，
// Check 永远不会被调用 —— 而「再也下不出单」恰恰是要检测的那件事。
func TestMinCapitalFloorDetectedWithoutOrders(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: true}
	ctx := ddCtx(1_000_000, 50_000)

	r.OnStep(ctx) // 一张订单都没有，只走每步钩子
	if failed, _, _ := r.Failed(); !failed {
		t.Fatal("没有订单的那一步也要能判出失败")
	}
}

// TestMinCapitalAutoDetectsUnaffordableLot floor=0 时按「开不起最小一手」自动判。
func TestMinCapitalAutoDetectsUnaffordableLot(t *testing.T) {
	r := &MinCapital{block: true} // floor=0 → 自动
	// 一手 = 100 股 × 10.00 元 = 1000.00 元，权益只有 500.00
	ctx := ddCtx(1_000_000, 50_000)
	ctx.pf = NewPortfolio(50_000)

	if _, rej, ok := r.Check(buyOrder(100), ctx); ok {
		t.Fatal("买不起最小一手时应判失败")
	} else if rej.Detail == "" {
		t.Error("拒单要说清楚为什么")
	}
	_, _, why := r.Failed()
	if why == "" {
		t.Error("自动判定也要记下依据")
	}
}

// TestMinCapitalAutoPassesWhenAffordable 买得起就不算失败。
func TestMinCapitalAutoPassesWhenAffordable(t *testing.T) {
	r := &MinCapital{block: true}
	ctx := ddCtx(1_000_000, 1_000_000)
	ctx.pf = NewPortfolio(1_000_000) // 10000.00 元，够买一手

	if _, _, ok := r.Check(buyOrder(100), ctx); !ok {
		t.Error("买得起最小一手时应放行")
	}
	if failed, _, _ := r.Failed(); failed {
		t.Error("买得起却被判成失败")
	}
}

// TestMinCapitalNeverBlocksExits 平仓永远放行 —— 破产了也要能把手里的平掉。
func TestMinCapitalNeverBlocksExits(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: true, failed: true, reason: "测试"}
	ctx := ddCtx(1_000_000, 50_000)

	sell := Order{Instrument: 1, Side: SideSell, Qty: 100, Reduce: true}
	if _, _, ok := r.Check(sell, ctx); !ok {
		t.Error("已判失败也不该挡住平仓")
	}
}

// TestMinCapitalBlockOffOnlyRecords 关掉 block 时只记录不拦截。
func TestMinCapitalBlockOffOnlyRecords(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: false}
	ctx := ddCtx(1_000_000, 50_000)

	if _, _, ok := r.Check(buyOrder(100), ctx); !ok {
		t.Error("block=false 时不该拦单")
	}
	if failed, _, _ := r.Failed(); !failed {
		t.Error("不拦截也要记下已失败 —— 报告要能说出来")
	}
}

// TestMinCapitalFailureIsSticky 判定失败之后不撤销。
//
// 无持仓、无成交的账户不会自己把钱变回来；若允许撤销，
// 权益的浮动会让「失败」反复横跳。
func TestMinCapitalFailureIsSticky(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: true}
	r.Check(buyOrder(100), ddCtx(1_000_000, 50_000)) // 判失败

	rich := ddCtx(1_000_000, 1_000_000)
	rich.pf = NewPortfolio(1_000_000)
	if _, _, ok := r.Check(buyOrder(100), rich); ok {
		t.Error("已判失败就不该再因为权益变化而恢复")
	}
}

// TestMinCapitalSnapshotRoundTrip 失败状态必须进快照。
func TestMinCapitalSnapshotRoundTrip(t *testing.T) {
	r := &MinCapital{floorCents: 100_000, block: true}
	r.Check(buyOrder(100), ddCtx(1_000_000, 50_000))

	b, err := r.SnapshotState()
	if err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	got := &MinCapital{floorCents: 100_000, block: true}
	if err := got.RestoreState(b); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}
	wf, wd, ww := r.Failed()
	gf, gd, gw := got.Failed()
	if gf != wf || gd != wd || gw != ww {
		t.Fatalf("失败状态没还原：%v/%d/%q，想要 %v/%d/%q", gf, gd, gw, wf, wd, ww)
	}
}

// TestDrawdownHaltRebasesAfterCooldown 冷静期结束后从当前权益重新起算回撤。
//
// **不重算的话冷静期是个陷阱**：勾了清仓之后账户全是现金、权益不再变动，
// 而历史峰值停在熔断之前 —— 回撤永远回不到阈值以内，冷静期一到就
// 立刻再次触发，从此再也不交易。
//
// 实测过一次：2021 年触发，之后 5 年 0 成交、285 次拒单，
// 而报告上只是一条平直的净值线，看不出策略已经死了。
func TestDrawdownHaltRebasesAfterCooldown(t *testing.T) {
	r := &DrawdownHalt{ppm: 150_000, cooldown: 2, flatten: true} // 15%

	// 峰值 1000，跌到 800（回撤 20%）→ 触发
	r.OnStep(ddCtx(1000, 800))
	if tripped, _ := r.Tripped(); !tripped {
		t.Fatal("回撤 20% 应触发")
	}
	// 清仓之后权益就不动了 —— 这正是陷阱的前提
	r.OnStep(ddCtx(1000, 800))
	r.OnStep(ddCtx(1000, 800)) // 冷静期走完

	if tripped, _ := r.Tripped(); tripped {
		t.Fatal("冷静期走完应解除")
	}
	// 关键：权益仍是 800、引擎峰值仍是 1000（回撤仍有 20%），
	// 但参考峰值已经重设到 800，所以不该再次触发
	if !passes(t, r, ddCtx(1000, 800)) {
		t.Fatal("重新起算后不该立刻再次熔断 —— 否则策略永远醒不过来")
	}
	r.OnStep(ddCtx(1000, 800))
	if tripped, _ := r.Tripped(); tripped {
		t.Fatal("重新起算后又被自己触发了")
	}

	// 从新基准再跌 15% 才该触发：800 → 680
	r.OnStep(ddCtx(1000, 680))
	if tripped, _ := r.Tripped(); !tripped {
		t.Fatal("相对新基准跌 15% 应当触发")
	}
}

// TestDrawdownHaltRebasedPeakFollowsEquityUp 重新起算后参考峰值跟着权益涨。
//
// 不跟涨的话它就成了一个死值：账户翻倍之后，回撤仍相对那个旧数字算，
// 越走越失真 —— 表现为「明明在赚钱却触发熔断」或反过来「跌了很多也不触发」。
func TestDrawdownHaltRebasedPeakFollowsEquityUp(t *testing.T) {
	r := &DrawdownHalt{ppm: 150_000, cooldown: 1}
	r.OnStep(ddCtx(1000, 800)) // 触发
	r.OnStep(ddCtx(1000, 800)) // 冷静期走完，基准重设为 800

	// 涨到 2000，基准应跟到 2000
	r.OnStep(ddCtx(1000, 2000))
	if tripped, _ := r.Tripped(); tripped {
		t.Fatal("一路上涨不该触发")
	}
	// 从 2000 跌 10% 到 1800 —— 不到 15%，不该触发
	r.OnStep(ddCtx(1000, 1800))
	if tripped, _ := r.Tripped(); tripped {
		t.Fatal("相对新高只跌 10%%，不该触发")
	}
	// 跌到 1600（相对 2000 是 20%）—— 该触发
	r.OnStep(ddCtx(1000, 1600))
	if tripped, _ := r.Tripped(); !tripped {
		t.Fatal("相对新高跌 20%% 应触发")
	}
}

// TestDrawdownHaltNoCooldownUsesEnginePeak 不设冷静期时仍用引擎的历史峰值。
//
// 重新起算只属于「冷静期」这个语义 —— 没有冷静期就没有「重新开始」的时刻，
// 行为必须与从前完全一致。
func TestDrawdownHaltNoCooldownUsesEnginePeak(t *testing.T) {
	r := &DrawdownHalt{ppm: 150_000} // cooldown = 0

	r.OnStep(ddCtx(1000, 800)) // 触发
	r.OnStep(ddCtx(1000, 900)) // 回撤 10%，回到线内 → 解除
	if tripped, _ := r.Tripped(); tripped {
		t.Fatal("回到阈值内应解除")
	}
	// 峰值仍是引擎的 1000：跌回 800 应再次触发
	r.OnStep(ddCtx(1000, 800))
	if tripped, _ := r.Tripped(); !tripped {
		t.Fatal("无冷静期时应始终相对引擎峰值判断")
	}
}
