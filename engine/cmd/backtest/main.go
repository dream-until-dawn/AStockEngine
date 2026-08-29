// Command backtest 跑一次完整回测：信号 → 订单 → 成交 → 记账 → 净值序列。
//
// 这是 v0.2 的端到端入口。**它只输出原始净值序列，不算绩效指标** ——
// 夏普、最大回撤、胜率属 v0.3 的 Metrics 模块。
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

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// equityPoint 是净值曲线上的一个点。全部为整数（分），
// 保留原始精度供下游自行换算，避免在这里就损失信息。
type equityPoint struct {
	TradingDay   int32
	EquityCents  int64
	CashCents    int64
	Positions    int
	FillsToday   int
	RejectsToday int
}

func main() {
	dataRoot := flag.String("data", "../data", "数据根目录")
	feePath := flag.String("fee", "../configs/fee/ashare_default.json", "费率配置")
	stratName := flag.String("strategy", "macd_cross",
		"策略："+strings.Join(strategies.Names(), " / "))
	nInst := flag.Int("instruments", 300, "抽样标的数，0 表示全部")
	from := flag.Int("from", 20200101, "起始交易日")
	to := flag.Int("to", 0, "结束交易日，0 表示不限")
	cashYuan := flag.Int64("cash", 1_000_000, "初始资金（元）")
	maxHold := flag.Int("max-hold", 10, "最多同时持有")
	volCapPct := flag.Float64("volume-cap", 10, "单笔成交占当日成交量上限（%）")
	slipBps := flag.Int64("slippage-bps", 5, "滑点（基点）")
	taxPct := flag.Float64("dividend-tax", 0, "红利税率（%）")
	equityOut := flag.String("equity-out", "", "净值序列输出 CSV 路径")
	snapAt := flag.Int("snapshot-at", 0, "在第 N 步做快照并验证往返")
	flag.Parse()

	mk, ok := strategies.Registry[*stratName]
	if !ok {
		fatal(fmt.Errorf("未知策略 %q，可选：%s", *stratName,
			strings.Join(strategies.Names(), " / ")))
	}

	// ---- 加载数据与配置 ----
	metaDir := filepath.Join(*dataRoot, "meta")
	uni, err := mktdata.LoadUniverse(filepath.Join(metaDir, "instruments.parquet"))
	if err != nil {
		fatal(err)
	}
	adj, err := mktdata.LoadAdjuster(filepath.Join(metaDir, "adj_factor.parquet"))
	if err != nil {
		fatal(err)
	}
	corp, err := mktdata.LoadCorpActions(filepath.Join(metaDir, "corporate_action.parquet"))
	if err != nil {
		fatal(err)
	}
	fee, err := trading.LoadFee(*feePath)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("标的 %d 只 | 复权因子 %d 只 | 公司行动 %d 条 | 费率 %q\n",
		uni.Len(), adj.NumInstruments(), corp.Total(), fee.Name())

	opt := mktdata.LoadOptions{
		Root:    filepath.Join(*dataRoot, "bar", "market=ashare", "freq=1d"),
		FromDay: int32(*from), ToDay: int32(*to),
	}
	if *nInst > 0 {
		var ids []mktdata.InstrumentID
		for _, in := range uni.All() {
			// 只取个股且有复权事件 —— 这样公司行动分支才会被真正走到
			if in.Type != mktdata.TypeStock || len(adj.ExDates(in.ID)) == 0 {
				continue
			}
			ids = append(ids, in.ID)
			if len(ids) >= *nInst {
				break
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		opt.Instruments = ids
	}
	col, st, err := mktdata.Load(opt)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("加载 %d 行 / %d 只 / %d 个时点 / %v\n\n",
		st.Rows, st.Instruments, st.Steps, st.Total.Round(time.Millisecond))

	initial := *cashYuan * 100
	params := eng.Params{
		"max_hold":   float64(*maxHold),
		"cash_cents": float64(initial),
	}

	build := func() *eng.Engine {
		br := trading.NewBroker(trading.NewAShareMarket(), fee,
			trading.BpsSlippage{Bps: *slipBps},
			trading.BrokerConfig{
				VolumeCapPPM:     int64(*volCapPct * 10_000),
				AllowPartialFill: true,
			})
		e, err := eng.New(eng.Deps{
			Columns: col, Universe: uni, Adjuster: adj, CorpAct: corp,
			Market: trading.NewAShareMarket(), Broker: br,
			Portfolio: trading.NewPortfolio(initial),
		}, mk(), eng.Config{
			Params:               params,
			IndicatorAdjMode:     mktdata.AdjHFQ,
			InitialCashCents:     initial,
			DividendTaxPPM:       int64(*taxPct * 10_000),
			ImplySplitFromFactor: true,
		})
		if err != nil {
			fatal(err)
		}
		return e
	}

	// ---- 主回测 ----
	e := build()
	curve := make([]equityPoint, 0, col.NumSteps())
	var snap []byte
	var fills, rejects int
	rejectBy := map[string]int{}

	t0 := time.Now()
	for !e.Done() {
		tp, err := e.Step()
		if err != nil {
			fatal(err)
		}
		nf, nr := e.LastCounts()
		fills += nf
		rejects += nr
		for _, r := range e.Rejections() {
			rejectBy[r.Reason.String()]++
		}
		pf := e.Portfolio()
		curve = append(curve, equityPoint{
			TradingDay: tp.TradingDay, EquityCents: e.EquityCents(),
			CashCents: pf.Cash, Positions: countPositions(pf),
			FillsToday: nf, RejectsToday: nr,
		})
		if *snapAt > 0 && e.Steps() == *snapAt {
			if snap, err = e.Snapshot(); err != nil {
				fatal(err)
			}
		}
	}
	dur := time.Since(t0)

	pf := e.Portfolio()
	final := e.EquityCents()
	fmt.Printf("=== %s ===\n", *stratName)
	fmt.Printf("  步数 %d  耗时 %v\n", e.Steps(), dur.Round(time.Millisecond))
	fmt.Printf("  成交 %d 笔  拒单 %d 笔\n", fills, rejects)
	fmt.Printf("  初始 %.2f 元 → 权益 %.2f 元（%+.2f%%）\n",
		cents(initial), cents(final), float64(final-initial)/float64(initial)*100)
	fmt.Printf("  现金 %.2f 元  持仓 %d 只  已实现 %.2f 元\n",
		cents(pf.Cash), countPositions(pf), cents(pf.RealizedCents))
	fmt.Printf("  费用合计 %.2f 元（占初始 %.2f%%）",
		cents(pf.TotalFeeCents()), float64(pf.TotalFeeCents())/float64(initial)*100)
	for _, k := range sortedKeys(pf.FeeCents) {
		fmt.Printf("  %s %.2f", k, cents(pf.FeeCents[k]))
	}
	fmt.Println()

	if len(rejectBy) > 0 {
		fmt.Println("  拒单原因分布：")
		for _, k := range sortedStrKeys(rejectBy) {
			fmt.Printf("    %-16s %d\n", k, rejectBy[k])
		}
	}
	if n := len(pf.Warnings); n > 0 {
		fmt.Printf("  账本告警 %d 条，首条：%s\n", n, pf.Warnings[0])
	}

	// 净值序列的简单描述性统计。**不是绩效指标** —— 那是 v0.3 的事，
	// 这里只报告序列本身的形态，便于确认曲线是否合理。
	if len(curve) > 0 {
		peak, maxDD := curve[0].EquityCents, 0.0
		for _, p := range curve {
			if p.EquityCents > peak {
				peak = p.EquityCents
			}
			if peak > 0 {
				dd := float64(peak-p.EquityCents) / float64(peak)
				if dd > maxDD {
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
		e2 := build()
		if err := e2.Restore(snap); err != nil {
			fatal(err)
		}
		if err := e2.RunAll(); err != nil {
			fatal(err)
		}
		b := e2.Portfolio()
		fmt.Printf("  全程 现金 %.2f / 已实现 %.2f\n", cents(pf.Cash), cents(pf.RealizedCents))
		fmt.Printf("  恢复 现金 %.2f / 已实现 %.2f\n", cents(b.Cash), cents(b.RealizedCents))
		if pf.Cash == b.Cash && pf.RealizedCents == b.RealizedCents {
			fmt.Println("  ✅ 快照恢复后继续步进，账本与全程完全一致")
		} else {
			fmt.Println("  ❌ 不一致")
			os.Exit(1)
		}
	}
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
		"positions", "fills", "rejects"}); err != nil {
		return err
	}
	for _, p := range curve {
		if err := w.Write([]string{
			strconv.FormatInt(int64(p.TradingDay), 10),
			strconv.FormatInt(p.EquityCents, 10),
			strconv.FormatInt(p.CashCents, 10),
			strconv.Itoa(p.Positions),
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
