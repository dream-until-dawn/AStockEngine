package sweep

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/parquet-go/parquet-go"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/metrics"
)

// Result 是结果表的一行：一次回测的全部可比较量。
//
// # 为什么绩效列用 float64 而不是定点
//
// SCHEMA 0.2 说「比率用 ppm 存 INT32」，那条规则针对的是**引擎的输入**：
// 定点是为了 C5 可复现（浮点累加顺序不同结果就不同）。
// 这里存的是**输出**，永远不会再喂回引擎；C5 由 InputFP / OutputFP 保证，
// 不由指标的编码方式保证。定点在这里只会让读表的人多做一次除法。
//
// # 为什么不引 DuckDB
//
// 5,400 行 × 30 列不到 2 MB，10 万次也就 40 MB。查询是过滤、排序、分组，
// 在 Go 里全量载入内存做只要几毫秒。而 Go 侧引 DuckDB 要 cgo
// （go-duckdb 是静态链接的 C 库），会打破当前纯 Go 的构建与交叉编译 ——
// 为一个「查 5,400 行」的需求背上永久成本不划算。
//
// Parquet 是开放格式，Python 侧要临时分析时
// `duckdb.sql("select * from 'data/results/**/*.parquet'")` 一行就能读。
type Result struct {
	SweepID string `parquet:"sweep_id"`
	// ParamID 本次海选内的序号；ParamFP 跨海选稳定
	ParamID int32  `parquet:"param_id"`
	ParamFP string `parquet:"param_fp"`
	Params  string `parquet:"params"`
	Window  int16  `parquet:"window"`
	Phase   int8   `parquet:"phase"`
	Probe   int8   `parquet:"probe"`

	FromDay int32 `parquet:"from_day"`
	ToDay   int32 `parquet:"to_day"`
	Steps   int32 `parquet:"steps"`

	TotalReturn  float64 `parquet:"total_return"`
	AnnualReturn float64 `parquet:"annual_return"`
	ExcessReturn float64 `parquet:"excess_return"`
	MaxDrawdown  float64 `parquet:"max_drawdown"`
	Sharpe       float64 `parquet:"sharpe"`
	AnnualVol    float64 `parquet:"annual_vol"`
	// HasBenchmark 为 false 时 ExcessReturn 无意义。
	// **不能靠「等于 0」判断** —— 超额恰好为 0 是可能的
	HasBenchmark bool `parquet:"has_benchmark"`
	// BenchCovered / BenchTotal 基准覆盖了多少个时点。
	// 宽基 ETF 最早 2012 年，Walk-Forward 的前几个窗口**根本没有基准** ——
	// 不把覆盖率一起存下来，那几个窗口的「超额 0」会被当成真的
	BenchCovered int32 `parquet:"bench_covered"`
	BenchTotal   int32 `parquet:"bench_total"`
	// AnnualReliable 样本不足一年时年化收益是外推出来的
	AnnualReliable bool `parquet:"annual_reliable"`

	Signals    int32   `parquet:"signals"`
	Fills      int32   `parquet:"fills"`
	Rejects    int32   `parquet:"rejects"`
	RoundTrips int32   `parquet:"round_trips"`
	WinRate    float64 `parquet:"win_rate"`

	FeeCents      int64   `parquet:"fee_cents"`
	SlippageCents int64   `parquet:"slippage_cents"`
	Turnover      float64 `parquet:"turnover"`

	FinalEquity int64 `parquet:"final_equity"`

	// InputFP / OutputFP —— C5：任何一行都能一键重跑并比对
	InputFP  string `parquet:"input_fp"`
	OutputFP string `parquet:"output_fp"`

	ElapsedMS int32 `parquet:"elapsed_ms"`
	// Err 非空表示这次跑失败了。**失败的行也留在表里** ——
	// 「这组参数在这个窗口跑不了」本身就是一个发现，删掉它等于藏起来
	Err string `parquet:"err"`
}

