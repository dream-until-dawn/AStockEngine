package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/config"
	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 单步调试会话（v0.4）。
//
// # 为什么是 HTTP 而不是 WebSocket
//
// ROADMAP 原本写的是「HTTP + WebSocket」。实现时改成纯 HTTP，理由：
// 步进是**用户驱动的请求/响应** —— 点一下走一步，等的是这一步的结果。
// WebSocket 换来的是「服务端主动推」，而这里没有服务端主动产生的事件；
// 代价却是一整套连接生命周期、重连、断线后的状态对齐。
// 长跑（run-to）最坏 5,260 步约 8 秒，一次请求扛得住。
//
// 真需要推送是 v0.6 实盘：那时事件由行情驱动，服务端确实要主动说话。
// 那时再加，比现在为了一个用不上的能力先背上协议复杂度划算。
//
// # 为什么会话要驻留而不是每次重放
//
// 引擎状态（指标、账本、在途队列）无法从「第 N 步」这个数字重建 ——
// 只能从头跑一遍。每次步进都重放 N 步，在 5,260 步的池子上是 8 秒。
// 所以会话驻留，代价是内存：每个会话一个引擎。
// 实测（cmd/enginebench）子集共享后每个引擎边际约 2.7 MB，
// 真正大的是标的池子集本身（3,376 只约 358 MB），而那一份由
// narrowCached 在同池子的会话间共享。

// maxSessions 同时存活的会话数上限。
//
// 不做 LRU 淘汰而是**拒绝新建**：调试会话里有用户攒下的状态
// （跑到了第 800 步），悄悄把它淘汰掉比报错难受得多。
const maxSessions = 8

// sessionIdleTTL 空闲多久后回收。
const sessionIdleTTL = 30 * time.Minute

// maxStepsPerCall 单次请求最多步进多少步。
//
// 全历史 5,260 步约 8 秒，留一倍余量。上限存在的意义是让「跑到末尾」
// 这种请求有个确定的时间上界，而不是让浏览器无限等下去。
const maxStepsPerCall = 20000

// maxDetailSteps 单次响应里最多返回多少步的明细。
//
// 步进 500 步时不需要 500 份明细 —— 需要的是**发生了事情的那些步**。
// 无事发生的步只计数，不占带宽也不占视线。
const maxDetailSteps = 200

// Session 是一次单步调试会话。
type Session struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Config  json.RawMessage `json:"-"`
	Created time.Time       `json:"created"`
	Used    time.Time       `json:"used"`

	mu  sync.Mutex
	cfg *config.Config
	ds  *config.DataSet
	e   *eng.Engine
}

// SessionStore 管理全部会话。
type SessionStore struct {
	mu   sync.Mutex
	byID map[string]*Session
	seq  int
}

func newSessionStore() *SessionStore {
	return &SessionStore{byID: make(map[string]*Session, maxSessions)}
}

// gcLocked 清掉超时空闲的会话。调用方须持锁。
func (ss *SessionStore) gcLocked(now time.Time) {
	for id, s := range ss.byID {
		if now.Sub(s.Used) > sessionIdleTTL {
			delete(ss.byID, id)
		}
	}
}

func (ss *SessionStore) add(s *Session) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.gcLocked(time.Now())
	if len(ss.byID) >= maxSessions {
		return fmt.Errorf("已有 %d 个调试会话（上限 %d）—— "+
			"每个会话驻留一台引擎。请先关掉不用的会话",
			len(ss.byID), maxSessions)
	}
	ss.seq++
	s.ID = fmt.Sprintf("s%d", ss.seq)
	ss.byID[s.ID] = s
	return nil
}

func (ss *SessionStore) get(id string) (*Session, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.byID[id]
	if ok {
		s.Used = time.Now()
	}
	return s, ok
}

func (ss *SessionStore) drop(id string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, ok := ss.byID[id]
	delete(ss.byID, id)
	return ok
}

func (ss *SessionStore) list() []*Session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.gcLocked(time.Now())
	out := make([]*Session, 0, len(ss.byID))
	for _, s := range ss.byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- DTO ----
//
// 单步调试要看的是一条**漏斗**：
//
//	信号 → 定量（Sizer）→ 风控 → 入队 → 撮合 → 成交 / 拒单
//
// 每一环都可能把单子吃掉，而只看成交记录时全部表现为「什么也没发生」。
// 所以每一环都单独给出来 —— 少一环，「为什么没买」就答不上来。

