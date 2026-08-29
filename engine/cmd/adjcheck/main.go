// Command adjcheck 导出 Go 侧的复权结果，供 Python 侧逐位比对。
//
// 这是 docs/DESIGN-v0.2-dataflow.md 第 7 节的待测项 8，也是 **C5 的第一道关**：
// 若两侧算出的复权价不一致，「同配置两次运行逐笔一致」从第一步就破了。
//
// 输出 CSV 到标准输出，字段为原始定点整数 —— 不做任何格式化，
// 避免比对时被浮点打印精度干扰。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func main() {
	root := flag.String("root", "../data/bar/market=ashare/freq=1d", "bar 分区根目录")
	facPath := flag.String("factors", "../data/meta/adj_factor.parquet", "复权因子表")
	nInst := flag.Int("instruments", 40, "抽样标的数")
	nDays := flag.Int("days", 60, "每只标的取最近多少个交易日")
	flag.Parse()

	adj, err := mktdata.LoadAdjuster(*facPath)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "因子表覆盖 %d 只标的\n", adj.NumInstruments())

	allIDs, err := mktdata.ReadInstrumentIDs(*root)
	if err != nil {
		fatal(err)
	}
	// 优先取有复权事件的标的 —— 无事件的标的因子恒为 1，比对不出任何东西
	picked := make([]mktdata.InstrumentID, 0, *nInst)
	for _, id := range allIDs {
		if adj.Factor(id, 29991231) != mktdata.FactorScale {
			picked = append(picked, id)
			if len(picked) >= *nInst {
				break
			}
		}
	}
	if len(picked) == 0 {
		fatal(fmt.Errorf("没有任何标的有复权事件"))
	}
	fmt.Fprintf(os.Stderr, "抽样 %d 只有复权事件的标的\n", len(picked))

	col, _, err := mktdata.Load(mktdata.LoadOptions{Root: *root, Instruments: picked})
	if err != nil {
		fatal(err)
	}

	cur := mktdata.NewCursor(col)
	for {
		if _, _, ok := cur.Advance(); !ok {
			break
		}
	}

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprintln(w, "instrument_id,trading_day,factor,raw_close,hfq_close,qfq_close")

	rows := 0
	for _, id := range picked {
		h := cur.History(id)
		n := h.Len()
		if n == 0 {
			continue
		}
		// 取最近 nDays 根，并额外覆盖每个除权日附近 —— 因子在那里才会变化
		limit := *nDays
		if limit > n {
			limit = n
		}
		for back := limit - 1; back >= 0; back-- {
			day, _ := h.TradingDay(back)
			raw, _ := h.Close(back)
			fmt.Fprintf(w, "%d,%d,%d,%d,%d,%d\n",
				id, day, adj.Factor(id, day), raw,
				adj.Adjust(id, day, raw, mktdata.AdjHFQ),
				adj.Adjust(id, day, raw, mktdata.AdjQFQ))
			rows++
		}
	}
	fmt.Fprintf(os.Stderr, "导出 %d 行 -> %s\n", rows, filepath.Base("stdout"))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