func (r *Result) fill(
	m metrics.Result, st config.RunStats, cfg *config.Config, dataFP, outFP string,
) {
	// **三者必须同口径**：Steps 取绩效的步数而不是引擎的步数。
	// 引擎在 Walk-Forward 下会多跑一段预热（4 年数据只交易最后 1 年），
	// 记成 970 步而区间写着 1 年，读表的人会以为哪里错了 —— 而确实错了
	r.FromDay, r.ToDay, r.Steps = m.FromDay, m.ToDay, int32(m.Steps)
	_ = st.Steps
	r.TotalReturn = m.TotalReturn
	r.AnnualReturn = m.AnnualReturn
	r.MaxDrawdown = m.MaxDrawdown.Ratio
	r.Sharpe = m.Sharpe
	r.AnnualVol = m.AnnualVol
	if m.Bench != nil {
		r.BenchCovered = int32(m.Bench.Covered)
		r.BenchTotal = int32(m.Bench.Total)
		// **覆盖为 0 时不能算「有基准」**：宽基 ETF 最早 2012 年，
		// Walk-Forward 的前 7 个窗口根本没有基准可比。
		// 此时 Excess 是 0，若当成真的超额，那几个窗口的排序会完全失真 ——
		// 而且不会报错，只是把「没有基准」悄悄变成了「超额恰好为零」
		if m.Bench.Covered > 0 {
			r.HasBenchmark = true
			r.ExcessReturn = m.Bench.Excess
		}
	}
	r.Signals = int32(st.Signals)
	r.Fills = int32(st.Fills)
	r.Rejects = int32(st.Rejects)
	r.RoundTrips = int32(m.Trades.RoundTrips)
	r.WinRate = m.Trades.WinRate
	r.FeeCents = m.FeeCents
	r.SlippageCents = m.SlippageCents
	r.Turnover = m.Turnover
	r.FinalEquity = m.FinalCents
	r.AnnualReliable = m.AnnualReliable
	if fp, err := cfg.InputFingerprint(dataFP); err == nil {
		r.InputFP = fp
	}
	r.OutputFP = outFP
}

// Score 返回排序用的分数。
//
// **不按总收益排** —— 那会选出高杠杆高回撤的东西。
// 默认「超额 ÷ 最大回撤」：同样的超额，回撤小的更值钱。
//
// 没有基准时退回「总收益 ÷ 最大回撤」，并且**调用方必须知道这件事**
// （HasBenchmark 一起报出去），否则两种口径的分数会被混在一张榜上比。
func (r Result) Score(rank string) float64 {
	num := r.TotalReturn
	if r.HasBenchmark {
		num = r.ExcessReturn
	}
	switch rank {
	case "sharpe":
		return r.Sharpe
	case "return":
		return num
	default: // excess_over_maxdd
		dd := r.MaxDrawdown
		if dd < 0.01 {
			dd = 0.01 // 回撤趋零时分母不能爆掉；1% 是个保守的地板
		}
		return num / dd
	}
}

// Passes 报告是否过硬门槛。
func (r Result) Passes(g Gate) bool {
	return r.Err == "" && int(r.RoundTrips) >= g.MinRoundTrips
}

// ---- 落盘 ----

// ResultDir 返回一次海选的结果目录。
func ResultDir(root, sweepID string) string {
	return filepath.Join(root, "results", "sweep="+sweepID)
}

// WritePart 写出一个分片。
//
// 按窗口分片，**跑完一个窗口就落一次盘** —— 13 分钟的任务被 Ctrl-C
// 打断后从头再来很难受，分片让续跑可以跳过已完成的窗口。
func WritePart(dir, name string, rows []Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := parquet.WriteFile(path, rows,
		parquet.Compression(&parquet.Zstd)); err != nil {
		return "", fmt.Errorf("写出 %s 失败: %w", path, err)
	}
	return path, nil
}

// ReadAll 读回一次海选的全部结果。
func ReadAll(dir string) ([]Result, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".parquet" {
			continue
		}
		rows, err := parquet.ReadFile[Result](filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", e.Name(), err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

// DoneWindows 扫描已有分片，返回哪些窗口已经跑完 —— 供续跑跳过。
func DoneWindows(dir string) map[int16]bool {
	done := map[int16]bool{}
	rows, err := ReadAll(dir)
	if err != nil {
		return done
	}
	for _, r := range rows {
		if r.Probe == 0 {
			done[r.Window] = true
		}
	}
	return done
}