type signalDTO struct {
	ID       int32   `json:"id"`
	Symbol   string  `json:"symbol"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Side     string  `json:"side"`
	Strength float64 `json:"strength"`
	Tag      string  `json:"tag,omitempty"`
}

type orderDTO struct {
	ID     int32  `json:"id"`
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	Side   string `json:"side"`
	Qty    int64  `json:"qty"`
	Tag    string `json:"tag,omitempty"`
}

type pendingDTO struct {
	ID       int32  `json:"id"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Side     string `json:"side"`
	Qty      int64  `json:"qty"`
	SignalD  int32  `json:"signalDay"`
	PriceRef string `json:"priceRef"`
	Tried    int    `json:"tried"`
	MaxSteps int    `json:"maxSteps"`
	Tag      string `json:"tag,omitempty"`
}

type positionDTO struct {
	ID        int32  `json:"id"`
	Symbol    string `json:"symbol"`
	Name      string `json:"name"`
	Qty       int64  `json:"qty"`
	Available int64  `json:"available"`
	Cost      int64  `json:"cost"`
	Last      int64  `json:"last"`
	Value     int64  `json:"value"`
	PnL       int64  `json:"pnl"`
	Suspended bool   `json:"suspended"`
}

// stepEvent 是一步的全部动静。**无事发生的步不产生 stepEvent** ——
// 步进 500 步时用户要看的是「哪几天有事」，不是 500 份空表。
type stepEvent struct {
	Step      int         `json:"step"`
	Day       int32       `json:"day"`
	Equity    int64       `json:"equity"`
	Cash      int64       `json:"cash"`
	Positions int         `json:"positions"`
	Signals   []signalDTO `json:"signals,omitempty"`
	Orders    []orderDTO  `json:"orders,omitempty"`
	Fills     []fillDTO   `json:"fills,omitempty"`
	Rejects   []rejectDTO `json:"rejects,omitempty"`
}

func (ev stepEvent) quiet() bool {
	return len(ev.Signals) == 0 && len(ev.Orders) == 0 &&
		len(ev.Fills) == 0 && len(ev.Rejects) == 0
}

// sessionState 是「现在停在哪儿、账上什么样」。
type sessionState struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Step       int           `json:"step"`
	TotalSteps int           `json:"totalSteps"`
	Day        int32         `json:"day"`
	FirstDay   int32         `json:"firstDay"`
	LastDay    int32         `json:"lastDay"`
	Done       bool          `json:"done"`
	Started    bool          `json:"started"`
	Universe   int           `json:"universe"`
	Equity     int64         `json:"equity"`
	Cash       int64         `json:"cash"`
	Initial    int64         `json:"initial"`
	Peak       int64         `json:"peak"`
	Realized   int64         `json:"realized"`
	Fee        int64         `json:"fee"`
	Slippage   int64         `json:"slippage"`
	Holdings   []positionDTO `json:"holdings"`
	Pending    []pendingDTO  `json:"pending"`
	Indicators []string      `json:"indicators"`
	// Evaluated / NotReady 上一步进入决策集合、以及仍在预热的标的数。
	// **预热是逐标的的**，看不到它就分不清「条件不满足」与「还没算出来」
	Evaluated   int      `json:"evaluated"`
	NotReady    int      `json:"notReady"`
	HasWarmup   bool     `json:"hasWarmup"`
	Warnings    []string `json:"warnings,omitempty"`
	Disclosures []string `json:"disclosures,omitempty"`
}

// ---- 会话内部：把引擎状态翻译成 DTO ----

