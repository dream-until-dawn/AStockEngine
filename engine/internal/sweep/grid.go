// Package sweep 实现策略海选（v0.5）。
//
// # 这一版真正要解决的不是「跑得快」
//
// 开工前实测：同一配置只把**初始资金改 ±0.1%**（100 万 ±1000 元），
// 跑 5 次的总收益是 −29.66% / −46.50% / −27.55% / −31.39% / −27.85%
// —— 极差 **18.95 个百分点**，标准差 7.09。
//
// **已在 v0.8 新口径下重测**（定量基准 cost、候选按成交额排、定量留摩擦）：
// A 股 slots=10 极差降到 **8.99 个百分点**、标准差 3.32；slots=100 则是
// 0.55 / 0.19。量级小了一半以上，但结论一条没变 ——
// 噪声仍然大到足以吞掉多数参数差异，而且仍然随 slots 急剧下降。
// 上面那组原始数字是当时那次测量的记录，保留原样。
//
// 1000 元经济上毫无意义，但它改变每一单的取整，进而改变哪些单能成交，
// 路径就此分叉。这就是**噪声基线**。
//
// 由此推出这个包的全部设计：
//
//   - 单点收益率不是可比较的量。输出必须是分布，不是一个数
//   - 任何差异旁边都要有噪声基线，小于基线的**明确标注「不可分辨」**
//   - 选邻域整体好的区域，不选排名第一 —— 第一名的领先量本来就在噪声里
//   - 先回答「这次海选有没有意义」：全网格散布 / 噪声基线 < 1.5 时，
//     结论是「该参数在此区间内无可辨别的影响」，不出排名
//
// 详见 docs/DESIGN-v0.5-selection.md。
package sweep

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/fingerprint"
)

// Config 是一次海选的配置。
type Config struct {
	Name string `json:"name"`
	// Base 基准回测配置的路径，相对本文件所在目录
	Base string `json:"base"`
	// Grid 参数网格：**点号路径 → 取值列表**。
	//
	// 值写**显式列表**而不是 [min,max,step]：`macd_cross` 的合法空间
	// 有 670 万组，全撒一定能找出一组「历史上完美」的参数，而它一文不值。
	// 先跑可辩护的粗网格，发现高原后再在高原附近加密。
	Grid map[string]json.RawMessage `json:"grid"`
	// Constraints 形如 "strategy.params.short < strategy.params.long"
	Constraints []string    `json:"constraints,omitempty"`
	WalkForward WalkForward `json:"walk_forward"`
	NoiseProbe  NoiseProbe  `json:"noise_probe"`
	Gate        Gate        `json:"gate"`
	// Rank 排序指标。默认 excess_over_maxdd ——
	// 按总收益排会选出高杠杆高回撤的东西
	Rank string `json:"rank,omitempty"`

	dir string
}

// WalkForward 滚动验证的切窗方式。
type WalkForward struct {
	ISYears   float64 `json:"is_years"`
	OOSYears  float64 `json:"oos_years"`
	StepYears float64 `json:"step_years"`
	// WarmupDays 预热前缀的交易日数。子集多裁这么长，
	// 引擎在 TradeFrom 之前只喂指标不交易 —— 否则每个窗口开头的
	// 指标未就绪，会在 18 个窗口上造成一致的偏差（那不是噪声，是偏差）
	WarmupDays int `json:"warmup_days"`
	// MinWindows 切出来的窗口数下限，少于它直接报错。0 表示用默认值 6。
	//
	// **不是保守，是这套方法论的前提**：Walk-Forward 的产出是 OOS 的
	// 中位数与四分位距，而三五个数算不出有意义的分布 ——
	// 报告照样会印出一个中位数，看不出它只基于 3 个窗口。
	//
	// 实测：加密数据 2019-11 起共 6.66 年，按 A 股那套 IS3/OOS1/步1
	// 只切得出 3 个窗口；换成 IS1.5/OOS0.5/步0.5 才有 10 个。
	MinWindows int `json:"min_windows,omitempty"`
	// Enabled 为 false 时整段跑一次，window 记 −1
	Enabled bool `json:"enabled"`
}

// NoiseProbe 噪声基线的测法。
//
// 不对每组参数都测（那是 Repeats 倍开销），取 Points 个代表点即可。
type NoiseProbe struct {
	Points  int `json:"points"`
	Repeats int `json:"repeats"`
	// PerturbPct 扰动幅度（%），作用在 portfolio.initial_cash_cents 上。
	//
	// 为什么是初始资金：要的是「经济上无意义、但会改变执行路径」的扰动。
	// 引擎里没有随机数（C5），种子无从扰动；改起始日会改变样本区间，
	// 那是真差异不是噪声；改滑点是改成本模型，属于参数。
	// 只有初始资金既不改规则也不改样本，只改每一单的取整。
	PerturbPct float64 `json:"perturb_pct"`
}

