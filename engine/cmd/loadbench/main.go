// Command loadbench 测量列式加载与两种访问模式的实际代价。
//
// 对应 docs/DESIGN-v0.2-dataflow.md 第 7 节的待测项 1/2/3/6 ——
// 设计里关于「列式更快」「内存约 1.16 GB」的说法都只是推断，
// 本程序负责把它们变成数字。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func main() {
	root := flag.String("root", "../data/bar/market=ashare/freq=1d", "bar 分区根目录")
	from := flag.Int("from", 0, "起始交易日 YYYYMMDD，0 表示不限")
	to := flag.Int("to", 0, "结束交易日 YYYYMMDD，0 表示不限")
	maxInst := flag.Int("instruments", 0, "只加载前 N 只标的，0 表示全部")
	flag.Parse()

	diskBytes, _ := mktdata.FileSizeBytes(*root)
	fmt.Printf("分区目录 %s\n磁盘占用 %.1f MB\n\n", *root, float64(diskBytes)/1024/1024)

	var opt = mktdata.LoadOptions{
		Root:    *root,
		FromDay: int32(*from),
		ToDay:   int32(*to),
	}
	if *maxInst > 0 {
		ids, err := mktdata.ReadInstrumentIDs(*root)
		if err != nil {
			fatal(err)
		}
		if *maxInst < len(ids) {
			ids = ids[:*maxInst]
		}
		opt.Instruments = ids
	}

	runtime.GC()
	memBefore := heapAlloc()
	col, st, err := mktdata.Load(opt)
	if err != nil {
		fatal(err)
	}
	runtime.GC() // 让临时切片先回收，测的才是常驻占用
	memAfter := heapAlloc()
	// 用有符号数相减：GC 可能使堆低于基线，无符号相减会下溢成天文数字
	heapDelta := memAfter - memBefore

	fmt.Println("=== 加载 ===")
	fmt.Println(st)
	fmt.Printf("  堆实际增长 %.2f MB（估算值 %.2f MB）\n",
		float64(heapDelta)/1024/1024,
		float64(st.MemoryBytes)/1024/1024)
	if diskBytes > 0 {
		fmt.Printf("  内存/磁盘 = %.2fx\n\n", float64(heapDelta)/float64(diskBytes))
	}

	// ---- 步进：走完全部时点 ----
	cur := mktdata.NewCursor(col)
	t0 := time.Now()
	steps, updates := 0, 0
	for {
		_, upd, ok := cur.Advance()
		if !ok {
			break
		}
		steps++
		updates += len(upd)
	}
	stepDur := time.Since(t0)
	fmt.Println("=== 步进（仅推进游标，不做任何计算）===")
	fmt.Printf("  %d 步 / %d 次 bar 更新 / %v\n", steps, updates, stepDur.Round(time.Millisecond))
	fmt.Printf("  %.1f 步/秒，%.0f ns/更新\n\n",
		float64(steps)/stepDur.Seconds(),
		float64(stepDur.Nanoseconds())/float64(maxi(updates, 1)))

	// ---- 单标的时序扫描 vs 每步重算窗口 ----
	ids := col.Instruments()
	sample := ids
	if len(sample) > 300 {
		sample = sample[:300]
	}

	cur2 := mktdata.NewCursor(col)
	for {
		if _, _, ok := cur2.Advance(); !ok {
			break
		}
	}
	// 走到末尾后，每个标的的 History 覆盖其全部历史
	const window = 20

	const repeat = 20 // 单轮太快测不准，重复多轮取均值
	t0 = time.Now()
	var sinkA int64
	var scanned int
	for r := 0; r < repeat; r++ {
		for _, id := range sample {
			h := cur2.History(id)
			n := h.Len()
			if r == 0 {
				scanned += n
			}
			for back := 0; back < n; back++ {
				c, _ := h.Close(back)
				sinkA += c
			}
		}
	}
	seqDur := time.Since(t0) / repeat

	// 模拟「每步重算 20 日窗口」的代价
	t0 = time.Now()
	var sinkB int64
	for r := 0; r < repeat; r++ {
		for _, id := range sample {
			h := cur2.History(id)
			n := h.Len()
			for back := 0; back+window <= n; back++ {
				var sum int64
				for k := 0; k < window; k++ {
					c, _ := h.Close(back + k)
					sum += c
				}
				sinkB += sum / window
			}
		}
	}
	winDur := time.Since(t0) / repeat

	fmt.Println("=== 单标的时序访问（样本 300 只）===")
	fmt.Printf("  顺序扫描 %d 行 / %v  → %.1f ns/行\n",
		scanned, seqDur.Round(time.Millisecond),
		float64(seqDur.Nanoseconds())/float64(maxi(scanned, 1)))
	fmt.Printf("  每步重算 MA%d %v  → 比顺序扫描慢 %.1fx\n",
		window, winDur.Round(time.Millisecond),
		float64(winDur)/float64(maxi64(int64(seqDur), 1)))
	fmt.Printf("  （增量指标每步 O(1)，理论上应接近顺序扫描的量级）\n\n")

	// ---- 横截面访问 ----
	cur3 := mktdata.NewCursor(col)
	t0 = time.Now()
	var sinkC int64
	crossSteps, crossBars := 0, 0
	for {
		_, upd, ok := cur3.Advance()
		if !ok {
			break
		}
		crossSteps++
		for _, id := range upd {
			if b, ok := cur3.Bar(id); ok {
				sinkC += b.Close
				crossBars++
			}
		}
	}
	crossDur := time.Since(t0)
	fmt.Println("=== 横截面访问 ===")
	fmt.Printf("  A. 完整 Bar（map 查 + 构造 10 字段）%v → %.1f ns/根\n",
		crossDur.Round(time.Millisecond),
		float64(crossDur.Nanoseconds())/float64(maxi(crossBars, 1)))

	// 变体 B：只取收盘价，跳过 map 查找与 Bar 构造。
	// 用于把「随机访问的 cache miss」与「map 查找 + 多列聚合」两部分代价分开 ——
	// 前者是标的主序布局的固有代价，后者是可以优化掉的实现开销。
	cur4 := mktdata.NewCursor(col)
	t0 = time.Now()
	var sinkD int64
	barsD := 0
	for {
		_, upd, ok := cur4.Advance()
		if !ok {
			break
		}
		for _, id := range upd {
			if c, ok := cur4.History(id).Close(0); ok {
				sinkD += c
				barsD++
			}
		}
	}
	closeDur := time.Since(t0)
	fmt.Printf("  B. 只取收盘价（仍走 map 查）  %v → %.1f ns/根\n",
		closeDur.Round(time.Millisecond),
		float64(closeDur.Nanoseconds())/float64(maxi(barsD, 1)))
	fmt.Printf("     → 多列聚合的额外代价占 %.0f%%\n",
		100*(1-float64(closeDur)/float64(maxi64(int64(crossDur), 1))))
	if sinkD == 0 {
		fmt.Println()
	}

	// 防止编译器把上面的循环优化掉
	if sinkA == 0 && sinkB == 0 && sinkC == 0 {
		fmt.Println()
	}
}

func heapAlloc() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxi64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