func (s *Store) snapDTOs(e *eng.Engine) ([]positionDTO, []pendingDTO) {
	led := e.Ledger()
	hold := make([]positionDTO, 0, led.NumPositions())
	led.EachExposure(func(id mktdata.InstrumentID, ex trading.Exposure) bool {
		d := positionDTO{
			ID: int32(id), Qty: ex.Long, Cost: ex.LongCost,
			Available: e.AvailableAt(id),
		}
		if in := s.Uni.Get(id); in != nil {
			d.Symbol, d.Name = in.Symbol, in.Name
		}
		if b, ok := e.BarAt(id); ok {
			d.Last = b.Close
			d.Value = trading.AmountCents(b.Close, ex.Long)
			d.PnL = d.Value - ex.LongCost
			d.Suspended = b.Suspended()
		}
		hold = append(hold, d)
		return true
	})
	sort.Slice(hold, func(i, j int) bool { return hold[i].ID < hold[j].ID })

	pend := make([]pendingDTO, 0, 16)
	for _, po := range e.Pending() {
		d := pendingDTO{
			ID: int32(po.Instrument), Side: sideName(po.Side), Qty: po.Qty,
			SignalD: po.SignalAt.TradingDay, PriceRef: po.PriceRef.String(),
			Tried: po.Tried, MaxSteps: po.MaxSteps, Tag: po.Tag,
		}
		if in := s.Uni.Get(po.Instrument); in != nil {
			d.Symbol, d.Name = in.Symbol, in.Name
		}
		pend = append(pend, d)
	}
	return hold, pend
}

// state 组装当前状态。调用方须持有 sess.mu。
func (s *Store) state(sess *Session) sessionState {
	e := sess.e
	st := sessionState{
		ID: sess.ID, Name: sess.Name,
		Step: e.Steps(), TotalSteps: e.NumSteps(),
		Done: e.Done(), Started: e.Steps() > 0,
		Universe:   len(e.Universe()),
		Equity:     e.EquityCents(),
		Cash:       e.Ledger().CashCents(),
		Initial:    e.Ledger().InitialCashCents(),
		Peak:       e.PeakEquityCents(),
		Realized:   e.Ledger().RealizedCents(),
		Fee:        e.Ledger().TotalFeeCents(),
		Slippage:   e.Ledger().SlippageCents(),
		Indicators: e.IndicatorKeys(),
		Warnings:   e.Ledger().Warnings(),
	}
	if tp, ok := e.Now(); ok {
		st.Day = tp.TradingDay
	}
	st.Evaluated, st.NotReady, st.HasWarmup = e.Warmup()
	if tp, ok := e.PeekAt(0); ok {
		st.FirstDay = tp.TradingDay
	}
	if tp, ok := e.PeekAt(e.NumSteps() - 1); ok {
		st.LastDay = tp.TradingDay
	}
	st.Holdings, st.Pending = s.snapDTOs(e)
	if ids, err := sess.cfg.ResolveUniverse(s.Uni, s.Adj); err == nil {
		st.Disclosures = sess.cfg.Disclosures(s.Uni, ids)
	}
	return st
}

// capture 把刚走完的这一步翻译成事件。调用方须持有 sess.mu。
func (s *Store) capture(e *eng.Engine) stepEvent {
	ev := stepEvent{
		Step: e.Steps(), Equity: e.EquityCents(),
		Cash: e.Ledger().CashCents(), Positions: e.Ledger().NumPositions(),
	}
	if tp, ok := e.Now(); ok {
		ev.Day = tp.TradingDay
	}
	for _, sig := range e.Signals() {
		d := signalDTO{
			ID: int32(sig.Instrument), Kind: sig.Kind.String(),
			Side: sideName(sig.Side), Strength: sig.Strength, Tag: sig.Tag,
		}
		if in := s.Uni.Get(sig.Instrument); in != nil {
			d.Symbol, d.Name = in.Symbol, in.Name
		}
		ev.Signals = append(ev.Signals, d)
	}
	for _, o := range e.Sized() {
		d := orderDTO{
			ID: int32(o.Instrument), Side: sideName(o.Side), Qty: o.Qty, Tag: o.Tag,
		}
		if in := s.Uni.Get(o.Instrument); in != nil {
			d.Symbol, d.Name = in.Symbol, in.Name
		}
		ev.Orders = append(ev.Orders, d)
	}
	for _, f := range e.Fills() {
		ev.Fills = append(ev.Fills, s.fillDTO(f))
	}
	for _, r := range e.Rejections() {
		ev.Rejects = append(ev.Rejects, s.rejectDTO(r))
	}
	return ev
}

// ---- 装配 ----

