package strategies

import (
	"encoding/json"
	"testing"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func cfgOK(t *testing.T, src string) *RuleTree {
	t.Helper()
	s := NewRuleTree()
	if err := s.Configure(json.RawMessage(src)); err != nil {
		t.Fatalf("配置应当通过，却报错: %v\n%s", err, src)
	}
	return s
}

func cfgErr(t *testing.T, src, why string) {
	t.Helper()
	if err := NewRuleTree().Configure(json.RawMessage(src)); err == nil {
		t.Errorf("%s：应当报错却通过了\n%s", why, src)
	}
}

const macdInds = `"indicators":[{"name":"macd","kind":"macd","params":{"short":12,"long":26,"signal":9}}]`

// ---- 配置校验 ----
//
// 这一组的意义是：**跑之前就把能查的都查掉**。
// 引用了不存在的指标、字段名写错、比较符不认识 —— 都不该等到
// 跑了三十秒之后才在某一步冒出来。

func TestRuleTreeAcceptsValidConfig(t *testing.T) {
	cfgOK(t, `{`+macdInds+`,
		"buy":{"left":{"kind":"ind","ind":"macd","field":"DIF"},"cmp":"cross_above",
		       "right":{"kind":"ind","ind":"macd","field":"DEA"}},
		"sell":{"left":{"kind":"ind","ind":"macd","field":"DIF"},"cmp":"cross_below",
		        "right":{"kind":"ind","ind":"macd","field":"DEA"}}}`)
}

func TestRuleTreeAcceptsNestedGroups(t *testing.T) {
	cfgOK(t, `{`+macdInds+`,
		"buy":{"op":"and","children":[
			{"left":{"kind":"ind","ind":"macd","field":"DIF"},"cmp":"gt","right":{"kind":"value","value":0}},
			{"op":"or","children":[
				{"left":{"kind":"bar","field":"close"},"cmp":"gt","right":{"kind":"value","value":10}},
				{"left":{"kind":"bar","field":"amount"},"cmp":"gt","right":{"kind":"value","value":50000000}}
			]}
		]},
		"sell":{"op":"not","children":[
			{"left":{"kind":"ind","ind":"macd","field":"DIF"},"cmp":"gt","right":{"kind":"value","value":0}}
		]}}`)
}

func TestRuleTreeRejectsUnknownIndicator(t *testing.T) {
	cfgErr(t, `{"indicators":[{"name":"x","kind":"nope"}],
		"buy":{"left":{"kind":"bar","field":"close"},"cmp":"gt","right":{"kind":"value","value":1}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"指标种类不存在")
}

func TestRuleTreeRejectsUndeclaredIndicatorRef(t *testing.T) {
	cfgErr(t, `{`+macdInds+`,
		"buy":{"left":{"kind":"ind","ind":"kdj","field":"J"},"cmp":"gt","right":{"kind":"value","value":0}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"条件引用了未声明的指标")
}

func TestRuleTreeRejectsBadField(t *testing.T) {
	cfgErr(t, `{`+macdInds+`,
		"buy":{"left":{"kind":"ind","ind":"macd","field":"J"},"cmp":"gt","right":{"kind":"value","value":0}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"MACD 没有 J 字段")
	cfgErr(t, `{`+macdInds+`,
		"buy":{"left":{"kind":"bar","field":"vwap"},"cmp":"gt","right":{"kind":"value","value":0}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"行情没有 vwap 列")
}

func TestRuleTreeRejectsDuplicateIndicatorName(t *testing.T) {
	cfgErr(t, `{"indicators":[{"name":"a","kind":"sma"},{"name":"a","kind":"ema"}],
		"buy":{"left":{"kind":"ind","ind":"a","field":"MA"},"cmp":"gt","right":{"kind":"value","value":1}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"指标重名会让条件里的引用有歧义")
}

// TestRuleTreeRejectsBothConstant 两侧都是常数的条件与行情无关。
func TestRuleTreeRejectsBothConstant(t *testing.T) {
	cfgErr(t, `{"indicators":[],
		"buy":{"left":{"kind":"value","value":1},"cmp":"gt","right":{"kind":"value","value":0}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"两侧都是常数")
}

// TestRuleTreeRequiresSellTree 没有卖出树的策略会一直持有到回测结束。
func TestRuleTreeRequiresSellTree(t *testing.T) {
	cfgErr(t, `{"indicators":[],
		"buy":{"left":{"kind":"bar","field":"close"},"cmp":"gt","right":{"kind":"value","value":1}}}`,
		"缺少卖出树")
}

func TestRuleTreeRejectsBadOp(t *testing.T) {
	cfgErr(t, `{"indicators":[],
		"buy":{"op":"xor","children":[
			{"left":{"kind":"bar","field":"close"},"cmp":"gt","right":{"kind":"value","value":1}}]},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"未知的逻辑运算")
	cfgErr(t, `{"indicators":[],
		"buy":{"left":{"kind":"bar","field":"close"},"cmp":"approx","right":{"kind":"value","value":1}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"未知的比较符")
	cfgErr(t, `{"indicators":[],
		"buy":{"op":"not","children":[
			{"left":{"kind":"bar","field":"close"},"cmp":"gt","right":{"kind":"value","value":1}},
			{"left":{"kind":"bar","field":"low"},"cmp":"gt","right":{"kind":"value","value":1}}]},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"not 只能有一个子节点")
}

func TestRuleTreeRejectsBadIndicatorParams(t *testing.T) {
	cfgErr(t, `{"indicators":[{"name":"m","kind":"macd","params":{"short":30,"long":20,"signal":9}}],
		"buy":{"left":{"kind":"ind","ind":"m","field":"DIF"},"cmp":"gt","right":{"kind":"value","value":0}},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"lt","right":{"kind":"value","value":1}}}`,
		"MACD 快线不小于慢线")
}

// ---- 穿越 ----

// TestCrossNeedsPriorObservation 第一次见到该标的时不算穿越。
//
// 这正是原生 crossover.go 的那个 bug：prevAbove 的零值是 false，
// 于是预热完成的那一根，所有已在上方的标的一起被判成金叉 ——
// 而那不是穿越，是状态检查。
func TestCrossNeedsPriorObservation(t *testing.T) {
	s := NewRuleTree()
	id := mktdata.InstrumentID(1)
	// 第一次：差值为正，但没有上一步 —— 应为「未知」而不是假
	if got := s.evalCross(cmpCrossU, +1, "buy", id); got != triUnknown {
		t.Errorf("第一次观察应为「未知」而不是 %v —— 没有上一步就答不上来", got)
	}
	// 仍为正 —— 没有穿越
	if triTrue == s.evalCross(cmpCrossU, +1, "buy", id) {
		t.Error("持续在上方不是上穿")
	}
	// 翻到负
	if triTrue == s.evalCross(cmpCrossU, -1, "buy", id) {
		t.Error("下穿不该报成上穿")
	}
	// 再翻回正 —— 这才是上穿
	if triTrue != s.evalCross(cmpCrossU, +1, "buy", id) {
		t.Error("由下方翻到上方应算上穿")
	}
}

func TestCrossBelow(t *testing.T) {
	s := NewRuleTree()
	id := mktdata.InstrumentID(1)
	s.evalCross(cmpCrossD, +1, "sell", id)
	if triTrue != s.evalCross(cmpCrossD, -1, "sell", id) {
		t.Error("由上方翻到下方应算下穿")
	}
}

// TestCrossIsPerNode 不同节点的穿越状态互不干扰。
//
// 买入树与卖出树都盯着 DIF/DEA 时，两者的 prev 必须各存一份 ——
// 共用一份会让「今天卖出树看过了」把买入树的判定也一起改掉。
func TestCrossIsPerNode(t *testing.T) {
	s := NewRuleTree()
	id := mktdata.InstrumentID(1)
	s.evalCross(cmpCrossU, -1, "buy", id)
	s.evalCross(cmpCrossD, -1, "sell", id)
	if triTrue != s.evalCross(cmpCrossU, +1, "buy", id) {
		t.Error("buy 节点应当记得自己的上一步")
	}
}

// ---- 快照 ----

// TestRuleTreeSnapshotRoundTrip 虚拟持仓与穿越状态都要能往返。
//
// 虚拟持仓丢了 → 被过滤掉的机会立刻重新触发买入。
// 穿越状态丢了 → 恢复后的第一根 bar 不再判穿越（或误判）。
// 两者都不报错，只是信号悄悄不对。而 C6 的实盘每天从快照恢复。
func TestRuleTreeSnapshotRoundTrip(t *testing.T) {
	a := NewRuleTree()
	a.virtual[7] = virtualPos{Day: 20250102, Price: 12_345}
	a.virtual[9] = virtualPos{Day: 20250103, Price: 67_890}
	a.vtrips = append(a.vtrips, VirtualTrip{
		Instrument: 5, OpenDay: 20241201, CloseDay: 20241220,
		OpenPrice: 10_000, ClosePrice: 11_000,
	})
	a.evalCross(cmpCrossU, -1, "buy", 7)

	b := NewRuleTree()
	snap, err := a.SnapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreState(snap); err != nil {
		t.Fatal(err)
	}
	if b.VirtualCount() != 2 {
		t.Errorf("虚拟持仓没恢复：%v", b.virtual)
	}
	// **开仓日与开仓价也要恢复**：只存标的 ID 的话，恢复后那笔
	// 虚拟持仓平掉时凑不出开仓端，整轮记录就丢了
	if got := b.virtual[7]; got.Day != 20250102 || got.Price != 12_345 {
		t.Errorf("虚拟持仓 7 的开仓记录没恢复：%+v", got)
	}
	if got := b.virtual[9]; got.Day != 20250103 || got.Price != 67_890 {
		t.Errorf("虚拟持仓 9 的开仓记录没恢复：%+v", got)
	}
	// 已走完的虚拟轮次不能丢 —— 丢了的话，从快照恢复之后
	// 之前被过滤掉的那些机会在报告里凭空消失
	if vt := b.vtrips; len(vt) != 1 || vt[0].Instrument != 5 ||
		vt[0].ClosePrice != 11_000 {
		t.Errorf("虚拟轮次没恢复：%+v", vt)
	}
	// 收益率：10000 → 11000 是 +10%
	if vt := b.VirtualTrips(); len(vt) != 1 || vt[0].Ratio < 0.0999 || vt[0].Ratio > 0.1001 {
		t.Errorf("虚拟轮次的收益率算错了：%+v", vt)
	}
	// 恢复后再喂一个正值，应当判为上穿（说明 prev=-1 被带过来了）
	if triTrue != b.evalCross(cmpCrossU, +1, "buy", 7) {
		t.Error("穿越状态没恢复")
	}
}

// TestRuleTreeSnapshotIsDeterministic 同一状态两次序列化必须逐字节相同。
//
// map 遍历顺序是随机的，不排序的话快照字节会变 —— 而快照进指纹（C5）。
func TestRuleTreeSnapshotIsDeterministic(t *testing.T) {
	s := NewRuleTree()
	for i := int32(1); i <= 50; i++ {
		s.virtual[mktdata.InstrumentID(i)] = virtualPos{Day: 20250100 + i, Price: int64(i) * 137}
	}
	a, _ := s.SnapshotState()
	b, _ := s.SnapshotState()
	if string(a) != string(b) {
		t.Error("同一状态两次快照的字节不同 —— map 顺序没定序")
	}
}

// ---- 三值逻辑 ----
//
// 「答不上来」不是「假」。指标预热未完成时条件既不真也不假，
// 而这个区别在组合里会**改变答案**。第一版把未就绪一律当假，
// 于是一个长周期指标能把整棵树拖住 —— 哪怕另一支已经确定为真。

// evalNode 是给测试用的求值入口：把一棵树在一个假 ctx 上求值。
// 用「未声明的指标引用」制造「未知」—— operand 取不到值就是未知。
// fakeStep 只覆盖 Indicator 一个方法 —— 嵌入接口后其余方法都在，
// 调用会 panic，而这些测试不该碰它们。碰了就是测试写错了地方。
type fakeStep struct{ eng.StepContext }

func (fakeStep) Indicator(mktdata.InstrumentID, string) (indicator.Indicator, bool) {
	return nil, false // 永远取不到 → 引用指标的条件必然「未知」
}

func evalTri(t *testing.T, n *Node) tri {
	t.Helper()
	s := NewRuleTree()
	ec := &evalCtx{s: s, ctx: fakeStep{}, id: 1}
	return s.eval(n, ec, "t")
}

func constCond(v float64, cmp string, r float64) *Node {
	return &Node{
		Left:  &Operand{Kind: "value", Value: v},
		Cmp:   cmp,
		Right: &Operand{Kind: "value", Value: r},
	}
}

// unknownCond 造一个必然「答不上来」的条件：引用一个不存在的指标。
// 求值时 ctx 为 nil，取不到指标 → 未知。
func unknownCond() *Node {
	return &Node{
		Left:  &Operand{Kind: "value", Value: 1},
		Cmp:   "gt",
		Right: &Operand{Kind: "ind", Ind: "nope", Field: "X"},
	}
}

func TestTriOrTrueBeatsUnknown(t *testing.T) {
	// 真 ∨ 未知 = 真（未知那支是什么都不影响）
	n := &Node{Op: "or", Children: []*Node{constCond(2, "gt", 1), unknownCond()}}
	if got := evalTri(t, n); got != triTrue {
		t.Errorf("真 ∨ 未知 应为真，得到 %v", got)
	}
}

func TestTriOrFalseWithUnknown(t *testing.T) {
	// 假 ∨ 未知 = 未知
	n := &Node{Op: "or", Children: []*Node{constCond(1, "gt", 2), unknownCond()}}
	if got := evalTri(t, n); got != triUnknown {
		t.Errorf("假 ∨ 未知 应为未知，得到 %v", got)
	}
}

func TestTriAndFalseBeatsUnknown(t *testing.T) {
	// 假 ∧ 未知 = 假
	n := &Node{Op: "and", Children: []*Node{constCond(1, "gt", 2), unknownCond()}}
	if got := evalTri(t, n); got != triFalse {
		t.Errorf("假 ∧ 未知 应为假，得到 %v", got)
	}
}

func TestTriAndTrueWithUnknown(t *testing.T) {
	// 真 ∧ 未知 = 未知
	n := &Node{Op: "and", Children: []*Node{constCond(2, "gt", 1), unknownCond()}}
	if got := evalTri(t, n); got != triUnknown {
		t.Errorf("真 ∧ 未知 应为未知，得到 %v", got)
	}
}

// TestTriNotUnknown 未知取反还是未知。
func TestTriNotUnknown(t *testing.T) {
	n := &Node{Op: "not", Children: []*Node{unknownCond()}}
	if got := evalTri(t, n); got != triUnknown {
		t.Errorf("¬未知 应为未知，得到 %v", got)
	}
}

// TestTriOnlyTrueEmitsSignal 只有确定为真才产生信号，未知与假一样不下单。
func TestTriOnlyTrueEmitsSignal(t *testing.T) {
	s := NewRuleTree()
	ec := &evalCtx{s: s, ctx: fakeStep{}, id: 1}
	if s.evalTree(unknownCond(), ec, "t") {
		t.Error("未知不该产生信号")
	}
	if s.evalTree(constCond(1, "gt", 2), ec, "t") {
		t.Error("假不该产生信号")
	}
	if !s.evalTree(constCond(2, "gt", 1), ec, "t") {
		t.Error("真应当产生信号")
	}
}

// ---- 升降 ----

// TestTrendRisingFalling 相对上一根是升还是降；**相等视为下降**。
func TestTrendRisingFalling(t *testing.T) {
	s := NewRuleTree()
	id := mktdata.InstrumentID(1)
	// 第一次没有上一根 —— 未知
	if got := s.evalTrend(cmpRising, 10, "n", id); got != triUnknown {
		t.Errorf("第一次应为未知，得到 %v", got)
	}
	if got := s.evalTrend(cmpRising, 11, "n", id); got != triTrue {
		t.Errorf("10 → 11 应为上升，得到 %v", got)
	}
	if got := s.evalTrend(cmpRising, 11, "n", id); got != triFalse {
		t.Errorf("11 → 11 不算上升（相等视为下降），得到 %v", got)
	}
}

func TestTrendFallingIncludesEqual(t *testing.T) {
	s := NewRuleTree()
	id := mktdata.InstrumentID(1)
	s.evalTrend(cmpFalling, 10, "n", id)
	if got := s.evalTrend(cmpFalling, 10, "n", id); got != triTrue {
		t.Errorf("相等应视为下降，得到 %v", got)
	}
	if got := s.evalTrend(cmpFalling, 9, "n", id); got != triTrue {
		t.Errorf("10 → 9 应为下降，得到 %v", got)
	}
	if got := s.evalTrend(cmpFalling, 12, "n", id); got != triFalse {
		t.Errorf("9 → 12 不是下降，得到 %v", got)
	}
}

// TestTrendConfigAcceptsUnaryCmp 升降不需要右侧。
func TestTrendConfigAcceptsUnaryCmp(t *testing.T) {
	cfgOK(t, `{`+macdInds+`,
		"buy":{"left":{"kind":"ind","ind":"macd","field":"DIF"},"cmp":"rising"},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"falling"}}`)
}

// TestTrendRejectsConstantLeft 常数不会升也不会降。
func TestTrendRejectsConstantLeft(t *testing.T) {
	cfgErr(t, `{"indicators":[],
		"buy":{"left":{"kind":"value","value":1},"cmp":"rising"},
		"sell":{"left":{"kind":"bar","field":"close"},"cmp":"falling"}}`,
		"常数不会升也不会降")
}

// TestPrevSnapshotKeepsZero 取值为 0 的状态不能在快照里被省掉。
//
// 键在表里就代表「见过」。省掉 0 会让「见过且当时为 0」退化成「没见过」，
// 恢复后第一根就误判 —— 而 C6 的实盘每天从快照恢复。
func TestPrevSnapshotKeepsZero(t *testing.T) {
	a := NewRuleTree()
	a.evalTrend(cmpRising, 0, "n", 1) // 记下 0
	snap, err := a.SnapshotState()
	if err != nil {
		t.Fatal(err)
	}
	b := NewRuleTree()
	if err := b.RestoreState(snap); err != nil {
		t.Fatal(err)
	}
	// 恢复后喂 1：若 0 被省掉，这里会是「未知」而不是「上升」
	if got := b.evalTrend(cmpRising, 1, "n", 1); got != triTrue {
		t.Errorf("恢复后 0 → 1 应为上升，得到 %v —— 取值 0 的状态被省掉了", got)
	}
}

// TestVirtualTripReturnRatio 虚拟轮次只有收益率，且做空方向要取反。
//
// 虚拟持仓从未占用资金，也就没经过 Sizer 定量 —— 硬给它安一个
// 「本该赚多少钱」的金额，那个金额是编出来的。
// 收益率是这笔决策唯一真实可算的东西。
func TestVirtualTripReturnRatio(t *testing.T) {
	long := VirtualTrip{OpenPrice: 10_000, ClosePrice: 11_000}
	if got := long.ReturnRatio(); got < 0.0999 || got > 0.1001 {
		t.Errorf("做多 10000→11000 应是 +10%%，得到 %.4f", got)
	}
	// 同样的价格走势，做空是亏的
	short := VirtualTrip{OpenPrice: 10_000, ClosePrice: 11_000, Short: true}
	if got := short.ReturnRatio(); got > -0.0999 || got < -0.1001 {
		t.Errorf("做空 10000→11000 应是 -10%%，得到 %.4f", got)
	}
	// 开仓价缺失时返回 0，而不是除零
	if got := (VirtualTrip{ClosePrice: 11_000}).ReturnRatio(); got != 0 {
		t.Errorf("没有开仓价时应返回 0，得到 %.4f", got)
	}
}
