// Command backtest 跑一次完整回测：信号 → 订单 → 成交 → 记账 → 权益曲线。
//
// 这是 v0.2 第二刀的端到端验证。示例策略用 MACD 金叉买入、死叉卖出，
// 等权分配到有限只标的上 —— 策略本身不是重点，重点是链路跑通且账目自洽。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// macdStrategy 是最简策略：MACD 金叉买入、死叉清仓，仓位等权。
//
// **它刻意只保留一项自有状态**（prevAbove）。「持有哪些标的」与
// 「哪些单在途」都从 StepContext 导出 —— 那些状态本就归引擎管，
// 策略自己再存一份，快照恢复时就会两边不一致。
// 首版正是这么写的，端到端回测立刻暴露：恢复后的策略不知道自己持有什么，
// 于是重复下单，账本偏离全程结果。
type macdStrategy struct {
	maxHold   int
	slotCents int64

	// prevAbove 是**真正的**跨步记忆：上一步 DIF 是否在 DEA 之上。
	// 无法从 ctx 导出，故必须实现 StatefulStrategy。
	prevAbove map[mktdata.InstrumentID]bool

	signals, buys, sells int
}

func (s *macdStrategy) Name() string { return "macd_cross" }

func (s *macdStrategy) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "short", Kind: eng.ParamInt, Default: 12, Min: 2, Max: 60, Step: 1, Desc: "快线周期"},
		{Name: "long", Kind: eng.ParamInt, Default: 26, Min: 5, Max: 200, Step: 1, Desc: "慢线周期"},
		{Name: "signal", Kind: eng.ParamInt, Default: 9, Min: 2, Max: 60, Step: 1, Desc: "信号周期"},
		{Name: "max_hold", Kind: eng.ParamInt, Default: 10, Min: 1, Max: 100, Step: 1, Desc: "最多同时持有"},
	}
}

func (s *macdStrategy) Init(ic eng.InitContext) error {
	p := ic.Params()
	short, long, sig := p.Int("short", 12), p.Int("long", 26), p.Int("signal", 9)
	s.maxHold = p.Int("max_hold", 10)
	ic.Use("macd", func() indicator.Indicator {
		return indicator.NewMACD(short, long, sig, indicator.DefaultPriceScale)
	})
	s.prevAbove = make(map[mktdata.InstrumentID]bool, 8192)
	return nil
}

// SnapshotState / RestoreState 只需处理 prevAbove ——
// 其余状态由引擎的账本与待撮合队列承载。
func (s *macdStrategy) SnapshotState() ([]byte, error) {
	m := make(map[string]bool, len(s.prevAbove))
	for id, v := range s.prevAbove {
		if v { // 只存 true，false 是默认值，可省一半体积
			m[fmt.Sprint(int32(id))] = true
		}
	}
	return json.Marshal(m)
}

func (s *macdStrategy) RestoreState(b []byte) error {
	var m map[string]bool
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	s.prevAbove = make(map[mktdata.InstrumentID]bool, len(m))
	for k, v := range m {
		var id int32
		if _, err := fmt.Sscan(k, &id); err != nil {
			return err
		}
		s.prevAbove[mktdata.InstrumentID(id)] = v
	}
	return nil
}

func (s *macdStrategy) OnBar(ctx eng.StepContext) ([]trading.Order, error) {
	for _, f := range ctx.Fills() {
		if f.Side == trading.SideBuy {
			s.buys++
		} else {
			s.sells++
		}
	}

	// 「哪些单在途」从引擎的待撮合队列导出，不自己维护
	inFlight := make(map[mktdata.InstrumentID]bool, len(ctx.Pending()))
	for _, po := range ctx.Pending() {
		inFlight[po.Instrument] = true
	}
	// 「持有哪些标的」从账本导出
	pf := ctx.Portfolio()
	holdCount := 0
	for _, p := range pf.Positions {
		if p.Total > 0 {
			holdCount++
		}
	}

	var orders []trading.Order
	for _, id := range ctx.Universe() {
		if inFlight[id] {
			continue
		}
		ind, ok := ctx.Indicator(id, "macd")
		if !ok || !ind.Ready() {
			continue
		}
		m := ind.(*indicator.MACD)
		above := m.DIF() > m.DEA()
		cross := above && !s.prevAbove[id]
		death := !above && s.prevAbove[id]
		s.prevAbove[id] = above

		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		pos := pf.Position(id)
		holding := pos != nil && pos.Total > 0

		if death && holding {
			if avail := ctx.Available(id); avail > 0 {
				orders = append(orders, trading.Order{
					Instrument: id, Side: trading.SideSell,
					Qty: avail, Tag: "macd_death",
				})
				inFlight[id] = true
				s.signals++
			}
			continue
		}
		if cross && !holding && holdCount+len(inFlight) < s.maxHold {
			qty := s.slotCents * 10 / bar.Close
			if qty < 100 {
				continue
			}
			orders = append(orders, trading.Order{
				Instrument: id, Side: trading.SideBuy, Qty: qty, Tag: "macd_cross",
			})
			inFlight[id] = true
			s.signals++
		}
	}
	return orders, nil
}