// assembleFor 由配置装出一台引擎。
//
// 抽出来是因为**其中有一处顺序不能错**：基准标的必须在裁子集之前从
// 全量数据里取（它不在标的池里，裁完就没了）。这条顺序写错不报错，
// 只会让报告里的对标区块凭空消失 —— v0.3.1 已经踩过一次。
// 两个入口各写一遍，迟早会有一个漏掉。
func (s *Store) assembleFor(cfg *config.Config) (*eng.Engine, *config.DataSet, error) {
	cfg.SetDataRoot(mustAbs(s.DataRoot))

	ids, err := cfg.ResolveUniverse(s.Uni, s.Adj)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) > maxUniverseForWeb {
		return nil, nil, fmt.Errorf(
			"标的池 %d 只，超过服务端上限 %d —— 装配时要把全量列式数据"+
				"裁成子集，那是一次拷贝，太大会把服务端撑爆。"+
				"请收窄 universe，或用命令行跑：go run ./cmd/backtest -config ...",
			len(ids), maxUniverseForWeb)
	}

	ds := &config.DataSet{
		Universe: s.Uni, Adjuster: s.Adj, CorpAct: s.Corp,
		Calendar: s.Cal, Root: mustAbs(s.DataRoot),
	}
	if cfg.Metrics.Benchmark != "" {
		in := s.Uni.BySymbol(cfg.Metrics.Benchmark)
		if in == nil {
			return nil, nil, fmt.Errorf("metrics.benchmark: 未找到标的 %q",
				cfg.Metrics.Benchmark)
		}
		if !ds.SetBenchmark(s.Col, in.ID) {
			return nil, nil, fmt.Errorf("metrics.benchmark: %q 没有行情数据",
				cfg.Metrics.Benchmark)
		}
	}
	ds.Columns = s.narrowCached(cfg, ids)

	e, err := cfg.Assemble(ds)
	if err != nil {
		return nil, nil, fmt.Errorf("装配失败: %w", err)
	}
	return e, ds, nil
}

// ---- 处理器 ----

func (s *Store) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
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
	e, ds, err := s.assembleFor(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	now := time.Now()
	sess := &Session{
		Name: cfg.Name, Config: body, Created: now, Used: now,
		cfg: cfg, ds: ds, e: e,
	}
	if err := s.Sessions.add(sess); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	writeJSON(w, map[string]any{"state": s.state(sess)})
}

func (s *Store) handleSessionList(w http.ResponseWriter, _ *http.Request) {
	out := make([]map[string]any, 0, maxSessions)
	for _, sess := range s.Sessions.list() {
		sess.mu.Lock()
		out = append(out, map[string]any{
			"id": sess.ID, "name": sess.Name,
			"step": sess.e.Steps(), "totalSteps": sess.e.NumSteps(),
			"created": sess.Created.Format("15:04:05"),
		})
		sess.mu.Unlock()
	}
	writeJSON(w, map[string]any{"sessions": out, "max": maxSessions})
}

func (s *Store) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Sessions.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已超时回收")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	writeJSON(w, map[string]any{"state": s.state(sess)})
}

func (s *Store) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.Sessions.drop(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSessionReset 把会话退回第 0 步。
//
// **重新装配而不是回放** —— 引擎没有「倒退一步」这回事：指标是增量的，
// 账本是累加的，回退需要每步都存快照。重装的代价是一次 Assemble
// （子集由 narrowCached 复用，实测 2.7 MB 边际），比存 5,260 份快照便宜得多。
func (s *Store) handleSessionReset(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Sessions.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已超时回收")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	e, ds, err := s.assembleFor(sess.cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "重置失败: %v", err)
		return
	}
	sess.e, sess.ds = e, ds
	writeJSON(w, map[string]any{"state": s.state(sess)})
}

// stepReq 步进请求。
//
// 三种停法可以叠加，谁先到停在谁：
//
//	n     走固定步数
//	day   走到某个交易日（含）
//	until 走到下一次「有事发生」——  signal / fill / reject / end
//
// **until 是这套 API 最有用的一个**。调试时真正想问的是
// 「下一次成交发生在哪天、为什么」，而不是「再走 37 步」。
// 没有它，用户只能一步一步点过去，在 5,260 步的池子上不现实。
type stepReq struct {
	N     int    `json:"n"`
	Day   int32  `json:"day"`
	Until string `json:"until"`
}

