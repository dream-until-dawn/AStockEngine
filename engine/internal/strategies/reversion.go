package strategies

import (
	"encoding/json"
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// RSIReversion 均值回归：超卖买入、超买卖出。
//
// 加这个策略的理由不是「再多一个策略」，而是**维度**。
// `ma_cross` 与 `macd_cross` 本质是同一个想法（两条 EMA 的关系）的两种
// 写法：都在赌趋势延续。只扫这两个，海选能得出的结论止于
// 「均线类在这个市场不行」—— 而那不等于「技术指标不行」。
//
// RSI 回归赌的是**反向**：价格偏离越远，回归的概率越大。
// 它与均线类在同一段行情上的表现常常相反，这正是它的价值 ——
// 一个能同时打败两者的结论，才是关于「技术分析」而不是关于「趋势跟随」的。
//
// 与交叉型策略的一处刻意区别：**用阈值穿越而不是水平比较**。
// 「RSI < 30 就买」会在超卖区里每天重复发信号；
// 「RSI 从上方跌破 30 才买」只在进入超卖的那一步发一次。
// 前者在日线上会把仓位打满并被手续费吃光 —— 网格已经演示过这条路
// （57,010 笔成交，佣金吃掉 28.6% 本金）。
type RSIReversion struct {
	// prevRSI 上一步的 RSI 值。**必须是跨步状态** ——
	// 「穿越」是两个时点之间的关系，ctx 只给当前时点
	prevRSI map[int32]float64
	period  int
	buyAt   float64 // 跌破此值买入
	sellAt  float64 // 升破此值卖出
}

func NewRSIReversion() *RSIReversion {
	return &RSIReversion{prevRSI: make(map[int32]float64, 8192)}
}

func (s *RSIReversion) Name() string { return "rsi_reversion" }

func (s *RSIReversion) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "period", Kind: eng.ParamInt, Default: 14, Min: 2, Max: 100, Step: 1,
			Desc: "RSI 周期"},
		{Name: "buy_at", Kind: eng.ParamFloat, Default: 30, Min: 1, Max: 50, Step: 1,
			Desc: "RSI 由上方跌破此值时买入（超卖）"},
		{Name: "sell_at", Kind: eng.ParamFloat, Default: 70, Min: 50, Max: 99, Step: 1,
			Desc: "RSI 由下方升破此值时卖出（超买）"},
	}
}

func (s *RSIReversion) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.period = p.Int("period", 14)
	s.buyAt, s.sellAt = p.Float("buy_at", 30), p.Float("sell_at", 70)
	if s.buyAt >= s.sellAt {
		return fmt.Errorf("买入阈值 %.1f 必须小于卖出阈值 %.1f", s.buyAt, s.sellAt)
	}
	s.prevRSI = make(map[int32]float64, 8192)
	ic.Use("rsi", func() indicator.Indicator { return indicator.NewRSI(s.period) })
	return nil
}

func (s *RSIReversion) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	held, inFlight := holdingSet(ctx)
	var sigs []eng.Signal

	for _, id := range ctx.Universe() {
		if inFlight[id] {
			continue
		}
		ind, ok := ctx.Indicator(id, "rsi")
		if !ok || !ind.Ready() {
			continue // 预热期的值是垃圾
		}
		cur := ind.Values()[0]
		key := int32(id)
		prev, seen := s.prevRSI[key]
		s.prevRSI[key] = cur
		if !seen {
			continue // 第一次见到，没有「上一步」可比，谈不上穿越
		}

		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		switch {
		case prev >= s.sellAt && cur < s.sellAt && held[id]:
			// 从超买区回落 —— 离场。**不在「升破 70」时卖**：
			// 那是趋势最强的时候，卖在那里等于把上涨段切掉
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalExit, Side: trading.SideSell,
				Tag: "rsi_overbought_exit",
			})
		case prev > s.buyAt && cur <= s.buyAt && !held[id]:
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: trading.SideBuy,
				Tag: "rsi_oversold_enter",
			})
		}
	}
	return sigs, nil
}

func (s *RSIReversion) SnapshotState() ([]byte, error) {
	return marshalFloatState(s.prevRSI)
}

func (s *RSIReversion) RestoreState(b []byte) error {
	m, err := unmarshalFloatState(b)
	if err != nil {
		return err
	}
	s.prevRSI = m
	return nil
}

var (
	_ eng.Strategy         = (*RSIReversion)(nil)
	_ eng.StatefulStrategy = (*RSIReversion)(nil)
)

// ---- 跨步状态的序列化 ----
//
// 与 crossState 的 map[ID]bool 同理，只是值是 float64（阈值穿越要比
// 上一步的数值，不是布尔）。放在这里而不是 crossover.go：
// 那个文件属于交叉型策略，均值回归借用它的存储会让两处耦合。

func marshalFloatState(m map[int32]float64) ([]byte, error) {
	out := make(map[string]float64, len(m))
	for id, v := range m {
		out[fmt.Sprint(id)] = v
	}
	return json.Marshal(out)
}

func unmarshalFloatState(b []byte) (map[int32]float64, error) {
	var in map[string]float64
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	out := make(map[int32]float64, len(in))
	for k, v := range in {
		var id int32
		if _, err := fmt.Sscan(k, &id); err != nil {
			return nil, fmt.Errorf("快照中的标的 ID %q 无法解析: %w", k, err)
		}
		out[id] = v
	}
	return out, nil
}
