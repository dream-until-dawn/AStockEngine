package sweep

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// Job 是一次回测：一组参数 × 一个窗口 × 一个阶段。
type Job struct {
	Param ParamSet
	// Window 窗口编号；−1 表示整段跑（未开 Walk-Forward）
	Window int16
	// Phase 0=IS 1=OOS。整段跑时恒为 1（当作样本外看待没有意义，
	// 但整段跑本来就不区分，记 1 是为了让下游筛选统一）
	Phase int8
	// Probe 0=正式；≥1=噪声探针的第几次重复
	Probe int8
	// From / To 数据区间（**含预热前缀**）
	From, To int32
	// TradeFrom 这天之前只喂指标不交易
	TradeFrom int32
	// CashPPM 非 0 时把初始资金按百万分之 CashPPM 缩放 —— 噪声探针靠它扰动。
	//
	// **是倍率不是绝对值。** 从前这里是绝对的分数，而调用方写的是一个
	// 硬编码的 100_000_000（100 万元）—— 于是基准配置写 10 万本金时，
	// 噪声探针实际是在 100 万本金下跑的，量出来的基线根本不属于
	// 它要去衡量的那批结果。A 股 5 元最低佣金下这两档差着一个数量级。
	//
	// 用倍率还顺带解决另一件事：初始资金本身可以是网格的一个轴
	// （「本金够不够大」正是要扫的东西），绝对值会把那个轴覆盖掉
	CashPPM int64
	// Symbol 非空时把标的池换成这一只，并把基准也设成它自己。
	//
	// **这是「按标的海选」那个模式**：重复的维度不是时间窗口而是标的。
	// 目标不是「在某只标的上最优」，而是「在任意一只上都还行」——
	// 于是中位数与四分位距要在**标的之间**取，而不是在窗口之间取。
	//
	// 单标的的基准就是它自己：跟大盘比没有意义，
	// 要答的是「这套网格参数比一直拿着强多少」
	Symbol string
}

