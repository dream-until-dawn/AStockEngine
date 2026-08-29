package engine

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// fixedStrategy 每步发出一组固定信号，用来隔离测试合并规则本身。
type fixedStrategy struct {
	name string
	out  []Signal
	// used 记录 Init 拿到的参数，用来验证参数确实分发到了各个源
	used spec.Params
	// indKeys 记录 Init 声明的指标名（未加前缀的），验证命名空间隔离
	declared []string
}

func (f *fixedStrategy) Name() string            { return f.name }
func (f *fixedStrategy) Specs() []spec.ParamSpec { return nil }
func (f *fixedStrategy) Init(ic InitContext) error {
	f.used = ic.Params()
	for _, k := range f.declared {
		ic.Use(k, func() indicator.Indicator {
			return indicator.NewSMA(5, indicator.DefaultPriceScale)
		})
	}
	return nil
}
func (f *fixedStrategy) OnBar(StepContext) ([]Signal, error) { return f.out, nil }

func buy(id int) Signal {
	return Signal{Instrument: mktdata.InstrumentID(id), Kind: trading.SignalEnter,
		Side: trading.SideBuy, Strength: 1}
}

func sell(id int) Signal {
	return Signal{Instrument: mktdata.InstrumentID(id), Kind: trading.SignalExit,
		Side: trading.SideSell, Strength: 1}
}

func ids(sigs []Signal) []int {
	out := make([]int, 0, len(sigs))
	for _, s := range sigs {
		v := int(s.Instrument)
		if s.Side == trading.SideSell {
			v = -v
		}
		out = append(out, v)
	}
	return out
}

func eq(t *testing.T, name string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s：期望 %v，得到 %v", name, want, got)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s：期望 %v，得到 %v", name, want, got)
			return
		}
	}
}

// ---- 合并规则 ----

func TestCombineUnion(t *testing.T) {
	// 源 0 买 1、2；源 1 买 2、3 —— 2 重复，保留源 0 的
	got := combineUnion([][]Signal{
		{buy(1), buy(2)},
		{buy(2), buy(3)},
	})
	eq(t, "union", ids(got), []int{1, 2, 3})
}

func TestCombineConfirmNeedsAll(t *testing.T) {
	// 只有 2 是两个源都同意的
	got := combineConfirm([][]Signal{
		{buy(1), buy(2)},
		{buy(2), buy(3)},
	})
	eq(t, "confirm", ids(got), []int{2})
}

func TestCombineConfirmTakesMinStrength(t *testing.T) {
	a := buy(1)
	a.Strength = 0.9
	b := buy(1)
	b.Strength = 0.3
	got := combineConfirm([][]Signal{{a}, {b}})
	if len(got) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(got))
	}
	// 一组判断的可信度不高于其中最弱的那个
	if got[0].Strength != 0.3 {
		t.Errorf("Strength 应取最小值 0.3，得到 %v", got[0].Strength)
	}
}

// TestCombineConfirmSameSideOnly 方向不同不算确认。
func TestCombineConfirmSameSideOnly(t *testing.T) {
	got := combineConfirm([][]Signal{{buy(1)}, {sell(1)}})
	if len(got) != 0 {
		t.Errorf("一个要买一个要卖，不该算作确认，得到 %v", ids(got))
	}
}

func TestCombineVeto(t *testing.T) {
	// 源 0 想买 1、2、3；源 1 对 2 发出卖信号 —— 买 2 被否决
	got := combineVeto([][]Signal{
		{buy(1), buy(2), buy(3)},
		{sell(2)},
	})
	eq(t, "veto", ids(got), []int{1, 3})
}

// TestCombineVetoIgnoresSameSide 同向信号不是否决。
//
// 否决者要表达「别做这笔」，就得发**反向**信号。
// 后续源发出同向信号只会被无视 —— 否则「两个源都想买」会变成「不买」。
func TestCombineVetoIgnoresSameSide(t *testing.T) {
	got := combineVeto([][]Signal{{buy(1), buy(2)}, {buy(2)}})
	eq(t, "veto 同向", ids(got), []int{1, 2})
}

