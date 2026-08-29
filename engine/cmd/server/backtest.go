package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/fingerprint"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/metrics"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/record"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 回测在**服务端**跑，用的是启动时就载入的那份数据。
//
// 这是 v0.3 那句设计注释的兑现：`Assemble(ds)` 当初就是按「多次装配共用
// 一份数据」写的。数据已经在内存里（1.25 GB / 30 秒），
// 于是一次回测只要 ~200 毫秒 —— 浏览器里点一下就能看到结果，
// 而不是每次等 30 秒重新读盘。
//
// **不做会话、不做单步、不做 WebSocket** —— 那些是 v0.4。
// 这里只回答「跑完是什么样」。

// maxUniverseForWeb 服务端回测的标的数上限。
//
// Assemble 会把全量列式数据 Subset 成配置要求的子集，那是一次拷贝：
// 285 只约 30 MB，3,194 只（在市主板）约 550 MB，全市场就是又一个 1.25 GB。
//
// 6000 这个数是按「A 股所有现实池子都能跑」定的：全部个股 5,549 只、
// 全部标的 7,200 只。取 6000 是让前者能跑、后者被挡住 ——
// 「所有个股加所有 ETF 一起回测」不是一个有意义的池子，
// 拦下来比让服务端吃满内存好。
//
// 命令行没有这个限制：LoadDataSet 只载入所需标的，压根不用 Subset。
const maxUniverseForWeb = 6000

// maxListedRejections 返回给前端的拒单条数上限。
// 超出部分只给计数 —— 8,720 条拒单全塞进 JSON 对看没有帮助。
const maxListedRejections = 5000

type runStats struct {
	Steps       int   `json:"steps"`
	DurationMS  int64 `json:"durationMs"`
	Instruments int   `json:"instruments"`
	Signals     int   `json:"signals"`
	Fills       int   `json:"fills"`
	Rejects     int   `json:"rejects"`
}

type fingerprints struct {
	Input        string `json:"input"`
	Output       string `json:"output"`
	Data         string `json:"data"`
	Engine       string `json:"engine"`
	Reproducible bool   `json:"reproducible"`
}

type curvePoint struct {
	D          int32 `json:"d"`
	Equity     int64 `json:"equity"`
	Cash       int64 `json:"cash"`
	Positions  int   `json:"positions"`
	NumSignals int   `json:"signals"`
	NumFills   int   `json:"fills"`
	NumRejects int   `json:"rejects"`
	// Bench 基准净值，**已归一化到初始资金**，便于与策略画在同一根坐标轴上。
	// 基准覆盖不到的时点为 0，前端据此断线而不是连成直线
	Bench int64 `json:"bench,omitempty"`
}

