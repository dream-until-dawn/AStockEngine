package strategies

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// RuleTree 是**用户在界面上拼出来的策略**：三棵决策树 + 一组指标。
//
//	是否买进（buy）   为真时考虑开仓
//	是否有效（valid） 与 buy 同时为真才真的下单；为假则记**虚拟持仓**
//	是否卖出（sell）  为真时平仓（真实持仓）或清掉虚拟持仓
//
// # 虚拟持仓是这套模型里最不显然的一件事
//
// 「买进为真但无效」时不下单，却把它记成持有 ——
// 于是**在它的卖出信号出现之前，不会再次触发买入**。
//
// 这解决的是一个真实存在的麻烦：像「MACD 在零轴上方」这类条件会
// **连续多根 bar 为真**，若只用 buy 树，每根 bar 都会尝试开仓；
// 而真正想表达的往往是「这一轮机会已经用掉了」。
// 有了虚拟持仓，「过滤掉的机会」与「还没出现的机会」就区分开了。
//
// 换个说法：buy 树管**什么时候是机会**，valid 树管**这次机会要不要下注**，
// 两者分开之后，「不下注」不再等于「继续等下一根 bar 再问一次」。
type RuleTree struct {
	inds  []indSpec
	buy   *Node
	valid *Node
	sell  *Node
	// short 为真时买入树开的是**空头**，卖出树平的也是空头
	short bool

	// virtual 虚拟持仓集合。**必须跨步保存** ——
	// 从快照恢复后它归零，被过滤掉的机会会立刻重新触发买入
	virtual map[mktdata.InstrumentID]bool
	// notReady 上一步里有多少只标的因指标未就绪而没进决策集合。
	//
	// **预热是逐标的的**：MACD(12,26,9) 要 35 根、MA200 要 200 根，
	// 而每只标的从自己上市那天起算。一只上市 30 天的新股在 MA200 的
	// 策略里就是「还没进决策范围」—— 这本来就由 Ready() 保证，
	// 但此前**没有任何地方把它显示出来**，用户只看到「信号少」。
	notReady int
	// evaluated 上一步实际进入决策集合的标的数
	evaluated int

	// prev 各**有记忆的条件**上一步的值，按 (节点路径, 标的) 索引。
	//
	//   穿越（cross_above / cross_below）存 左−右 的差值
	//   升降（rising / falling）      存 左侧本身的值
	//
	// **键在表里就代表「见过」** —— 0 是合法取值，不能拿它当哨兵。
	prev map[stateKey]float64
}

type stateKey struct {
	node string
	id   mktdata.InstrumentID
}

