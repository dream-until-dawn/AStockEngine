// Command sweep 跑一次策略海选（v0.5）。
//
//	go run ./cmd/sweep -config ../configs/sweep/macd_coarse.json -workers 8
//
// # 它输出的不是排名
//
// 实测：同一配置只把初始资金改 ±0.1%（100 万 ±1000 元），
// 五次的总收益是 −29.66% / −46.50% / −27.55% / −31.39% / −27.85%
// —— 极差 18.95 个百分点。在这个量级下，
// 「参数组 A 收益 12%、B 收益 5%，选 A」是纯粹的随机数。
//
// **已在 v0.8 新口径下重测**（定量基准 cost、候选按成交额排、定量留摩擦）：
// A 股 slots=10 极差降到 **8.99 个百分点**、标准差 3.32；slots=100 则是
// 0.55 / 0.19。量级小了一半以上，但结论一条没变 ——
// 噪声仍然大到足以吞掉多数参数差异，而且仍然随 slots 急剧下降。
// 上面那组原始数字是当时那次测量的记录，保留原样。
//
// 所以本程序**先量噪声，再判断这次海选有没有意义，最后才谈区域**：
//
//  1. 噪声基线：几个代表点各跑几次无意义扰动
//  2. 判定：全网格散布 / 噪声基线 < 1.5 → 结论是「参数无可辨别影响」，
//     不出排名
//  3. 若有意义：给出**邻域整体好的区域**及其分布，不给 top-1
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/fingerprint"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/sweep"

	// 策略与交易模块靠 init() 注册，不导入就一个都认不出来
	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

