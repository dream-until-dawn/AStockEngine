package sweep

import (
	"fmt"
	"math"
	"sort"
)

// 这个文件回答三个问题，**顺序不能颠倒**：
//
//  1. 噪声基线是多少？（同一组参数，仅因无意义扰动能差多少）
//  2. 这次海选有没有意义？（全网格的散布是否明显大于噪声基线）
//  3. 如果有，哪片区域整体好？（不是「哪一组最好」）
//
// 先问 1 和 2 是因为：v0.3.4 实测噪声基线在 `slots=10` 时是 **18.95 个
// 百分点**。在那个量级下，「参数组 A 收益 12%、B 收益 5%，选 A」这句话
// 是纯粹的随机数。不先量噪声就排名，等于把噪声当成结论。

// ---- 网格几何 ----

// GridGeom 由参数组反推网格的坐标，用来找邻居。
type GridGeom struct {
	Keys []string
	// Axes 每维的取值，升序
	Axes [][]float64
	// Coord paramID → 各维下标
	Coord map[int32][]int
}

// BuildGrid 由展开后的参数组反推网格几何。
//
// 从结果反推而不是让 Expand 传出来：结果表里只有参数 JSON，
// 从 Parquet 读回来做分析时拿不到 Expand 的中间产物。
// 反推能让「分析」与「跑」完全解耦 —— 跑完之后随时可以换阈值重新分析。
func BuildGrid(sets []ParamSet) (*GridGeom, error) {
	if len(sets) == 0 {
		return nil, fmt.Errorf("没有参数组")
	}
	keys := make([]string, 0, len(sets[0].Values))
	for k := range sets[0].Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	uniq := make([]map[float64]bool, len(keys))
	for i := range uniq {
		uniq[i] = map[float64]bool{}
	}
	for _, s := range sets {
		for i, k := range keys {
			f, ok := toFloat(s.Values[k])
			if !ok {
				return nil, fmt.Errorf("维度 %q 的取值不是数（%v）—— "+
					"非数值维度无法定义「邻居」，也就谈不上高原", k, s.Values[k])
			}
			uniq[i][f] = true
		}
	}
	g := &GridGeom{Keys: keys, Coord: make(map[int32][]int, len(sets))}
	g.Axes = make([][]float64, len(keys))
	for i := range keys {
		ax := make([]float64, 0, len(uniq[i]))
		for v := range uniq[i] {
			ax = append(ax, v)
		}
		sort.Float64s(ax)
		g.Axes[i] = ax
	}
	for _, s := range sets {
		c := make([]int, len(keys))
		for i, k := range keys {
			f, _ := toFloat(s.Values[k])
			c[i] = sort.SearchFloat64s(g.Axes[i], f)
		}
		g.Coord[s.ID] = c
	}
	return g, nil
}