// TestCombineVetoOnlyFirstSourceTrades 后续源只做否决，不产生自己的单。
func TestCombineVetoOnlyFirstSourceTrades(t *testing.T) {
	got := combineVeto([][]Signal{{buy(1)}, {buy(9), sell(9)}})
	eq(t, "veto 不采纳否决者的信号", ids(got), []int{1})
}

// ---- 装配 ----

func TestCompositeDispatchesParamsPerSource(t *testing.T) {
	a := &fixedStrategy{name: "a"}
	b := &fixedStrategy{name: "b"}
	c, err := NewComposite(CombineUnion, []Source{
		{Name: "a", Strategy: a, Params: spec.Params{"x": 1}},
		{Name: "b", Strategy: b, Params: spec.Params{"x": 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(&fakeInit{}); err != nil {
		t.Fatal(err)
	}
	// 一份 Params 装不下两个源的参数 —— 必须各拿各的
	if a.used["x"] != 1 || b.used["x"] != 2 {
		t.Errorf("参数没有按源分发：a=%v b=%v", a.used, b.used)
	}
}

// TestCompositeNamespacesIndicators 两个源都声明 "macd" 时不能互相覆盖。
//
// 引擎的 Use 遇到重名会**让第一个赢**，第二个源于是静默拿到别人的参数 ——
// 这种错不报警、不崩溃，只是结果悄悄不对。
func TestCompositeNamespacesIndicators(t *testing.T) {
	a := &fixedStrategy{name: "a", declared: []string{"macd"}}
	b := &fixedStrategy{name: "b", declared: []string{"macd"}}
	c, _ := NewComposite(CombineUnion, []Source{
		{Name: "a", Strategy: a}, {Name: "b", Strategy: b},
	})
	fi := &fakeInit{}
	if err := c.Init(fi); err != nil {
		t.Fatal(err)
	}
	if len(fi.keys) != 2 {
		t.Fatalf("期望注册 2 个指标（各带前缀），得到 %v", fi.keys)
	}
	if fi.keys[0] == fi.keys[1] {
		t.Errorf("两个源的指标名撞了：%v", fi.keys)
	}
}

func TestCompositeRejectsDegenerate(t *testing.T) {
	one := []Source{{Name: "a", Strategy: &fixedStrategy{name: "a"}}}
	if _, err := NewComposite(CombineConfirm, one); err == nil {
		t.Error("只有一个源时 confirm 该报错 —— 它会原样通过，等于没配")
	}
	if _, err := NewComposite(CombineVeto, one); err == nil {
		t.Error("只有一个源时 veto 该报错 —— 没有否决者")
	}
	if _, err := NewComposite(CombineUnion, nil); err == nil {
		t.Error("零个源该报错")
	}
}

func TestParseCombineMode(t *testing.T) {
	for in, want := range map[string]CombineMode{
		"union": CombineUnion, "": CombineUnion,
		"confirm": CombineConfirm, "veto": CombineVeto,
	} {
		got, err := ParseCombineMode(in)
		if err != nil || got != want {
			t.Errorf("ParseCombineMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseCombineMode("majority"); err == nil {
		t.Error("未知模式该报错")
	}
}

// ---- 测试替身 ----

type fakeInit struct {
	keys []string
}

func (f *fakeInit) Params() Params                     { return Params{} }
func (f *fakeInit) Use(key string, _ IndicatorFactory) { f.keys = append(f.keys, key) }
func (f *fakeInit) Universe() []mktdata.InstrumentID   { return nil }
func (f *fakeInit) Instrument(mktdata.InstrumentID) *mktdata.Instrument {
	return nil
}

var _ InitContext = (*fakeInit)(nil)
