// Command enginebench 测量**每个 worker 的引擎常驻内存**。
//
// 这是 v0.5 海选并发度的真实上限，而它此前只有推断没有数字。
// 载入的 490 万行数据是 N 个 worker 共享的只读内存（Go 的零拷贝优势
// 就在这里），但每个 worker 还要各自持有：
//
//	指标实例   每只标的 × 每个指标 × 各自的环形缓冲
//	账本       持仓、成交流水
//	记录器     summary 级也要留全部成交（算指标要用）
//
// 这三样是**乘以 worker 数**的，1 GB 的共享数据配 16 个 worker 未必装得下。
//
// 用法：
//
//	go run ./cmd/enginebench -config ../configs/backtest/macd_full.json -workers 1,4,8
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"

	// 策略与交易模块靠 init() 注册进 registry，不导入就一个都认不出来
	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（必填）")
	workersArg := flag.String("workers", "1,4,8", "并发度列表，逗号分隔")
	flag.Parse()
	if *cfgPath == "" {
		fatal(fmt.Errorf("需要 -config"))
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("配置 %s\n策略 %s\n\n", cfg.Name, cfg.Strategy.Impl)

	t0 := time.Now()
	ds, err := config.LoadDataSet(cfg)
	if err != nil {
		fatal(err)
	}
	loadDur := time.Since(t0)

	shared := heapMB()
	fmt.Printf("=== 共享只读数据（N 个 worker 共用一份）===\n")
	fmt.Printf("  载入 %v  %d 行 / %d 只 / %d 时点\n",
		loadDur.Round(time.Millisecond), ds.Stats.Rows,
		ds.Stats.Instruments, ds.Stats.Steps)
	fmt.Printf("  堆常驻 %.0f MB\n\n", shared)

	warm, err := cfg.Assemble(ds)
	if err != nil {
		fatal(err)
	}
	afterFirst := heapMB()
	fmt.Printf("=== 单个引擎实例 ===\n")
	fmt.Printf("  Assemble 后 %.0f MB（+%.1f MB）\n", afterFirst, afterFirst-shared)

	t0 = time.Now()
	if err := warm.RunAll(); err != nil {
		fatal(err)
	}
	runDur := time.Since(t0)
	afterRun := heapMB()
	fills, rejects := warm.LastCounts()
	fmt.Printf("  RunAll  %v  %d 步 / %d 成交 / %d 拒单\n",
		runDur.Round(time.Millisecond), warm.Steps(), fills, rejects)
	fmt.Printf("  跑完后 %.0f MB（+%.1f MB，含指标预热后的稳态）\n\n",
		afterRun, afterRun-shared)
	warm = nil
	runtime.GC()

	// 预先裁一次子集，让各 worker 共用同一份 Columns。
	//
	// **不这么做的话每次 Assemble 都会复制一份子集**：LoadDataSet 会把
	// 基准标的一并载入（它不在标的池里），于是 narrow 看到的集合总是
	// 差一个，sameSet 判定为 false，每个 worker 各拷一份。
	// 服务端的 narrowCached 已经踩过这个坑，海选的 BatchDriver 同样要走这条路。
	beforeNarrow := heapMB()
	sharedDS, err := preNarrow(cfg, ds)
	if err != nil {
		fatal(err)
	}
	ids, _ := cfg.ResolveUniverse(ds.Universe, ds.Adjuster)
	fmt.Printf("  载入的 Columns 含 %d 只（含基准），标的池 %d 只，"+
		"预裁子集 %d 只\n", len(ds.Columns.Instruments()), len(ids),
		len(sharedDS.Columns.Instruments()))
	fmt.Printf("  预裁一次子集 +%.1f MB\n\n", heapMB()-beforeNarrow)

	fmt.Printf("=== 并发（稳态口径：装配/跑完后强制 GC 再读堆）===\n")
	fmt.Printf("  %-7s %-13s %-11s %-13s %-11s %s\n",
		"worker", "口径", "墙钟", "装配后/worker", "跑完/worker", "堆总量")
	for _, w := range parseWorkers(*workersArg) {
		r1 := runConcurrent(cfg, ds, w)
		report(w, r1, shared, "每次 Subset")
		r2 := runConcurrent(cfg, sharedDS, w)
		report(w, r2, shared, "共享子集")
	}

	fmt.Printf("\n共享 %.0f MB 是固定开销；并发度的上限由「跑完/worker」那一列决定。\n", shared)
	fmt.Printf("两种口径的差额就是「每个 worker 各拷一份子集」的代价。\n")
}

type runResult struct {
	wall               time.Duration
	afterAsm, afterRun float64
}

func report(w int, r runResult, shared float64, tag string) {
	fmt.Printf("  %-7d %-13s %-11v %-13s %-11s %.0f MB\n",
		w, tag, r.wall.Round(time.Millisecond),
		fmt.Sprintf("%.1f MB", (r.afterAsm-shared)/float64(w)),
		fmt.Sprintf("%.1f MB", (r.afterRun-shared)/float64(w)),
		r.afterRun)
}

// preNarrow 把子集裁好放进一份浅拷贝的 DataSet。
//
// 之后 Assemble 里的 narrow 会认出「集合与区间都吻合」从而原样透传。
// 浅拷贝是安全的：Columns 之外的字段（Universe / Adjuster / 基准曲线）
// 本来就是只读共享的。
func preNarrow(cfg *config.Config, ds *config.DataSet) (*config.DataSet, error) {
	ids, err := cfg.ResolveUniverse(ds.Universe, ds.Adjuster)
	if err != nil {
		return nil, err
	}
	sub, err := ds.Columns.Subset(ids, cfg.Data.From, cfg.Data.To)
	if err != nil {
		return nil, err
	}
	out := *ds
	out.Columns = sub
	return &out, nil
}

// runConcurrent 起 n 个引擎并发跑完。
//
// **分两段量，中间强制 GC。** 先前用「采样堆峰值」量不出任何差别 ——
// 峰值被瞬时垃圾淹没，两种口径给出一模一样的数。
// 稳态口径（GC 后读 HeapAlloc，且 n 份引擎全都还活着）才有可比性。
func runConcurrent(cfg *config.Config, ds *config.DataSet, n int) runResult {
	runtime.GC()
	engines := make([]*eng.Engine, n)

	t0 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := cfg.Assemble(ds)
			if err != nil {
				fatal(err)
			}
			engines[i] = e
		}(i)
	}
	wg.Wait()
	afterAsm := heapMB() // n 份引擎都还被 engines 引用着

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := engines[i].RunAll(); err != nil {
				fatal(err)
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(t0)
	afterRun := heapMB()

	// 显式让 n 份引擎活到测量之后 —— 否则编译器/GC 有权提前回收
	for _, e := range engines {
		_ = e.EquityCents()
	}
	return runResult{wall: wall, afterAsm: afterAsm, afterRun: afterRun}
}

func heapMB() float64 {
	runtime.GC()
	return heapMBNoGC()
}

func heapMBNoGC() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1024 / 1024
}

func parseWorkers(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		out = []int{1}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

var _ = eng.Engine{}
