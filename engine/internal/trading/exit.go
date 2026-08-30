package trading

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// 离场规则：止损、止盈、移动止损、持有超时。
//
// # 为什么不是一条 Risk
//
// `Risk.Check(o Order, ...) (Order, ...)` 是**过滤器** —— 它只能把
// 已有的订单缩量或拒掉。而止损要**产生**一张卖单，形状根本不同。
// 硬塞进 Risk 只有两条路：要么让 Check 返回额外的订单（那它就不是
// 过滤器了，链式语义随之崩掉），要么在别处偷偷下单（那就绕过了
// Sizer 与 Risk 链）。两条都不行。
//
// # 为什么不是一个 Strategy
//
// 组合策略（Composite）确实能把它当成一个决策源接进来，但有两个问题：
//
//  1. **优先级**。止损必须压过策略的判断 —— 策略说「继续持有」时
//     止损也要能平掉。union 合并下先到先得，靠源的顺序来保证优先级
//     太脆弱；confirm / veto 更是直接把它废掉。
//  2. **性质**。止损是风险管理，不是 alpha。混进策略组合里，
//     「这个收益是策略赚的还是止损救的」就归因不清了。
//
// 所以它是**第三类决策来源**，在引擎里排在策略**之前**（第 5.5 步），
// 且它发出的离场信号会**覆盖**策略在同一标的上的信号。
//
// # 与 Ledger.Mark() 的强平是两回事
//
// 强平是市场施加的（保证金不足），离场是自己事先定好的。
// 形状相似，但一个不可抗、一个是决策 —— 分开记，归因才清楚。

// ExitContext 是离场规则可见的状态。与 SizeContext 同一组信息，
// 单列一个接口是为了让「离场规则不该下新单」这件事体现在类型上。
type ExitContext interface {
	Time() mktdata.TimePoint
	Ledger() Ledger
	EquityCents() int64
	Available(id mktdata.InstrumentID) int64
	Bar(id mktdata.InstrumentID) (mktdata.Bar, bool)
	Instrument(id mktdata.InstrumentID) *mktdata.Instrument
	Pending() []PendingOrder
}

// ExitRule 检查已有持仓，产生离场信号。
type ExitRule interface {
	Name() string
	// OnStep 返回本步要离场的标的。**返回整批而不是逐个判断**，
	// 与 Strategy.OnBar 同形 —— 组合级的离场规则（如「亏损最大的三只
	// 一起砍掉」）才写得出来
	OnStep(ctx ExitContext) []Signal
}

// StatefulExit 供**确有跨步状态**的离场规则实现（如移动止损要记住峰值）。
//
// 与 StatefulStrategy 同理：引擎的快照涵盖不了规则自己的字段，
// 不实现这个接口的话，从快照恢复后峰值归零，移动止损立刻失效。
type StatefulExit interface {
	SnapshotState() ([]byte, error)
	RestoreState([]byte) error
}

// Exits 是离场规则的注册表。
var Exits = registry.New[ExitRule]("exit")

// ExitChain 顺序执行多条离场规则。
//
// **同一标的只发一次信号**：三条规则同时触发时，靠前的那条赢 ——
// 重复的离场信号会让 Sizer 算出两张卖单，第二张必然因无券可卖被拒。
type ExitChain []ExitRule

// OnStep 汇总全链的离场信号。
func (c ExitChain) OnStep(ctx ExitContext) []Signal {
	if len(c) == 0 {
		return nil
	}
	seen := make(map[mktdata.InstrumentID]bool, 16)
	out := make([]Signal, 0, 16)
	for _, r := range c {
		for _, s := range r.OnStep(ctx) {
			if seen[s.Instrument] {
				continue
			}
			seen[s.Instrument] = true
			out = append(out, s)
		}
	}
	return out
}

// Names 返回链上各规则名，供报告与配置回显。
func (c ExitChain) Names() []string {
	out := make([]string, len(c))
	for i, r := range c {
		out[i] = r.Name()
	}
	return out
}

// SnapshotState 逐条快照。**总是实现**，即使当前链上都是无状态规则 ——
// 链的成员会在配置里换掉，让快照格式随成员而变会让恢复莫名其妙地失败。
func (c ExitChain) SnapshotState() ([]byte, error) {
	out := make([]json.RawMessage, len(c))
	for i, r := range c {
		sr, ok := r.(StatefulExit)
		if !ok {
			out[i] = json.RawMessage("null")
			continue
		}
		b, err := sr.SnapshotState()
		if err != nil {
			return nil, fmt.Errorf("离场规则 %s 快照失败: %w", r.Name(), err)
		}
		out[i] = b
	}
	return json.Marshal(out)
}

