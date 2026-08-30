package strategies

import (
	"encoding/json"
	"testing"

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
	// 第一次：差值为正，但没有上一步
	if s.evalCross(cmpCrossU, +1, "buy", id) {
		t.Error("第一次观察不该算作上穿")
	}
	// 仍为正 —— 没有穿越
	if s.evalCross(cmpCrossU, +1, "buy", id) {
		t.Error("持续在上方不是上穿")
	}
	// 翻到负
	if s.evalCross(cmpCrossU, -1, "buy", id) {
		t.Error("下穿不该报成上穿")
	}
	// 再翻回正 —— 这才是上穿
	if !s.evalCross(cmpCrossU, +1, "buy", id) {
		t.Error("由下方翻到上方应算上穿")
	}
}

func TestCrossBelow(t *testing.T) {
	s := NewRuleTree()
	id := mktdata.InstrumentID(1)
	s.evalCross(cmpCrossD, +1, "sell", id)
	if !s.evalCross(cmpCrossD, -1, "sell", id) {
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
	if !s.evalCross(cmpCrossU, +1, "buy", id) {
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
	a.virtual[7] = true
	a.virtual[9] = true
	a.evalCross(cmpCrossU, -1, "buy", 7)

	b := NewRuleTree()
	snap, err := a.SnapshotState()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RestoreState(snap); err != nil {
		t.Fatal(err)
	}
	if !b.virtual[7] || !b.virtual[9] || b.VirtualCount() != 2 {
		t.Errorf("虚拟持仓没恢复：%v", b.virtual)
	}
	// 恢复后再喂一个正值，应当判为上穿（说明 prev=-1 被带过来了）
	if !b.evalCross(cmpCrossU, +1, "buy", 7) {
		t.Error("穿越状态没恢复")
	}
}

// TestRuleTreeSnapshotIsDeterministic 同一状态两次序列化必须逐字节相同。
//
// map 遍历顺序是随机的，不排序的话快照字节会变 —— 而快照进指纹（C5）。
func TestRuleTreeSnapshotIsDeterministic(t *testing.T) {
	s := NewRuleTree()
	for i := int32(1); i <= 50; i++ {
		s.virtual[mktdata.InstrumentID(i)] = true
	}
	a, _ := s.SnapshotState()
	b, _ := s.SnapshotState()
	if string(a) != string(b) {
		t.Error("同一状态两次快照的字节不同 —— map 顺序没定序")
	}
}