// Gate 硬门槛，不达标直接淘汰。
type Gate struct {
	// MinRoundTrips 完整轮次下限。样本不足时任何统计量都不可信
	MinRoundTrips int `json:"min_round_trips"`

	// 下面三条拦的是同一类自欺：**这个结果不是策略挣来的**。
	// 光看收益分不出「策略有边际」「强平替你止损」「熔断替你择时」
	// 「低摩擦低换手显得稳」这四种情况。

	// MaxLiquidationRatio 强平轮次占总轮次的上限（0 = 不限）。
	//
	// 高杠杆下强平相当于一道很紧的止损 —— 实测同一份配置杠杆 1 → 20，
	// 收益从 +115% 涨到 +202%，而强平从 0 次涨到 139 次。
	// 那不是杠杆挣的钱，是强平替你砍掉了亏损腿，换个行情就反过来
	MaxLiquidationRatio float64 `json:"max_liquidation_ratio,omitempty"`
	// MaxHaltExitRatio 熔断清仓轮次占总轮次的上限（0 = 不限）。
	// 收益来自风控而不是信号时，换个市场就没了
	MaxHaltExitRatio float64 `json:"max_halt_exit_ratio,omitempty"`
	// MaxFrictionRatio 摩擦占初始资金的上限（0 = 不限）。
	//
	// 实测 A 股两万本金切 10 份时它是 0.47 —— 那种组比的是费率不是策略
	MaxFrictionRatio float64 `json:"max_friction_ratio,omitempty"`
}

// LoadConfig 读一份海选配置。
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取海选配置 %s 失败: %w", path, err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("解析海选配置 %s 失败: %w", path, err)
	}
	c.dir = filepath.Dir(path)
	if c.Base == "" {
		return nil, fmt.Errorf("海选配置缺少 base")
	}
	if len(c.Grid) == 0 {
		return nil, fmt.Errorf("海选配置的 grid 为空 —— 没有要扫的维度")
	}
	if c.Rank == "" {
		c.Rank = "excess_over_maxdd"
	}
	if c.WalkForward.Enabled {
		if c.WalkForward.ISYears <= 0 || c.WalkForward.OOSYears <= 0 ||
			c.WalkForward.StepYears <= 0 {
			return nil, fmt.Errorf("walk_forward 的 is_years / oos_years / step_years 都必须为正")
		}
	}
	return &c, nil
}

// BasePath 返回基准配置的绝对路径。
func (c *Config) BasePath() string {
	if filepath.IsAbs(c.Base) {
		return c.Base
	}
	return filepath.Join(c.dir, c.Base)
}

// ---- 参数组 ----

// ParamSet 是网格上的一个点。
type ParamSet struct {
	// ID 本次海选内的序号。按规范化 JSON 排序后编号，
	// 同一份海选配置两次运行编号一致（C5）
	ID int32
	// FP 参数指纹，**跨海选稳定** —— 同一组参数在另一次海选里
	// 序号可能不同，指纹一定相同。结果表两个都存
	FP string
	// Values 点号路径 → 值
	Values map[string]any
	// JSON 规范化后的参数（键升序），供落盘与指纹
	JSON []byte
	// Config 应用到 base 之后的完整配置 JSON
	Config []byte
}

