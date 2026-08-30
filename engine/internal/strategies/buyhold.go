// Package strategies 提供样例策略。
//
// 策略用 Go 实现并编译进引擎（ROADMAP 技术选型），Web 端只能配参数不能写逻辑，
// 因此每个策略都必须用 Specs() 声明参数元数据 —— 它同时喂三处：
// Web 自动生成表单、海选自动展开参数网格、配置校验。
//
// v0.3 起策略**只出信号不出数量**。原来写在这里的
// `qty := slotCents * 10 / bar.Close` 已经搬到 Sizer，
// `cash_cents` / `max_hold` 两个参数也随之搬到 `equal_weight{slots}`。
// 策略只剩自己的信号参数 —— 这本身就说明分层对了。
package strategies

import (
	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// BuyAndHold 等权买入并长期持有，作为其他策略的基准。
//
// **它是无状态的** —— 没有实现 StatefulStrategy，因为它需要的一切
// 都能从 StepContext 导出：持仓看账本、在途看待撮合队列。
// 这正是策略状态的首选形态：从 ctx 导出的状态天然随引擎快照恢复。
//
// v0.3 之后它连参数都没有了：买多少、买几只全归 Sizer。
type BuyAndHold struct{}

// NewBuyAndHold 创建基准策略。
func NewBuyAndHold() *BuyAndHold { return &BuyAndHold{} }

func (s *BuyAndHold) Name() string { return "buy_and_hold" }

// Specs 无参数。仓位相关的参数在 Sizer 上（如 equal_weight.slots）。
func (s *BuyAndHold) Specs() []eng.ParamSpec { return nil }

func (s *BuyAndHold) Init(eng.InitContext) error { return nil }

func (s *BuyAndHold) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	held, inFlight := holdingSet(ctx)

	var sigs []eng.Signal
	for _, id := range ctx.Universe() {
		if held[id] || inFlight[id] {
			continue
		}
		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		sigs = append(sigs, eng.Signal{
			Instrument: id, Kind: eng.SignalEnter, Side: trading.SideBuy, Tag: "buy_hold",
		})
	}
	return sigs, nil
}

// holdingSet 从 StepContext 导出「已持有」与「在途」两个集合。
//
// 刻意不让策略自己维护这两份状态：它们归引擎管，
// 策略再存一份必然在快照恢复时不一致 —— 这是端到端回测暴露过的真实缺陷。
func holdingSet(ctx eng.StepContext) (held, inFlight map[mktdata.InstrumentID]bool) {
	held = make(map[mktdata.InstrumentID]bool, 32)
	ctx.Ledger().EachExposure(func(id mktdata.InstrumentID, _ trading.Exposure) bool {
		held[id] = true
		return true
	})
	pending := ctx.Pending()
	inFlight = make(map[mktdata.InstrumentID]bool, len(pending))
	for _, po := range pending {
		inFlight[po.Instrument] = true
	}
	return held, inFlight
}
