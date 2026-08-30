package strategies

import (
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// DonchianBreakout 唐奇安通道突破：创 N 日新高买入、跌破 M 日新低卖出。
//
// 这是策略池里的第三个维度。均线类看「价格与均值的关系」，
// RSI 看「涨跌动能的失衡」，突破看**价格是否走出了近期的区间** ——
// 三者对「趋势」的定义不同，才谈得上正交，海选的结论才可推广。
//
// 出场通道**独立于入场通道**（经典海龟是 20 进 10 出）。用同一个通道
// 进出会让策略在区间震荡里反复被打脸：刚突破上轨买入，第二天价格回到
// 通道内就跌破下轨卖出。出场窗口更短意味着「跌得更快才认输」。
//
// **本策略无跨步状态。** 通道是否被突破完全由当前 bar 与指标值决定，
// 不需要记住上一步 —— 与交叉型策略的根本区别。指标自己有状态，
// 而指标的状态由引擎的快照统一负责。
type DonchianBreakout struct {
	entry, exit int
}

func NewDonchianBreakout() *DonchianBreakout { return &DonchianBreakout{} }

func (s *DonchianBreakout) Name() string { return "donchian_breakout" }

func (s *DonchianBreakout) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "entry", Kind: eng.ParamInt, Default: 20, Min: 2, Max: 250, Step: 1,
			Desc: "入场通道周期：创此周期新高时买入"},
		{Name: "exit", Kind: eng.ParamInt, Default: 10, Min: 2, Max: 250, Step: 1,
			Desc: "出场通道周期：跌破此周期新低时卖出"},
	}
}

func (s *DonchianBreakout) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.entry, s.exit = p.Int("entry", 20), p.Int("exit", 10)
	if s.exit > s.entry {
		return fmt.Errorf("出场周期 %d 不应长于入场周期 %d —— "+
			"那样会在区间震荡里反复进出", s.exit, s.entry)
	}
	ic.Use("dc_entry", func() indicator.Indicator {
		return indicator.NewDonchian(s.entry, indicator.DefaultPriceScale)
	})
	ic.Use("dc_exit", func() indicator.Indicator {
		return indicator.NewDonchian(s.exit, indicator.DefaultPriceScale)
	})
	return nil
}

func (s *DonchianBreakout) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	held, inFlight := holdingSet(ctx)
	var sigs []eng.Signal

	for _, id := range ctx.Universe() {
		if inFlight[id] {
			continue
		}
		ei, ok1 := ctx.Indicator(id, "dc_entry")
		xi, ok2 := ctx.Indicator(id, "dc_exit")
		if !ok1 || !ok2 || !ei.Ready() || !xi.Ready() {
			continue
		}
		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		// 与通道比的必须是**后复权价**：通道由指标算出，而指标喂的就是
		// 后复权价（引擎负责）。拿原始收盘价去比，除权日会凭空出现一次
		// 「跌破下轨」—— 价格跳空的那一档根本不是行情，是分红。
		// 这是本策略最容易写错的一行
		pxFP, ok := ctx.AdjClose(id, 0, mktdata.AdjHFQ)
		if !ok {
			continue
		}
		px := float64(pxFP) / indicator.DefaultPriceScale

		switch {
		case held[id] && px < xi.Values()[1]: // 跌破出场通道下轨
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalExit, Side: trading.SideSell,
				Tag: "dc_breakdown",
			})
		case !held[id] && px > ei.Values()[0]: // 创入场通道新高
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: trading.SideBuy,
				Tag: "dc_breakout",
			})
		}
	}
	return sigs, nil
}

var _ eng.Strategy = (*DonchianBreakout)(nil)
