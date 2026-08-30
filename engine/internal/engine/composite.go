package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 一个引擎只能装一个策略，是 v0.3 之前的硬限制。
//
// 它挡住的不是「写更复杂的策略」—— 那总能把逻辑塞进一个 OnBar 里 ——
// 而是**把不同来源的判断组合起来**：技术信号 + AI 否决、网格 + 趋势叠加、
// 多个模型投票。这些的共同形态是「N 个独立的决策源，一套合并规则」，
// 塞进一个策略里就等于把合并规则也写死了。
//
// 所以这里加的是**组合**而不是新的一层抽象。AI 接入就是实现 Strategy 的
// 一个源，或者挂在 veto 位 —— 不需要专门为它造一个 Decider 接口，
// 那只会让「Strategy 和 Decider 有什么区别」成为一个日常问题。

// CombineMode 多个决策源的合并方式。
type CombineMode int8

const (
	// CombineUnion 并集：任一源发出的信号都采纳。
	// 同标的同方向只保留第一个源的（避免同一只标的被下两次单）
	CombineUnion CombineMode = iota
	// CombineConfirm 交集：**所有**源都对同一标的发出同向信号才采纳。
	// 典型用法是多模型互相确认，代价是信号变少
	CombineConfirm
	// CombineVeto 否决：第一个源出信号，后续源发出**反向**信号即否决它。
	// AI 覆盖层最自然的形态 —— AI 不必替你选股，只需要说「这笔别做」
	CombineVeto
)

func (m CombineMode) String() string {
	switch m {
	case CombineUnion:
		return "union"
	case CombineConfirm:
		return "confirm"
	case CombineVeto:
		return "veto"
	}
	return "unknown"
}

// ParseCombineMode 解析合并方式名。
func ParseCombineMode(s string) (CombineMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "union", "":
		return CombineUnion, nil
	case "confirm":
		return CombineConfirm, nil
	case "veto":
		return CombineVeto, nil
	}
	return 0, fmt.Errorf("未知的合并方式 %q，可选：union / confirm / veto", s)
}

// Source 是组合里的一个决策源。
type Source struct {
	Name     string
	Strategy Strategy
	Params   Params
}

// Composite 把多个决策源合并成一个策略。
type Composite struct {
	sources []Source
	mode    CombineMode
	// hedge 所在市场是否双向持仓。**决定「卖」是平多还是开空**，
	// 从而决定两个源发出的信号算不算同一个动作
	hedge bool
}

// NewComposite 装配组合策略。
func NewComposite(mode CombineMode, sources []Source, hedge bool) (*Composite, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("组合策略至少要有一个决策源")
	}
	if len(sources) == 1 && mode != CombineUnion {
		return nil, fmt.Errorf("只有一个决策源时 %s 没有意义 —— "+
			"confirm 会原样通过，veto 没有否决者", mode)
	}
	return &Composite{sources: sources, mode: mode, hedge: hedge}, nil
}

func (c *Composite) Name() string {
	names := make([]string, len(c.sources))
	for i, s := range c.sources {
		names[i] = s.Name
	}
	return "composite[" + c.mode.String() + ":" + strings.Join(names, "+") + "]"
}

// Specs 组合本身没有参数 —— 参数属于各个源。
func (c *Composite) Specs() []spec.ParamSpec { return nil }

func (c *Composite) Init(ic InitContext) error {
	for i := range c.sources {
		s := &c.sources[i]
		if err := s.Strategy.Init(&scopedInit{
			InitContext: ic, prefix: sourcePrefix(i), params: s.Params,
		}); err != nil {
			return fmt.Errorf("决策源 %d（%s）初始化失败: %w", i, s.Name, err)
		}
	}
	return nil
}

func (c *Composite) OnBar(ctx StepContext) ([]Signal, error) {
	perSource := make([][]Signal, len(c.sources))
	for i := range c.sources {
		sigs, err := c.sources[i].Strategy.OnBar(&scopedStep{
			StepContext: ctx, prefix: sourcePrefix(i),
		})
		if err != nil {
			return nil, fmt.Errorf("决策源 %d（%s）报错: %w", i, c.sources[i].Name, err)
		}
		perSource[i] = sigs
	}

	switch c.mode {
	case CombineConfirm:
		return c.combineConfirm(perSource), nil
	case CombineVeto:
		return c.combineVeto(perSource), nil
	default:
		return c.combineUnion(perSource), nil
	}
}

// sigKey 标识「同一只标的的同一个动作」。
//
// 单向市场用 (标的, 方向)：Enter 与 Target 都是加仓意图，合并时该当成
// 一回事；真正互斥的是买与卖。
//
// # 双向市场必须再加上仓位槽
//
// 「卖」在双向下有两个完全不同的意思：**平多**与**开空**。
// 一多一空两棵镜像的树组在一起时，MACD 下穿那一根上
// 多头树发「平多（卖）」、空头树发「开空（卖）」——
// 只按 (标的, 卖) 去重，后者会被前者顶掉，而且不报任何错。
// 实测：336 笔成交里只有 2 笔是开空，整个空头腿等于没接上。
type sigKey struct {
	id   mktdata.InstrumentID
	side trading.Side
	// leg 仓位槽与开平。单向市场下恒为零值，键退化成 (标的, 方向)
	leg trading.PosLeg
}

func (c *Composite) keyOf(s Signal) sigKey {
	k := sigKey{id: s.Instrument, side: s.Side}
	if c.hedge {
		k.leg = trading.LegOf(s.Side, s.Kind == SignalExit, true)
	}
	return k
}