// indSpec 是配置里声明的一个指标实例。
type indSpec struct {
	// Name 用户起的名字，条件里按它引用（如 "ma20" → ma20.MA）
	Name string `json:"name"`
	// Kind 指标目录里的种类（sma / macd / kdj / ...）
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ---- 表达式 ----

// Node 是一棵决策树的节点：要么是**逻辑组**，要么是**条件**。
//
// 用一个结构体而不是接口 + 多态：它要从 JSON 直接解出来，
// 而 JSON 的多态解码要么写自定义 UnmarshalJSON、要么加一个 type 字段。
// 靠「有没有 op」区分更直白，也让配置读起来更短。
type Node struct {
	// Op 逻辑运算：and / or / not。为空表示这是一个条件节点
	Op       string  `json:"op,omitempty"`
	Children []*Node `json:"children,omitempty"`

	// 条件节点用下面三个
	Left  *Operand `json:"left,omitempty"`
	Cmp   string   `json:"cmp,omitempty"`
	Right *Operand `json:"right,omitempty"`
}

// Operand 是条件的一侧。
type Operand struct {
	// Kind：bar（行情列）/ ind（指标列）/ value（常数）
	Kind string `json:"kind"`
	// Field：Kind==bar 时是 open/high/low/close/volume/amount/preclose；
	// Kind==ind 时是指标的输出字段（如 J / DIF / MA）
	Field string `json:"field,omitempty"`
	// Ind：Kind==ind 时引用的指标实例名
	Ind string `json:"ind,omitempty"`
	// Value：Kind==value 时的常数
	Value float64 `json:"value,omitempty"`
}

// 比较运算。cross_above / cross_below 需要上一步的状态 ——
// 没有它们就写不出金叉死叉，而那是最常用的一类条件。
const (
	cmpGT     = "gt"
	cmpGTE    = "gte"
	cmpLT     = "lt"
	cmpLTE    = "lte"
	cmpEQ     = "eq"
	cmpNE     = "ne"
	cmpCrossU = "cross_above" // 左侧由下方上穿右侧（金叉）
	cmpCrossD = "cross_below" // 左侧由上方下穿右侧（死叉）
	// 升降只看左侧与**它自己上一根**的关系，右侧不用。
	// **相等视为下降** —— 这是明确定死的口径，不是随手选的：
	// 留一个「持平」的第三态会让「上升 or 下降」不再覆盖全部情形，
	// 用户拼出来的树会出现自己想不到的空隙。
	cmpRising  = "rising"
	cmpFalling = "falling"
)

// unaryCmp 报告该比较符只用左侧。
func unaryCmp(c string) bool { return c == cmpRising || c == cmpFalling }

// BarFields 是可用作条件左右侧的行情列。
//
// **价格列一律是后复权价**，与指标同基准。用原始价去和均线比，
// 除权日会凭空产生一次穿越 —— 那不是行情，是分红。
var BarFields = []string{"close", "open", "high", "low", "preclose", "volume", "amount"}

// ---- 配置 ----

type ruleTreeCfg struct {
	Indicators []indSpec `json:"indicators"`
	Buy        *Node     `json:"buy"`
	Valid      *Node     `json:"valid,omitempty"`
	Sell       *Node     `json:"sell"`
	// Direction long（默认，做多）/ short（做空）。
	//
	// 只允许**单一方向**：一棵树表达一个意图。
	// 真正的双向用 `composite union` 组两个 rule_tree ——
	// 一个做多一个做空，各有各的条件。硬塞进一棵树只会让
	// 「这个条件是给多头还是空头的」变成日常问题。
	//
	// 做空只在允许做空的市场有意义（A 股恒为 false，见 Market.AllowsShort）。
	Direction string `json:"direction,omitempty"`
}

func NewRuleTree() *RuleTree {
	return &RuleTree{
		virtual: make(map[mktdata.InstrumentID]bool, 1024),
		prev:    make(map[stateKey]float64, 4096),
	}
}

func (s *RuleTree) Name() string { return "rule_tree" }

// Specs 返回空：规则树的「参数」是三棵树与指标表，不是标量。
//
// 这也是它**不能进海选参数网格**的原因 —— 网格扫的是标量维度，
// 而树的形状不是一个可以 ±1 格的量。要扫的话应当把树里的**阈值**
// 提成命名参数，那是另一件事。
func (s *RuleTree) Specs() []eng.ParamSpec { return nil }

// Configure 由配置装配。规则树的配置不走 Params（那是 map[string]float64），
// 而是整段 JSON —— 树是结构不是标量。
func (s *RuleTree) Configure(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("rule_tree 需要 params：indicators / buy / sell")
	}
	var c ruleTreeCfg
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return fmt.Errorf("解析规则树失败: %w", err)
	}
	if c.Buy == nil || c.Sell == nil {
		return fmt.Errorf("买入树与卖出树都不能为空 —— " +
			"没有卖出树的策略会一直持有到回测结束")
	}
	seen := map[string]bool{}
	for i, in := range c.Indicators {
		if in.Name == "" {
			return fmt.Errorf("indicators[%d] 没有名字", i)
		}
		if seen[in.Name] {
			return fmt.Errorf("指标名 %q 重复 —— 条件里引用它会有歧义", in.Name)
		}
		seen[in.Name] = true
		if len(indicator.Fields(in.Kind)) == 0 {
			return fmt.Errorf("indicators[%d]: 未知的指标种类 %q，可选：%s",
				i, in.Kind, strings.Join(indicator.Kinds(), " / "))
		}
		// 构造一次做预检 —— 参数不合法要在跑之前就报出来
		if _, err := indicator.Catalog.Build(in.Kind, in.Params); err != nil {
			return fmt.Errorf("indicators[%d] (%s): %w", i, in.Name, err)
		}
	}
	for _, t := range []struct {
		name string
		n    *Node
	}{{"buy", c.Buy}, {"valid", c.Valid}, {"sell", c.Sell}} {
		if t.n == nil {
			continue
		}
		if err := validateNode(t.n, c.Indicators, t.name); err != nil {
			return err
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Direction)) {
	case "", "long":
		s.short = false
	case "short":
		s.short = true
	default:
		return fmt.Errorf("未知的 direction %q，可选：long / short", c.Direction)
	}
	s.inds, s.buy, s.valid, s.sell = c.Indicators, c.Buy, c.Valid, c.Sell
	return nil
}