// buildConfig 把 Job 的区间与扰动写进参数组的配置。
func (j Job) buildConfig(dir string) (*config.Config, error) {
	var obj map[string]any
	if err := decodeNumbers(j.Param.Config, &obj); err != nil {
		return nil, err
	}
	set := func(path string, v any) error { return setPath(obj, path, v) }
	if j.Symbol != "" {
		// 标的池整个换掉而不是往里加 —— 按标的海选时每次只跑一只
		uni, _ := obj["data"].(map[string]any)
		if uni == nil {
			return nil, fmt.Errorf("基准配置缺少 data 段")
		}
		uni["universe"] = map[string]any{"symbols": []any{j.Symbol}}
		if m, ok := obj["metrics"].(map[string]any); ok {
			m["benchmark"] = j.Symbol
		} else {
			obj["metrics"] = map[string]any{"benchmark": j.Symbol}
		}
	}
	if j.From != 0 {
		if err := set("data.from", json.Number(fmt.Sprint(j.From))); err != nil {
			return nil, err
		}
	}
	if err := set("data.to", json.Number(fmt.Sprint(j.To))); err != nil {
		return nil, err
	}
	if j.TradeFrom != 0 {
		// engine.trade_from 在基准配置里可能没写，setPath 只改已有字段，
		// 所以这里直接往 engine 对象里塞 —— **没有就建一个**。
		//
		// 从前这里是报错。而 `engine` 段是可选的（引擎自己有默认值），
		// 于是任何一份没写它的配置都跑不了海选，报的还是
		// 「基准配置缺少 engine 段」—— 实测一整批 8,687 次全挂在这一句上，
		// 而那份配置作为普通回测跑得好好的。
		//
		// trade_from 是**海选自己要写的字段**，不是用户可能拼错的名字，
		// 所以这里放宽是安全的：setPath 的严格性是防拼错，不适用于这里。
		engObj, ok := obj["engine"].(map[string]any)
		if !ok {
			if _, exists := obj["engine"]; exists {
				return nil, fmt.Errorf("基准配置的 engine 不是对象")
			}
			engObj = map[string]any{}
			obj["engine"] = engObj
		}
		engObj["trade_from"] = json.Number(fmt.Sprint(j.TradeFrom))
	}
	if j.CashPPM != 0 {
		pf, _ := obj["portfolio"].(map[string]any)
		if pf == nil {
			return nil, fmt.Errorf("基准配置缺少 portfolio 段")
		}
		cur, ok := toInt64(pf["initial_cash_cents"])
		if !ok {
			return nil, fmt.Errorf("portfolio.initial_cash_cents 不是整数")
		}
		pf["initial_cash_cents"] = json.Number(
			fmt.Sprint(cur * j.CashPPM / 1_000_000))
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return config.Parse(b, dir)
}

// Progress 由驱动在每完成一次回测时调用。
type Progress func(done, total int, r Result)

// RunJobs 并发跑一批 Job。
//
// **调用方必须保证这批 Job 共用同一份已裁好的 ds** —— 这是本包的核心前提。
// v0.3.4 实测：每个 worker 各自 Subset 一份是 358 MB，8 个就是 2.8 GB，
// 而共享之后每个 worker 的边际只有 2.7 MB。
// 所以外层按**窗口**分批（窗口串行、参数组并发），不能反过来。
func RunJobs(
	ds *config.DataSet, jobs []Job, dir string, workers int, sweepID, dataFP string,
	onDone Progress,
) []Result {
	if workers <= 0 {
		workers = runtime.NumCPU() - 2
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	out := make([]Result, len(jobs))
	var done int64
	var wg sync.WaitGroup
	ch := make(chan int, len(jobs))
	for i := range jobs {
		ch <- i
	}
	close(ch)

	var mu sync.Mutex // 只保护 onDone —— 回调多半要打印，不能并发写
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				out[i] = runOne(ds, jobs[i], dir, sweepID, dataFP)
				n := atomic.AddInt64(&done, 1)
				if onDone != nil {
					mu.Lock()
					onDone(int(n), len(jobs), out[i])
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return out
}

// runOne 跑一次，**任何错误都变成一行带 Err 的结果而不是中断整批**。
//
// 海选跑几千次，其中一组参数装配失败（比如某个窗口里标的池为空）
// 不该让另外几千次白跑。失败的行留在结果表里，比消失掉更有用 ——
// 「这组参数在这个窗口跑不了」本身就是一个发现。
func runOne(ds *config.DataSet, j Job, dir, sweepID, dataFP string) Result {
	r := Result{
		SweepID: sweepID, ParamID: j.Param.ID, ParamFP: j.Param.FP,
		Params: string(j.Param.JSON),
		Window: j.Window, Phase: j.Phase, Probe: j.Probe,
	}
	t0 := time.Now()
	cfg, err := j.buildConfig(dir)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	e, m, st, err := config.RunToEnd(cfg, ds)
	r.ElapsedMS = int32(time.Since(t0).Milliseconds())
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.fill(e, m, st, cfg, dataFP, e.ResultFingerprint())
	return r
}

// ---- 切窗 ----

// Window 是一个 Walk-Forward 窗口。
type Window struct {
	Index int16
	// DataFrom 含预热前缀的数据起点；ISFrom 才是开始交易的日子
	DataFrom int32
	ISFrom   int32
	ISTo     int32
	OOSFrom  int32
	OOSTo    int32
}

// MakeWindows 由交易日序列切出 Walk-Forward 窗口。
//
// **按交易日切而不是按自然日切**：一年的交易日数是实测出来的
// （A 股约 242.9 天），按自然日切会让不同年份的窗口样本量不同。
//
// days 必须是升序的全部交易日。
func MakeWindows(days []int32, wf WalkForward, annualDays float64) ([]Window, error) {
	if !wf.Enabled {
		return nil, nil
	}
	if len(days) == 0 {
		return nil, fmt.Errorf("没有交易日")
	}
	if annualDays <= 0 {
		annualDays = 243
	}
	is := int(wf.ISYears * annualDays)
	oos := int(wf.OOSYears * annualDays)
	step := int(wf.StepYears * annualDays)
	if is <= 0 || oos <= 0 || step <= 0 {
		return nil, fmt.Errorf("窗口长度算出来是 0：is=%d oos=%d step=%d", is, oos, step)
	}
	if is+oos > len(days) {
		return nil, fmt.Errorf("数据只有 %d 个交易日（约 %.2f 年），"+
			"装不下 IS %d + OOS %d 天 —— 换更短的窗口参数，或换区间更长的市场",
			len(days), float64(len(days))/annualDays, is, oos)
	}

	var out []Window
	for start := 0; start+is+oos <= len(days); start += step {
		warm := start - wf.WarmupDays
		if warm < 0 {
			warm = 0
		}
		out = append(out, Window{
			Index:    int16(len(out)),
			DataFrom: days[warm],
			ISFrom:   days[start],
			ISTo:     days[start+is-1],
			OOSFrom:  days[start+is],
			OOSTo:    days[start+is+oos-1],
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("切不出任何窗口")
	}
	// 窗口数下限。**不是保守，是这套方法论的前提**：Walk-Forward 的产出是
	// OOS 的中位数与四分位距，而三五个数算不出有意义的分布 ——
	// 报告照样会印出一个中位数，看不出它只基于 3 个窗口。
	min := wf.MinWindows
	if min == 0 {
		min = DefaultMinWindows
	}
	if len(out) < min {
		return nil, fmt.Errorf(
			"只切出 %d 个窗口，少于下限 %d —— 数据 %d 个交易日（约 %.2f 年），"+
				"IS %.2f 年 / OOS %.2f 年 / 步进 %.2f 年。"+
				"少于 %d 个窗口的 OOS 中位数与四分位距没有意义，"+
				"报告却不会告诉你这一点。请调短窗口参数，"+
				"或把 walk_forward.min_windows 显式调低（你得知道自己在放弃什么）",
			len(out), min, len(days), float64(len(days))/annualDays,
			wf.ISYears, wf.OOSYears, wf.StepYears, min)
	}
	return out, nil
}

// DefaultMinWindows 窗口数下限的默认值。
//
// 6 是个下限而不是目标：A 股 21.6 年按 IS3/OOS1/步1 有 18 个窗口，
// 加密 6.66 年按 IS1.5/OOS0.5/步0.5 有 10 个。低于 6 的话，
// 一两个窗口的运气就能主导中位数。
const DefaultMinWindows = 6

// TradingDays 取出数据里全部时点的交易日，升序。
func TradingDays(col *mktdata.Columns) []int32 {
	n := col.NumSteps()
	out := make([]int32, n)
	for i := 0; i < n; i++ {
		out[i] = col.StepAt(i).TradingDay
	}
	return out
}