func main() {
	cfgPath := flag.String("config", "", "海选配置路径（必填）")
	workers := flag.Int("workers", 0, "并发度，0=CPU 核数−2")
	resume := flag.Bool("resume", true, "跳过已经跑完的窗口")
	dryRun := flag.Bool("dry-run", false, "只展开网格并预估耗时，不跑")
	// 分析与跑完全解耦：高原判据里的阈值是会调的，
	// 每调一次就重跑 8,000 次回测显然不行
	reportOnly := flag.String("report-only", "", "只读已有结果重新分析（给 sweep_id）")
	flag.Parse()
	if *cfgPath == "" {
		fatal(fmt.Errorf("需要 -config"))
	}

	sc, err := sweep.LoadConfig(*cfgPath)
	if err != nil {
		fatal(err)
	}
	baseCfg, err := config.Load(sc.BasePath())
	if err != nil {
		fatal(err)
	}
	baseDir := filepath.Dir(sc.BasePath())
	baseJSON, err := os.ReadFile(sc.BasePath())
	if err != nil {
		fatal(err)
	}

	fmt.Printf("海选 %s\n基准配置 %s\n\n", sc.Name, sc.BasePath())

	if *reportOnly != "" {
		sets, err := sc.Expand(baseJSON)
		if err != nil {
			fatal(err)
		}
		dir := sweep.ResultDir(baseCfg.DataRoot(), *reportOnly)
		rows, err := sweep.ReadAll(dir)
		if err != nil {
			fatal(fmt.Errorf("读取 %s 失败：%w", dir, err))
		}
		fmt.Printf("只分析不跑：从 %s 读回 %d 行\n", dir, len(rows))
		report(rows, sets, sc)
		return
	}

	// ---- 展开与预检 ----
	sets, err := sc.Expand(baseJSON)
	if err != nil {
		fatal(err)
	}
	// 每一组都真构造一遍模块。跑到第 137 组才发现参数名拼错，
	// 是这一版最不该出现的失败
	if err := sweep.Validate(sets, baseDir); err != nil {
		fatal(fmt.Errorf("网格预检失败：%w", err))
	}
	fmt.Printf("参数网格 %d 组（已逐组预检通过）\n", len(sets))
	for _, k := range gridKeys(sets) {
		fmt.Printf("  %-32s %v\n", k, distinct(sets, k))
	}

	// ---- 数据 ----
	t0 := time.Now()
	ds, err := config.LoadDataSet(baseCfg)
	if err != nil {
		fatal(err)
	}
	loadDur := time.Since(t0)
	days := sweep.TradingDays(ds.Columns)
	// 年化系数**必须问市场**，不能写死查 A 股日历。
	//
	// 加密在 calendar 表里没有行，查出来是 A 股的 242.44，而正确值是 365 ——
	// 相差 50%，切窗的年数、OOS 的年化收益全都跟着错，且不报任何错。
	// 这与 ComputeMetrics 里那条是同一个坑，海选这边有自己的一份拷贝。
	annual := 243.0
	if mkt, err := trading.Markets.Build(baseCfg.Market.Impl, baseCfg.Market.Params); err == nil {
		annual = mkt.AnnualDays(ds.Calendar, days[0], days[len(days)-1])
	} else if ds.Calendar != nil {
		annual = ds.Calendar.TradingDaysPerYear(
			mktdata.MarketAShare, days[0], days[len(days)-1])
	}
	fmt.Printf("\n数据 %d 只 / %d 行 / %d 个交易日 %d~%d / 载入 %v\n",
		ds.Stats.Instruments, ds.Stats.Rows, len(days),
		days[0], days[len(days)-1], loadDur.Round(time.Millisecond))

	windows, err := sweep.MakeWindows(days, sc.WalkForward, annual)
	if err != nil {
		fatal(err)
	}
	if len(windows) == 0 {
		// 整段跑：Walk-Forward 关掉时也要能用，一刀落地就有产出
		windows = []sweep.Window{{
			Index: -1, DataFrom: days[0], ISFrom: 0, ISTo: 0,
			OOSFrom: days[0], OOSTo: days[len(days)-1],
		}}
		fmt.Println("Walk-Forward 未开启 —— 整段跑一次，window 记 −1")
	} else {
		fmt.Printf("Walk-Forward %d 个窗口（IS %.2f 年 / OOS %.2f 年 / 步进 %.2f 年，"+
			"年化系数 %.2f 交易日）\n", len(windows),
			sc.WalkForward.ISYears, sc.WalkForward.OOSYears,
			sc.WalkForward.StepYears, annual)
		fmt.Printf("  窗口 0  IS %d~%d  OOS %d~%d\n",
			windows[0].ISFrom, windows[0].ISTo, windows[0].OOSFrom, windows[0].OOSTo)
		last := windows[len(windows)-1]
		fmt.Printf("  窗口 %d  IS %d~%d  OOS %d~%d\n",
			last.Index, last.ISFrom, last.ISTo, last.OOSFrom, last.OOSTo)
	}

	// ---- 规模 ----
	probes := probePoints(sets, sc.NoiseProbe)
	perWindow := len(sets)
	if sc.WalkForward.Enabled {
		perWindow *= 2 // IS + OOS
	}
	total := perWindow*len(windows) + len(probes)*sc.NoiseProbe.Repeats*len(windows)
	nw := *workers
	if nw <= 0 {
		nw = runtime.NumCPU() - 2
		if nw < 1 {
			nw = 1
		}
	}
	fmt.Printf("\n合计 %d 次回测（正式 %d + 噪声探针 %d），%d 并发\n",
		total, perWindow*len(windows), len(probes)*sc.NoiseProbe.Repeats*len(windows), nw)
	if *dryRun {
		fmt.Println("\n-dry-run：只展开不跑。")
		return
	}

	sweepID := fingerprint.Short(fingerprint.Hex(mustJSON(sc), baseJSON))
	outDir := sweep.ResultDir(ds.Root, sweepID)
	fmt.Printf("结果目录 %s\n\n", outDir)

	dataFP, _, err := fingerprint.Data(baseCfg.DataRoot())
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ 数据指纹算不出来（%v）—— 结果表里的 input_fp 会是空的\n", err)
	}

	done := map[int16]bool{}
	if *resume {
		done = sweep.DoneWindows(outDir)
		if len(done) > 0 {
			fmt.Printf("续跑：已完成 %d 个窗口，跳过\n\n", len(done))
		}
	}

	// ---- 按窗口分批 ----
	//
	// **窗口串行、参数组并发**，不能反过来。反过来会让每个 worker
	// 各自持有一份不同区间的子集（实测 358 MB），8 个就是 2.8 GB。
	runStart := time.Now()
	for _, w := range windows {
		if done[w.Index] {
			continue
		}
		jobs := buildJobs(sets, probes, w, sc)
		wds := narrowFor(ds, baseCfg, w)

		t := time.Now()
		var fails int
		rows := sweep.RunJobs(wds, jobs, baseDir, nw, sweepID, dataFP,
			func(n, tot int, r sweep.Result) {
				if r.Err != "" {
					fails++
				}
				if n%50 == 0 || n == tot {
					fmt.Printf("\r  窗口 %-3d  %d/%d  %.0f%%  失败 %d      ",
						w.Index, n, tot, float64(n)/float64(tot)*100, fails)
				}
			})
		name := fmt.Sprintf("window-%03d.parquet", w.Index+1) // −1 → 000
		if _, err := sweep.WritePart(outDir, name, rows); err != nil {
			fatal(err)
		}
		fmt.Printf("\r  窗口 %-3d  %d 次  %v  失败 %d            \n",
			w.Index, len(rows), time.Since(t).Round(time.Millisecond), fails)
	}
	fmt.Printf("\n全部跑完，耗时 %v\n", time.Since(runStart).Round(time.Second))

	// ---- 分析 ----
	rows, err := sweep.ReadAll(outDir)
	if err != nil {
		fatal(err)
	}
	report(rows, sets, sc)
}

