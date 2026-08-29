package strategies

import (
	"encoding/json"
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// crossState 是「上一步快线是否在慢线之上」的跨步记忆。
//
// 这是**真正**需要策略自己保存的状态 —— 它无法从 StepContext 导出，
// 因为 ctx 只给当前时点。故实现 StatefulStrategy，让它随引擎快照一并恢复。
type crossState struct {
	prevAbove map[mktdata.InstrumentID]bool
}

func newCrossState() crossState {
	return crossState{prevAbove: make(map[mktdata.InstrumentID]bool, 8192)}
}

// snapshot 只序列化取值为 true 的项 —— false 是零值，可省一半体积。
func (c *crossState) snapshot() ([]byte, error) {
	m := make(map[string]bool, len(c.prevAbove))
	for id, v := range c.prevAbove {
		if v {
			m[fmt.Sprint(int32(id))] = true
		}
	}
	return json.Marshal(m)
}

func (c *crossState) restore(b []byte) error {
	var m map[string]bool
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	c.prevAbove = make(map[mktdata.InstrumentID]bool, len(m))
	for k, v := range m {
		var id int32
		if _, err := fmt.Sscan(k, &id); err != nil {
			return fmt.Errorf("快照中的标的 ID %q 无法解析: %w", k, err)
		}
		c.prevAbove[mktdata.InstrumentID(id)] = v
	}
	return nil
}

// cross 判定金叉与死叉，并更新记忆。
func (c *crossState) cross(id mktdata.InstrumentID, above bool) (golden, death bool) {
	prev := c.prevAbove[id]
	c.prevAbove[id] = above
	return above && !prev, !above && prev
}

// emit 是两个交叉型策略共用的信号生成：金叉建仓、死叉清仓。
//
// above 由各策略自己判定（均线比大小 / DIF 比 DEA），其余完全一致。
func (c *crossState) emit(ctx eng.StepContext, key, tagPrefix string,
	above func(indicator.Indicator) bool) []eng.Signal {

	held, inFlight := holdingSet(ctx)
	var sigs []eng.Signal

	for _, id := range ctx.Universe() {
		if inFlight[id] {
			continue
		}
		ind, ok := ctx.Indicator(id, key)
		if !ok || !ind.Ready() {
			// **预热期内的指标值是垃圾**，据此下单会让回测前 N 步产生虚假交易
			continue
		}
		golden, death := c.cross(id, above(ind))

		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		switch {
		case death && held[id]:
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalExit, Side: trading.SideSell,
				Tag: tagPrefix + "_death",
			})
		case golden && !held[id]:
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: trading.SideBuy,
				Tag: tagPrefix + "_golden",
			})
		}
	}
	return sigs
}

// ---- 双均线 ----

// MACross 双均线：快线上穿慢线买入、下穿卖出。
type MACross struct {
	crossState
	fast, slow int
}

func NewMACross() *MACross { return &MACross{crossState: newCrossState()} }

func (s *MACross) Name() string { return "ma_cross" }

func (s *MACross) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "fast", Kind: eng.ParamInt, Default: 5, Min: 2, Max: 60, Step: 1, Desc: "快线周期"},
		{Name: "slow", Kind: eng.ParamInt, Default: 20, Min: 5, Max: 250, Step: 1, Desc: "慢线周期"},
	}
}

func (s *MACross) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.fast, s.slow = p.Int("fast", 5), p.Int("slow", 20)
	if s.fast >= s.slow {
		return fmt.Errorf("快线周期 %d 必须小于慢线周期 %d", s.fast, s.slow)
	}
	s.crossState = newCrossState()
	// 指标喂的是后复权价（引擎负责），序列连续才不会在除权日产生假信号
	ic.Use("ma_fast", func() indicator.Indicator {
		return indicator.NewSMA(s.fast, indicator.DefaultPriceScale)
	})
	ic.Use("ma_slow", func() indicator.Indicator {
		return indicator.NewSMA(s.slow, indicator.DefaultPriceScale)
	})
	return nil
}

func (s *MACross) SnapshotState() ([]byte, error) { return s.snapshot() }
func (s *MACross) RestoreState(b []byte) error    { return s.restore(b) }

func (s *MACross) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	// 双均线要比两条线，emit 的单指标签名不够用，这里自己走一遍
	held, inFlight := holdingSet(ctx)
	var sigs []eng.Signal

	for _, id := range ctx.Universe() {
		if inFlight[id] {
			continue
		}
		fi, ok1 := ctx.Indicator(id, "ma_fast")
		si, ok2 := ctx.Indicator(id, "ma_slow")
		if !ok1 || !ok2 || !fi.Ready() || !si.Ready() {
			continue
		}
		golden, death := s.cross(id, fi.Values()[0] > si.Values()[0])

		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		switch {
		case death && held[id]:
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalExit, Side: trading.SideSell, Tag: "ma_death",
			})
		case golden && !held[id]:
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: trading.SideBuy, Tag: "ma_golden",
			})
		}
	}
	return sigs, nil
}

// ---- MACD ----

// MACDCross MACD 金叉买入、死叉卖出。
type MACDCross struct {
	crossState
	short, long, signal int
}

func NewMACDCross() *MACDCross { return &MACDCross{crossState: newCrossState()} }

func (s *MACDCross) Name() string { return "macd_cross" }

func (s *MACDCross) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "short", Kind: eng.ParamInt, Default: 12, Min: 2, Max: 60, Step: 1, Desc: "快线周期"},
		{Name: "long", Kind: eng.ParamInt, Default: 26, Min: 5, Max: 200, Step: 1, Desc: "慢线周期"},
		{Name: "signal", Kind: eng.ParamInt, Default: 9, Min: 2, Max: 60, Step: 1, Desc: "信号周期"},
	}
}

func (s *MACDCross) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.short, s.long, s.signal = p.Int("short", 12), p.Int("long", 26), p.Int("signal", 9)
	if s.short >= s.long {
		return fmt.Errorf("快线周期 %d 必须小于慢线周期 %d", s.short, s.long)
	}
	s.crossState = newCrossState()
	ic.Use("macd", func() indicator.Indicator {
		return indicator.NewMACD(s.short, s.long, s.signal, indicator.DefaultPriceScale)
	})
	return nil
}

func (s *MACDCross) SnapshotState() ([]byte, error) { return s.snapshot() }
func (s *MACDCross) RestoreState(b []byte) error    { return s.restore(b) }

func (s *MACDCross) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	return s.emit(ctx, "macd", "macd", func(ind indicator.Indicator) bool {
		m := ind.(*indicator.MACD)
		return m.DIF() > m.DEA()
	}), nil
}

// ---- 注册 ----

func init() {
	eng.RegisterStrategy("buy_and_hold", func() eng.Strategy { return NewBuyAndHold() })
	eng.RegisterStrategy("ma_cross", func() eng.Strategy { return NewMACross() })
	eng.RegisterStrategy("macd_cross", func() eng.Strategy { return NewMACDCross() })
	eng.RegisterStrategy("grid", func() eng.Strategy { return NewGrid() })
	// 下面两个不是「再多两个策略」，是给海选补维度：
	// ma_cross 与 macd_cross 都在赌趋势延续，只扫这两个得不出
	// 关于「技术分析」的结论，只能得出关于「均线」的结论
	eng.RegisterStrategy("rsi_reversion", func() eng.Strategy { return NewRSIReversion() })
	eng.RegisterStrategy("donchian_breakout", func() eng.Strategy { return NewDonchianBreakout() })
}

// Names 返回全部可用策略名，供 CLI 提示。
func Names() []string { return eng.Strategies.Names() }
