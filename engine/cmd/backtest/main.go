// Command backtest 按一份 JSON 配置跑一次完整回测：
// 信号 → 定量 → 风控 → 撮合 → 记账 → 净值序列。
//
// v0.3 起**装配全由配置决定**：换 sizer、加风控规则、改费率都不需要重新编译。
// 命令行只剩「配置在哪」与「结果往哪写」。
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/fingerprint"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/metrics"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/record"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"

	// 策略经 init() 注册进 engine.Strategies，必须导入才会生效。
	// 这是唯一需要空导入的地方 —— trading 的模块随包一起进来。
	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
)

// gitCommit 由构建时注入：
//
//	go build -ldflags "-X main.gitCommit=$(git rev-parse HEAD)" ./cmd/backtest
//
// `go run` 拿不到它，指纹会退化为 dev 并在报告里标明不可复现。
var gitCommit string

func main() {
	fingerprint.SetEngineVersion(gitCommit)

	cfgPath := flag.String("config", "", "配置文件路径（必填）")
	equityOut := flag.String("equity-out", "", "净值序列输出 CSV 路径")
	snapAt := flag.Int("snapshot-at", 0, "在第 N 步做快照并验证往返")
	listModules := flag.Bool("modules", false, "列出全部可用模块与参数规格后退出")
	flag.Parse()

	if *listModules {
		printModules()
		return
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "错误: 必须用 -config 指定配置文件")
		fmt.Fprintln(os.Stderr, "      用 -modules 查看可用模块")
		os.Exit(1)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}

	t0 := time.Now()
	ds, err := config.LoadDataSet(cfg)
	if err != nil {
		fatal(err)
	}
	loadDur := time.Since(t0)

	e, err := cfg.Assemble(ds)
	if err != nil {
		fatal(err)
	}
	for _, line := range cfg.Describe(e, ds, loadDur) {
		fmt.Println(line)
	}

	// 输入指纹在**跑之前**就能算出来 —— 它只依赖配置、数据与引擎版本。
	// 先印出来，跑完再印输出指纹，两者配成一条可验证的记录。
	dataFP, nFiles, err := fingerprint.Data(cfg.DataRoot())
	if err != nil {
		fatal(err)
	}
	inputFP, err := cfg.InputFingerprint(dataFP)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("指纹    输入 %s   数据 %s（%d 个 parquet）  引擎 %s\n",
		fingerprint.Short(inputFP), fingerprint.Short(dataFP), nFiles,
		fingerprint.EngineVersion())
	if !fingerprint.Reproducible() {
		// 必须说出来：两次 dev 构建之间源码可能已经变了，
		// 指纹相同并不代表结果可复现
		fmt.Println("        [!] dev 构建，指纹不保证跨构建可复现" +
			"（用 -ldflags 注入 git commit 可解）")
	}
	fmt.Println()

	// ---- 主回测 ----
	//
	// 逐步记录由**引擎内的 Recorder** 负责，这里只做拒单归类与快照 ——
	// 记录逻辑写在驱动里就得写三遍（单步 / 海选 / 实盘），而三者
	// 共用同一个核心正是 C4 的意义。
	initial := cfg.Portfolio.InitialCashCents
	var snap []byte
	var signals, fills, rejects int
	rejectBy := map[string]int{}

	t1 := time.Now()
	for !e.Done() {
		if _, err := e.Step(); err != nil {
			fatal(err)
		}
		nf, nr := e.LastCounts()
		signals += len(e.Signals())
		fills += nf
		rejects += nr
		for _, r := range e.Rejections() {
			rejectBy[reasonKey(r)]++
		}
		if *snapAt > 0 && e.Steps() == *snapAt {
			var err error
			if snap, err = e.Snapshot(); err != nil {
				fatal(err)
			}
		}
	}
	dur := time.Since(t1)
	rec := e.Recorder()

	led := e.Ledger()
	final := e.EquityCents()
	fmt.Println("=== 结果 ===")
	fmt.Printf("  步数 %d  耗时 %v\n", e.Steps(), dur.Round(time.Millisecond))
	fmt.Printf("  信号 %d 条  成交 %d 笔  拒单 %d 笔\n", signals, fills, rejects)
	fmt.Printf("  初始 %.2f 元 → 权益 %.2f 元（%+.2f%%）\n",
		cents(initial), cents(final), float64(final-initial)/float64(initial)*100)
	fmt.Printf("  现金 %.2f 元  持仓 %d 只  已实现 %.2f 元\n",
		cents(led.CashCents()), led.NumPositions(), cents(led.RealizedCents()))
	fmt.Printf("  峰值权益 %.2f 元\n", cents(e.PeakEquityCents()))
	fmt.Printf("  费用合计 %.2f 元（占初始 %.2f%%）",
		cents(led.TotalFeeCents()), float64(led.TotalFeeCents())/float64(initial)*100)
	fees := led.FeeCents()
	for _, k := range sortedKeys(fees) {
		fmt.Printf("  %s %.2f", k, cents(fees[k]))
	}
	fmt.Println()
	// 滑点单列。它以前藏在成交价里，报告只看得到佣金印花税，
	// 看不到执行摩擦到底吃掉多少 —— 而它经常比佣金还大
	fmt.Printf("  滑点合计 %.2f 元（占初始 %.2f%%）  摩擦合计 %.2f 元\n",
		cents(led.SlippageCents()), float64(led.SlippageCents())/float64(initial)*100,
		cents(led.TotalFeeCents()+led.SlippageCents()))

	if len(rejectBy) > 0 {
		fmt.Println("  拒单原因分布：")
		for _, k := range sortedStrKeys(rejectBy) {
			fmt.Printf("    %-24s %d\n", k, rejectBy[k])
		}
	}
	if w := led.Warnings(); len(w) > 0 {
		fmt.Printf("  账本告警 %d 条，首条：%s\n", len(w), w[0])
	}

	if m, ok := rec.(*record.Memory); ok {
		if len(m.Warnings) > 0 {
			fmt.Println(warnLine(m.Warnings))
		}
	}
	fmt.Println()

	// ---- 绩效 ----
	if rec.Level() == record.None {
		fmt.Println("=== 绩效 ===")
		fmt.Println("  recorder.level = none，没有逐步记录，算不了绩效指标。")
		fmt.Println("  海选内层用 none 省内存；要看指标请改成 summary。")
		fmt.Println()
	} else {
		printReport(computeMetrics(cfg, ds, rec))
	}

	fmt.Println("=== 指纹（C5）===")
	fmt.Printf("  输入 %s\n", inputFP)
	fmt.Printf("  输出 %s\n", e.ResultFingerprint())
	fmt.Println("  同输入指纹必须给出同输出指纹。对不上就是可复现性出了问题。")
	fmt.Println()

	if *equityOut != "" {
		if err := writeCurve(*equityOut, rec.Steps()); err != nil {
			fatal(err)
		}
		fmt.Printf("净值序列已写出 -> %s\n\n", *equityOut)
	}

	// ---- 快照往返 ----
	if snap != nil {
		fmt.Printf("=== 快照往返（第 %d 步，%.2f MB）===\n", *snapAt, float64(len(snap))/1024/1024)
		e2, err := cfg.Assemble(ds)
		if err != nil {
			fatal(err)
		}
		if err := e2.Restore(snap); err != nil {
			fatal(err)
		}
		if err := e2.RunAll(); err != nil {
			fatal(err)
		}
		b := e2.Ledger()
		fmt.Printf("  全程 现金 %.2f / 已实现 %.2f / 峰值 %.2f\n",
			cents(led.CashCents()), cents(led.RealizedCents()), cents(e.PeakEquityCents()))
		fmt.Printf("  恢复 现金 %.2f / 已实现 %.2f / 峰值 %.2f\n",
			cents(b.CashCents()), cents(b.RealizedCents()), cents(e2.PeakEquityCents()))
		fpFull, fpRestored := e.ResultFingerprint(), e2.ResultFingerprint()
		fmt.Printf("  全程 指纹 %s\n", fpFull)
		fmt.Printf("  恢复 指纹 %s\n", fpRestored)
		ok := led.CashCents() == b.CashCents() &&
			led.RealizedCents() == b.RealizedCents() &&
			e.PeakEquityCents() == e2.PeakEquityCents()
		if ok && fpFull == fpRestored {
			// 指纹相同才是真的一致：账本三个数相同只说明结果相同，
			// 指纹相同才说明**逐笔成交**都相同（C5 + C6 合起来的要求）
			fmt.Println("  [OK] 快照恢复后继续步进，账本与逐笔成交均与全程一致")
		} else if ok {
			fmt.Println("  [!!] 账本一致但指纹不同 —— 中间某笔成交不同，只是恰好抵消了")
			os.Exit(1)
		} else {
			fmt.Println("  [XX] 不一致")
			os.Exit(1)
		}
	}
}