// buildJobs 展开一个窗口内的全部 Job。
func buildJobs(
	sets []sweep.ParamSet, probes []sweep.ParamSet, w sweep.Window, sc *sweep.Config,
) []sweep.Job {
	jobs := make([]sweep.Job, 0, len(sets)*2+len(probes)*sc.NoiseProbe.Repeats)
	add := func(ps sweep.ParamSet, phase, probe int8, from, to, tradeFrom int32, cash int64) {
		jobs = append(jobs, sweep.Job{
			Param: ps, Window: w.Index, Phase: phase, Probe: probe,
			From: from, To: to, TradeFrom: tradeFrom, CashCents: cash,
		})
	}
	for _, ps := range sets {
		if sc.WalkForward.Enabled {
			add(ps, 0, 0, w.DataFrom, w.ISTo, w.ISFrom, 0)
			// OOS 的数据从 DataFrom 起（含 IS 段做预热），
			// 但 TradeFrom 是 OOSFrom —— 样本内那段只喂指标不交易。
			// **不能只给 OOS 那一年的数据**：那样指标从零预热，
			// 头一个月的信号被系统性削掉，18 个窗口上是一致的偏差
			add(ps, 1, 0, w.DataFrom, w.OOSTo, w.OOSFrom, 0)
		} else {
			add(ps, 1, 0, w.OOSFrom, w.OOSTo, 0, 0)
		}
	}
	// 噪声探针只跑 OOS —— 基线是用来跟 OOS 的散布比的
	for _, ps := range probes {
		for i := 1; i <= sc.NoiseProbe.Repeats; i++ {
			cash := perturbCash(100_000_000, sc.NoiseProbe.PerturbPct, i,
				sc.NoiseProbe.Repeats)
			if sc.WalkForward.Enabled {
				add(ps, 1, int8(i), w.DataFrom, w.OOSTo, w.OOSFrom, cash)
			} else {
				add(ps, 1, int8(i), w.OOSFrom, w.OOSTo, 0, cash)
			}
		}
	}
	return jobs
}

// perturbCash 把初始资金在 ±pct% 内均匀取第 i 个点。
//
// 用初始资金做扰动的理由：要的是「经济上无意义、但会改变执行路径」的量。
// 引擎里没有随机数（C5），种子无从扰动；改起始日会改变样本区间，
// 那是真差异；改滑点是改成本模型，属于参数。
// 只有初始资金既不改规则也不改样本，只改每一单的取整。
func perturbCash(base int64, pct float64, i, n int) int64 {
	if n <= 1 {
		return base
	}
	// i 从 1 到 n，映射到 [−pct, +pct]
	frac := -1.0 + 2.0*float64(i-1)/float64(n-1)
	return base + int64(float64(base)*pct/100.0*frac)
}

// probePoints 选噪声探针的代表点：网格的首、尾、中，以及四分位处。
//
// 不对每组参数都测 —— 那是 Repeats 倍开销。
func probePoints(sets []sweep.ParamSet, np sweep.NoiseProbe) []sweep.ParamSet {
	if np.Points <= 0 || np.Repeats <= 1 {
		return nil
	}
	n := np.Points
	if n > len(sets) {
		n = len(sets)
	}
	out := make([]sweep.ParamSet, 0, n)
	for i := 0; i < n; i++ {
		idx := 0
		if n > 1 {
			idx = i * (len(sets) - 1) / (n - 1)
		}
		out = append(out, sets[idx])
	}
	return out
}

// narrowFor 为一个窗口裁一份子集，**整个窗口内的全部参数组共用它**。
func narrowFor(ds *config.DataSet, base *config.Config, w sweep.Window) *config.DataSet {
	ids, err := base.ResolveUniverse(ds.Universe, ds.Adjuster)
	if err != nil {
		return ds
	}
	to := w.OOSTo
	sub, err := ds.Columns.Subset(ids, w.DataFrom, to)
	if err != nil {
		return ds
	}
	out := *ds // 浅拷贝：Columns 之外的字段本来就是只读共享的
	out.Columns = sub
	return &out
}

func gridKeys(sets []sweep.ParamSet) []string {
	if len(sets) == 0 {
		return nil
	}
	ks := make([]string, 0, len(sets[0].Values))
	for k := range sets[0].Values {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func distinct(sets []sweep.ParamSet, key string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sets {
		v := fmt.Sprint(s.Values[key])
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func mustJSON(v any) []byte {
	b, err := jsonMarshal(v)
	if err != nil {
		fatal(err)
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