// RestoreState 逐条恢复。
func (c ExitChain) RestoreState(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var in []json.RawMessage
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("解析离场链快照失败: %w", err)
	}
	if len(in) != len(c) {
		return fmt.Errorf("快照有 %d 条离场规则，当前链有 %d 条 —— "+
			"该快照多半来自另一份配置", len(in), len(c))
	}
	for i, raw := range in {
		sr, ok := c[i].(StatefulExit)
		if !ok {
			continue
		}
		if len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("离场规则 %s 有跨步状态，但快照里是空的", c[i].Name())
		}
		if err := sr.RestoreState(raw); err != nil {
			return fmt.Errorf("离场规则 %s 恢复失败: %w", c[i].Name(), err)
		}
	}
	return nil
}

// ---- 公共工具 ----

// holding 是一个持仓在当前时点的可判定状态。
type holding struct {
	id mktdata.InstrumentID
	// ratio 未实现盈亏比 = 现市值 / 成本。1.0 表示不赚不亏
	ratio float64
	bar   mktdata.Bar
}

// eachSellable 遍历**当前真能卖**的多头持仓。
//
// 三条过滤，每条都是必要的：
//
//   - `Available == 0`：T+1 锁着。此时发信号只会得到一条「无券可卖」的
//     拒单，而且**每天都会重发**。买入次日触发止损是真实存在的情形，
//     跳过它才是对的 —— 那一天你确实卖不掉
//   - 已有在途单：重复下单，第二张必然被拒
//   - 无 bar / 停牌 / 零成交：撮合不了
//
// 成本用 `Exposure.LongCost`（**实际付出的钱**）而不是复权价推算：
// 送转会同时改变 qty 与每股成本，用钱算天然对；
// 但**分红收到的现金不会冲减 LongCost**，所以高分红标的的止损会略偏保守。
func eachSellable(ctx ExitContext, fn func(h holding)) {
	inFlight := make(map[mktdata.InstrumentID]bool, 8)
	for _, po := range ctx.Pending() {
		inFlight[po.Instrument] = true
	}
	ctx.Ledger().EachExposure(func(id mktdata.InstrumentID, e Exposure) bool {
		if e.Long <= 0 || e.LongCost <= 0 || inFlight[id] {
			return true
		}
		if ctx.Available(id) <= 0 {
			return true
		}
		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			return true
		}
		value := NotionalCents(ctx.Instrument(id), bar.Close, e.Long)
		fn(holding{id: id, ratio: float64(value) / float64(e.LongCost), bar: bar})
		return true
	})
}

func exitSignal(id mktdata.InstrumentID, tag string) Signal {
	return Signal{Instrument: id, Kind: SignalExit, Side: SideSell, Tag: tag}
}

// ---- 止损 ----

var stopLossSpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 10, Min: 0.1, Max: 99, Step: 0.5,
		Desc: "亏损超过此比例即平仓（%），按持仓成本计"},
}

// StopLoss 固定比例止损。
type StopLoss struct{ ratio float64 }

func (r *StopLoss) Name() string { return "stop_loss" }

func (r *StopLoss) OnStep(ctx ExitContext) []Signal {
	var out []Signal
	eachSellable(ctx, func(h holding) {
		if h.ratio <= r.ratio {
			out = append(out, exitSignal(h.id, "stop_loss"))
		}
	})
	return out
}

// ---- 止盈 ----

var takeProfitSpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 20, Min: 0.1, Max: 1000, Step: 1,
		Desc: "盈利超过此比例即平仓（%），按持仓成本计"},
}

// TakeProfit 固定比例止盈。
type TakeProfit struct{ ratio float64 }

func (r *TakeProfit) Name() string { return "take_profit" }

func (r *TakeProfit) OnStep(ctx ExitContext) []Signal {
	var out []Signal
	eachSellable(ctx, func(h holding) {
		if h.ratio >= r.ratio {
			out = append(out, exitSignal(h.id, "take_profit"))
		}
	})
	return out
}

// ---- 移动止损 ----

var trailingStopSpecs = []spec.ParamSpec{
	{Name: "pct", Kind: spec.ParamFloat, Default: 10, Min: 0.1, Max: 99, Step: 0.5,
		Desc: "自持仓期间最高点回落此比例即平仓（%）"},
	{Name: "arm_pct", Kind: spec.ParamFloat, Default: 0, Min: 0, Max: 1000, Step: 1,
		Desc: "盈利达到此比例后才启用（%）。0 表示一直启用"},
}