func (s *Store) handleSessionStep(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Sessions.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已超时回收")
		return
	}
	body, err := readAll(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: %v", err)
		return
	}
	var req stepReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: %v", err)
			return
		}
	}
	req.Until = strings.ToLower(strings.TrimSpace(req.Until))
	switch req.Until {
	case "", "signal", "fill", "reject", "end":
	default:
		writeErr(w, http.StatusBadRequest,
			"未知的 until %q，可选：signal / fill / reject / end", req.Until)
		return
	}
	// 只给 n 时按 n 走；给了 day/until 时 n 退化为**步数上限**
	limit := req.N
	if limit <= 0 {
		if req.Day == 0 && req.Until == "" {
			limit = 1 // 什么都不给 = 单步
		} else {
			limit = maxStepsPerCall
		}
	}
	if limit > maxStepsPerCall {
		limit = maxStepsPerCall
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	e := sess.e

	events := make([]stepEvent, 0, 32)
	advanced, quiet, truncated := 0, 0, false
	stoppedBy := "步数"
	t0 := time.Now()

	for advanced < limit {
		if e.Done() {
			stoppedBy = "已到末尾"
			break
		}
		if _, err := e.Step(); err != nil {
			writeErr(w, http.StatusInternalServerError, "引擎步进失败: %v", err)
			return
		}
		advanced++
		ev := s.capture(e)

		// 无事发生的步只计数不入列 —— 走 500 步时用户要看的是
		// 「哪几天有事」，不是 500 份空表
		if ev.quiet() {
			quiet++
		} else if len(events) < maxDetailSteps {
			events = append(events, ev)
		} else {
			truncated = true
		}

		if hit(ev, req.Until) {
			stoppedBy = "命中 " + req.Until
			break
		}
		if req.Day != 0 && ev.Day >= req.Day {
			stoppedBy = "到达指定交易日"
			break
		}
	}
	if e.Done() && stoppedBy == "步数" {
		stoppedBy = "已到末尾"
	}

	// 停在无事发生的一步时，也把它给出来 —— 否则界面上「当前在哪」是空的
	if len(events) == 0 && advanced > 0 {
		events = append(events, s.capture(e))
	}

	writeJSON(w, map[string]any{
		"state": s.state(sess), "steps": events,
		"advanced": advanced, "quiet": quiet,
		"stoppedBy": stoppedBy, "truncated": truncated,
		"elapsedMs": time.Since(t0).Milliseconds(),
	})
}

// hit 判断这一步是否满足停止条件。
func hit(ev stepEvent, until string) bool {
	switch until {
	case "signal":
		return len(ev.Signals) > 0
	case "fill":
		return len(ev.Fills) > 0
	case "reject":
		return len(ev.Rejects) > 0
	}
	return false // "" 与 "end" 都不靠单步条件停
}

// ---- 标的检视 ----

// inspectDTO 回答「这一天，这只标的到底是什么情况」。
//
// **这是单步调试的核心问题**，而它的答案不在成交记录里：
// 要同时看到当日 bar、涨跌停价、指标值（含是否就绪）、持仓、可卖、在途。
// 少任何一项，「今天为什么没买它」都答不上来 ——
// 可能是指标没就绪、可能是钱不够、可能是单还在队列里、也可能是一字涨停。
type inspectDTO struct {
	ID        int32  `json:"id"`
	Symbol    string `json:"symbol"`
	Name      string `json:"name"`
	Day       int32  `json:"day"`
	HasBar    bool   `json:"hasBar"`
	Open      int64  `json:"open"`
	High      int64  `json:"high"`
	Low       int64  `json:"low"`
	Close     int64  `json:"close"`
	PreClose  int64  `json:"preclose"`
	Volume    int64  `json:"volume"`
	Amount    int64  `json:"amount"`
	Suspended bool   `json:"suspended"`
	IsST      bool   `json:"isST"`
	LimitUp   int64  `json:"limitUp"`
	LimitDn   int64  `json:"limitDn"`
	HasLimit  bool   `json:"hasLimit"`
	// AdjClose 后复权收盘价 —— 指标吃的是这个，与 Close 不同是正常的
	AdjClose   int64          `json:"adjClose"`
	PriceScale int32          `json:"priceScale"`
	Held       int64          `json:"held"`
	Available  int64          `json:"available"`
	Cost       int64          `json:"cost"`
	Indicators []indicatorDTO `json:"indicators"`
	Pending    []pendingDTO   `json:"pending"`
}