// combineUnion 并集，同标的同方向保留第一个源的。
//
// **保留第一个而不是最后一个**：源的顺序在配置里是显式写的，
// 靠前即优先，这样「主策略 + 补充策略」的意图能直接表达。
func (c *Composite) combineUnion(per [][]Signal) []Signal {
	seen := make(map[sigKey]bool, 64)
	out := make([]Signal, 0, 64)
	for _, sigs := range per {
		for _, s := range sigs {
			k := c.keyOf(s)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// combineConfirm 交集：所有源都发出同向信号才采纳。
//
// Strength 取各源的**最小值** —— 一组判断的可信度不高于其中最弱的那个。
func (c *Composite) combineConfirm(per [][]Signal) []Signal {
	if len(per) == 0 {
		return nil
	}
	count := make(map[sigKey]int, 64)
	minStrength := make(map[sigKey]float64, 64)
	for _, sigs := range per {
		local := make(map[sigKey]bool, len(sigs))
		for _, s := range sigs {
			k := c.keyOf(s)
			if local[k] {
				continue // 同一源内重复只算一次
			}
			local[k] = true
			count[k]++
			if v, ok := minStrength[k]; !ok || s.Strength < v {
				minStrength[k] = s.Strength
			}
		}
	}
	out := make([]Signal, 0, 32)
	for _, s := range per[0] {
		k := c.keyOf(s)
		if count[k] != len(per) {
			continue
		}
		count[k] = -1 // 已产出，避免第一个源里的重复项被算两次
		s.Strength = minStrength[k]
		out = append(out, s)
	}
	return out
}

// vetoKey 否决只认 (标的, 方向)。
//
// **不带仓位槽**：否决表达的是「这只标的这个方向别做」，
// 而不是「别平多但可以开空」—— 后者不是一个覆盖层能表达的意图。
type vetoKey struct {
	id   mktdata.InstrumentID
	side trading.Side
}

// combineVeto 否决：第一个源出信号，后续源的**反向**信号把它挡掉。
//
// 「反向」= 同一标的、相反方向。AI 覆盖层只需要在不看好的标的上
// 发一个反向信号，不必替你选股。
func (c *Composite) combineVeto(per [][]Signal) []Signal {
	if len(per) == 0 {
		return nil
	}
	vetoed := make(map[vetoKey]bool, 32)
	for _, sigs := range per[1:] {
		for _, s := range sigs {
			// 记下「反对方向」：卖单否决买信号，买单否决卖信号
			vetoed[vetoKey{s.Instrument, opposite(s.Side)}] = true
		}
	}
	out := make([]Signal, 0, len(per[0]))
	for _, s := range per[0] {
		if vetoed[vetoKey{s.Instrument, s.Side}] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func opposite(s trading.Side) trading.Side {
	if s == trading.SideBuy {
		return trading.SideSell
	}
	return trading.SideBuy
}

// ---- 快照 ----

// SnapshotState 逐源快照。
//
// **总是实现 StatefulStrategy**，即使所有源都无状态 —— 组合的成员
// 可能在配置里换掉，让快照格式随成员有无状态而变，恢复时会莫名其妙地失败。
func (c *Composite) SnapshotState() ([]byte, error) {
	out := make([]json.RawMessage, len(c.sources))
	for i, s := range c.sources {
		ss, ok := s.Strategy.(StatefulStrategy)
		if !ok {
			out[i] = json.RawMessage("null")
			continue
		}
		b, err := ss.SnapshotState()
		if err != nil {
			return nil, fmt.Errorf("决策源 %d（%s）快照失败: %w", i, s.Name, err)
		}
		out[i] = b
	}
	return json.Marshal(out)
}

func (c *Composite) RestoreState(b []byte) error {
	var in []json.RawMessage
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("解析组合快照失败: %w", err)
	}
	if len(in) != len(c.sources) {
		return fmt.Errorf("快照有 %d 个决策源，当前组合有 %d 个 —— "+
			"该快照多半来自另一份配置", len(in), len(c.sources))
	}
	for i, raw := range in {
		ss, ok := c.sources[i].Strategy.(StatefulStrategy)
		if !ok {
			continue
		}
		if len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("决策源 %d（%s）有跨步状态，但快照里是空的",
				i, c.sources[i].Name)
		}
		if err := ss.RestoreState(raw); err != nil {
			return fmt.Errorf("决策源 %d（%s）恢复失败: %w", i, c.sources[i].Name, err)
		}
	}
	return nil
}

// ---- 作用域包装 ----
//
// 两个源都 Use("macd") 时，引擎的去重会让**第一个赢**，第二个源
// 静默拿到别人的参数。加前缀让每个源有自己的指标命名空间，
// 而源内部对此毫无感知 —— 它写 "macd"，读也写 "macd"。

func sourcePrefix(i int) string { return fmt.Sprintf("s%d/", i) }

type scopedInit struct {
	InitContext
	prefix string
	params Params
}

func (s *scopedInit) Params() Params { return s.params }

func (s *scopedInit) Use(key string, f IndicatorFactory) {
	s.InitContext.Use(s.prefix+key, f)
}

type scopedStep struct {
	StepContext
	prefix string
}

func (s *scopedStep) Indicator(id mktdata.InstrumentID, key string) (indicator.Indicator, bool) {
	return s.StepContext.Indicator(id, s.prefix+key)
}

var (
	_ Strategy         = (*Composite)(nil)
	_ StatefulStrategy = (*Composite)(nil)
	_ InitContext      = (*scopedInit)(nil)
	_ StepContext      = (*scopedStep)(nil)
)
