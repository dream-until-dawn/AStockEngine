package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/sweep"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// pp 把小数收益印成「个百分点」。
func pp(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%+.2f", v*100)
}

func ppAbs(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%.2f", v*100)
}

// report 按「先噪声、再判定、后区域」的顺序输出。
//
// **顺序就是结论的可信度链条**：不先量噪声，后面的排名就没有意义；
// 判定说没意义时，再漂亮的排名也是幻觉。
func report(rows []sweep.Result, sets []sweep.ParamSet, sc *sweep.Config) {
	fmt.Println()
	fmt.Println("================ 海选结果 ================")

	var failed, gated int
	gateBy := map[string]int{}
	for _, r := range rows {
		if r.Probe != 0 {
			continue
		}
		if r.Err != "" {
			failed++
		} else if r.Phase == 1 && !r.Passes(sc.Gate) {
			gated++
			gateBy[r.GateReason(sc.Gate)]++
		}
	}
	fmt.Printf("\n共 %d 行结果", len(rows))
	if failed > 0 {
		fmt.Printf("，其中 %d 次跑失败（已留在表里，不删）", failed)
	}
	if gated > 0 {
		fmt.Printf("，%d 次未过硬门槛（完整轮次 ≥ %d）", gated, sc.Gate.MinRoundTrips)
	}
	fmt.Println()
	printFailures(rows)
	printGateBreakdown(gateBy)
	printAttribution(rows, sc)

	// ---- 1. 噪声基线 ----
	n := sweep.MeasureNoise(rows)
	fmt.Println("\n=== 1. 噪声基线 ===")
	if n.Samples == 0 {
		fmt.Println("  ⚠ 没有噪声探针数据 —— 无法判断任何差异是不是真的。")
		fmt.Println("    在海选配置里给 noise_probe.points ≥ 2 且 repeats ≥ 2。")
	} else {
		fmt.Printf("  同一组参数、同一窗口，仅把初始资金扰动 ±%.2f%%，重复 %d 次：\n",
			sc.NoiseProbe.PerturbPct, n.Repeats)
		fmt.Printf("  收益标准差 %s 个百分点，极差 %s 个百分点（%d 组样本）\n",
			ppAbs(n.StdDev), ppAbs(n.Range), n.Samples)
		fmt.Println("  这是「什么都没改」时结果本身的抖动 —— 小于它的差异不可分辨。")
	}

	// ---- 2. 这次海选有没有意义 ----
	aggs := sweep.Aggregate(rows, sc.Gate, sc.Rank)
	v := sweep.Judge(aggs, n)
	var thin, total int
	for _, a := range aggs {
		total++
		if a.ThinCoverage {
			thin++
		}
	}
	if thin > 0 {
		// **必须说出来**：被剔掉的那些不是「差」，是「没跑够窗口」。
		// 不说的话，参与排名的组数会莫名其妙地少一大截。
		//
		// 实测这一条把散布削掉三成到六成 —— 也就是说，之前那部分
		// 「参数有影响」其实是一两个走运窗口贡献的
		fmt.Printf("\n  %d / %d 组参数活下来的窗口不足 %.0f%%，不参与排名 ——\n"+
			"  只在一两个窗口里过了门槛的话，它的 OOS 中位数就是那一两次的运气\n",
			thin, total, float64(sweep.MinWindowCoveragePPM)/10_000)
	}
	fmt.Println("\n=== 2. 参数到底有没有影响 ===")
	fmt.Printf("  全网格逐参数 OOS 中位数的标准差  %s 个百分点\n", ppAbs(v.Spread))
	fmt.Printf("  噪声基线                        %s 个百分点\n", ppAbs(v.Noise))
	if math.IsNaN(v.Ratio) {
		fmt.Println("  比值                            无法计算（没有噪声基线）")
	} else {
		fmt.Printf("  比值                            %.2f×（判定阈值 %.1f）\n",
			v.Ratio, sweep.MeaningfulThreshold)
	}
	if !v.Meaningful {
		fmt.Println("\n  ⛔ 结论：**这些参数在此区间内没有可辨别的影响。**")
		fmt.Println("     整张网格的散布与噪声同量级 —— 任何排名都是随机的，故不出排名。")
		fmt.Println("     这不是失败：便宜地知道「这条路走不通」，")
		fmt.Println("     比拿着一个不可信的第一名去实盘便宜得多。")
		fmt.Println("\n     可以试的方向：换取值范围、换标的池、换策略族，")
		fmt.Println("     或先把噪声压下去 —— 实测 A 股 slots 10→100，" +
			"极差从 8.99 降到 0.55 个百分点（v0.8 新口径下重测）。")
		printTopAnyway(aggs, sc, "上面已判定这个排名不可分辨")
		return
	}
	fmt.Println("\n  ✅ 参数确有影响，可以谈区域。")

	// ---- 3. 逐维边际 ----
	//
	// **放在高原之前**：高原要「邻居」，而邻居要求维度是有序的数。
	// 子树开关这类非数值轴会让几何反推整个失败，
	// 放在后面的话它们连一个数字都拿不到
	printAxisMargins(aggs, sets)

	// ---- 4. 高原 ----
	geom, err := sweep.BuildGrid(sets)
	if err != nil {
		fmt.Printf("\n  ⚠ 网格几何反推失败（%v）—— 跳过高原分析\n", err)
		return
	}
	crit := sweep.DefaultCriteria()
	ps := sweep.FindPlateaus(geom, aggs, n, crit, sc.Rank)
	fmt.Println("\n=== 3. 稳健参数区域 ===")
	fmt.Printf("  判据：邻居 ≥ %d、邻域 OOS 中位数 > 0、正的比例 ≥ %.0f%%、"+
		"IQR ≤ %.1f× 噪声基线\n", crit.MinNeighbors, crit.MinPosRatio*100,
		crit.MaxFlatVsNoise)
	if len(ps) == 0 {
		fmt.Println("\n  没有区域同时满足这几条。")
		fmt.Println("  **这是一个结论，不是一次失败** —— 说明网格里没有稳健的参数区域，")
		fmt.Println("  排名第一那组多半是尖峰。")
		printTopAnyway(aggs, sc, "没有一片区域够稳健，单点排名不可依赖")
		return
	}
	fmt.Printf("\n  找到 %d 片区域，按邻域 %s 中位数降序（前 %d 片）：\n\n",
		len(ps), sc.Rank, minInt(len(ps), 8))
	for i, p := range ps {
		if i >= 8 {
			break
		}
		fmt.Printf("  #%d  邻居 %d 个 / 样本 %d\n", i+1, p.Neighbors, p.Samples)
		fmt.Printf("      中心参数 %s\n", p.Params)
		fmt.Printf("      OOS 中位数 %s%%   四分位 [%s, %s]%%   正的比例 %.0f%%\n",
			pp(p.Median), pp(p.Q1), pp(p.Q3), p.PosRatio*100)
		fmt.Printf("      IQR %s 个百分点 = %.2f× 噪声基线 %s\n\n",
			ppAbs(p.IQR), p.FlatVsNoise, flatNote(p.FlatVsNoise))
	}
	fmt.Println("  **注意这里给的是区域不是排名。** 每片区域的中心参数只是坐标，")
	fmt.Println("  真正的结论是「这一带整体表现如何」—— 单点估计在噪声下没有意义。")
}