type indicatorDTO struct {
	Key    string    `json:"key"`
	Names  []string  `json:"names"`
	Values []float64 `json:"values"`
	// Ready 为 false 时 Values 是**垃圾**（预热未完成）。
	// 必须原样传给界面 —— 把未就绪的指标画成 0 会让人以为
	// 「指标是 0 所以没信号」，而真相是「指标还没算出来」
	Ready bool `json:"ready"`
}

func (s *Store) handleSessionInspect(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Sessions.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已超时回收")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("instrument"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "需要 ?instrument=<代码或 ID>")
		return
	}
	in := s.Uni.BySymbol(q)
	if in == nil {
		if n, err := strconv.Atoi(q); err == nil {
			in = s.Uni.Get(mktdata.InstrumentID(n))
		}
	}
	if in == nil {
		writeErr(w, http.StatusNotFound, "未找到标的 %q", q)
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	e := sess.e

	d := inspectDTO{
		ID: int32(in.ID), Symbol: in.Symbol, Name: in.Name,
		PriceScale: in.PriceScale,
	}
	if tp, ok := e.Now(); ok {
		d.Day = tp.TradingDay
	}
	if b, ok := e.BarAt(in.ID); ok {
		d.HasBar = true
		d.Open, d.High, d.Low, d.Close = b.Open, b.High, b.Low, b.Close
		d.PreClose, d.Volume, d.Amount = b.PreClose, b.Volume, b.Amount
		d.Suspended, d.IsST = b.Suspended(), b.IsST != 0
		d.LimitUp, d.LimitDn, d.HasLimit = e.Market().LimitPrices(in, b)
		d.AdjClose = s.Adj.Adjust(in.ID, b.TradingDay, b.Close, mktdata.AdjHFQ)
	}
	ex := e.Ledger().Exposure(in.ID)
	d.Held, d.Cost = ex.Long, ex.LongCost
	d.Available = e.AvailableAt(in.ID)

	for _, key := range e.IndicatorKeys() {
		names, vals, ready, ok := e.IndicatorAt(in.ID, key)
		if !ok {
			// 指标实例要到该标的第一根 bar 才创建 —— 「还没有」
			// 与「有但没就绪」是两回事，都要能看出来
			d.Indicators = append(d.Indicators,
				indicatorDTO{Key: key, Ready: false})
			continue
		}
		d.Indicators = append(d.Indicators,
			indicatorDTO{Key: key, Names: names, Values: vals, Ready: ready})
	}
	for _, po := range e.Pending() {
		if po.Instrument != in.ID {
			continue
		}
		d.Pending = append(d.Pending, pendingDTO{
			ID: int32(po.Instrument), Symbol: in.Symbol, Name: in.Name,
			Side: sideName(po.Side), Qty: po.Qty,
			SignalD: po.SignalAt.TradingDay, PriceRef: po.PriceRef.String(),
			Tried: po.Tried, MaxSteps: po.MaxSteps, Tag: po.Tag,
		})
	}
	writeJSON(w, d)
}

// ---- 快照 ----
//
// 快照是 v0.4 与 v0.6 的共同地基：单步调试的「存档/读档」与实盘的
// 「每日从昨天恢复」是同一个能力。这里顺带把它跑通 ——
// 不然快照只在单元测试里被验证过，从没在真实工作流里用过。

func (s *Store) handleSessionSnapshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Sessions.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已超时回收")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	b, err := sess.e.Snapshot()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "快照失败: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="snapshot-%s-step%d.json"`,
			sess.ID, sess.e.Steps()))
	_, _ = w.Write(b)
}

func (s *Store) handleSessionRestore(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Sessions.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已超时回收")
		return
	}
	body, err := readAll(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: %v", err)
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// 先重装一台干净的引擎再恢复 —— 直接往跑过的引擎上盖，
	// 恢复失败时会留下一个半新半旧的状态，比报错难查得多
	e, ds, err := s.assembleFor(sess.cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "重装失败: %v", err)
		return
	}
	if err := e.Restore(body); err != nil {
		writeErr(w, http.StatusBadRequest, "恢复失败: %v", err)
		return
	}
	sess.e, sess.ds = e, ds
	writeJSON(w, map[string]any{"state": s.state(sess)})
}