// reasonKey 把拒单归类。风控拦截要细到规则名 ——
// 一堆「风控拦截」堆在一起等于没说。
func reasonKey(r trading.Rejection) string {
	if r.Reason == trading.RejectRisk && r.Rule != "" {
		return "风控:" + r.Rule
	}
	return r.Reason.String()
}

func printModules() {
	fmt.Println("可用模块（配置里按 impl 名选用）：")
	fmt.Printf("\n  market    %s\n", strings.Join(trading.Markets.Names(), " / "))
	fmt.Printf("  fee       %s\n", strings.Join(trading.Fees.Names(), " / "))
	fmt.Printf("  slippage  %s\n", strings.Join(trading.Slippages.Names(), " / "))
	fmt.Printf("  sizer     %s\n", strings.Join(trading.Sizers.Names(), " / "))
	fmt.Printf("  risk      %s\n", strings.Join(trading.Risks.Names(), " / "))
	fmt.Printf("  strategy  %s\n\n", strings.Join(eng.Strategies.Names(), " / "))

	show := func(kind string, names []string, get func(string) ([]eng.ParamSpec, bool)) {
		for _, n := range names {
			specs, _ := get(n)
			if len(specs) == 0 {
				fmt.Printf("  %s.%s  (无参数)\n", kind, n)
				continue
			}
			fmt.Printf("  %s.%s\n", kind, n)
			for _, s := range specs {
				if s.Kind == eng.ParamString {
					fmt.Printf("      %-16s %-6s 默认 %-14s %v  %s\n",
						s.Name, s.Kind, s.DefaultStr, s.Options, s.Desc)
					continue
				}
				fmt.Printf("      %-16s %-6s 默认 %-14g [%g, %g]  %s\n",
					s.Name, s.Kind, s.Default, s.Min, s.Max, s.Desc)
			}
		}
	}
	fmt.Println("参数规格：")
	show("sizer", trading.Sizers.Names(), trading.Sizers.Specs)
	show("risk", trading.Risks.Names(), trading.Risks.Specs)
	show("slippage", trading.Slippages.Names(), trading.Slippages.Specs)
	show("fee", trading.Fees.Names(), trading.Fees.Specs)
	show("strategy", eng.Strategies.Names(), eng.Strategies.Specs)
}

