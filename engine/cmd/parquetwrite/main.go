// Command parquetwrite 验证反方向链路：Go 写出的 Parquet 能否被 Python 读回。
//
// 行情数据是 Python 写、Go 读；但海选结果（v0.5）是 Go 写、Python/DuckDB 读。
// 两个方向都必须实证，只验证其一不足以支撑「本地文件 + 跨语言」的存储方案。
//
// 写出的表模拟海选结果：每行一次回测的配置指纹与绩效指标。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/parquet-go/parquet-go"
)

// BacktestResult 模拟 v0.5 海选的结果行。
// 字段类型刻意覆盖多种情形：字符串、整数、浮点、布尔、可空字段。
type BacktestResult struct {
	ConfigHash   string   `parquet:"config_hash"`
	Strategy     string   `parquet:"strategy"`
	ParamFast    int32    `parquet:"param_fast"`
	ParamSlow    int32    `parquet:"param_slow"`
	StartDate    int32    `parquet:"start_date"`
	EndDate      int32    `parquet:"end_date"`
	AnnualReturn float64  `parquet:"annual_return"`
	Sharpe       float64  `parquet:"sharpe"`
	MaxDrawdown  float64  `parquet:"max_drawdown"`
	WinRate      float64  `parquet:"win_rate"`
	Trades       int64    `parquet:"trades"`
	IsOOS        bool     `parquet:"is_oos"`
	Note         *string  `parquet:"note,optional"`
}

func main() {
	dir := "../data/_bench"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建目录失败: %v\n", err)
		os.Exit(1)
	}
	path := filepath.Join(dir, "go_written_results.parquet")

	const n = 5000
	note := "sample"
	rows := make([]BacktestResult, 0, n)
	var sumSharpe float64
	var sumTrades int64
	for i := 0; i < n; i++ {
		fast := int32(2 + i%59)
		slow := int32(5 + i%246)
		// 用确定性算式生成，便于 Python 侧独立复算校验和
		sharpe := float64(i%997) / 100.0
		trades := int64(i%311 + 1)
		var np *string
		if i%3 == 0 {
			np = &note
		}
		rows = append(rows, BacktestResult{
			ConfigHash:   fmt.Sprintf("cfg-%08d", i),
			Strategy:     []string{"ma_cross", "buy_hold", "breakout"}[i%3],
			ParamFast:    fast,
			ParamSlow:    slow,
			StartDate:    20180101,
			EndDate:      20261231,
			AnnualReturn: float64(i%523)/1000.0 - 0.2,
			Sharpe:       sharpe,
			MaxDrawdown:  -float64(i%401) / 1000.0,
			WinRate:      float64(i%100) / 100.0,
			Trades:       trades,
			IsOOS:        i%2 == 0,
			Note:         np,
		})
		sumSharpe += sharpe
		sumTrades += trades
	}

	if err := parquet.WriteFile(path, rows,
		parquet.Compression(&parquet.Zstd),
	); err != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		os.Exit(1)
	}

	fi, _ := os.Stat(path)
	fmt.Printf("已写出 %s\n", path)
	fmt.Printf("rows=%d size=%.1f KB sum_sharpe=%.2f sum_trades=%d\n",
		n, float64(fi.Size())/1024, sumSharpe, sumTrades)
}