type fillDTO struct {
	D        int32  `json:"d"`
	ID       int32  `json:"id"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Side     string `json:"side"`
	Price    int64  `json:"price"`
	Qty      int64  `json:"qty"`
	Amount   int64  `json:"amount"`
	Fee      int64  `json:"fee"`
	Slippage int64  `json:"slippage"`
	Tag      string `json:"tag"`
}

type rejectDTO struct {
	D      int32  `json:"d"`
	ID     int32  `json:"id"`
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	Side   string `json:"side"`
	Qty    int64  `json:"qty"`
	Reason string `json:"reason"`
	Rule   string `json:"rule,omitempty"`
	Detail string `json:"detail"`
}

type tripDTO struct {
	ID        int32  `json:"id"`
	Symbol    string `json:"symbol"`
	Name      string `json:"name"`
	OpenDay   int32  `json:"openDay"`
	CloseDay  int32  `json:"closeDay"`
	Qty       int64  `json:"qty"`
	Cost      int64  `json:"cost"`
	Proceed   int64  `json:"proceed"`
	PnL       int64  `json:"pnl"`
	HoldDays  int    `json:"holdDays"`
	FromBonus bool   `json:"fromBonus"`
}

type runResult struct {
	Name        string          `json:"name"`
	Config      json.RawMessage `json:"config"`
	Stats       runStats        `json:"stats"`
	Fingerprint fingerprints    `json:"fingerprint"`
	Metrics     metrics.Result  `json:"metrics"`
	Curve       []curvePoint    `json:"curve"`
	Fills       []fillDTO       `json:"fills"`
	Rejections  []rejectDTO     `json:"rejections"`
	RejectTotal int             `json:"rejectTotal"`
	RejectBy    map[string]int  `json:"rejectBy"`
	RoundTrips  []tripDTO       `json:"roundTrips"`
	Warnings    []string        `json:"warnings,omitempty"`
}

// handleConfigs 列出 configs 目录下的配置，连同解析结果一起给前端。
//
// 给解析结果而不只是文件名：前端要能把参数改一改再 POST 回来跑，
// 而「改一改」得先看到当前值。
func (s *Store) handleConfigs(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(s.ConfigDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError,
			"读取配置目录 %s 失败: %v", s.ConfigDir, err)
		return
	}
	type item struct {
		Name   string          `json:"name"`
		Title  string          `json:"title"`
		Config json.RawMessage `json:"config"`
		Error  string          `json:"error,omitempty"`
	}
	out := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		it := item{Name: e.Name(), Title: e.Name()}
		raw, err := os.ReadFile(filepath.Join(s.ConfigDir, e.Name()))
		if err != nil {
			it.Error = err.Error()
			out = append(out, it)
			continue
		}
		it.Config = raw
		// 顺带校验一遍：坏掉的配置要在列表里就能看出来，
		// 而不是点了「跑」才报错
		if cfg, err := config.Parse(raw, s.ConfigDir); err != nil {
			it.Error = err.Error()
		} else if cfg.Name != "" {
			it.Title = cfg.Name
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"dir": mustAbs(s.ConfigDir), "configs": out})
}

// handleBacktest 跑一次回测并返回全部结果。
func (s *Store) handleBacktest(w http.ResponseWriter, r *http.Request) {
	body, err := readAll(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: %v", err)
		return
	}
	cfg, err := config.Parse(body, s.ConfigDir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	// 强制指向服务端已载入的那份数据 —— 配置里写的路径在这里没有意义，
	// 服务端手里只有启动时载入的这一份
	cfg.SetDataRoot(mustAbs(s.DataRoot))

	ids, err := cfg.ResolveUniverse(s.Uni, s.Adj)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(ids) > maxUniverseForWeb {
		writeErr(w, http.StatusBadRequest,
			"标的池 %d 只，超过服务端回测上限 %d —— 装配时要把全量列式数据"+
				"裁成子集，那是一次拷贝，太大会把服务端撑爆。"+
				"请收窄 universe，或用命令行跑：go run ./cmd/backtest -config ...",
			len(ids), maxUniverseForWeb)
		return
	}

	ds := &config.DataSet{
		Universe: s.Uni, Adjuster: s.Adj, CorpAct: s.Corp,
		Calendar: s.Cal, Root: mustAbs(s.DataRoot),
	}
	// 基准要在**裁子集之前**从全量数据里取 —— 它不在标的池里，
	// 裁完就没了。这一步顺序错了不会报错，只会让报告里的对标区块凭空消失
	if cfg.Metrics.Benchmark != "" {
		in := s.Uni.BySymbol(cfg.Metrics.Benchmark)
		if in == nil {
			writeErr(w, http.StatusBadRequest,
				"metrics.benchmark: 未找到标的 %q", cfg.Metrics.Benchmark)
			return
		}
		if !ds.SetBenchmark(s.Col, in.ID) {
			writeErr(w, http.StatusBadRequest,
				"metrics.benchmark: %q 没有行情数据", cfg.Metrics.Benchmark)
			return
		}
	}
	ds.Columns = s.narrowCached(cfg, ids)

	e, err := cfg.Assemble(ds)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "装配失败: %v", err)
		return
	}

	// ---- 跑 ----
	//
	// 拒单在循环里收集，而不是把记录级别提到 full —— full 级会把
	// 每步的信号也留下来，buy_and_hold 有 41 万条信号，那是白吃内存
	var res runResult
	res.Name = cfg.Name
	res.Config = body
	res.RejectBy = map[string]int{}
	rejects := make([]rejectDTO, 0, 512)

	t0 := time.Now()
	for !e.Done() {
		if _, err := e.Step(); err != nil {
			writeErr(w, http.StatusInternalServerError, "引擎步进失败: %v", err)
			return
		}
		nf, nr := e.LastCounts()
		res.Stats.Signals += len(e.Signals())
		res.Stats.Fills += nf
		res.Stats.Rejects += nr
		for _, rj := range e.Rejections() {
			res.RejectBy[reasonKey(rj)]++
			if len(rejects) < maxListedRejections {
				rejects = append(rejects, s.rejectDTO(rj))
			}
		}
	}
	res.Stats.DurationMS = time.Since(t0).Milliseconds()
	res.Stats.Steps = e.Steps()
	// 报**实际有行情的标的数**，不是过滤出来的标的池大小 ——
	// limit 300 里可能只有 285 只在这个区间内有 bar，报 300 会让人以为跑了 300 只
	res.Stats.Instruments = len(e.Universe())
	res.Rejections = rejects
	res.RejectTotal = res.Stats.Rejects

	rec, _ := e.Recorder().(*record.Memory)
	res.Warnings = append(res.Warnings, rec.Warnings...)
	if pfw := e.Portfolio().Warnings; len(pfw) > 0 {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("账本告警 %d 条，首条：%s", len(pfw), pfw[0]))
	}

	// ---- 绩效 ----
	days, eq := rec.Curve()
	var from, to int32
	if len(days) > 0 {
		from, to = days[0], days[len(days)-1]
	}
	in := metrics.Input{
		Curve:              metrics.Curve{Days: days, Equity: eq},
		InitialCents:       cfg.Portfolio.InitialCashCents,
		Fills:              rec.Fills(),
		RiskFreePPM:        cfg.Metrics.RiskFreePPM,
		TradingDaysPerYear: s.Cal.TradingDaysPerYear(mktdata.MarketAShare, from, to),
	}
	benchByDay := map[int32]int64{}
	if bd, be, ok := ds.BenchmarkCurve(); ok {
		in.Benchmark = &metrics.Curve{Days: bd, Equity: be}
		in.BenchmarkName = cfg.Metrics.Benchmark
		benchByDay = normalizeBench(bd, be, days, cfg.Portfolio.InitialCashCents)
	}
	res.Metrics = metrics.Compute(in)

	// ---- 曲线 ----
	res.Curve = make([]curvePoint, 0, len(rec.Steps()))
	for _, st := range rec.Steps() {
		res.Curve = append(res.Curve, curvePoint{
			D: st.Time.TradingDay, Equity: st.EquityCents, Cash: st.CashCents,
			Positions: st.Positions, NumSignals: st.NumSignals,
			NumFills: st.NumFills, NumRejects: st.NumRejects,
			Bench: benchByDay[st.Time.TradingDay],
		})
	}

	// ---- 成交与逐轮 ----
	fills := rec.Fills()
	res.Fills = make([]fillDTO, 0, len(fills))
	for _, f := range fills {
		res.Fills = append(res.Fills, s.fillDTO(f))
	}
	trips, _ := metrics.MatchRoundTrips(fills)
	res.RoundTrips = make([]tripDTO, 0, len(trips))
	for _, t := range trips {
		in := s.Uni.Get(t.Instrument)
		d := tripDTO{
			ID: int32(t.Instrument), OpenDay: t.OpenDay, CloseDay: t.CloseDay,
			Qty: t.Qty, Cost: t.CostCents, Proceed: t.ProceedCents,
			PnL: t.PnLCents, HoldDays: t.HoldDays, FromBonus: t.FromBonus,
		}
		if in != nil {
			d.Symbol, d.Name = in.Symbol, in.Name
		}
		res.RoundTrips = append(res.RoundTrips, d)
	}

	// ---- 指纹 ----
	dataFP, _, err := fingerprint.Data(mustAbs(s.DataRoot))
	if err == nil {
		if inputFP, err := cfg.InputFingerprint(dataFP); err == nil {
			res.Fingerprint = fingerprints{
				Input: inputFP, Output: e.ResultFingerprint(), Data: dataFP,
				Engine: fingerprint.EngineVersion(), Reproducible: fingerprint.Reproducible(),
			}
		}
	}
	writeJSON(w, res)

	// 大池子的子集是一大块临时内存（3,194 只约 550 MB）。
	// Go 的 GC 不会主动还给操作系统，本地工具没必要攥着它不放。
	if len(ids) > 1000 {
		go func() {
			runtime.GC()
			debug.FreeOSMemory()
		}()
	}
}

// narrowCached 把全量列式数据裁成本次要用的子集，并缓存最近一次的结果。
//
// **缓存的意义在于工作流**：调策略参数时会反复跑同一个标的池 ——
// slots 从 10 改到 5 再跑，标的池没变，没有理由再拷贝一次 550 MB。
// 只缓存一份：换池子就换掉，不做 LRU（多份缓存等于多份内存，
// 而同时用两个大池子来回切不是常见用法）。
//
// 返回的 Columns 交给 Assemble，那边的 narrow 会认出「集合与区间都吻合」
// 从而原样透传，不会再拷一次。
func (s *Store) narrowCached(cfg *config.Config, ids []mktdata.InstrumentID) *mktdata.Columns {
	// 键要含区间：同一批标的、不同起止日，切出来的是不同的子集
	parts := make([]byte, 0, len(ids)*4+16)
	parts = append(parts, byte(cfg.Data.From), byte(cfg.Data.From>>8),
		byte(cfg.Data.To), byte(cfg.Data.To>>8))
	for _, id := range ids {
		parts = append(parts, byte(id), byte(id>>8), byte(id>>16), byte(id>>24))
	}
	key := fingerprint.Hex(parts)

	s.uniMu.Lock()
	defer s.uniMu.Unlock()
	if s.uniKey == key && s.uniCols != nil {
		return s.uniCols
	}
	sub, err := s.Col.Subset(ids, cfg.Data.From, cfg.Data.To)
	if err != nil {
		// 裁不出来就把全量交回去，让 Assemble 自己报错 ——
		// 在这里吞掉错误会让失败原因指向一个和它无关的地方
		return s.Col
	}
	s.uniKey, s.uniCols = key, sub
	return sub
}

// normalizeBench 把基准价格序列归一化到初始资金，并只保留策略也有的交易日。
//
// 归一化是为了画在同一根坐标轴上。**覆盖不到的日子留空而不是补值** ——
// 拉一条直线过去会让人以为那段基准没涨没跌，而事实是那段根本没有数据。
func normalizeBench(bd []int32, be []int64, days []int32, initial int64) map[int32]int64 {
	if len(bd) == 0 || len(days) == 0 {
		return nil
	}
	src := make(map[int32]int64, len(bd))
	for i, d := range bd {
		src[d] = be[i]
	}
	// 基准的起点取「策略区间内基准第一个有数的交易日」
	var base int64
	for _, d := range days {
		if v, ok := src[d]; ok && v > 0 {
			base = v
			break
		}
	}
	if base == 0 {
		return nil
	}
	out := make(map[int32]int64, len(days))
	for _, d := range days {
		if v, ok := src[d]; ok && v > 0 {
			out[d] = initial * v / base
		}
	}
	return out
}

func (s *Store) fillDTO(f trading.Fill) fillDTO {
	d := fillDTO{
		D: f.At.TradingDay, ID: int32(f.Instrument), Side: sideName(f.Side),
		Price: f.Price, Qty: f.Qty, Amount: trading.AmountCents(f.Price, f.Qty),
		Fee: f.Fee.Total, Slippage: f.SlippageCents, Tag: f.Tag,
	}
	if in := s.Uni.Get(f.Instrument); in != nil {
		d.Symbol, d.Name = in.Symbol, in.Name
	}
	return d
}

func (s *Store) rejectDTO(r trading.Rejection) rejectDTO {
	d := rejectDTO{
		D: r.At.TradingDay, ID: int32(r.Instrument), Side: sideName(r.Side),
		Qty: r.Qty, Reason: r.Reason.String(), Rule: r.Rule, Detail: r.Detail,
	}
	if in := s.Uni.Get(r.Instrument); in != nil {
		d.Symbol, d.Name = in.Symbol, in.Name
	}
	return d
}

func sideName(s trading.Side) string {
	if s == trading.SideBuy {
		return "buy"
	}
	return "sell"
}

// reasonKey 把拒单归类。风控拦截要细到规则名 ——
// 一堆「风控拦截」堆在一起等于没说。
func reasonKey(r trading.Rejection) string {
	if r.Reason == trading.RejectRisk && r.Rule != "" {
		return "风控:" + r.Rule
	}
	return r.Reason.String()
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	const maxBody = 1 << 20 // 配置不该有 1 MB 那么大
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if len(b) > maxBody {
			return nil, fmt.Errorf("请求体超过 %d 字节", maxBody)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return b, nil
			}
			return b, nil // io.EOF 之外的错误交给上层的 JSON 解析报
		}
	}
}

// handleUniverse 解析一份 universe 规格，返回命中的标的数与样例。
//
// 存在的理由：选池子时得先知道选中了多少、都是些什么，
// 否则只能靠「跑一次看报错」——而超过上限的池子根本跑不起来。
//
// 请求体就是一段 universe JSON，与配置里 data.universe 的形状一致。
func (s *Store) handleUniverse(w http.ResponseWriter, r *http.Request) {
	body, err := readAll(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: %v", err)
		return
	}
	// 套一层最小配置壳，复用同一套解析与校验 ——
	// 预览和真跑必须走同一条路径，否则预览说 3193 只、真跑却是别的数
	var u config.Universe
	if err := json.Unmarshal(body, &u); err != nil {
		writeErr(w, http.StatusBadRequest, "universe 不是合法 JSON: %v", err)
		return
	}
	cfg := &config.Config{}
	cfg.Data.Univers = u
	ids, err := cfg.ResolveUniverse(s.Uni, s.Adj)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	type row struct {
		ID     int32  `json:"id"`
		Symbol string `json:"symbol"`
		Name   string `json:"name"`
		Type   int8   `json:"type"`
		Board  int8   `json:"board"`
		Bars   int    `json:"bars"`
		First  int32  `json:"firstDay"`
		Last   int32  `json:"lastDay"`
	}
	// 只回样例：3000 只标的的完整列表对「选池子」没有帮助，
	// 而两端各看几只就能判断选对没有
	const sampleN = 12
	sample := make([]row, 0, sampleN*2)
	add := func(id mktdata.InstrumentID) {
		in := s.Uni.Get(id)
		if in == nil {
			return
		}
		st := s.Stat(id)
		sample = append(sample, row{
			ID: int32(id), Symbol: in.Symbol, Name: in.Name,
			Type: int8(in.Type), Board: int8(in.Board),
			Bars: st.Bars, First: st.FirstDay, Last: st.LastDay,
		})
	}
	head := ids
	if len(head) > sampleN {
		head = head[:sampleN]
	}
	for _, id := range head {
		add(id)
	}
	truncated := 0
	if len(ids) > sampleN*2 {
		truncated = len(ids) - sampleN*2
		for _, id := range ids[len(ids)-sampleN:] {
			add(id)
		}
	} else if len(ids) > sampleN {
		for _, id := range ids[sampleN:] {
			add(id)
		}
	}

	// 有多少只在所选区间内**真的有行情** —— 过滤命中数不等于能跑的数
	withBars := 0
	for _, id := range ids {
		if s.Stat(id).Bars > 0 {
			withBars++
		}
	}
	writeJSON(w, map[string]any{
		"count": len(ids), "withBars": withBars,
		"limit": maxUniverseForWeb, "overLimit": len(ids) > maxUniverseForWeb,
		"sample": sample, "truncated": truncated,
	})
}