func main() {
	dataRoot := flag.String("data", "../data", "数据根目录")
	nInst := flag.Int("instruments", 400, "抽样标的数，0 表示全部")
	from := flag.Int("from", 20180101, "起始交易日")
	cashYuan := flag.Int64("cash", 1_000_000, "初始资金（元）")
	maxHold := flag.Int("max-hold", 10, "最多同时持有")
	volCapPct := flag.Float64("volume-cap", 10, "单笔成交占当日成交量上限（%）")
	slipBps := flag.Int64("slippage-bps", 5, "滑点（基点）")
	snapAt := flag.Int("snapshot-at", 0, "在第 N 步做快照并验证往返")
	flag.Parse()

	barRoot := filepath.Join(*dataRoot, "bar", "market=ashare", "freq=1d")
	uni, err := mktdata.LoadUniverse(filepath.Join(*dataRoot, "meta", "instruments.parquet"))
	if err != nil {
		fatal(err)
	}
	adj, err := mktdata.LoadAdjuster(filepath.Join(*dataRoot, "meta", "adj_factor.parquet"))
	if err != nil {
		fatal(err)
	}
	corp, err := mktdata.LoadCorpActions(filepath.Join(*dataRoot, "meta", "corporate_action.parquet"))
	if err != nil {
		fatal(err)
	}
	fee, err := trading.LoadFee(filepath.Join("..", "configs", "fee", "ashare_default.json"))
	if err != nil {
		fatal(err)
	}
	fmt.Printf("标的 %d 只 | 复权因子 %d 只 | 公司行动 %d 条 | 费率 %q\n",
		uni.Len(), adj.NumInstruments(), corp.Total(), fee.Name())

	opt := mktdata.LoadOptions{Root: barRoot, FromDay: int32(*from)}
	if *nInst > 0 {
		// 只取个股，且优先有复权事件的 —— 这样公司行动分支才会被真正走到
		var ids []mktdata.InstrumentID
		for _, in := range uni.All() {
			if in.Type != mktdata.TypeStock {
				continue
			}
			if len(adj.ExDates(in.ID)) == 0 {
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

	initial := *cashYuan * 100 // 元 → 分
	run := func(label string, snapshotStep int) (*macdStrategy, *eng.Engine, []byte) {
		strat := &macdStrategy{slotCents: initial / int64(*maxHold)}
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
		}, strat, eng.Config{
			Params:               eng.Params{"max_hold": float64(*maxHold)},
			IndicatorAdjMode:     mktdata.AdjHFQ,
			InitialCashCents:     initial,
			DividendTaxPPM:       0,
			ImplySplitFromFactor: true,
		})
		if err != nil {
			fatal(err)
		}
		var snap []byte
		t0 := time.Now()
		for !e.Done() {
			if _, err := e.Step(); err != nil {
				fatal(err)
			}
			if snapshotStep > 0 && e.Steps() == snapshotStep {
				if snap, err = e.Snapshot(); err != nil {
					fatal(err)
				}
			}
		}
		d := time.Since(t0)
		pf := e.Portfolio()
		equity := e.EquityCents()
		fmt.Printf("=== %s ===\n", label)
		fmt.Printf("  步数 %d  耗时 %v\n", e.Steps(), d.Round(time.Millisecond))
		fmt.Printf("  信号 %d  买入成交 %d  卖出成交 %d\n", strat.signals, strat.buys, strat.sells)
		fmt.Printf("  初始 %.2f 元 → 权益 %.2f 元（%.2f%%）\n",
			float64(initial)/100, float64(equity)/100,
			float64(equity-initial)/float64(initial)*100)
		fmt.Printf("  现金 %.2f 元  持仓 %d 只  已实现 %.2f 元\n",
			float64(pf.Cash)/100, countPositions(pf), float64(pf.RealizedCents)/100)
		fmt.Printf("  费用合计 %.2f 元", float64(pf.TotalFeeCents())/100)
		for _, k := range sortedKeys(pf.FeeCents) {
			fmt.Printf("  %s %.2f", k, float64(pf.FeeCents[k])/100)
		}
		fmt.Println()
		if n := len(pf.Warnings); n > 0 {
			fmt.Printf("  账本告警 %d 条，首条：%s\n", n, pf.Warnings[0])
		}
		fmt.Println()
		return strat, e, snap
	}

	_, base, snap := run("完整回测", *snapAt)

	if *snapAt > 0 && snap != nil {
		fmt.Printf("=== 快照往返（第 %d 步，%.2f MB）===\n",
			*snapAt, float64(len(snap))/1024/1024)
		strat2 := &macdStrategy{slotCents: initial / int64(*maxHold)}
		br := trading.NewBroker(trading.NewAShareMarket(), fee,
			trading.BpsSlippage{Bps: *slipBps},
			trading.BrokerConfig{VolumeCapPPM: int64(*volCapPct * 10_000), AllowPartialFill: true})
		e2, err := eng.New(eng.Deps{
			Columns: col, Universe: uni, Adjuster: adj, CorpAct: corp,
			Market: trading.NewAShareMarket(), Broker: br,
			Portfolio: trading.NewPortfolio(initial),
		}, strat2, eng.Config{
			Params: eng.Params{"max_hold": float64(*maxHold)},
			IndicatorAdjMode: mktdata.AdjHFQ, InitialCashCents: initial,
			ImplySplitFromFactor: true,
		})
		if err != nil {
			fatal(err)
		}
		if err := e2.Restore(snap); err != nil {
			fatal(err)
		}
		if err := e2.RunAll(); err != nil {
			fatal(err)
		}
		a, b := base.Portfolio(), e2.Portfolio()
		fmt.Printf("  全程现金 %.2f 元 vs 恢复后 %.2f 元\n",
			float64(a.Cash)/100, float64(b.Cash)/100)
		if a.Cash == b.Cash && a.RealizedCents == b.RealizedCents {
			fmt.Println("  ✅ 快照恢复后继续步进，账本与全程完全一致")
		} else {
			fmt.Printf("  ❌ 不一致：已实现 %.2f vs %.2f\n",
				float64(a.RealizedCents)/100, float64(b.RealizedCents)/100)
			os.Exit(1)
		}
	}
}

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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
