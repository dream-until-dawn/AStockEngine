// Package strategies 提供样例策略。
//
// 策略用 Go 实现并编译进引擎（ROADMAP 技术选型），Web 端只能配参数不能写逻辑，
// 因此每个策略都必须用 Specs() 声明参数元数据 —— 它同时喂三处：
// Web 自动生成表单、海选自动展开参数网格、配置校验。
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
type BuyAndHold struct {
	maxHold   int
	slotCents int64
}

// NewBuyAndHold 创建基准策略。slotCents 是每只标的分配的资金（分）。
func NewBuyAndHold() *BuyAndHold { return &BuyAndHold{} }

func (s *BuyAndHold) Name() string { return "buy_and_hold" }

func (s *BuyAndHold) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "max_hold", Kind: eng.ParamInt, Default: 10, Min: 1, Max: 200, Step: 1,
			Desc: "最多同时持有多少只标的"},
		{Name: "cash_cents", Kind: eng.ParamFloat, Default: 100_000_000, Min: 0, Max: 1e15, Step: 1,
			Desc: "总资金（分），用于等权切分"},
	}
}

func (s *BuyAndHold) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.maxHold = p.Int("max_hold", 10)
	if s.maxHold < 1 {
		s.maxHold = 1
	}
	s.slotCents = int64(p.Float("cash_cents", 100_000_000)) / int64(s.maxHold)
	return nil
}

func (s *BuyAndHold) OnBar(ctx eng.StepContext) ([]trading.Order, error) {
	pf := ctx.Portfolio()
	held, inFlight := holdingSet(ctx)
	if len(held)+len(inFlight) >= s.maxHold {
		return nil, nil // 已满仓，此后不再动作
	}

	var orders []trading.Order
	for _, id := range ctx.Universe() {
		if len(held)+len(inFlight)+len(orders) >= s.maxHold {
			break
		}
		if held[id] || inFlight[id] {
			continue
		}
		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		qty := s.slotCents * 10 / bar.Close
		if qty < 100 {
			continue
		}
		if pos := pf.Position(id); pos != nil && pos.Total > 0 {
			continue
		}
		orders = append(orders, trading.Order{
			Instrument: id, Side: trading.SideBuy, Qty: qty, Tag: "buy_hold",
		})
	}
	return orders, nil
}

// holdingSet 从 StepContext 导出「已持有」与「在途」两个集合。
//
// 刻意不让策略自己维护这两份状态：它们归引擎管，
// 策略再存一份必然在快照恢复时不一致 —— 这是端到端回测暴露过的真实缺陷。
func holdingSet(ctx eng.StepContext) (held, inFlight map[mktdata.InstrumentID]bool) {
	held = make(map[mktdata.InstrumentID]bool, 32)
	for id, p := range ctx.Portfolio().Positions {
		if p.Total > 0 {
			held[id] = true
		}
	}
	pending := ctx.Pending()
	inFlight = make(map[mktdata.InstrumentID]bool, len(pending))
	for _, po := range pending {
		inFlight[po.Instrument] = true
	}
	return held, inFlight
}