// computeMetrics 把记录喂给绩效模块。
//
// 年化系数**由日历数出来**（按本次样本区间实测），不是 252 ——
// 252 是美股惯例，A 股实测年均 242.90 天。
func computeMetrics(cfg *config.Config, ds *config.DataSet, rec record.Recorder) metrics.Result {
	m, _ := rec.(*record.Memory)
	days, eq := m.Curve()
	in := metrics.Input{
		Curve:        metrics.Curve{Days: days, Equity: eq},
		InitialCents: cfg.Portfolio.InitialCashCents,
		Fills:        rec.Fills(),
		RiskFreePPM:  cfg.Metrics.RiskFreePPM,
	}
	var from, to int32
	if len(days) > 0 {
		from, to = days[0], days[len(days)-1]
	}
	in.TradingDaysPerYear = ds.Calendar.TradingDaysPerYear(mktdata.MarketAShare, from, to)

	if bd, be, ok := ds.BenchmarkCurve(); ok {
		in.Benchmark = &metrics.Curve{Days: bd, Equity: be}
		in.BenchmarkName = cfg.Metrics.Benchmark
	}
	return metrics.Compute(in)
}

func writeCurve(path string, curve []record.Step) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	defer bw.Flush()
	w := csv.NewWriter(bw)
	defer w.Flush()

	// 输出整数分，不做单位换算 —— 下游要什么精度自己定
	if err := w.Write([]string{"trading_day", "equity_cents", "cash_cents",
		"positions", "signals", "fills", "rejects"}); err != nil {
		return err
	}
	for _, p := range curve {
		if err := w.Write([]string{
			strconv.FormatInt(int64(p.Time.TradingDay), 10),
			strconv.FormatInt(p.EquityCents, 10),
			strconv.FormatInt(p.CashCents, 10),
			strconv.Itoa(p.Positions),
			strconv.Itoa(p.NumSignals),
			strconv.Itoa(p.NumFills),
			strconv.Itoa(p.NumRejects),
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

func cents(v int64) float64 { return float64(v) / 100 }

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