func flatNote(r float64) string {
	switch {
	case r <= 1.2:
		return "（≈1，这片是平的，好）"
	case r <= 2:
		return "（略有起伏）"
	default:
		return "（起伏明显，勉强算区域）"
	}
}

// printTopAnyway 即使结论是「不可分辨」，也把分数最高的几组印出来 ——
// 但**必须带着「不可分辨」的标签**，否则读的人还是会去用它。
func printTopAnyway(aggs map[int32]*sweep.ParamAgg, sc *sweep.Config, why string) {
	list := make([]*sweep.ParamAgg, 0, len(aggs))
	for _, a := range aggs {
		if a.Windows > 0 && !a.ThinCoverage {
			list = append(list, a)
		}
	}
	if len(list) == 0 {
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Score != list[j].Score {
			return list[i].Score > list[j].Score
		}
		return list[i].ParamID < list[j].ParamID
	})
	fmt.Printf("\n  （以下按分数排前 5，仅供参考 —— %s）\n", why)
	for i, a := range list {
		if i >= 5 {
			break
		}
		fmt.Printf("    %s  OOS 中位数 %s%%  正的比例 %.0f%%  %d 窗口\n",
			a.Params, pp(a.Median), a.PosRatio*100, a.Windows)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// printFailures 按原因把失败归类印出来。
//
// **只报一个「8262 次失败」等于什么都没说** —— 全批失败通常是同一个原因
// （一个写错的网格路径、一个装配不出来的组合），而那个原因就在
// 结果行的 Err 字段里躺着。不印出来的话，人得自己去翻 parquet。
func printFailures(rows []sweep.Result) {
	by := map[string]int{}
	for _, r := range rows {
		if r.Err != "" {
			by[r.Err]++
		}
	}
	if len(by) == 0 {
		return
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return by[keys[i]] > by[keys[j]] })

	fmt.Println()
	fmt.Println("=== 失败原因 ===")
	for i, k := range keys {
		if i >= 5 {
			fmt.Printf("  …… 另有 %d 种原因\n", len(keys)-5)
			break
		}
		msg := k
		if len(msg) > 220 {
			msg = msg[:220] + "…"
		}
		fmt.Printf("  %6d 次  %s\n", by[k], msg)
	}
}