// Neighbors 返回与 id 的切比雪夫距离 ≤1 的全部参数组（**含自己**）。
func (g *GridGeom) Neighbors(id int32) []int32 {
	c, ok := g.Coord[id]
	if !ok {
		return nil
	}
	var out []int32
	for other, oc := range g.Coord {
		near := true
		for i := range c {
			if d := c[i] - oc[i]; d > 1 || d < -1 {
				near = false
				break
			}
		}
		if near {
			out = append(out, other)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---- 噪声基线 ----

// Noise 是噪声基线。
type Noise struct {
	// StdDev 同一组参数在无意义扰动下的收益标准差（小数，0.01 = 1pp）
	StdDev float64
	// Range 极差
	Range float64
	// Samples 参与统计的 (参数组, 窗口) 组数
	Samples int
	// Repeats 每组重复了几次
	Repeats int
}

// MeasureNoise 由探针行算出噪声基线。
//
// 探针行是同一组参数、同一个窗口，只把初始资金扰动 ±perturb% 的重复。
// 对每个 (参数组, 窗口) 算一次标准差，然后**取中位数**汇总 ——
// 用中位数不用均值：个别组的扰动可能碰上极端级联，
// 让它把整个基线抬起来，会使后面的「不可分辨」判定过于宽松。
func MeasureNoise(rows []Result) Noise {
	type key struct {
		param  int32
		window int16
	}
	groups := map[key][]float64{}
	for _, r := range rows {
		if r.Probe == 0 || r.Err != "" || r.Phase != phaseOOS {
			continue
		}
		k := key{r.ParamID, r.Window}
		groups[k] = append(groups[k], r.TotalReturn)
	}
	var stds, rngs []float64
	maxRep := 0
	for _, vs := range groups {
		if len(vs) < 2 {
			continue
		}
		stds = append(stds, stddev(vs))
		rngs = append(rngs, maxOf(vs)-minOf(vs))
		if len(vs) > maxRep {
			maxRep = len(vs)
		}
	}
	return Noise{
		StdDev: median(stds), Range: median(rngs),
		Samples: len(stds), Repeats: maxRep,
	}
}

const (
	phaseIS  int8 = 0
	phaseOOS int8 = 1
)

// ---- 逐参数汇总 ----

// ParamAgg 是一组参数在**全部 OOS 窗口**上的表现。
type ParamAgg struct {
	ParamID int32
	ParamFP string
	Params  string
	// OOS 各窗口的样本外收益
	OOS []float64
	// IS 各窗口的样本内收益，只用于看 IS/OOS 落差
	IS []float64

	Median   float64
	Mean     float64
	Q1, Q3   float64
	PosRatio float64
	// Score 排序分的中位数
	Score float64
	// Windows 有效窗口数；Failed 跑失败的窗口数
	Windows int
	Failed  int
	// Gated 未过硬门槛的窗口数
	Gated int
}

// Aggregate 按参数组汇总正式行（Probe==0）。
func Aggregate(rows []Result, gate Gate, rank string) map[int32]*ParamAgg {
	out := map[int32]*ParamAgg{}
	get := func(r Result) *ParamAgg {
		a, ok := out[r.ParamID]
		if !ok {
			a = &ParamAgg{ParamID: r.ParamID, ParamFP: r.ParamFP, Params: r.Params}
			out[r.ParamID] = a
		}
		return a
	}
	var scores map[int32][]float64 = map[int32][]float64{}
	for _, r := range rows {
		if r.Probe != 0 {
			continue
		}
		a := get(r)
		if r.Err != "" {
			a.Failed++
			continue
		}
		if r.Phase == phaseIS {
			a.IS = append(a.IS, r.TotalReturn)
			continue
		}
		if !r.Passes(gate) {
			a.Gated++
			continue
		}
		a.OOS = append(a.OOS, r.TotalReturn)
		scores[r.ParamID] = append(scores[r.ParamID], r.Score(rank))
	}
	for id, a := range out {
		a.Windows = len(a.OOS)
		if a.Windows == 0 {
			continue
		}
		sort.Float64s(a.OOS)
		a.Median = median(a.OOS)
		a.Mean = mean(a.OOS)
		a.Q1, a.Q3 = quantile(a.OOS, 0.25), quantile(a.OOS, 0.75)
		a.PosRatio = posRatio(a.OOS)
		a.Score = median(scores[id])
	}
	return out
}

// ---- 有没有意义 ----

// Verdict 回答「这次海选有没有意义」。
type Verdict struct {
	// Spread 全网格的逐参数 OOS 中位数的标准差
	Spread float64
	Noise  float64
	// Ratio = Spread / Noise
	Ratio float64
	// Meaningful 为 false 时**不该出高原排名** ——
	// 整张网格都是同一片平地，「找到了高原」是幻觉
	Meaningful bool
	Params     int
}

// MeaningfulThreshold 判定阈值。
//
// 1.5 是个保守的选择：散布只有噪声的 1.5 倍时，
// 排名前后几名之间的差距几乎必然在噪声之内。
const MeaningfulThreshold = 1.5

// Judge 先回答「参数到底有没有影响」，再决定要不要谈高原。
func Judge(aggs map[int32]*ParamAgg, n Noise) Verdict {
	meds := make([]float64, 0, len(aggs))
	for _, a := range aggs {
		if a.Windows > 0 {
			meds = append(meds, a.Median)
		}
	}
	v := Verdict{Spread: stddev(meds), Noise: n.StdDev, Params: len(meds)}
	if n.StdDev > 0 {
		v.Ratio = v.Spread / n.StdDev
		v.Meaningful = v.Ratio >= MeaningfulThreshold
	} else {
		// 没测噪声就没法判断。**不能默认「有意义」** ——
		// 那正是这一版要防的自欺
		v.Ratio = math.NaN()
		v.Meaningful = false
	}
	return v
}

// ---- 高原 ----

// Plateau 是一片邻域整体表现良好的区域。
type Plateau struct {
	CenterID int32
	Params   string
	// Neighbors 邻居数（含自己）
	Neighbors int
	// Median / Q1 / Q3 在**邻域的全部 (邻居 × 窗口) 样本**上算
	Median   float64
	Q1, Q3   float64
	IQR      float64
	PosRatio float64
	Samples  int
	// Score 邻域内 Score 的中位数，排序用
	Score float64
	// FlatVsNoise = IQR / 噪声基线。**≈1 是好事** ——
	// 说明邻居之间的差异不超过噪声，这片区域是平的。
	// 远大于 1 说明邻居之间有真实差异，那就不是高原
	FlatVsNoise float64
}

// PlateauCriteria 高原判据。
type PlateauCriteria struct {
	MinNeighbors int
	MinPosRatio  float64
	// MaxFlatVsNoise IQR 相对噪声基线的上限
	MaxFlatVsNoise float64
}

// DefaultCriteria 见 DESIGN-v0.5-selection.md §6.2。
func DefaultCriteria() PlateauCriteria {
	return PlateauCriteria{MinNeighbors: 6, MinPosRatio: 0.6, MaxFlatVsNoise: 3.0}
}

// FindPlateaus 找出满足判据的区域，按邻域 Score 中位数降序。
//
// **不返回 top-1 排名。** 返回的每一项都是一片区域，
// 且带着它的邻域分布（中位数、四分位、正的比例）——
// 单点估计在 19 个百分点的噪声下没有意义。
func FindPlateaus(
	g *GridGeom, aggs map[int32]*ParamAgg, n Noise, c PlateauCriteria, rank string,
) []Plateau {
	var out []Plateau
	for id, a := range aggs {
		if a.Windows == 0 {
			continue
		}
		nb := g.Neighbors(id)
		var pool, scores []float64
		valid := 0
		for _, other := range nb {
			oa, ok := aggs[other]
			if !ok || oa.Windows == 0 {
				continue
			}
			valid++
			pool = append(pool, oa.OOS...)
			scores = append(scores, oa.Score)
		}
		if valid < c.MinNeighbors || len(pool) == 0 {
			continue
		}
		sort.Float64s(pool)
		p := Plateau{
			CenterID: id, Params: a.Params, Neighbors: valid,
			Median: median(pool), Q1: quantile(pool, 0.25), Q3: quantile(pool, 0.75),
			PosRatio: posRatio(pool), Samples: len(pool), Score: median(scores),
		}
		p.IQR = p.Q3 - p.Q1
		if n.StdDev > 0 {
			p.FlatVsNoise = p.IQR / n.StdDev
		}
		if p.Median <= 0 || p.PosRatio < c.MinPosRatio {
			continue
		}
		if n.StdDev > 0 && p.FlatVsNoise > c.MaxFlatVsNoise {
			continue // 邻居之间差异远大于噪声 —— 不是平地，是斜坡
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].CenterID < out[j].CenterID // 定序，供 C5
	})
	return out
}

// ---- 统计小工具 ----
//
// 自己写而不引第三方：就这几个函数，且**中位数与分位数的定义要固定**
// —— 不同库的分位数插值方式不同，换库会让阈值悄悄失效。

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func stddev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := mean(v)
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v))) // 总体标准差
}

func median(v []float64) float64 { return quantile(v, 0.5) }

// quantile 线性插值。**输入会被排序**，调用方若在意顺序须自行拷贝。
func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return s[lo]
	}
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func posRatio(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	n := 0
	for _, x := range v {
		if x > 0 {
			n++
		}
	}
	return float64(n) / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func minOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}
