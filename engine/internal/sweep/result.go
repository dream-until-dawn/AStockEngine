package sweep

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/parquet-go/parquet-go"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/metrics"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
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

	// ---- 「这个结果是不是策略挣来的」----
	//
	// 光看收益分不出四种情况：策略有边际、强平替你止损、熔断替你择时、
	// 以及压根就是低摩擦低换手显得稳。下面这几列是用来把它们分开的。

	// Liquidations 被强平的轮次数。
	//
	// **高杠杆下强平相当于一道很紧的止损** —— 实测同一份配置杠杆从 1 调到 20，
	// 收益反而从 +115% 涨到 +202%，而强平从 0 次涨到 139 次。
	// 那不是杠杆挣的钱，是强平替你砍掉了亏损腿
	Liquidations int32 `parquet:"liquidations"`
	// HaltExits 被熔断清仓的轮次数。收益来自风控而不是信号时，
	// 换个市场就没了
	HaltExits int32 `parquet:"halt_exits"`
	// StopExits 被止损平掉的轮次数。不做门槛，但要看得见
	StopExits int32 `parquet:"stop_exits"`

	// FrictionRatio 摩擦（费用 + 滑点）占初始资金的比例。
	//
	// 实测 A 股两万本金切 10 份时它是 0.47 —— 那种组比的是费率不是策略
	FrictionRatio float64 `parquet:"friction_ratio"`
	// OpenCostRatio 回测结束时仍未平仓的开仓金额占初始资金的比例。
	// 高的话，收益挂在没结算的浮盈上
	OpenCostRatio float64 `parquet:"open_cost_ratio"`

	// VirtualTrips 被「有效性」树过滤掉的轮次数；
	// VirtualEdge = 实仓平均收益率 − 虚拟平均收益率。
	//
	// **为正才说明那棵树该留** —— 它过滤掉的确实比留下的差。
	// 为负就是在帮倒忙，而不装这两列的话，这件事一次也看不出来
	VirtualTrips int32   `parquet:"virtual_trips"`
	VirtualEdge  float64 `parquet:"virtual_edge"`

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
	e *eng.Engine, m metrics.Result, st config.RunStats,
	cfg *config.Config, dataFP, outFP string,
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
	r.fillAttribution(e, m)
}

// fillAttribution 填「这个结果是不是策略挣来的」那几列。
//
// 光看收益分不出四种情况：策略有边际、强平替你止损、熔断替你择时、
// 以及压根就是低摩擦低换手显得稳。
func (r *Result) fillAttribution(e *eng.Engine, m metrics.Result) {
	by := m.Trades.CloseBy
	r.Liquidations = int32(by[trading.TagLiquidation])
	r.HaltExits = int32(by["drawdown_halt"])
	// **策略自己的止损也要算进来。** 只数风控引擎那两个 tag 的话，
	// 网格海选里「止损 0 轮」会一直挂着 —— 而那不是「止损没触发」，
	// 是「这一列压根没在看网格的止损」。止损线定在哪是海选要回答的问题之一，
	// 答不了的话那个轴就成了摆设
	r.StopExits = int32(by["stop_loss"] + by["trailing_stop"] + by["grid_stop"])

	if m.InitialCents > 0 {
		r.FrictionRatio = float64(m.FeeCents+m.SlippageCents) / float64(m.InitialCents)
		r.OpenCostRatio = float64(m.Trades.OpenCostCents) / float64(m.InitialCents)
	}

	// 虚拟轮次：策略说该买、被自己的有效性判断挡下来的那些。
	//
	// **VirtualEdge = 实仓平均收益率 − 虚拟平均收益率**，为正才说明
	// 那棵有效性树该留（它挡掉的确实比放行的差）。为负就是在帮倒忙 ——
	// 而不装这一列的话，这件事一次也看不出来。
	//
	// 两边都用**逐轮**口径（每轮的收益率再取平均），不是总盈亏除以总成本 ——
	// 后者被大额轮次主导，与虚拟那边的逐笔价差口径对不上。
	vt := e.VirtualTrips()
	r.VirtualTrips = int32(len(vt))
	if len(vt) == 0 || m.Trades.RoundTrips == 0 {
		return
	}
	var vs float64
	for _, t := range vt {
		vs += t.Ratio
	}
	r.VirtualEdge = m.Trades.AvgReturnRatio - vs/float64(len(vt))
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
	if r.Err != "" || int(r.RoundTrips) < g.MinRoundTrips {
		return false
	}
	return r.GateReason(g) == ""
}

// GateReason 返回被门槛拦下的原因，通过则为空。
//
// **分开写是为了报告能说清楚被拦掉的是什么** —— 只报一个总数的话，
// 「是轮次太少还是强平太多」看不出来，而这两者要采取的行动完全不同。
func (r Result) GateReason(g Gate) string {
	if r.Err != "" {
		return "跑失败"
	}
	if int(r.RoundTrips) < g.MinRoundTrips {
		return "完整轮次不足"
	}
	if r.RoundTrips > 0 {
		if g.MaxLiquidationRatio > 0 {
			if ratio := float64(r.Liquidations) / float64(r.RoundTrips); ratio > g.MaxLiquidationRatio {
				return "强平占比过高"
			}
		}
		if g.MaxHaltExitRatio > 0 {
			if ratio := float64(r.HaltExits) / float64(r.RoundTrips); ratio > g.MaxHaltExitRatio {
				return "熔断清仓占比过高"
			}
		}
	}
	if g.MaxFrictionRatio > 0 && r.FrictionRatio > g.MaxFrictionRatio {
		return "摩擦占比过高"
	}
	return ""
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
// DoneWindows 扫描已有分片，返回哪些窗口**成功**跑完 —— 供续跑跳过。
//
// **失败的行不算跑完。** 从前只看 `Probe == 0`，于是一批全军覆没的运行
// （比如基准配置少了一段、网格路径写错）会被当成「已完成」，
// 修好病因之后再跑只会得到一句「已完成 17 个窗口，跳过」，
// 而报告里还是那批旧的失败 —— 人会以为没修好。
//
// 一个窗口里只要有失败的行，这个窗口就要重跑：失败往往是整批同因的，
// 挑着补比整窗重跑更容易留下不一致的半截结果。
func DoneWindows(dir string) map[int16]bool {
	done := map[int16]bool{}
	bad := map[int16]bool{}
	rows, err := ReadAll(dir)
	if err != nil {
		return done
	}
	for _, r := range rows {
		if r.Probe != 0 {
			continue
		}
		if r.Err != "" {
			bad[r.Window] = true
			continue
		}
		done[r.Window] = true
	}
	for w := range bad {
		delete(done, w)
	}
	return done
}
