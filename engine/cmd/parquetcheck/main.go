// Command parquetcheck 验证 Python 写出的 Parquet 变体能否被 Go 完整读回。
//
// 存储方案选用本地文件而非数据库，前提是「Python 写、Go 读」这条链路可靠。
// Parquet 是开放格式，但各语言实现对压缩算法与列编码的支持并不一致 ——
// 该结论必须实证，不能靠格式规范推定。
//
// 本程序对每个变体：
//   1. 读取全部行
//   2. 与 Python 侧写出的 checksum.json 逐项比对（行数、收盘价合计、成交量合计、标的数）
//
// 校验和比对是关键：只验证「能打开」不足以说明数据正确，
// 编码不匹配时常表现为静默的数值错乱而非报错。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/parquet-go/parquet-go"
)

// BarFloat 对应 float64 基线变体。
type BarFloat struct {
	Date        int32   `parquet:"date"`
	Code        string  `parquet:"code"`
	Open        float64 `parquet:"open"`
	High        float64 `parquet:"high"`
	Low         float64 `parquet:"low"`
	Close       float64 `parquet:"close"`
	Preclose    float64 `parquet:"preclose"`
	Turn        float64 `parquet:"turn"`
	Volume      int64   `parquet:"volume"`
	Amount      float64 `parquet:"amount"`
	TradeStatus int8    `parquet:"tradestatus"`
	IsST        int8    `parquet:"isST"`
}

// BarFixed 对应定点整数变体 —— 价格以厘为单位存 int32。
type BarFixed struct {
	Date        int32  `parquet:"date"`
	Code        string `parquet:"code"`
	Open        int32  `parquet:"open"`
	High        int32  `parquet:"high"`
	Low         int32  `parquet:"low"`
	Close       int32  `parquet:"close"`
	Preclose    int32  `parquet:"preclose"`
	Turn        int32  `parquet:"turn"`
	Volume      int64  `parquet:"volume"`
	Amount      int64  `parquet:"amount"`
	TradeStatus int8   `parquet:"tradestatus"`
	IsST        int8   `parquet:"isST"`
}

type checksum struct {
	Rows           int64 `json:"rows"`
	SumCloseFixed  int64 `json:"sum_close_fixed"`
	SumVolume      int64 `json:"sum_volume"`
	DistinctCodes  int   `json:"distinct_codes"`
}

type result struct {
	Variant       string  `json:"variant"`
	OK            bool    `json:"ok"`
	Rows          int64   `json:"rows"`
	SumClose      int64   `json:"sum_close"`
	SumVolume     int64   `json:"sum_volume"`
	DistinctCodes int     `json:"distinct_codes"`
	SizeMB        float64 `json:"size_mb"`
	Err           string  `json:"err,omitempty"`
	Mismatch      string  `json:"mismatch,omitempty"`
}

// PRICE_SCALE 与 Python 侧保持一致：价格以厘为单位。
const priceScale = 1000

func main() {
	dir := "../data/_bench"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	var want checksum
	raw, err := os.ReadFile(filepath.Join(dir, "checksum.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 checksum.json 失败: %v\n", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		fmt.Fprintf(os.Stderr, "解析 checksum.json 失败: %v\n", err)
		os.Exit(1)
	}

	// 只校验行情变体（v* 前缀）。同目录下还有其他 schema 的文件
	// （如 Go 侧写出的海选结果表），它们不适用这份基准。
	files, err := filepath.Glob(filepath.Join(dir, "v*.parquet"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "在 %s 下未找到行情变体文件（v*.parquet）\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)

	fmt.Printf("基准: rows=%d sum_close=%d sum_volume=%d codes=%d\n\n",
		want.Rows, want.SumCloseFixed, want.SumVolume, want.DistinctCodes)

	results := make([]result, 0, len(files))
	failed := 0
	for _, path := range files {
		name := filepath.Base(path)
		r := check(path, want)
		r.Variant = name
		results = append(results, r)
		status := "PASS"
		if !r.OK {
			status = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-34s %7.2f MB  rows=%-8d codes=%d\n",
			status, name, r.SizeMB, r.Rows, r.DistinctCodes)
		if r.Err != "" {
			fmt.Printf("       └─ 错误: %s\n", r.Err)
		}
		if r.Mismatch != "" {
			fmt.Printf("       └─ 不一致: %s\n", r.Mismatch)
		}
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "go_readback.json"), out, 0o644)

	fmt.Printf("\n%d/%d 变体通过 Go 回读校验\n", len(results)-failed, len(results))
	if failed > 0 {
		os.Exit(1)
	}
}

func check(path string, want checksum) result {
	r := result{}
	fi, err := os.Stat(path)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.SizeMB = float64(fi.Size()) / 1024 / 1024

	// float64 基线变体与定点变体的 schema 不同，按文件名分派
	isFloat := filepath.Base(path)[:2] == "v1" || filepath.Base(path)[:2] == "v2"

	var (
		rows      int64
		sumClose  int64
		sumVolume int64
		codes     = map[string]struct{}{}
	)

	if isFloat {
		recs, err := parquet.ReadFile[BarFloat](path)
		if err != nil {
			r.Err = err.Error()
			return r
		}
		rows = int64(len(recs))
		for i := range recs {
			// 折算成与定点变体同一量纲，才能与同一份基准比对
			sumClose += int64(recs[i].Close*priceScale + 0.5)
			sumVolume += recs[i].Volume
			codes[recs[i].Code] = struct{}{}
		}
	} else {
		recs, err := parquet.ReadFile[BarFixed](path)
		if err != nil {
			r.Err = err.Error()
			return r
		}
		rows = int64(len(recs))
		for i := range recs {
			sumClose += int64(recs[i].Close)
			sumVolume += recs[i].Volume
			codes[recs[i].Code] = struct{}{}
		}
	}

	r.Rows, r.SumClose, r.SumVolume, r.DistinctCodes = rows, sumClose, sumVolume, len(codes)

	switch {
	case rows != want.Rows:
		r.Mismatch = fmt.Sprintf("行数 %d != %d", rows, want.Rows)
	case sumVolume != want.SumVolume:
		r.Mismatch = fmt.Sprintf("成交量合计 %d != %d", sumVolume, want.SumVolume)
	case len(codes) != want.DistinctCodes:
		r.Mismatch = fmt.Sprintf("标的数 %d != %d", len(codes), want.DistinctCodes)
	case sumClose != want.SumCloseFixed:
		// float64 变体存在往返舍入，允许极小偏差
		diff := sumClose - want.SumCloseFixed
		if diff < 0 {
			diff = -diff
		}
		if !isFloat || diff > want.Rows {
			r.Mismatch = fmt.Sprintf("收盘价合计 %d != %d（差 %d）", sumClose, want.SumCloseFixed, diff)
		} else {
			r.OK = true
		}
	default:
		r.OK = true
	}
	return r
}
