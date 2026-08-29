package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// K 线视图的指标值**必须由真引擎算出**，否则这个工具就没有意义 ——
// 前端自己算一遍等于新增一条实现，两条实现不一致时反而分不清谁错了。
//
// 因此这里不是「读数据再算指标」，而是真的装配一个 engine.Engine、
// 灌入单标的子集、一步步 Step 过去，把每步的指标读出来。
// 走的是与回测**逐字节相同**的代码路径：同样的 AdjustBar、同样的 Update 顺序。
//
// 复权模式为什么会跑两遍引擎：
//
//	展示用的复权模式由用户选（要跟同花顺这类软件对得上），
//	而**回测固定用后复权**（序列必须连续，否则除权日产生假信号）。
//	两者不同时，同一根 bar 就有两组指标值。
//	与其挑一组显示，不如两组都给 —— 「我看到的」与「引擎回测时看到的」
//	并排摆着，才真的能核对。

// probeStrategy 是只观察不下单的策略。
type probeStrategy struct {
	id    mktdata.InstrumentID
	specs []indSpec
	recs  []stepRec
}

type indSpec struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Pane  string   `json:"pane"` // main / macd / kdj
	Names []string `json:"names"`
	make  eng.IndicatorFactory
}

type stepRec struct {
	day   int32
	bar   mktdata.Bar
	vals  map[string][]float64
	ready map[string]bool
}

func (p *probeStrategy) Name() string           { return "probe" }
func (p *probeStrategy) Specs() []eng.ParamSpec { return nil }

func (p *probeStrategy) Init(ic eng.InitContext) error {
	for _, sp := range p.specs {
		ic.Use(sp.Key, sp.make)
	}
	return nil
}

func (p *probeStrategy) OnBar(ctx eng.StepContext) ([]trading.Order, error) {
	bar, ok := ctx.Bar(p.id)
	if !ok {
		return nil, nil
	}
	rec := stepRec{
		day:   bar.TradingDay,
		bar:   bar,
		vals:  make(map[string][]float64, len(p.specs)),
		ready: make(map[string]bool, len(p.specs)),
	}
	for _, sp := range p.specs {
		ind, ok := ctx.Indicator(p.id, sp.Key)
		if !ok {
			continue
		}
		// Values 返回的是指标内部切片的视图，必须拷贝 —— 下一步会被改写
		v := ind.Values()
		cp := make([]float64, len(v))
		copy(cp, v)
		rec.vals[sp.Key] = cp
		rec.ready[sp.Key] = ind.Ready()
	}
	p.recs = append(p.recs, rec)
	return nil, nil // 观察者不下单
}

// runProbe 跑一遍引擎，返回逐步记录。
func (s *Store) runProbe(
	in *mktdata.Instrument, sub *mktdata.Columns, specs []indSpec, mode mktdata.AdjMode,
) ([]stepRec, int, error) {
	probe := &probeStrategy{id: in.ID, specs: specs}
	e, err := eng.New(eng.Deps{
		Columns: sub, Universe: s.Uni, Adjuster: s.Adj, CorpAct: s.Corp,
		Market: s.Mkt,
		Broker: trading.NewBroker(s.Mkt, s.Fee, trading.BpsSlippage{}, trading.BrokerConfig{}),
		Portfolio: trading.NewPortfolio(0),
	}, probe, eng.Config{IndicatorAdjMode: mode})
	if err != nil {
		return nil, 0, fmt.Errorf("装配引擎失败: %w", err)
	}
	if err := e.RunAll(); err != nil {
		return nil, 0, fmt.Errorf("引擎步进失败: %w", err)
	}
	return probe.recs, e.Steps(), nil
}

// parseInts 解析 `12,26,9` 形式的参数，长度不符时回退到默认值。
func parseInts(v string, def []int) []int {
	if strings.TrimSpace(v) == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 {
			return def
		}
		out = append(out, n)
	}
	if len(def) > 0 && len(out) != len(def) {
		return def
	}
	return out
}

func parseAdj(v string, def mktdata.AdjMode) mktdata.AdjMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "raw", "0":
		return mktdata.AdjNone
	case "hfq", "1":
		return mktdata.AdjHFQ
	case "qfq", "2":
		return mktdata.AdjQFQ
	}
	return def
}