// validateNode 递归校验一棵树。**在跑之前把能查的都查掉** ——
// 引用了不存在的指标、字段名写错、比较符不认识，都该在这里失败。
func validateNode(n *Node, inds []indSpec, path string) error {
	if n == nil {
		return fmt.Errorf("%s: 节点为空", path)
	}
	if n.Op != "" {
		switch n.Op {
		case "and", "or":
			if len(n.Children) == 0 {
				return fmt.Errorf("%s: %s 组没有子节点", path, n.Op)
			}
		case "not":
			if len(n.Children) != 1 {
				return fmt.Errorf("%s: not 只能有一个子节点，有 %d 个",
					path, len(n.Children))
			}
		default:
			return fmt.Errorf("%s: 未知的逻辑运算 %q，可选：and / or / not", path, n.Op)
		}
		for i, c := range n.Children {
			if err := validateNode(c, inds, fmt.Sprintf("%s.%d", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	switch n.Cmp {
	case cmpGT, cmpGTE, cmpLT, cmpLTE, cmpEQ, cmpNE,
		cmpCrossU, cmpCrossD, cmpRising, cmpFalling:
	default:
		return fmt.Errorf("%s: 未知的比较符 %q", path, n.Cmp)
	}
	if err := validateOperand(n.Left, inds, path+".left"); err != nil {
		return err
	}
	if unaryCmp(n.Cmp) {
		if n.Left.Kind == "value" {
			return fmt.Errorf("%s: 常数不会升也不会降", path)
		}
		return nil // 升降只看左侧，不用右侧
	}
	if err := validateOperand(n.Right, inds, path+".right"); err != nil {
		return err
	}
	if n.Left.Kind == "value" && n.Right.Kind == "value" {
		return fmt.Errorf("%s: 两侧都是常数 —— 这个条件的取值与行情无关", path)
	}
	if (n.Cmp == cmpCrossU || n.Cmp == cmpCrossD) && n.Left.Kind == "value" {
		return fmt.Errorf("%s: 穿越条件的左侧不能是常数", path)
	}
	return nil
}

func validateOperand(o *Operand, inds []indSpec, path string) error {
	if o == nil {
		return fmt.Errorf("%s: 缺少操作数", path)
	}
	switch o.Kind {
	case "value":
		return nil
	case "bar":
		for _, f := range BarFields {
			if f == o.Field {
				return nil
			}
		}
		return fmt.Errorf("%s: 未知的行情列 %q，可选：%s",
			path, o.Field, strings.Join(BarFields, " / "))
	case "ind":
		for _, in := range inds {
			if in.Name != o.Ind {
				continue
			}
			for _, f := range indicator.Fields(in.Kind) {
				if f == o.Field {
					return nil
				}
			}
			return fmt.Errorf("%s: 指标 %s（%s）没有字段 %q，可选：%s",
				path, o.Ind, in.Kind, o.Field,
				strings.Join(indicator.Fields(in.Kind), " / "))
		}
		return fmt.Errorf("%s: 引用了未声明的指标 %q", path, o.Ind)
	}
	return fmt.Errorf("%s: 未知的操作数类型 %q，可选：bar / ind / value", path, o.Kind)
}

// ---- 装配 ----

func (s *RuleTree) Init(ic eng.InitContext) error {
	if s.buy == nil || s.sell == nil {
		return fmt.Errorf("rule_tree 尚未配置")
	}
	s.virtual = make(map[mktdata.InstrumentID]bool, 1024)
	s.prev = make(map[stateKey]float64, 4096)
	for _, in := range s.inds {
		kind, params := in.Kind, in.Params
		ic.Use(in.Name, func() indicator.Indicator {
			// 这里不可能失败 —— Configure 已经构造过一次做预检
			ind, err := indicator.Catalog.Build(kind, params)
			if err != nil {
				panic(fmt.Sprintf("指标 %s 构造失败（Configure 应已拦下）: %v", kind, err))
			}
			return ind
		})
	}
	return nil
}

// evalCtx 是一次求值需要的全部东西。
type evalCtx struct {
	s   *RuleTree
	ctx eng.StepContext
	id  mktdata.InstrumentID
	// bar 后复权 bar。价格列与指标同基准，否则除权日会凭空穿越
	bar mktdata.Bar
}

func (s *RuleTree) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	_, inFlight := holdingSet(ctx)
	var sigs []eng.Signal
	s.notReady, s.evaluated = 0, 0

	for _, id := range ctx.Universe() {
		bar, ok := ctx.Bar(id)
		if !ok {
			continue // 该标的今天没有行情，什么也谈不上
		}
		adj, ok2 := ctx.AdjBar(id, mktdata.AdjHFQ)
		if !ok2 {
			adj = bar
		}
		ec := &evalCtx{s: s, ctx: ctx, id: id, bar: adj}

		// **三棵树每根 bar 都要求值，与是否持仓、是否在途无关。**
		//
		// 穿越条件（cross_above / cross_below）靠上一步的差值符号判定，
		// 那份状态必须像指标一样每步更新。若持仓期间只算卖出树，
		// 买入树里的穿越状态就此过期 —— 等到卖掉之后再评估买入，
		// 拿的是很久以前的 prev，于是凭空多出或漏掉一次穿越。
		// 这类错不报错、不崩溃，只是信号悄悄不对。
		// 三棵树里只要有一棵**确定不了**，这只标的这一步就不在决策集合里。
		// 记下来供报告与单步调试展示 —— 否则用户只看到「信号少」，
		// 看不出是「条件不满足」还是「指标还没预热完」
		buyTri := s.eval(s.buy, ec, "buy")
		validTri := s.eval(s.valid, ec, "valid")
		sellTri := s.eval(s.sell, ec, "sell")
		if buyTri == triUnknown || validTri == triUnknown || sellTri == triUnknown {
			s.notReady++
		} else {
			s.evaluated++
		}
		buyOK, validOK, sellOK := buyTri == triTrue, validTri == triTrue, sellTri == triTrue

		// 求值完了才谈能不能下单。停牌 / 零成交 / 在途单都只影响**下单**，
		// 不影响上面的状态更新
		if inFlight[id] || bar.Suspended() || bar.Close <= 0 {
			continue
		}

		// **按方向看持仓**，不能用「有没有敞口」笼统判断：
		// 双向下把一多一空两棵树用 composite 组起来时，
		// 笼统判断会让多头树因为已有空头而不开仓（反之亦然）。
		held := s.holdsMine(ctx, id)
		if held || s.virtual[id] {
			if sellOK {
				if held {
					// 平仓方向与开仓相反：做多发卖出平、做空发买入平。
					// Sizer 会据此把 Reduce 置上，双向账本才知道要动哪个槽位
					sigs = append(sigs, eng.Signal{
						Instrument: id, Kind: eng.SignalExit,
						Side: s.closeSide(), Tag: "tree_sell",
					})
				}
				// 虚拟持仓在卖出信号出现时清掉 —— 之后才可能再次买入
				delete(s.virtual, id)
			}
			continue
		}
		if !buyOK {
			continue
		}
		if validOK {
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter,
				Side: s.openSide(), Tag: "tree_buy",
			})
		} else {
			// 不下单，但记成持有 —— 在卖出信号出现前不再触发买入
			s.virtual[id] = true
		}
	}
	// 已经真实持有的标的不必再留虚拟标记（开仓成交后会走到这一步）
	for id := range s.virtual {
		if s.holdsMine(ctx, id) {
			delete(s.virtual, id)
		}
	}
	return sigs, nil
}

// tri 是三值逻辑：真 / 假 / **还答不上来**。
//
// 「答不上来」不是「假」。指标预热未完成时条件既不真也不假 ——
// 而这个区别在**组合**里会改变答案：
//
//	A ∨ B，A 为真、B 未知  →  真（B 是什么都不影响）
//	A ∧ B，A 为假、B 未知  →  假
//	A ∧ B，A 为真、B 未知  →  未知
//
// 第一版把「任一处未就绪」一律当假，于是一个长周期指标会把整棵树拖住，
// 哪怕另一支已经确定为真。那不是保守，是错的。
type tri int8

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func triOf(b bool) tri {
	if b {
		return triTrue
	}
	return triFalse
}

// evalTree 求一棵树的值。**只有确定为真才产生信号** ——
// 未知与假一样不下单，但两者在组合里的行为不同（见 tri）。
//
// nil 树视为恒真：valid 为空时行为与普通的两棵树策略一致。
func (s *RuleTree) evalTree(n *Node, ec *evalCtx, path string) bool {
	if n == nil {
		return true
	}
	return s.eval(n, ec, path) == triTrue
}

// eval 求一棵树的值。path 用于给**有记忆的条件**（穿越 / 升降）定位状态槽。
//
// **两种组合都不短路**：有记忆的条件要每步更新自己的 prev，
// 短路掉的分支下次就会拿一个过期的值去比，判定随之错乱。
func (s *RuleTree) eval(n *Node, ec *evalCtx, path string) tri {
	if n == nil {
		return triTrue
	}
	switch n.Op {
	case "and":
		res := triTrue
		for i, c := range n.Children {
			switch s.eval(c, ec, fmt.Sprintf("%s.%d", path, i)) {
			case triFalse:
				res = triFalse
			case triUnknown:
				if res != triFalse {
					res = triUnknown
				}
			}
		}
		return res
	case "or":
		res := triFalse
		for i, c := range n.Children {
			switch s.eval(c, ec, fmt.Sprintf("%s.%d", path, i)) {
			case triTrue:
				res = triTrue
			case triUnknown:
				if res != triTrue {
					res = triUnknown
				}
			}
		}
		return res
	case "not":
		switch s.eval(n.Children[0], ec, path+".0") {
		case triTrue:
			return triFalse
		case triFalse:
			return triTrue
		}
		return triUnknown
	}
	return s.evalCond(n, ec, path)
}

func (s *RuleTree) evalCond(n *Node, ec *evalCtx, path string) tri {
	l, okL := ec.operand(n.Left)
	if !okL {
		return triUnknown
	}
	if unaryCmp(n.Cmp) {
		return s.evalTrend(n.Cmp, l, path, ec.id)
	}
	r, okR := ec.operand(n.Right)
	if !okR {
		return triUnknown
	}
	switch n.Cmp {
	case cmpGT:
		return triOf(l > r)
	case cmpGTE:
		return triOf(l >= r)
	case cmpLT:
		return triOf(l < r)
	case cmpLTE:
		return triOf(l <= r)
	case cmpEQ:
		return triOf(l == r)
	case cmpNE:
		return triOf(l != r)
	case cmpCrossU, cmpCrossD:
		return s.evalCross(n.Cmp, l-r, path, ec.id)
	}
	return triFalse
}

// evalTrend 判定「相对上一根 bar 是升还是降」。
//
// **相等视为下降**（口径定死，见 cmpRising 的注释）。
// 第一次见到该标的时返回未知 —— 没有上一根就谈不上升降。
func (s *RuleTree) evalTrend(cmp string, v float64, path string, id mktdata.InstrumentID) tri {
	k := stateKey{node: path, id: id}
	prev, seen := s.prev[k]
	s.prev[k] = v
	if !seen {
		return triUnknown
	}
	if cmp == cmpRising {
		return triOf(v > prev)
	}
	return triOf(v <= prev)
}

// evalCross 判定穿越：看差值的符号是否从一侧翻到另一侧。
//
// **第一次见到该标的时恒为假** —— 没有上一步就谈不上「穿过」。
// 这与指标预热同理：答不上来时不产生信号，而不是猜一个。
func (s *RuleTree) evalCross(cmp string, diff float64, path string, id mktdata.InstrumentID) tri {
	k := stateKey{node: path, id: id}
	prev, seen := s.prev[k]
	s.prev[k] = diff
	if !seen {
		return triUnknown
	}
	if prev == 0 || diff == 0 {
		// 恰好相等的那一根既算不上穿过也算不上没穿过，等下一根再说
		return triFalse
	}
	if cmp == cmpCrossU {
		return triOf(prev < 0 && diff > 0)
	}
	return triOf(prev > 0 && diff < 0)
}

// operand 取一侧的值。ok 为 false 表示「还答不上来」（指标未就绪）。
func (e *evalCtx) operand(o *Operand) (float64, bool) {
	switch o.Kind {
	case "value":
		return o.Value, true
	case "bar":
		return barField(e.bar, o.Field), true
	case "ind":
		ind, ok := e.ctx.Indicator(e.id, o.Ind)
		if !ok || !ind.Ready() {
			return 0, false
		}
		names, vals := ind.Names(), ind.Values()
		for i, n := range names {
			if n == o.Field && i < len(vals) {
				return vals[i], true
			}
		}
		return 0, false
	}
	return 0, false
}

// barField 取行情列。价格换算成「元」，与指标同量纲。
func barField(b mktdata.Bar, f string) float64 {
	const s = indicator.DefaultPriceScale
	switch f {
	case "close":
		return float64(b.Close) / s
	case "open":
		return float64(b.Open) / s
	case "high":
		return float64(b.High) / s
	case "low":
		return float64(b.Low) / s
	case "preclose":
		return float64(b.PreClose) / s
	case "volume":
		return float64(b.Volume)
	case "amount":
		// 成交额以分存，换成元 —— 用户写 "amount > 5000000" 时想的是元
		return float64(b.Amount) / 100
	}
	return 0
}

// ---- 跨步状态 ----

type ruleTreeState struct {
	Virtual []int32            `json:"virtual"`
	Prev    map[string]float64 `json:"prev"`
}

func (s *RuleTree) SnapshotState() ([]byte, error) {
	st := ruleTreeState{
		Virtual: make([]int32, 0, len(s.virtual)),
		Prev:    make(map[string]float64, len(s.prev)),
	}
	for id := range s.virtual {
		st.Virtual = append(st.Virtual, int32(id))
	}
	// 排序：map 遍历顺序随机，不排的话同一状态会序列化出不同字节，
	// 而快照本身会进指纹（C5）
	sort.Slice(st.Virtual, func(i, j int) bool { return st.Virtual[i] < st.Virtual[j] })
	// **不能省略取值为 0 的项**：键在表里就代表「见过」，
	// 省掉 0 会让「见过且当时为 0」退化成「没见过」，恢复后第一根就误判
	for k, v := range s.prev {
		st.Prev[fmt.Sprintf("%s|%d", k.node, int32(k.id))] = v
	}
	return json.Marshal(st)
}

func (s *RuleTree) RestoreState(b []byte) error {
	var st ruleTreeState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	s.virtual = make(map[mktdata.InstrumentID]bool, len(st.Virtual))
	for _, id := range st.Virtual {
		s.virtual[mktdata.InstrumentID(id)] = true
	}
	s.prev = make(map[stateKey]float64, len(st.Prev))
	for k, v := range st.Prev {
		i := strings.LastIndex(k, "|")
		if i < 0 {
			return fmt.Errorf("快照中的状态键 %q 格式不对", k)
		}
		var id int32
		if _, err := fmt.Sscan(k[i+1:], &id); err != nil {
			return fmt.Errorf("快照中的标的 ID %q 无法解析: %w", k[i+1:], err)
		}
		s.prev[stateKey{node: k[:i], id: mktdata.InstrumentID(id)}] = v
	}
	return nil
}

// holdsMine 报告**本策略这个方向**上是否已有仓位。
//
// ⚠ 在途单仍然是按标的整体挡的（inFlight）：一多一空两棵树组在一起时，
// 多头的在途单会挡住空头这一步的开仓。保守但不精确 ——
// 精确做法要按 (标的, 方向) 过滤在途队列，等真有人这么用再说。
func (s *RuleTree) holdsMine(ctx eng.StepContext, id mktdata.InstrumentID) bool {
	ex := ctx.Ledger().Exposure(id)
	if s.short {
		return ex.Short > 0
	}
	return ex.Long > 0
}

func (s *RuleTree) openSide() trading.Side {
	if s.short {
		return trading.SideSell
	}
	return trading.SideBuy
}

func (s *RuleTree) closeSide() trading.Side {
	if s.short {
		return trading.SideBuy
	}
	return trading.SideSell
}

// VirtualCount 返回当前虚拟持仓数，供单步调试展示。
func (s *RuleTree) VirtualCount() int { return len(s.virtual) }

// Warmup 返回上一步「进了决策集合」与「指标未就绪」的标的数。
//
// **预热是逐标的的**：MACD(12,26,9) 要 35 根、MA200 要 200 根，
// 每只标的从自己上市那天起算。一只上市 30 天的新股在 MA200 的策略里
// 就是还没进决策范围 —— 这由 Ready() 保证，但要能看见才有意义。
func (s *RuleTree) Warmup() (evaluated, notReady int) {
	return s.evaluated, s.notReady
}

var (
	_ eng.Strategy         = (*RuleTree)(nil)
	_ eng.StatefulStrategy = (*RuleTree)(nil)
)