// printGateBreakdown 把被硬门槛拦掉的按原因分开报。
//
// 只报一个总数的话，「是轮次太少还是强平太多」看不出来 ——
// 而这两者要采取的行动完全不同：前者该放宽门槛或换更长的窗口，
// 后者该降杠杆。
func printGateBreakdown(by map[string]int) {
	if len(by) <= 1 {
		return
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return by[keys[i]] > by[keys[j]] })
	fmt.Print("  门槛拦下的原因：")
	for i, k := range keys {
		if i > 0 {
			fmt.Print("    ")
		}
		fmt.Printf("%s %d", k, by[k])
	}
	fmt.Println()
}

// printAttribution 报「这个结果是不是策略挣来的」。
//
// 光看收益分不出四种情况：策略有边际、强平替你止损、熔断替你择时、
// 以及压根就是低摩擦低换手显得稳。这一段就是把它们分开。
func printAttribution(rows []sweep.Result, sc *sweep.Config) {
	var n, liq, halt, stop, vt int
	var fric, openc, edge float64
	var edgeN int
	for _, r := range rows {
		if r.Probe != 0 || r.Err != "" || r.Phase != 1 {
			continue
		}
		n++
		liq += int(r.Liquidations)
		halt += int(r.HaltExits)
		stop += int(r.StopExits)
		vt += int(r.VirtualTrips)
		fric += r.FrictionRatio
		openc += r.OpenCostRatio
		if r.VirtualTrips > 0 {
			edge += r.VirtualEdge
			edgeN++
		}
	}
	if n == 0 {
		return
	}
	fmt.Println("\n=== 结果从哪来（全部样本外窗口合计）===")
	fmt.Printf("  强平 %d 轮    熔断清仓 %d 轮    止损 %d 轮\n", liq, halt, stop)
	if liq > 0 {
		fmt.Println("  ⚠ 有强平 —— 高杠杆下它相当于一道很紧的止损。" +
			"收益里有多少是它砍出来的，收益本身答不了")
	}
	if halt > 0 {
		fmt.Println("  ⚠ 有熔断清仓 —— 那部分收益来自风控而不是信号，换个市场就没了")
	}
	fmt.Printf("  平均摩擦占初始资金 %.2f%%    平均未平仓开仓金额占比 %.2f%%\n",
		fric/float64(n)*100, openc/float64(n)*100)
	if edgeN > 0 {
		fmt.Printf("  有效性树：过滤掉 %d 轮，平均边际 %+.2f 个百分点"+
			"（实仓逐轮收益率 − 虚拟逐轮收益率）\n", vt, edge/float64(edgeN)*100)
		if edge < 0 {
			fmt.Println("  ⚠ 边际为负 —— 这棵有效性树挡掉的比它放行的更好，它在帮倒忙")
		}
	}
}

// printAxisMargins 逐维报「这个轴的每个取值，OOS 中位数是多少」。
//
// # 为什么需要它
//
// 高原分析要「邻居」，而邻居要求维度是**有序的数**。
// 子树开关（有效性树留 / 不留）、离场链的有无这类轴不是数 ——
// 高原分析对它们直接跳过，于是「这棵树到底值不值得留」这个问题
// 反而没人回答。
//
// 边际视图不需要有序：把参数按这一维的取值分组，各看各的中位数。
// 它答不了「哪片区域稳健」，但答得了「这个轴有没有用、往哪边用」。
func printAxisMargins(aggs map[int32]*sweep.ParamAgg, sets []sweep.ParamSet) {
	// 每组参数的取值表：param_id → {轴 → 取值}。
	// ParamSet.Values 就是这张表，不必再从 JSON 解一遍
	byID := map[int32]map[string]any{}
	axes := map[string]bool{}
	for _, s := range sets {
		byID[s.ID] = s.Values
		for k := range s.Values {
			axes[k] = true
		}
	}
	if len(axes) == 0 {
		return
	}
	names := make([]string, 0, len(axes))
	for k := range axes {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Println("\n=== 逐维边际（把参数按这一维分组，各看各的 OOS 中位数）===")
	fmt.Println("  它答不了「哪片区域稳健」，但答得了「这个轴有没有用、往哪边用」——")
	fmt.Println("  子树开关这类非数值轴只有这一个视图，高原分析对它们无能为力。")
	for _, ax := range names {
		groups := map[string][]float64{}
		for id, a := range aggs {
			if a.Windows == 0 || a.ThinCoverage {
				continue
			}
			vals, ok := byID[id]
			if !ok {
				continue
			}
			groups[axisLabel(vals[ax])] = append(groups[axisLabel(vals[ax])], a.Median)
		}
		if len(groups) < 2 {
			continue // 只有一个取值活下来，比不出东西
		}
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("\n  %s\n", ax)
		for _, k := range keys {
			v := groups[k]
			fmt.Printf("    %-28s OOS 中位数 %s%%   （%d 组）\n",
				k, pp(median(v)), len(v))
		}
	}
}

// axisLabel 把一个取值印成短标签。子树用「有 / 无」而不是整段 JSON ——
// 一棵树打印出来占三行，而这里要的是「哪一档」。
func axisLabel(v any) string {
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

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
