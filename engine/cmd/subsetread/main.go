// Command subsetread 验证「核心列 struct 读取扩展列文件」是否可行。
//
// 跨市场统一 schema 的方案是：核心 9 列全市场严格统一，市场特定列直接附在
// 同一张表上（A 股的 preclose/tradestatus、期货的 open_interest、加密的 funding_rate）。
// 该方案成立的前提是 Go 侧能用只声明核心列的 struct 读取任意市场的文件，
// 多出来的列被安全忽略。
//
// 若不成立，就必须退回「核心表 + 扩展表 join」的方案（实测多占 1.3% 体积且读取更复杂）。
package main

import (
	"fmt"
	"os"

	"github.com/parquet-go/parquet-go"
)

// CoreBar 只声明跨市场共有的核心列，不含任何 A 股特定字段。
type CoreBar struct {
	Date   int32  `parquet:"date"`
	Code   string `parquet:"code"`
	Open   int32  `parquet:"open"`
	High   int32  `parquet:"high"`
	Low    int32  `parquet:"low"`
	Close  int32  `parquet:"close"`
	Volume int64  `parquet:"volume"`
	Amount int64  `parquet:"amount"`
}

func main() {
	// 该文件含 12 列：核心 8 列之外还有 preclose/turn/tradestatus/isST
	path := "../data/_bench/v6_fixed_delta_zstd.parquet"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	recs, err := parquet.ReadFile[CoreBar](path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL 用核心列 struct 读取扩展列文件失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "=> 需退回「核心表 + 扩展表」方案")
		os.Exit(1)
	}

	var sumClose, sumVolume int64
	codes := map[string]struct{}{}
	for i := range recs {
		sumClose += int64(recs[i].Close)
		sumVolume += recs[i].Volume
		codes[recs[i].Code] = struct{}{}
	}

	fmt.Printf("PASS 核心列 struct 成功读取扩展列文件\n")
	fmt.Printf("  文件: %s\n", path)
	fmt.Printf("  rows=%d sum_close=%d sum_volume=%d codes=%d\n",
		len(recs), sumClose, sumVolume, len(codes))
	fmt.Printf("=> 市场特定列可直接附于同表，无需拆分扩展表\n")
}
