package sweep

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 这个文件把「一次海选读出来是什么结论」算成一个结构体。
//
// # 为什么要有它
//
// 结论要给两个地方看：命令行报告与 Web 视图。而这两处**各算一遍**的话，
// 迟早有一处口径不同 —— 这个项目里同一张表写四遍的教训已经吃过一次
// （开平方向那张表，见 trading.LegOf）。
//
// 所以：数字在这里算一次，命令行负责排版，Web 负责画图，两边都不再算。

// Manifest 是一次海选的自描述文件，与 parquet 分片放在同一个目录。
//
// **没有它，结果目录读不懂**：分析要用到硬门槛、排序口径与参数集，
// 而这些只在海选配置里，不在结果行里。命令行靠 `-config` 再传一遍，
// Web 没有那个机会 —— 它只知道目录名。
type Manifest struct {
	SweepID string `json:"sweep_id"`
	Name    string `json:"name"`
	// Base 基准配置的路径，供「复现这一行」时指回去
	Base string `json:"base"`
	// Config 海选配置原样。含 grid / gate / rank / walk_forward
	Config *Config `json:"config"`
	// Params 展开后的参数集：序号 → 各维取值。
	// BuildGrid 与逐维边际都要它，而它从结果行反推不回来
	// （结果行只有 JSON 字符串，维度顺序与类型都丢了）
	Params []ManifestParam `json:"params"`
	// Windows 切出来的窗口
	Windows []Window `json:"windows"`
	// AnnualDays 年化系数，按市场取出来的那个
	AnnualDays float64 `json:"annual_days"`
	CreatedAt  string  `json:"created_at"`
}

// ManifestParam 一组参数在清单里的样子。
type ManifestParam struct {
	ID     int32          `json:"id"`
	FP     string         `json:"fp"`
	Values map[string]any `json:"values"`
}

const manifestName = "manifest.json"

// WriteManifest 把清单写进结果目录。
func WriteManifest(dir string, m *Manifest) error {
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化海选清单失败: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestName), b, 0o644)
}

// ReadManifest 读回清单。目录里没有清单时返回 nil, nil ——
// **不当成错误**：v0.5.1 之前跑出来的结果目录没有清单，
// 它们仍然能被列出来，只是分析不了。
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("解析海选清单失败: %w", err)
	}
	return &m, nil
}

// ParamSets 把清单里的参数集还原成 ParamSet（BuildGrid 要这个类型）。
func (m *Manifest) ParamSets() []ParamSet {
	out := make([]ParamSet, 0, len(m.Params))
	for _, p := range m.Params {
		out = append(out, ParamSet{ID: p.ID, FP: p.FP, Values: p.Values})
	}
	return out
}

// ---- 分析结果 ----

// Analysis 是一次海选的全部结论，按「可信度链条」的顺序排列。
//
// 字段顺序就是该读的顺序：先看这次海选有没有意义（Noise + Verdict），
// 再看轴（Margins），最后才谈区域（Plateaus）。
type Analysis struct {
	SweepID string `json:"sweepId"`
	Name    string `json:"name"`

	Rows    int `json:"rows"`
	Failed  int `json:"failed"`
	Gated   int `json:"gated"`
	Params  int `json:"params"`
	Windows int `json:"windows"`

	// FailBy / GateBy 失败与被门槛拦下的原因分布
	FailBy map[string]int `json:"failBy,omitempty"`
	GateBy map[string]int `json:"gateBy,omitempty"`

	// ThinParams 因窗口覆盖不足退出排名的组数。
	// ThinWarn 为真时这个比例高到会让下面每个数字都不可靠
	ThinParams int  `json:"thinParams"`
	ThinWarn   bool `json:"thinWarn"`

	Noise   Noise   `json:"noise"`
	Verdict Verdict `json:"verdict"`

	// Attribution 「这个结果是不是策略挣来的」
	Attribution Attribution `json:"attribution"`

	// Margins 逐维边际。**非数值轴只有这一个视图**
	Margins []AxisMargin `json:"margins"`

	// Plateaus 稳健区域。为空时 PlateauNote 说明为什么
	Plateaus    []Plateau `json:"plateaus"`
	PlateauNote string    `json:"plateauNote,omitempty"`

	// Top 分数最高的几组。**只在没有区域时才该看它**，
	// 而且要带着「单点排名不可依赖」的标签
	Top []TopParam `json:"top"`
}

// Attribution 收益从哪来。
type Attribution struct {
	Liquidations int `json:"liquidations"`
	HaltExits    int `json:"haltExits"`
	StopExits    int `json:"stopExits"`
	// AvgFrictionRatio / AvgOpenCostRatio 摩擦与未平仓开仓金额占初始资金
	AvgFrictionRatio float64 `json:"avgFrictionRatio"`
	AvgOpenCostRatio float64 `json:"avgOpenCostRatio"`
	// VirtualTrips 被有效性树过滤掉的轮次；
	// AvgVirtualEdge 实仓逐轮收益率 − 虚拟逐轮收益率，为正才说明那棵树该留
	VirtualTrips   int     `json:"virtualTrips"`
	AvgVirtualEdge float64 `json:"avgVirtualEdge"`
	HasVirtual     bool    `json:"hasVirtual"`
}

