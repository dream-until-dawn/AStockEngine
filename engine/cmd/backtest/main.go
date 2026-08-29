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
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"

	// 策略经 init() 注册进 engine.Strategies，必须导入才会生效。
	// 这是唯一需要空导入的地方 —— trading 的模块随包一起进来。
	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
)

// equityPoint 是净值曲线上的一个点。全部为整数（分），
// 保留原始精度供下游自行换算，避免在这里就损失信息。
type equityPoint struct {
	TradingDay   int32
	EquityCents  int64
	CashCents    int64
	Positions    int
	SignalsToday int
	FillsToday   int
	RejectsToday int
}

func main() {
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
	fmt.Println()

	// ---- 主回测 ----
	initial := cfg.Portfolio.InitialCashCents
	curve := make([]equityPoint, 0, 4096)
	var snap []byte
	var signals, fills, rejects int
	rejectBy := map[string]int{}

	t1 := time.Now()
	for !e.Done() {
		tp, err := e.Step()
		if err != nil {
			fatal(err)
		}
		ns := len(e.Signals())
		nf, nr := e.LastCounts()
		signals += ns
		fills += nf
		rejects += nr
		for _, r := range e.Rejections() {
			rejectBy[reasonKey(r)]++
		}
		pf := e.Portfolio()
		curve = append(curve, equityPoint{
			TradingDay: tp.TradingDay, EquityCents: e.EquityCents(),
			CashCents: pf.Cash, Positions: countPositions(pf),
			SignalsToday: ns, FillsToday: nf, RejectsToday: nr,
		})
		if *snapAt > 0 && e.Steps() == *snapAt {
			if snap, err = e.Snapshot(); err != nil {
				fatal(err)
			}
		}
	}
	dur := time.Since(t1)

	pf := e.Portfolio()
	final := e.EquityCents()
	fmt.Println("=== 结果 ===")
	fmt.Printf("  步数 %d  耗时 %v\n", e.Steps(), dur.Round(time.Millisecond))
	fmt.Printf("  信号 %d 条  成交 %d 笔  拒单 %d 笔\n", signals, fills, rejects)
	fmt.Printf("  初始 %.2f 元 → 权益 %.2f 元（%+.2f%%）\n",
		cents(initial), cents(final), float64(final-initial)/float64(initial)*100)
	fmt.Printf("  现金 %.2f 元  持仓 %d 只  已实现 %.2f 元\n",
		cents(pf.Cash), countPositions(pf), cents(pf.RealizedCents))
	fmt.Printf("  峰值权益 %.2f 元\n", cents(e.PeakEquityCents()))
	fmt.Printf("  费用合计 %.2f 元（占初始 %.2f%%）",
		cents(pf.TotalFeeCents()), float64(pf.TotalFeeCents())/float64(initial)*100)
	for _, k := range sortedKeys(pf.FeeCents) {
		fmt.Printf("  %s %.2f", k, cents(pf.FeeCents[k]))
	}
	fmt.Println()
	// 滑点单列。它以前藏在成交价里，报告只看得到佣金印花税，
	// 看不到执行摩擦到底吃掉多少 —— 而它经常比佣金还大
	fmt.Printf("  滑点合计 %.2f 元（占初始 %.2f%%）  摩擦合计 %.2f 元\n",
		cents(pf.SlippageCents), float64(pf.SlippageCents)/float64(initial)*100,
		cents(pf.TotalFeeCents()+pf.SlippageCents))

	if len(rejectBy) > 0 {
		fmt.Println("  拒单原因分布：")
		for _, k := range sortedStrKeys(rejectBy) {
			fmt.Printf("    %-24s %d\n", k, rejectBy[k])
		}
	}
	if n := len(pf.Warnings); n > 0 {
		fmt.Printf("  账本告警 %d 条，首条：%s\n", n, pf.Warnings[0])
	}

	// 净值序列的简单描述性统计。**不是绩效指标** —— 那是第二刀的 Metrics 模块，
	// 这里只报告序列本身的形态，便于确认曲线是否合理。
	if len(curve) > 0 {
		peak, maxDD := curve[0].EquityCents, 0.0
		for _, p := range curve {
			if p.EquityCents > peak {
				peak = p.EquityCents
			}
			if peak > 0 {
				if dd := float64(peak-p.EquityCents) / float64(peak); dd > maxDD {
					maxDD = dd
				}
			}
		}
		fmt.Printf("  净值序列 %d 点，区间 %d ~ %d，峰值回撤 %.2f%%\n",
			len(curve), curve[0].TradingDay, curve[len(curve)-1].TradingDay, maxDD*100)
	}
	fmt.Println()

	if *equityOut != "" {
		if err := writeCurve(*equityOut, curve); err != nil {
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
		b := e2.Portfolio()
		fmt.Printf("  全程 现金 %.2f / 已实现 %.2f / 峰值 %.2f\n",
			cents(pf.Cash), cents(pf.RealizedCents), cents(e.PeakEquityCents()))
		fmt.Printf("  恢复 现金 %.2f / 已实现 %.2f / 峰值 %.2f\n",
			cents(b.Cash), cents(b.RealizedCents), cents(e2.PeakEquityCents()))
		if pf.Cash == b.Cash && pf.RealizedCents == b.RealizedCents &&
			e.PeakEquityCents() == e2.PeakEquityCents() {
			fmt.Println("  ✅ 快照恢复后继续步进，账本与全程完全一致")
		} else {
			fmt.Println("  ❌ 不一致")
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

func writeCurve(path string, curve []equityPoint) error {
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
			strconv.FormatInt(int64(p.TradingDay), 10),
			strconv.FormatInt(p.EquityCents, 10),
			strconv.FormatInt(p.CashCents, 10),
			strconv.Itoa(p.Positions),
			strconv.Itoa(p.SignalsToday),
			strconv.Itoa(p.FillsToday),
			strconv.Itoa(p.RejectsToday),
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

func cents(v int64) float64 { return float64(v) / 100 }

func countPositions(pf *trading.Portfolio) int {
	n := 0
	for _, p := range pf.Positions {
		if p.Total > 0 {
			n++
		}
	}
	return n
}

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
