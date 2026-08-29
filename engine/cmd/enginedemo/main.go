// Command enginedemo 把数据层、复权与指标串成一个能跑的空引擎。
//
// 它验证 v0.2 第一刀的验收标准：按事件时点推进、每步把当前可见数据交给策略、
// 指标增量更新、快照可往返 —— 但**不下单**（Broker 不在本刀范围）。
//
// 示例策略用 MACD 金叉与 KDJ 超卖统计信号数量，只统计不交易。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// signalCounter 是示例策略：统计 MACD 金叉与 KDJ 超卖信号，不产生订单。
type signalCounter struct {
	macdCross int // DIF 上穿 DEA
	kdjOver   int // K 上穿 D 且 K < 20
	bothSame  int // 两者同日出现

	prevDIFAbove map[mktdata.InstrumentID]bool
	prevKAbove   map[mktdata.InstrumentID]bool
	skippedWarm  int
	skippedSusp  int
	bars         int
}

func (s *signalCounter) Name() string { return "signal_counter" }

func (s *signalCounter) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "macd_short", Kind: eng.ParamInt, Default: 12, Min: 2, Max: 60, Step: 1, Desc: "MACD 快线周期"},
		{Name: "macd_long", Kind: eng.ParamInt, Default: 26, Min: 5, Max: 200, Step: 1, Desc: "MACD 慢线周期"},
		{Name: "macd_signal", Kind: eng.ParamInt, Default: 9, Min: 2, Max: 60, Step: 1, Desc: "MACD 信号周期"},
		{Name: "kdj_period", Kind: eng.ParamInt, Default: 9, Min: 3, Max: 60, Step: 1, Desc: "KDJ 周期"},
		{Name: "kdj_oversold", Kind: eng.ParamFloat, Default: 20, Min: 5, Max: 50, Step: 1, Desc: "KDJ 超卖阈值"},
	}
}

func (s *signalCounter) Init(ic eng.InitContext) error {
	p := ic.Params()
	short := p.Int("macd_short", 12)
	long := p.Int("macd_long", 26)
	sig := p.Int("macd_signal", 9)
	kn := p.Int("kdj_period", 9)

	ic.Use("macd", func() indicator.Indicator {
		return indicator.NewMACD(short, long, sig, indicator.DefaultPriceScale)
	})
	ic.Use("kdj", func() indicator.Indicator {
		return indicator.NewKDJ(kn, 3, 3)
	})

	s.prevDIFAbove = make(map[mktdata.InstrumentID]bool, 8192)
	s.prevKAbove = make(map[mktdata.InstrumentID]bool, 8192)
	return nil
}

func (s *signalCounter) OnBar(ctx eng.StepContext) error {
	oversold := 20.0
	for _, id := range ctx.Universe() {
		s.bars++

		// 停牌日不产生信号：OHLC 全等于停牌前收盘价，任何「突破」都是假的
		if b, ok := ctx.Bar(id); ok && b.Suspended() {
			s.skippedSusp++
			continue
		}

		macdI, ok1 := ctx.Indicator(id, "macd")
		kdjI, ok2 := ctx.Indicator(id, "kdj")
		if !ok1 || !ok2 {
			continue
		}
		// Ready 之前的值是垃圾，据此下单会让回测前 N 步产生虚假交易
		if !macdI.Ready() || !kdjI.Ready() {
			s.skippedWarm++
			continue
		}
		m := macdI.(*indicator.MACD)
		k := kdjI.(*indicator.KDJ)

		difAbove := m.DIF() > m.DEA()
		kAbove := k.K() > k.D()

		macdSig := difAbove && !s.prevDIFAbove[id]
		kdjSig := kAbove && !s.prevKAbove[id] && k.K() < oversold

		if macdSig {
			s.macdCross++
		}
		if kdjSig {
			s.kdjOver++
		}
		if macdSig && kdjSig {
			s.bothSame++
		}
		s.prevDIFAbove[id] = difAbove
		s.prevKAbove[id] = kAbove
	}
	return nil
}

