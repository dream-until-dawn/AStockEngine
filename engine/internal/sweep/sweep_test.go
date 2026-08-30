package sweep

import (
	"encoding/json"
	"math"
	"testing"
)

const baseCfg = `{
  "name": "base",
  "data": {"root": "../data", "market": "ashare", "freq": "1d",
           "from": 20200101, "to": 0, "universe": {"type": "stock"}},
  "market": {"impl": "ashare"},
  "sizer": {"impl": "equal_weight", "params": {"slots": 10, "base": "initial"}},
  "engine": {"indicator_adj": "hfq"},
  "portfolio": {"initial_cash_cents": 100000000},
  "strategy": {"impl": "macd_cross", "params": {"short": 12, "long": 26, "signal": 9}}
}`

func mustConfig(t *testing.T, grid, cons string) *Config {
	t.Helper()
	src := `{"name":"t","base":"b.json","grid":` + grid
	if cons != "" {
		src += `,"constraints":` + cons
	}
	src += `,"walk_forward":{"enabled":false},"noise_probe":{},"gate":{}}`
	var c Config
	if err := json.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("解析测试配置失败: %v", err)
	}
	return &c
}

// ---- 网格展开 ----

func TestExpandCartesian(t *testing.T) {
	c := mustConfig(t, `{"strategy.params.short":[5,8],"strategy.params.signal":[9,14]}`, "")
	sets, err := c.Expand([]byte(baseCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 4 {
		t.Fatalf("2×2 应展开成 4 组，得到 %d", len(sets))
	}
	// 编号必须是 0..n-1 且按规范化 JSON 定序（C5：两次展开一致）
	for i, s := range sets {
		if s.ID != int32(i) {
			t.Errorf("第 %d 组的 ID 是 %d", i, s.ID)
		}
	}
	again, _ := c.Expand([]byte(baseCfg))
	for i := range sets {
		if string(sets[i].JSON) != string(again[i].JSON) {
			t.Fatalf("两次展开顺序不一致：%s vs %s", sets[i].JSON, again[i].JSON)
		}
	}
}

// TestExpandConstraint 约束把非法组合滤掉。
//
// `short < long` 不只是省算力：短线周期不小于长线时 MACD 的语义就反了，
// 而策略的 Init 会直接报错 —— 让它进网格等于制造一批必然失败的行。
func TestExpandConstraint(t *testing.T) {
	c := mustConfig(t,
		`{"strategy.params.short":[5,30],"strategy.params.long":[20,26]}`,
		`["strategy.params.short < strategy.params.long"]`)
	sets, err := c.Expand([]byte(baseCfg))
	if err != nil {
		t.Fatal(err)
	}
	// 4 组里 (30,20) 与 (30,26) 都不合法
	if len(sets) != 2 {
		t.Fatalf("约束后应剩 2 组，得到 %d", len(sets))
	}
	for _, s := range sets {
		short, _ := toFloat(s.Values["strategy.params.short"])
		long, _ := toFloat(s.Values["strategy.params.long"])
		if short >= long {
			t.Errorf("漏掉了非法组合 short=%v long=%v", short, long)
		}
	}
}

// TestExpandRejectsUnknownPath 网格只改已有字段，不新增。
//
// 新增多半是把名字写错了，而配置层的 DisallowUnknownFields 会在
// **跑到那一组时**才报错。在展开阶段拦下来，才叫「跑之前就知道」。
func TestExpandRejectsUnknownPath(t *testing.T) {
	c := mustConfig(t, `{"strategy.params.shrot":[5,8]}`, "")
	if _, err := c.Expand([]byte(baseCfg)); err == nil {
		t.Error("路径拼错时应当在展开阶段报错")
	}
}

func TestExpandRejectsNonObjectPath(t *testing.T) {
	c := mustConfig(t, `{"name.foo":[1,2]}`, "")
	if _, err := c.Expand([]byte(baseCfg)); err == nil {
		t.Error("中间节点不是对象时应报错")
	}
}

// TestExpandKeepsIntegerLiterals 定点与日期字段不能被浮点化。
//
// 用 json.Number 解码就是为了这个：把 20200101 变成 2.0200101e+07
// 再写回去，配置层可能仍然能解析，但 initial_cash_cents 这类大整数
// 会在往返里悄悄失真。
func TestExpandKeepsIntegerLiterals(t *testing.T) {
	c := mustConfig(t, `{"sizer.params.slots":[30]}`, "")
	sets, err := c.Expand([]byte(baseCfg))
	if err != nil {
		t.Fatal(err)
	}
	s := string(sets[0].Config)
	for _, want := range []string{`"from":20200101`, `"initial_cash_cents":100000000`} {
		if !contains(s, want) {
			t.Errorf("展开后的配置丢了整数字面量 %q：\n%s", want, s)
		}
	}
}

func TestExpandSugarRange(t *testing.T) {
	c := mustConfig(t, `{"sizer.params.slots":{"from":10,"to":30,"step":10}}`, "")
	sets, err := c.Expand([]byte(baseCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 3 {
		t.Fatalf("10~30 步进 10 应得 3 组，得到 %d", len(sets))
	}
}

func TestExpandEmptyAfterConstraints(t *testing.T) {
	c := mustConfig(t,
		`{"strategy.params.short":[30],"strategy.params.long":[20]}`,
		`["strategy.params.short < strategy.params.long"]`)
	if _, err := c.Expand([]byte(baseCfg)); err == nil {
		t.Error("一组都不剩时应报错，而不是静默跑 0 组")
	}
}

// ---- 切窗 ----

func days(n int) []int32 {
	out := make([]int32, n)
	for i := range out {
		out[i] = int32(20000000 + i) // 只要求升序且唯一
	}
	return out
}

func TestMakeWindows(t *testing.T) {
	wf := WalkForward{Enabled: true, ISYears: 3, OOSYears: 1, StepYears: 1, WarmupDays: 10}
	ws, err := MakeWindows(days(1000), wf, 100) // 一年 100 天 → IS 300 OOS 100 步进 100
	if err != nil {
		t.Fatal(err)
	}
	// start = 0,100,...,600 共 7 个（600+300+100=1000 刚好）
	if len(ws) != 7 {
		t.Fatalf("期望 7 个窗口，得到 %d", len(ws))
	}
	for i, w := range ws {
		if w.ISFrom > w.ISTo || w.ISTo >= w.OOSFrom || w.OOSFrom > w.OOSTo {
			t.Fatalf("窗口 %d 的区间不自洽: %+v", i, w)
		}
		// **IS 与 OOS 不得重叠** —— 重叠就等于拿样本内的数据当样本外
		if w.OOSFrom <= w.ISTo {
			t.Fatalf("窗口 %d 的 OOS 与 IS 重叠: %+v", i, w)
		}
		// 预热前缀只能往前，不能越过数据起点
		if w.DataFrom > w.ISFrom {
			t.Fatalf("窗口 %d 的预热起点晚于 IS 起点: %+v", i, w)
		}
	}
	if ws[0].DataFrom != days(1000)[0] {
		t.Error("第一个窗口的预热前缀应被夹到数据起点")
	}
}

func TestMakeWindowsTooShort(t *testing.T) {
	wf := WalkForward{Enabled: true, ISYears: 3, OOSYears: 1, StepYears: 1}
	if _, err := MakeWindows(days(200), wf, 100); err == nil {
		t.Error("数据装不下一个窗口时应报错，而不是给出半截窗口")
	}
}

func TestMakeWindowsDisabled(t *testing.T) {
	ws, err := MakeWindows(days(1000), WalkForward{Enabled: false}, 100)
	if err != nil || ws != nil {
		t.Errorf("未开启时应返回 nil, nil，得到 %v, %v", ws, err)
	}
}

// ---- 噪声与判定 ----

func probeRows(param int32, window int16, vals ...float64) []Result {
	out := make([]Result, 0, len(vals))
	for i, v := range vals {
		out = append(out, Result{
			ParamID: param, Window: window, Phase: phaseOOS,
			Probe: int8(i + 1), TotalReturn: v, RoundTrips: 999,
		})
	}
	return out
}

func TestMeasureNoise(t *testing.T) {
	rows := append(probeRows(0, 0, 0.10, 0.12, 0.08),
		probeRows(1, 0, 0.20, 0.24, 0.16)...)
	n := MeasureNoise(rows)
	if n.Samples != 2 || n.Repeats != 3 {
		t.Fatalf("样本数/重复数不对: %+v", n)
	}
	if n.StdDev <= 0 {
		t.Error("标准差应为正")
	}
	// 两组的极差分别是 0.04 与 0.08，中位数应在两者之间
	if n.Range < 0.04-1e-9 || n.Range > 0.08+1e-9 {
		t.Errorf("极差 %v 不在 [0.04, 0.08] 内", n.Range)
	}
}

// TestMeasureNoiseIgnoresFormal 正式行（Probe==0）不能混进噪声基线。
func TestMeasureNoiseIgnoresFormal(t *testing.T) {
	rows := []Result{
		{ParamID: 0, Window: 0, Phase: phaseOOS, Probe: 0, TotalReturn: 5},
		{ParamID: 0, Window: 0, Phase: phaseOOS, Probe: 0, TotalReturn: -5},
	}
	if n := MeasureNoise(rows); n.Samples != 0 {
		t.Errorf("只有正式行时不该算出基线，得到 %+v", n)
	}
}

// TestJudgeRefusesWithoutNoise 没有噪声基线时**不能默认「有意义」**。
//
// 这正是这一版要防的自欺：不量噪声就排名，等于把噪声当成结论。
func TestJudgeRefusesWithoutNoise(t *testing.T) {
	aggs := map[int32]*ParamAgg{
		0: {ParamID: 0, Median: 0.10, Windows: 3},
		1: {ParamID: 1, Median: -0.20, Windows: 3},
	}
	v := Judge(aggs, Noise{})
	if v.Meaningful {
		t.Error("没有噪声基线时不该判定为有意义")
	}
	if !math.IsNaN(v.Ratio) {
		t.Errorf("比值应为 NaN，得到 %v", v.Ratio)
	}
}

func TestJudgeThreshold(t *testing.T) {
	aggs := map[int32]*ParamAgg{
		0: {Median: 0.00, Windows: 3}, 1: {Median: 0.01, Windows: 3},
		2: {Median: -0.01, Windows: 3},
	}
	// 散布约 0.008；噪声 0.01 → 比值 0.8 < 1.5，不可分辨
	if Judge(aggs, Noise{StdDev: 0.01}).Meaningful {
		t.Error("散布小于噪声时应判定为不可分辨")
	}
	// 噪声 0.001 → 比值约 8，可分辨
	if !Judge(aggs, Noise{StdDev: 0.001}).Meaningful {
		t.Error("散布远大于噪声时应判定为有影响")
	}
}

// ---- 邻域 ----

func TestNeighbors(t *testing.T) {
	sets := []ParamSet{}
	id := int32(0)
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			sets = append(sets, ParamSet{
				ID: id,
				Values: map[string]any{
					"x": json.Number(itoa(a)), "y": json.Number(itoa(b)),
				},
			})
			id++
		}
	}
	g, err := BuildGrid(sets)
	if err != nil {
		t.Fatal(err)
	}
	// 中心点 (1,1) 的邻居是全部 9 个（含自己）
	center := int32(4)
	if n := len(g.Neighbors(center)); n != 9 {
		t.Errorf("3×3 网格中心的邻居应有 9 个（含自己），得到 %d", n)
	}
	// 角点 (0,0) 只有 4 个
	if n := len(g.Neighbors(0)); n != 4 {
		t.Errorf("角点的邻居应有 4 个，得到 %d", n)
	}
}

// TestBuildGridRejectsNonNumeric 非数值维度定义不了「邻居」。
func TestBuildGridRejectsNonNumeric(t *testing.T) {
	sets := []ParamSet{
		{ID: 0, Values: map[string]any{"base": "initial"}},
		{ID: 1, Values: map[string]any{"base": "equity"}},
	}
	if _, err := BuildGrid(sets); err == nil {
		t.Error("非数值维度应报错 —— 它没有「±1 格」这回事")
	}
}

// ---- 统计 ----

func TestQuantile(t *testing.T) {
	v := []float64{1, 2, 3, 4}
	cases := []struct {
		q, want float64
	}{{0, 1}, {0.5, 2.5}, {1, 4}, {0.25, 1.75}}
	for _, c := range cases {
		if got := quantile(v, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("quantile(%v) = %v，期望 %v", c.q, got, c.want)
		}
	}
}

// TestQuantileDoesNotMutate 分位数不得改动调用方的切片。
//
// 汇总时同一份 OOS 切片会被多次取分位，原地排序会让它悄悄变序 ——
// 而顺序在别处（例如按窗口对齐）是有意义的。
func TestQuantileDoesNotMutate(t *testing.T) {
	v := []float64{3, 1, 2}
	_ = quantile(v, 0.5)
	if v[0] != 3 || v[1] != 1 || v[2] != 2 {
		t.Errorf("quantile 改动了入参: %v", v)
	}
}

func TestPosRatio(t *testing.T) {
	if r := posRatio([]float64{1, -1, 1, 0}); math.Abs(r-0.5) > 1e-9 {
		t.Errorf("posRatio = %v，期望 0.5（0 不算正）", r)
	}
}

// ---- 门槛与排序 ----

func TestPassesGate(t *testing.T) {
	g := Gate{MinRoundTrips: 100}
	if (Result{RoundTrips: 99}).Passes(g) {
		t.Error("轮次不足应被拦下")
	}
	if !(Result{RoundTrips: 100}).Passes(g) {
		t.Error("轮次达标应放行")
	}
	if (Result{RoundTrips: 999, Err: "boom"}).Passes(g) {
		t.Error("跑失败的行不该过门槛")
	}
}

// TestScoreUsesExcessOnlyWithBenchmark 没有基准时不能拿 0 当超额。
//
// 宽基 ETF 最早 2012 年，Walk-Forward 的前几个窗口根本没有基准。
// 把「没有基准」当成「超额恰好为零」，那几个窗口的排序会完全失真。
func TestScoreUsesExcessOnlyWithBenchmark(t *testing.T) {
	noBench := Result{TotalReturn: 0.5, MaxDrawdown: 0.25}
	withBench := Result{TotalReturn: 0.5, ExcessReturn: 0.1,
		MaxDrawdown: 0.25, HasBenchmark: true}
	if got := noBench.Score("excess_over_maxdd"); math.Abs(got-2.0) > 1e-9 {
		t.Errorf("无基准时应退回总收益 ÷ 回撤 = 2.0，得到 %v", got)
	}
	if got := withBench.Score("excess_over_maxdd"); math.Abs(got-0.4) > 1e-9 {
		t.Errorf("有基准时应用超额 ÷ 回撤 = 0.4，得到 %v", got)
	}
}

// TestScoreDrawdownFloor 回撤趋零时分母不能爆掉。
func TestScoreDrawdownFloor(t *testing.T) {
	r := Result{TotalReturn: 0.01, MaxDrawdown: 0}
	if s := r.Score("excess_over_maxdd"); math.IsInf(s, 0) || math.IsNaN(s) {
		t.Errorf("回撤为 0 时分数不该是 %v", s)
	}
}

// ---- 小工具 ----

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(i int) string { return string(rune('0' + i)) }