// Expand 展开参数网格。
//
// 三步：笛卡尔积 → 按 constraints 过滤 → 应用到 base 配置。
// **每一组都要能 Parse 通过**，调用方随后再过一遍 dryBuild ——
// 跑到第 137 组才发现参数名拼错，是这一版最不该出现的失败。
func (c *Config) Expand(base []byte) ([]ParamSet, error) {
	keys := make([]string, 0, len(c.Grid))
	for k := range c.Grid {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 顺序决定笛卡尔积的枚举顺序，必须确定（C5）

	axes := make([][]any, len(keys))
	for i, k := range keys {
		vs, err := expandValues(c.Grid[k])
		if err != nil {
			return nil, fmt.Errorf("grid[%q]: %w", k, err)
		}
		if len(vs) == 0 {
			return nil, fmt.Errorf("grid[%q] 没有取值", k)
		}
		axes[i] = vs
	}

	cons, err := parseConstraints(c.Constraints)
	if err != nil {
		return nil, err
	}

	var out []ParamSet
	idx := make([]int, len(keys))
	for {
		vals := make(map[string]any, len(keys))
		for i, k := range keys {
			vals[k] = axes[i][idx[i]]
		}
		if allSatisfied(cons, vals) {
			ps, err := makeParamSet(base, keys, vals)
			if err != nil {
				return nil, err
			}
			out = append(out, ps)
		}
		// 进位
		i := len(keys) - 1
		for i >= 0 {
			idx[i]++
			if idx[i] < len(axes[i]) {
				break
			}
			idx[i] = 0
			i--
		}
		if i < 0 {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("网格展开后一组都不剩 —— constraints 是不是写反了")
	}
	// 按规范化 JSON 定序后编号：同一份配置两次展开编号一致
	sort.Slice(out, func(i, j int) bool {
		return string(out[i].JSON) < string(out[j].JSON)
	})
	for i := range out {
		out[i].ID = int32(i)
	}
	return out, nil
}

func makeParamSet(base []byte, keys []string, vals map[string]any) (ParamSet, error) {
	obj, err := decodeJSONObject(base)
	if err != nil {
		return ParamSet{}, fmt.Errorf("基准配置不是合法 JSON 对象: %w", err)
	}
	for _, k := range keys {
		if err := setPath(obj, k, vals[k]); err != nil {
			return ParamSet{}, fmt.Errorf("grid[%q]: %w", k, err)
		}
	}
	cfgJSON, err := json.Marshal(obj)
	if err != nil {
		return ParamSet{}, err
	}
	// 参数 JSON 的键顺序由 map 的序列化保证升序（Go 的 encoding/json 如此），
	// 故它是规范形式，可直接做指纹
	pj, err := json.Marshal(vals)
	if err != nil {
		return ParamSet{}, err
	}
	return ParamSet{
		FP:     fingerprint.Short(fingerprint.Hex(pj)),
		Values: vals, JSON: pj, Config: cfgJSON,
	}, nil
}

// Validate 让每一组都真构造一遍引擎模块（dryBuild），跑之前就报错。
func Validate(sets []ParamSet, dir string) error {
	for _, ps := range sets {
		if _, err := config.Parse(ps.Config, dir); err != nil {
			return fmt.Errorf("参数组 %s %s：%w", ps.FP, ps.JSON, err)
		}
	}
	return nil
}

// ---- 取值展开 ----

// expandValues 解析一个维度的取值。
//
// 支持两种写法：显式列表 [5,8,12]，以及糖 {"from":5,"to":20,"step":3}。
// **文档默认写显式列表** —— 糖会让人不自觉地把范围拉满，
// 而全撒的网格一定能找出一组「历史上完美」的参数。
func expandValues(raw json.RawMessage) ([]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var list []any
		if err := decodeNumbers(raw, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var r struct {
		From *float64 `json:"from"`
		To   *float64 `json:"to"`
		Step *float64 `json:"step"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("既不是列表也不是 {from,to,step}: %w", err)
	}
	if r.From == nil || r.To == nil || r.Step == nil || *r.Step <= 0 {
		return nil, fmt.Errorf("{from,to,step} 三项都要有且 step 为正")
	}
	var out []any
	for v := *r.From; v <= *r.To+1e-9; v += *r.Step {
		out = append(out, json.Number(strconv.FormatFloat(v, 'g', -1, 64)))
		if len(out) > 10000 {
			return nil, fmt.Errorf("展开超过 10000 个取值 —— 这不像是有意的")
		}
	}
	return out, nil
}

func decodeNumbers(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber() // 保住整数字面量，别把 20200101 变成 2.0200101e+07
	return dec.Decode(v)
}

func decodeJSONObject(b []byte) (map[string]any, error) {
	var obj map[string]any
	if err := decodeNumbers(b, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// setPath 按点号路径写值。中间不是对象时报错而不是硬造 ——
// 路径写错时应当立刻知道，而不是在配置里多出一个没人读的字段。
// setPath 按点号路径往配置里写一个值。
//
// 支持数组下标：`strategy.params.indicators[0].params.short`。
//
// **规则树的参数全在数组里** —— 指标是一个列表、离场规则是一个列表。
// 只认对象的话，网格够不着规则树的任何一个参数，
// 而规则树正是现在的主力策略。
//
// 路径指不到就报错，**不新建字段** —— 新增多半是写错了名字，
// 而写错名字的网格会安安静静地扫出一批一模一样的结果。
func setPath(obj map[string]any, path string, val any) error {
	steps, err := parsePath(path)
	if err != nil {
		return err
	}
	var cur any = obj
	for i, st := range steps[:len(steps)-1] {
		cur, err = stepInto(cur, st)
		if err != nil {
			return fmt.Errorf("%q: %w", joinSteps(steps[:i+1]), err)
		}
	}
	return setLeaf(cur, steps[len(steps)-1], val, path)
}

// pathStep 路径上的一段：要么是字段名，要么是数组下标。
type pathStep struct {
	key   string
	index int
	isIdx bool
}

// parsePath 把 `a.b[2].c` 拆成 a / b / [2] / c。
func parsePath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, fmt.Errorf("路径为空")
	}
	var out []pathStep
	for _, seg := range strings.Split(path, ".") {
		name, rest, hasIdx := strings.Cut(seg, "[")
		if name != "" {
			out = append(out, pathStep{key: name})
		} else if !hasIdx {
			return nil, fmt.Errorf("路径 %q 里有空的一段", path)
		}
		for hasIdx {
			var num string
			num, rest, _ = strings.Cut(rest, "]")
			i, err := strconv.Atoi(num)
			if err != nil {
				return nil, fmt.Errorf("路径 %q 的下标 %q 不是整数", path, num)
			}
			if i < 0 {
				return nil, fmt.Errorf("路径 %q 的下标不能为负", path)
			}
			out = append(out, pathStep{index: i, isIdx: true})
			_, rest, hasIdx = strings.Cut(rest, "[")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("路径 %q 解析不出任何一段", path)
	}
	return out, nil
}

func joinSteps(steps []pathStep) string {
	var b strings.Builder
	for i, st := range steps {
		if st.isIdx {
			fmt.Fprintf(&b, "[%d]", st.index)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(st.key)
	}
	return b.String()
}

func stepInto(cur any, st pathStep) (any, error) {
	if st.isIdx {
		arr, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("不是数组，取不了下标")
		}
		if st.index >= len(arr) {
			return nil, fmt.Errorf("下标 %d 越界（长度 %d）", st.index, len(arr))
		}
		return arr[st.index], nil
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("不是对象，无法继续往下写")
	}
	v, ok := m[st.key]
	if !ok {
		return nil, fmt.Errorf("基准配置里没有这一段")
	}
	return v, nil
}

func setLeaf(cur any, st pathStep, val any, path string) error {
	if st.isIdx {
		arr, ok := cur.([]any)
		if !ok {
			return fmt.Errorf("%q 的父级不是数组", path)
		}
		if st.index >= len(arr) {
			return fmt.Errorf("%q 下标越界（长度 %d）", path, len(arr))
		}
		arr[st.index] = val
		return nil
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return fmt.Errorf("%q 的父级不是对象", path)
	}
	if _, ok := m[st.key]; !ok {
		return fmt.Errorf("基准配置里没有 %q —— "+
			"网格只改已有的字段，不新增（新增多半是写错了名字）", path)
	}
	m[st.key] = val
	return nil
}

// getPath 按点号路径读值。
func getPath(obj map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	cur := any(obj)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// ---- 约束 ----

type constraint struct {
	lhs, rhs string
	op       string
}

var constraintOps = []string{"<=", ">=", "!=", "==", "<", ">"}

func parseConstraints(ss []string) ([]constraint, error) {
	out := make([]constraint, 0, len(ss))
	for _, s := range ss {
		var found bool
		for _, op := range constraintOps {
			i := strings.Index(s, op)
			if i < 0 {
				continue
			}
			out = append(out, constraint{
				lhs: strings.TrimSpace(s[:i]),
				op:  op,
				rhs: strings.TrimSpace(s[i+len(op):]),
			})
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("约束 %q 里没有比较符（可用 %s）",
				s, strings.Join(constraintOps, " "))
		}
	}
	return out, nil
}

func allSatisfied(cs []constraint, vals map[string]any) bool {
	for _, c := range cs {
		l, okL := numOf(c.lhs, vals)
		r, okR := numOf(c.rhs, vals)
		if !okL || !okR {
			// 约束引用了不在网格里的路径 —— 无法判定，放行。
			// 报错更严格，但那会让「约束里写了一个固定值」这种合理写法失效
			continue
		}
		ok := false
		switch c.op {
		case "<":
			ok = l < r
		case "<=":
			ok = l <= r
		case ">":
			ok = l > r
		case ">=":
			ok = l >= r
		case "==":
			ok = l == r
		case "!=":
			ok = l != r
		}
		if !ok {
			return false
		}
	}
	return true
}

// numOf 把约束的一侧解成数：先当网格路径查，查不到再当字面量。
func numOf(tok string, vals map[string]any) (float64, bool) {
	if v, ok := vals[tok]; ok {
		return toFloat(v)
	}
	f, err := strconv.ParseFloat(tok, 64)
	return f, err == nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