// TrailingStop 移动止损：自持仓期间的最高点回落一定比例即平仓。
//
// # 盯的是「盈亏比」而不是价格
//
// 峰值记的是 `市值 / 成本`，不是收盘价。这样对**送转与部分卖出都免疫**：
// 送转时 qty 与每股成本同时变，比值不变；部分卖出时市值与成本同比例缩，
// 比值也不变。若直接记价格，一次 10 送 10 就会让峰值凭空高一倍，
// 移动止损当天必然触发 —— 而那一天什么也没发生。
//
// 无公司行动、无部分卖出时，它与教科书定义**完全等价**：
// 比值 = 现价 / 均价，回落 x% 即现价自峰值回落 x%。
type TrailingStop struct {
	drop float64 // 1 - pct/100
	arm  float64 // 1 + arm_pct/100
	// peak 各标的持仓期间的最高盈亏比。**必须跨步保存** ——
	// 从快照恢复后峰值归零，移动止损会立刻失效且不报错
	peak map[mktdata.InstrumentID]float64
}

func (r *TrailingStop) Name() string { return "trailing_stop" }

func (r *TrailingStop) OnStep(ctx ExitContext) []Signal {
	if r.peak == nil {
		r.peak = make(map[mktdata.InstrumentID]float64, 64)
	}
	live := make(map[mktdata.InstrumentID]bool, len(r.peak))
	var out []Signal
	eachSellable(ctx, func(h holding) {
		live[h.id] = true
		p := r.peak[h.id]
		if h.ratio > p {
			p = h.ratio
			r.peak[h.id] = p
		}
		// arm 之前不触发：刚建仓就被一点回撤扫出去，
		// 移动止损就成了「买入即卖出」
		if p < r.arm {
			return
		}
		if h.ratio <= p*r.drop {
			out = append(out, exitSignal(h.id, "trailing_stop"))
		}
	})
	// 清掉已经不持有的标的，否则重新买回同一只时会用上一轮的峰值。
	// **只在这里清**：eachSellable 会跳过 T+1 锁定与停牌的持仓，
	// 若照着它清就会把还持有着的标的误删
	for id := range r.peak {
		if !live[id] && ctx.Ledger().Exposure(id).Long <= 0 {
			delete(r.peak, id)
		}
	}
	return out
}

type trailingState struct {
	Peak map[string]float64 `json:"peak"`
}

func (r *TrailingStop) SnapshotState() ([]byte, error) {
	st := trailingState{Peak: make(map[string]float64, len(r.peak))}
	for id, v := range r.peak {
		st.Peak[fmt.Sprint(int32(id))] = v
	}
	return json.Marshal(st)
}

func (r *TrailingStop) RestoreState(b []byte) error {
	var st trailingState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	r.peak = make(map[mktdata.InstrumentID]float64, len(st.Peak))
	for k, v := range st.Peak {
		var id int32
		if _, err := fmt.Sscan(k, &id); err != nil {
			return fmt.Errorf("快照中的标的 ID %q 无法解析: %w", k, err)
		}
		r.peak[mktdata.InstrumentID(id)] = v
	}
	return nil
}

// ---- 注册 ----

func init() {
	Exits.Register("stop_loss",
		"止损：亏损超过设定比例即平仓（按持仓成本计）",
		stopLossSpecs, func(raw json.RawMessage) (ExitRule, error) {
			p, err := registry.DecodeParams(stopLossSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &StopLoss{ratio: 1 - p.Float("pct", 10)/100}, nil
		})

	Exits.Register("take_profit",
		"止盈：盈利超过设定比例即平仓（按持仓成本计）",
		takeProfitSpecs, func(raw json.RawMessage) (ExitRule, error) {
			p, err := registry.DecodeParams(takeProfitSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &TakeProfit{ratio: 1 + p.Float("pct", 20)/100}, nil
		})

	Exits.Register("trailing_stop",
		"移动止损：自持仓期间最高点回落设定比例即平仓",
		trailingStopSpecs, func(raw json.RawMessage) (ExitRule, error) {
			p, err := registry.DecodeParams(trailingStopSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &TrailingStop{
				drop: 1 - p.Float("pct", 10)/100,
				arm:  1 + p.Float("arm_pct", 0)/100,
				peak: make(map[mktdata.InstrumentID]float64, 64),
			}, nil
		})
}

var (
	_ ExitRule     = (*StopLoss)(nil)
	_ ExitRule     = (*TakeProfit)(nil)
	_ ExitRule     = (*TrailingStop)(nil)
	_ StatefulExit = (*TrailingStop)(nil)
)