func (s *Store) handleKline(w http.ResponseWriter, r *http.Request) {
	in, err := s.lookup(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	q := r.URL.Query()

	// 一个开关管到底：K 线与其上的指标同基准，否则均线会和 K 线差出几十倍
	// 而彻底不可读（第一版就是这么错的）。
	dispAdj := parseAdj(q.Get("adj"), mktdata.AdjNone)
	from := int32(qInt(r, "from", 0))
	to := int32(qInt(r, "to", 0))

	macdP := parseInts(q.Get("macd"), []int{12, 26, 9})
	kdjP := parseInts(q.Get("kdj"), []int{9, 3, 3})
	maP := parseInts(q.Get("ma"), []int{5, 10, 20, 60})

	scale := float64(in.PriceScale)
	if scale <= 0 {
		scale = indicator.DefaultPriceScale
	}

	specs := []indSpec{{
		Key: "macd", Label: fmt.Sprintf("MACD(%d,%d,%d)", macdP[0], macdP[1], macdP[2]),
		Pane: "macd", Names: []string{"DIF", "DEA", "HIST"},
		make: func() indicator.Indicator {
			return indicator.NewMACD(macdP[0], macdP[1], macdP[2], scale)
		},
	}, {
		Key: "kdj", Label: fmt.Sprintf("KDJ(%d,%d,%d)", kdjP[0], kdjP[1], kdjP[2]),
		Pane: "kdj", Names: []string{"K", "D", "J"},
		make: func() indicator.Indicator {
			return indicator.NewKDJ(kdjP[0], kdjP[1], kdjP[2])
		},
	}}
	for _, n := range maP {
		n := n
		specs = append(specs, indSpec{
			Key: fmt.Sprintf("ma%d", n), Label: fmt.Sprintf("MA%d", n),
			Pane: "main", Names: []string{fmt.Sprintf("MA%d", n)},
			make: func() indicator.Indicator { return indicator.NewSMA(n, scale) },
		})
	}

	// **从有数据的第一天开始跑，而不是从 from 开始。**
	//
	// 指标是增量的：从 from 起步意味着 from 当天的 MACD 处于冷启动状态，
	// 与真实回测（跑完整历史）算出的值不同。若照那样返回，这个工具就会
	// 在「数据准不准」这个问题上给出错误答案 —— 所以引擎跑全程，只裁剪输出。
	sub, err := s.Col.Subset([]mktdata.InstrumentID{in.ID}, 0, to)
	if err != nil {
		writeErr(w, http.StatusNotFound, "标的 %s 无行情数据: %v", in.Symbol, err)
		return
	}

	// 展示基准的一遍
	recs, steps, err := s.runProbe(in, sub, specs, dispAdj)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	// 回测基准（后复权）的一遍。与展示基准相同就不重复跑。
	var btByDay map[int32]stepRec
	if dispAdj != mktdata.AdjHFQ {
		btRecs, _, err := s.runProbe(in, sub, specs, mktdata.AdjHFQ)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "%v", err)
			return
		}
		btByDay = make(map[int32]stepRec, len(btRecs))
		for _, rec := range btRecs {
			btByDay[rec.day] = rec
		}
	}

	// 除权日集合，用于在图上标注
	exDates := map[int32]bool{}
	for _, f := range s.Adj.Factors(in.ID) {
		exDates[f.ExDate] = true
	}

	type barDTO struct {
		D       int32                `json:"d"`
		O       int64                `json:"o"`
		H       int64                `json:"h"`
		L       int64                `json:"l"`
		C       int64                `json:"c"`
		RawC    int64                `json:"rawC"`
		V       int64                `json:"v"`
		Amt     int64                `json:"amt"`
		Pre     int64                `json:"pre"`
		Susp    bool                 `json:"susp"`
		ST      bool                 `json:"st"`
		LimitUp int64                `json:"limitUp"`
		LimitDn int64                `json:"limitDn"`
		Factor  int64                `json:"factor"`
		Ex      bool                 `json:"ex"`
		Ind     map[string][]float64 `json:"ind"`
		Ready   map[string]bool      `json:"ready"`
		// IndBT 是回测基准（后复权）下的同一批指标。
		// 展示基准本身就是后复权时为空 —— 那时两者是同一组数。
		IndBT   map[string][]float64 `json:"indBt,omitempty"`
		ReadyBT map[string]bool      `json:"readyBt,omitempty"`
	}

	rows := make([]barDTO, 0, len(recs))
	warmup := 0
	for _, rec := range recs {
		b := rec.bar
		if from != 0 && b.TradingDay < from {
			warmup++
			continue
		}
		// 展示价按所选方式复权。**在引擎之外做** —— 引擎的 StepContext
		// 根本不提供前复权，展示路径与决策路径由此保持分离。
		adj := func(v int64) int64 { return s.Adj.Adjust(in.ID, b.TradingDay, v, dispAdj) }
		up, dn, hasLimit := s.Mkt.LimitPrices(in, b)
		if !hasLimit {
			up, dn = 0, 0
		}
		row := barDTO{
			D: b.TradingDay,
			O: adj(b.Open), H: adj(b.High), L: adj(b.Low), C: adj(b.Close),
			RawC: b.Close, V: b.Volume, Amt: b.Amount, Pre: b.PreClose,
			Susp: b.Suspended(), ST: b.IsST != 0,
			LimitUp: up, LimitDn: dn,
			Factor: s.Adj.Factor(in.ID, b.TradingDay), Ex: exDates[b.TradingDay],
			Ind: rec.vals, Ready: rec.ready,
		}
		if btByDay != nil {
			if bt, ok := btByDay[b.TradingDay]; ok {
				row.IndBT, row.ReadyBT = bt.vals, bt.ready
			}
		}
		rows = append(rows, row)
	}

	// specs 里的工厂函数不能进 JSON，转成纯数据
	specDTO := make([]indSpec, len(specs))
	for i, sp := range specs {
		specDTO[i] = indSpec{Key: sp.Key, Label: sp.Label, Pane: sp.Pane, Names: sp.Names}
	}

	writeJSON(w, map[string]any{
		"instrument": s.dto(in),
		"adj":        dispAdj.String(),
		"btAdj":      mktdata.AdjHFQ.String(),
		"sameAsBT":   dispAdj == mktdata.AdjHFQ,
		"indicators": specDTO,
		"bars":       rows,
		"engine": map[string]any{
			// 明示引擎实际跑了多少步、其中多少步只是为了把指标喂热。
			// 少了这个数字，就无法判断区间首日的指标是否可信。
			"steps":      steps,
			"runs":       map[bool]int{true: 1, false: 2}[btByDay == nil],
			"warmupBars": warmup,
			"returned":   len(rows),
			"priceScale": in.PriceScale,
		},
	})
}