// AxisMargin 一个维度上，各取值分别对应的 OOS 中位数。
type AxisMargin struct {
	Axis   string       `json:"axis"`
	Values []MarginItem `json:"values"`
	// Spread 该维最好与最差取值之间的差（个百分点的小数形式）。
	// 与噪声基线比才有意义
	Spread float64 `json:"spread"`
	// Inert 跨度小于噪声基线 —— 这个轴分辨不出来
	Inert bool `json:"inert"`
}

// MarginItem 一个取值。
type MarginItem struct {
	Label  string  `json:"label"`
	Median float64 `json:"median"`
	Count  int     `json:"count"`
}

// TopParam 排行榜上的一行。
type TopParam struct {
	ParamID int32   `json:"paramId"`
	Params  string  `json:"params"`
	Median  float64 `json:"median"`
	Q1      float64 `json:"q1"`
	Q3      float64 `json:"q3"`
	// PosRatio 正收益窗口占比；Windows 有效窗口数
	PosRatio float64 `json:"posRatio"`
	Windows  int     `json:"windows"`
	// OOS 各窗口的样本外收益，供画箱线图 / 逐窗展开
	OOS []float64 `json:"oos"`
}

// Analyze 把结果行算成结论。
//
// 顺序即可信度链条：不先量噪声，后面的排名就没有意义；
// 判定说没意义时，再漂亮的排名也是幻觉。
func Analyze(rows []Result, sets []ParamSet, sc *Config, sweepID, name string) Analysis {
	a := Analysis{
		SweepID: sweepID, Name: name, Rows: len(rows),
		FailBy: map[string]int{}, GateBy: map[string]int{},
		// 空切片而不是 nil —— nil 序列化成 JSON null，
		// 前端每处都得先判空，漏一处就是一个运行时错误
		Plateaus: []Plateau{}, Margins: []AxisMargin{}, Top: []TopParam{},
	}
	wins := map[int16]bool{}
	for _, r := range rows {
		if r.Probe != 0 {
			continue
		}
		if r.Phase == phaseOOS {
			wins[r.Window] = true
		}
		if r.Err != "" {
			a.Failed++
			a.FailBy[r.Err]++
			continue
		}
		if r.Phase == phaseOOS && !r.Passes(sc.Gate) {
			a.Gated++
			a.GateBy[r.GateReason(sc.Gate)]++
		}
	}
	a.Windows = len(wins)

	a.Noise = MeasureNoise(rows)
	aggs := Aggregate(rows, sc.Gate, sc.Rank)
	a.Params = len(aggs)
	for _, g := range aggs {
		if g.ThinCoverage {
			a.ThinParams++
		}
	}
	// 超过一半退出排名时，下面每个数字都建在一个偏窄的子集上 ——
	// 实测这个坑造出过一个完全相反的结论，而且没有任何地方报错
	a.ThinWarn = a.Params > 0 && float64(a.ThinParams)/float64(a.Params) > 0.5
	a.Verdict = Judge(aggs, a.Noise)
	a.Attribution = attribution(rows)
	if ms := axisMargins(aggs, sets, a.Noise); ms != nil {
		a.Margins = ms
	}
	if tp := topParams(aggs, 10); tp != nil {
		a.Top = tp
	}

	// 判定「不可分辨」时**不出区域** —— 排名是随机的，画出来只会被当真
	if !a.Verdict.Meaningful {
		a.PlateauNote = "已判定参数不可分辨，故不做高原分析"
		return a
	}
	geom, err := BuildGrid(sets)
	if err != nil {
		a.PlateauNote = err.Error()
		return a
	}
	// **不能直接赋值** —— FindPlateaus 没找到时返回 nil，
	// 会把上面初始化的空切片盖掉，序列化又变回 JSON null。
	// 初始化非 nil 只在「没人再赋值」时才有效，这就是那个反例
	if ps := FindPlateaus(geom, aggs, a.Noise, DefaultCriteria(), sc.Rank); ps != nil {
		a.Plateaus = ps
	}
	if len(a.Plateaus) == 0 {
		a.PlateauNote = "没有区域同时满足全部判据 —— " +
			"说明网格里没有稳健的参数区域，排名第一那组多半是尖峰"
	}
	return a
}