func main() {
	root := flag.String("root", "../data/bar/market=ashare/freq=1d", "bar 分区根目录")
	facPath := flag.String("factors", "../data/meta/adj_factor.parquet", "复权因子表")
	nInst := flag.Int("instruments", 300, "抽样标的数，0 表示全部")
	from := flag.Int("from", 0, "起始交易日 YYYYMMDD")
	snapAt := flag.Int("snapshot-at", 0, "在第 N 步做快照并验证往返；0 表示不验")
	flag.Parse()

	adj, err := mktdata.LoadAdjuster(*facPath)
	if err != nil {
		fatal(err)
	}

	opt := mktdata.LoadOptions{Root: *root, FromDay: int32(*from)}
	if *nInst > 0 {
		ids, err := mktdata.ReadInstrumentIDs(*root)
		if err != nil {
			fatal(err)
		}
		if *nInst < len(ids) {
			ids = ids[:*nInst]
		}
		opt.Instruments = ids
	}
	col, st, err := mktdata.Load(opt)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("加载 %d 行 / %d 只标的 / %d 个时点  耗时 %v\n\n",
		st.Rows, st.Instruments, st.Steps, st.Total.Round(time.Millisecond))

	run := func(label string) (*signalCounter, time.Duration) {
		strat := &signalCounter{}
		e, err := eng.New(col, adj, strat, eng.Config{
			Params:           eng.Params{},
			IndicatorAdjMode: mktdata.AdjHFQ, // 指标喂后复权价
		})
		if err != nil {
			fatal(err)
		}
		t0 := time.Now()
		if err := e.RunAll(); err != nil {
			fatal(err)
		}
		d := time.Since(t0)
		fmt.Printf("=== %s ===\n", label)
		fmt.Printf("  步数 %d  处理 bar %d  耗时 %v  → %.0f ns/bar\n",
			e.Steps(), strat.bars, d.Round(time.Millisecond),
			float64(d.Nanoseconds())/float64(maxi(strat.bars, 1)))
		fmt.Printf("  预热期跳过 %d，停牌跳过 %d\n", strat.skippedWarm, strat.skippedSusp)
		fmt.Printf("  MACD 金叉 %d  KDJ 超卖金叉 %d  同日共振 %d\n\n",
			strat.macdCross, strat.kdjOver, strat.bothSame)
		return strat, d
	}

	base, _ := run("全程运行")

	// ---- 快照往返：C6 的前提 ----
	if *snapAt > 0 {
		strat := &signalCounter{}
		e, err := eng.New(col, adj, strat, eng.Config{IndicatorAdjMode: mktdata.AdjHFQ})
		if err != nil {
			fatal(err)
		}
		for i := 0; i < *snapAt && !e.Done(); i++ {
			if _, err := e.Step(); err != nil {
				fatal(err)
			}
		}
		snap, err := e.Snapshot()
		if err != nil {
			fatal(err)
		}
		fmt.Printf("=== 快照往返（在第 %d 步）===\n", *snapAt)
		fmt.Printf("  快照大小 %.2f MB\n", float64(len(snap))/1024/1024)

		// 新引擎从快照恢复后继续跑完
		strat2 := &signalCounter{}
		// 恢复的是指标与游标；策略自身的计数从零开始，
		// 故只比对「恢复点之后」的增量
		e2, err := eng.New(col, adj, strat2, eng.Config{IndicatorAdjMode: mktdata.AdjHFQ})
		if err != nil {
			fatal(err)
		}
		if err := e2.Restore(snap); err != nil {
			fatal(err)
		}
		// 策略的 prev 状态也需承接，否则恢复后的第一根会误判为金叉
		strat2.prevDIFAbove = strat.prevDIFAbove
		strat2.prevKAbove = strat.prevKAbove
		if err := e2.RunAll(); err != nil {
			fatal(err)
		}
		gotTotal := strat.macdCross + strat2.macdCross
		fmt.Printf("  分段合计 MACD 金叉 %d（前段 %d + 后段 %d）\n",
			gotTotal, strat.macdCross, strat2.macdCross)
		fmt.Printf("  全程     MACD 金叉 %d\n", base.macdCross)
		if gotTotal == base.macdCross {
			fmt.Println("  ✅ 快照恢复后继续步进，结果与全程一致")
		} else {
			fmt.Printf("  ❌ 不一致，差 %d\n", base.macdCross-gotTotal)
			os.Exit(1)
		}
	}

	_ = sort.Ints
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
