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
	// CashCents 非 0 时覆盖初始资金 —— 噪声探针靠它扰动
	CashCents int64
}

// buildConfig 把 Job 的区间与扰动写进参数组的配置。
func (j Job) buildConfig(dir string) (*config.Config, error) {
	var obj map[string]any
	if err := decodeNumbers(j.Param.Config, &obj); err != nil {
		return nil, err
	}
	set := func(path string, v any) error { return setPath(obj, path, v) }
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
		// 所以这里直接往 engine 对象里塞
		engObj, ok := obj["engine"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("基准配置缺少 engine 段")
		}
		engObj["trade_from"] = json.Number(fmt.Sprint(j.TradeFrom))
	}
	if j.CashCents != 0 {
		if err := set("portfolio.initial_cash_cents",
			json.Number(fmt.Sprint(j.CashCents))); err != nil {
			return nil, err
		}
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
	r.fill(m, st, cfg, dataFP, e.ResultFingerprint())
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
		return nil, fmt.Errorf("数据只有 %d 个交易日，装不下 IS %d + OOS %d 天",
			len(days), is, oos)
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
	return out, nil
}

// TradingDays 取出数据里全部时点的交易日，升序。
func TradingDays(col *mktdata.Columns) []int32 {
	n := col.NumSteps()
	out := make([]int32, n)
	for i := 0; i < n; i++ {
		out[i] = col.StepAt(i).TradingDay
	}
	return out
}