func attribution(rows []Result) Attribution {
	var at Attribution
	var n, edgeN int
	var fric, openc, edge float64
	for _, r := range rows {
		if r.Probe != 0 || r.Err != "" || r.Phase != phaseOOS {
			continue
		}
		n++
		at.Liquidations += int(r.Liquidations)
		at.HaltExits += int(r.HaltExits)
		at.StopExits += int(r.StopExits)
		at.VirtualTrips += int(r.VirtualTrips)
		fric += r.FrictionRatio
		openc += r.OpenCostRatio
		if r.VirtualTrips > 0 {
			edge += r.VirtualEdge
			edgeN++
		}
	}
	if n > 0 {
		at.AvgFrictionRatio = fric / float64(n)
		at.AvgOpenCostRatio = openc / float64(n)
	}
	if edgeN > 0 {
		at.AvgVirtualEdge = edge / float64(edgeN)
		at.HasVirtual = true
	}
	return at
}

// axisMargins 逐维边际：把参数按某一维的取值分组，各看各的 OOS 中位数。
//
// 高原要「邻居」，邻居要求维度是有序的数。子树开关这类非数值轴
// 会让几何反推整个失败 —— 边际视图不需要有序，是它们唯一的视图。
func axisMargins(aggs map[int32]*ParamAgg, sets []ParamSet, n Noise) []AxisMargin {
	byID := make(map[int32]map[string]any, len(sets))
	axes := map[string]bool{}
	for _, s := range sets {
		byID[s.ID] = s.Values
		for k := range s.Values {
			axes[k] = true
		}
	}
	names := make([]string, 0, len(axes))
	for k := range axes {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]AxisMargin, 0, len(names))
	for _, ax := range names {
		groups := map[string][]float64{}
		for id, g := range aggs {
			if g.Windows == 0 || g.ThinCoverage {
				continue
			}
			vals, ok := byID[id]
			if !ok {
				continue
			}
			lbl := AxisLabel(vals[ax])
			groups[lbl] = append(groups[lbl], g.Median)
		}
		if len(groups) < 2 {
			continue // 只有一个取值活下来，比不出东西
		}
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		m := AxisMargin{Axis: ax}
		lo, hi := 0.0, 0.0
		for i, k := range keys {
			med := medianOf(groups[k])
			m.Values = append(m.Values, MarginItem{Label: k, Median: med, Count: len(groups[k])})
			if i == 0 || med < lo {
				lo = med
			}
			if i == 0 || med > hi {
				hi = med
			}
		}
		m.Spread = hi - lo
		// 跨度小于噪声基线 = 这个轴分辨不出来。
		// **实测这条能直接答「止损值不值得留」**：止损触发 6,596 次，
		// 而有无之间只差 0.18 个百分点，低于噪声 0.60
		m.Inert = n.StdDev > 0 && m.Spread < n.Range
		out = append(out, m)
	}
	return out
}

// AxisLabel 把一个取值印成短标签。
//
// 子树用「有 / 无」而不是整段 JSON —— 一棵树打印出来占三行，
// 而这里要的是「哪一档」。
func AxisLabel(v any) string {
	switch t := v.(type) {
	case nil:
		return "（无）"
	case map[string]any:
		if r, ok := t["right"].(map[string]any); ok {
			if val, ok := r["value"]; ok {
				return fmt.Sprintf("有 · 阈值 %v", val)
			}
		}
		return "有"
	case []any:
		if len(t) == 0 {
			return "（空链）"
		}
		return fmt.Sprintf("%d 条", len(t))
	}
	return fmt.Sprint(v)
}

func topParams(aggs map[int32]*ParamAgg, k int) []TopParam {
	list := make([]*ParamAgg, 0, len(aggs))
	for _, a := range aggs {
		if a.Windows > 0 && !a.ThinCoverage {
			list = append(list, a)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Score != list[j].Score {
			return list[i].Score > list[j].Score
		}
		return list[i].ParamID < list[j].ParamID
	})
	out := make([]TopParam, 0, k)
	for i, a := range list {
		if i >= k {
			break
		}
		out = append(out, TopParam{
			ParamID: a.ParamID, Params: a.Params,
			Median: a.Median, Q1: a.Q1, Q3: a.Q3,
			PosRatio: a.PosRatio, Windows: a.Windows,
			OOS: append([]float64(nil), a.OOS...),
		})
	}
	return out
}

func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// ListSweeps 扫结果根目录，返回各次海选的清单（新的在前）。
//
// 没有清单的目录也列出来 —— v0.5.1 之前跑的结果没有清单，
// 藏起来只会让人以为数据丢了。
func ListSweeps(root string) ([]*Manifest, error) {
	dir := filepath.Join(root, "results")
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range ents {
		if !e.IsDir() || len(e.Name()) < 7 || e.Name()[:6] != "sweep=" {
			continue
		}
		id := e.Name()[6:]
		m, err := ReadManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if m == nil {
			m = &Manifest{SweepID: id, Name: "（无清单，v0.5.1 之前跑的）"}
		}
		m.SweepID = id
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
