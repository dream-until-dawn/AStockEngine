package engine

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Config 是一次运行的装配配置。
type Config struct {
	Params Params
	// IndicatorAdjMode 指标喂入的复权方式。默认后复权 —— 序列必须连续，
	// 否则除权日会产生假信号（设计 6.1）。
	IndicatorAdjMode mktdata.AdjMode
	// InitialCashCents 初始资金（分）
	InitialCashCents int64
	// DividendTaxPPM 红利税率（百万分之一）。税率属规则不属数据，
	// 实际随持股期限分档，本刀先用固定值。
	DividendTaxPPM int64
	// ImplySplitFromFactor 当某日有复权因子事件却无 corporate_action 记录时，
	// 是否按因子比例推算送转并入账。
	//
	// 默认开启（评审决议 2）：ETL 侧约 6,770 个因子事件缺记录，
	// 完全不处理会让这些日期出现「价格跳变但账户没变」的失真。
	// 这是有损近似，Portfolio 会逐条留痕。
	ImplySplitFromFactor bool
}

// Engine 是回测的核心状态机。
//
// **它不含任何内部 for 循环**（C4）：每调用一次 Step 前进一个事件时点。
// 单步调试、批量海选、实盘增量三种模式因此共用同一个核心。
type Engine struct {
	col      *mktdata.Columns
	cur      *mktdata.Cursor
	adj      *mktdata.Adjuster
	uni      *mktdata.Universe
	corp     *mktdata.CorpActions
	broker   *trading.Broker
	market   trading.Market
	pf       *trading.Portfolio
	strategy Strategy
	cfg      Config

	factories  map[string]IndicatorFactory
	keys       []string
	indicators map[string]map[mktdata.InstrumentID]indicator.Indicator

	pending []trading.PendingOrder
	fills   []trading.Fill
	rejects []trading.Rejection

	steps int
	ctx   *stepCtx
	// prices 复用同一张表用于估值，避免每步分配
	prices map[mktdata.InstrumentID]int64
}

// Deps 是引擎依赖的数据与模块。
type Deps struct {
	Columns   *mktdata.Columns
	Universe  *mktdata.Universe
	Adjuster  *mktdata.Adjuster
	CorpAct   *mktdata.CorpActions
	Market    trading.Market
	Broker    *trading.Broker
	Portfolio *trading.Portfolio
}

// New 装配一个引擎。
func New(d Deps, s Strategy, cfg Config) (*Engine, error) {
	if d.Columns == nil || d.Universe == nil || s == nil {
		return nil, fmt.Errorf("列式数据、标的表与策略均不可为空")
	}
	if d.Market == nil {
		d.Market = trading.NewAShareMarket()
	}
	if d.Broker == nil {
		d.Broker = trading.NewBroker(d.Market, trading.ZeroFee{}, nil,
			trading.DefaultBrokerConfig())
	}
	if d.Portfolio == nil {
		d.Portfolio = trading.NewPortfolio(cfg.InitialCashCents)
	}
	e := &Engine{
		col: d.Columns, cur: mktdata.NewCursor(d.Columns), adj: d.Adjuster,
		uni: d.Universe, corp: d.CorpAct, broker: d.Broker, market: d.Market,
		pf: d.Portfolio, strategy: s, cfg: cfg,
		factories:  make(map[string]IndicatorFactory),
		indicators: make(map[string]map[mktdata.InstrumentID]indicator.Indicator),
		prices:     make(map[mktdata.InstrumentID]int64, 1024),
	}
	if e.cfg.Params == nil {
		e.cfg.Params = Params{}
	}
	e.ctx = &stepCtx{e: e}
	if err := s.Init(e); err != nil {
		return nil, fmt.Errorf("策略 %s 初始化失败: %w", s.Name(), err)
	}
	return e, nil
}

// ---- InitContext ----

func (e *Engine) Params() Params                   { return e.cfg.Params }
func (e *Engine) Universe() []mktdata.InstrumentID { return e.col.Instruments() }

func (e *Engine) Instrument(id mktdata.InstrumentID) *mktdata.Instrument {
	return e.uni.Get(id)
}

func (e *Engine) Use(key string, f IndicatorFactory) {
	if _, dup := e.factories[key]; dup {
		return
	}
	e.factories[key] = f
	e.keys = append(e.keys, key)
	e.indicators[key] = make(map[mktdata.InstrumentID]indicator.Indicator, 1024)
}

// ---- 状态机 ----

func (e *Engine) Done() bool                    { return e.cur.Done() }
func (e *Engine) Steps() int                    { return e.steps }
func (e *Engine) Portfolio() *trading.Portfolio { return e.pf }

// Step 前进一个事件时点。
//
// 阶段顺序不可颠倒，每一处都有理由：
//
//	1. 推进游标           当前 bar 变为可见
//	2. 更新指标           策略读到的指标必须**已含当前 bar**
//	3. 公司行动入账        除权除息在开盘前生效，**必须先于撮合** ——
//	                      否则除权日的成交价与持仓基准会错配
//	4. 撮合到期订单        **必须先于策略** —— 否则策略看不到已成交结果会重复下单
//	5. 调用策略           策略此时看到的是最新持仓与指标
//	6. 新订单定价入队      由 Market 决定最早可执行时点；
//	                      可在本时点成交的（盘后定价）立即撮合
func (e *Engine) Step() (mktdata.TimePoint, error) {
	tp, updated, ok := e.cur.Advance()
	if !ok {
		return mktdata.TimePoint{}, fmt.Errorf("已到末尾")
	}
	e.steps++
	e.fills = e.fills[:0]
	e.rejects = e.rejects[:0]

	// 2. 指标
	for _, id := range updated {
		bar, ok := e.cur.Bar(id)
		if !ok {
			continue
		}
		fed := bar
		if e.adj != nil && e.cfg.IndicatorAdjMode != mktdata.AdjNone {
			fed = e.adj.AdjustBar(id, bar, e.cfg.IndicatorAdjMode)
		}
		for _, key := range e.keys {
			m := e.indicators[key]
			ind, exists := m[id]
			if !exists {
				ind = e.factories[key]()
				m[id] = ind
			}
			ind.Update(fed)
		}
	}

	// 3. 公司行动
	e.applyCorporateActions(tp, updated)

	// 4. 撮合到期订单
	e.matchPending(tp)

	// 5. 策略
	orders, err := e.strategy.OnBar(e.ctxFor(tp, updated))
	if err != nil {
		return tp, fmt.Errorf("策略在 %d 报错: %w", tp.TradingDay, err)
	}

	// 6. 定价入队；可在本时点成交的立即撮合
	e.enqueue(orders, tp)
	return tp, nil
}

// applyCorporateActions 处理当日的分红送配。
func (e *Engine) applyCorporateActions(tp mktdata.TimePoint, updated []mktdata.InstrumentID) {
	if e.corp != nil {
		for _, a := range e.corp.OnDay(tp.TradingDay) {
			if e.pf.Position(a.Instrument) == nil {
				continue // 未持仓则无需入账
			}
			e.pf.ApplyCorporateAction(trading.CorporateAction{
				Instrument: a.Instrument, ExDate: a.ExDate,
				CashBeforeTax: a.CashBeforeTax,
				StockDividend: a.StockDividend, StockTransfer: a.StockTransfer,
				RightsRatio:   a.RightsRatio, RightsPrice: a.RightsPrice,
			}, e.cfg.DividendTaxPPM, tp.TsClose)
		}
	}

	// 有因子事件却无分红记录时按因子推算（评审决议 2）。
	// 只对**持仓中**的标的做，避免在全市场上做无谓的查表。
	if !e.cfg.ImplySplitFromFactor || e.adj == nil {
		return
	}
	for id, p := range e.pf.Positions {
		if p.Total <= 0 {
			continue
		}
		ratio, isEvent := e.adj.EventRatio(id, tp.TradingDay)
		if !isEvent || ratio <= 1.0 {
			continue
		}
		if e.corp != nil && e.corp.Has(id, tp.TradingDay) {
			continue // 已有记录，按记录处理即可
		}
		e.pf.ApplyImpliedSplit(id, tp.TradingDay, ratio, tp.TsClose)
	}
}

// matchPending 撮合已到期的订单，并淘汰过期的。
func (e *Engine) matchPending(tp mktdata.TimePoint) {
	if len(e.pending) == 0 {
		return
	}
	keep := e.pending[:0]
	for i := range e.pending {
		po := e.pending[i]
		if po.NotBefore > tp.TsClose {
			keep = append(keep, po)
			continue
		}
		if e.tryMatch(&po, tp) {
			continue // 已成交
		}
		po.Tried++
		if po.Tried >= po.MaxSteps {
			// 过期而非无限挂着。涨停买不进的订单若一直存活，
			// 会在几个月后突然成交 —— 隐蔽但严重的回测失真。
			e.rejects = append(e.rejects, trading.Rejection{
				Order: po.Order, At: tp, Reason: trading.RejectExpired,
				Detail: fmt.Sprintf("信号于 %d 发出，%d 次尝试未成交",
					po.SignalAt.TradingDay, po.Tried),
			})
			continue
		}
		keep = append(keep, po)
	}
	e.pending = keep
}

// tryMatch 尝试撮合一笔订单，成交则记账。返回是否成交。
func (e *Engine) tryMatch(po *trading.PendingOrder, tp mktdata.TimePoint) bool {
	inst := e.uni.Get(po.Instrument)
	if inst == nil {
		e.rejects = append(e.rejects, trading.Rejection{
			Order: po.Order, At: tp, Reason: trading.RejectNotListed,
			Detail: "标的表中不存在",
		})
		return true // 不再重试
	}
	bar, ok := e.cur.Bar(po.Instrument)
	if !ok {
		return false // 该时点无 bar，留待下次
	}
	fill, rej, ok := e.broker.Match(po, inst, bar, tp, e.pf)
	if !ok {
		e.rejects = append(e.rejects, rej)
		return false
	}
	sellable := e.market.SellableFrom(inst, tp)
	if err := e.pf.ApplyFill(fill, sellable); err != nil {
		// 撮合已校验过资金与持仓，走到这里说明有内部不一致，必须留痕
		e.rejects = append(e.rejects, trading.Rejection{
			Order: po.Order, At: tp, Reason: trading.RejectInsufficientCash,
			Detail: "记账失败: " + err.Error(),
		})
		return false
	}
	e.fills = append(e.fills, fill)
	return true
}

// enqueue 给新订单定价并入队；可在本时点成交的立即撮合。
func (e *Engine) enqueue(orders []trading.Order, tp mktdata.TimePoint) {
	for _, o := range orders {
		if o.Qty <= 0 {
			continue
		}
		inst := e.uni.Get(o.Instrument)
		if inst == nil {
			e.rejects = append(e.rejects, trading.Rejection{
				Order: o, At: tp, Reason: trading.RejectNotListed,
				Detail: "标的表中不存在",
			})
			continue
		}
		w, ok := e.market.NextExecutable(inst, tp)
		if !ok {
			continue
		}
		po := trading.PendingOrder{
			Order: o, SignalAt: tp, NotBefore: w.NotBefore,
			PriceRef: w.PriceRef, MaxSteps: w.MaxSteps,
		}
		if po.MaxSteps < 1 {
			po.MaxSteps = 1
		}
		// 盘后固定价格交易：同一时点即可成交
		if po.NotBefore <= tp.TsClose {
			if e.tryMatch(&po, tp) {
				continue
			}
			po.Tried++
			if po.Tried >= po.MaxSteps {
				e.rejects = append(e.rejects, trading.Rejection{
					Order: po.Order, At: tp, Reason: trading.RejectExpired,
					Detail: "盘后定价当日未成交",
				})
				continue
			}
		}
		e.pending = append(e.pending, po)
	}
}

// RunAll 一直步进到结束。**它只是 Step 的外壳**，核心仍是状态机。
func (e *Engine) RunAll() error {
	for !e.Done() {
		if _, err := e.Step(); err != nil {
			return err
		}
	}
	return nil
}

// EquityCents 按当前时点的收盘价计算总权益。
func (e *Engine) EquityCents() int64 {
	for id := range e.prices {
		delete(e.prices, id)
	}
	for id, p := range e.pf.Positions {
		if p.Total <= 0 {
			continue
		}
		if b, ok := e.cur.Bar(id); ok {
			e.prices[id] = b.Close
		}
	}
	return e.pf.EquityCents(e.prices)
}

// ---- 快照（C6）----

type snapshot struct {
	Steps      int                                   `json:"steps"`
	Cursor     mktdata.CursorState                   `json:"cursor"`
	Indicators map[string]map[string]indicator.State `json:"indicators"`
	Portfolio  trading.PortfolioState                `json:"portfolio"`
	// Pending 必须进快照 —— 否则实盘恢复后，昨日挂出的未成交单会凭空消失
	Pending []trading.PendingOrder `json:"pending"`
	// Strategy 策略自身的跨步状态。实现 StatefulStrategy 的策略才有。
	Strategy []byte `json:"strategy,omitempty"`
}

func (e *Engine) Snapshot() ([]byte, error) {
	s := snapshot{
		Steps: e.steps, Cursor: e.cur.Snapshot(),
		Indicators: make(map[string]map[string]indicator.State, len(e.keys)),
		Portfolio:  e.pf.Snapshot(),
		Pending:    append([]trading.PendingOrder(nil), e.pending...),
	}
	if ss, ok := e.strategy.(StatefulStrategy); ok {
		b, err := ss.SnapshotState()
		if err != nil {
			return nil, fmt.Errorf("策略状态快照失败: %w", err)
		}
		s.Strategy = b
	}
	for _, key := range e.keys {
		m := make(map[string]indicator.State, len(e.indicators[key]))
		for id, ind := range e.indicators[key] {
			m[fmt.Sprint(int32(id))] = ind.Snapshot()
		}
		s.Indicators[key] = m
	}
	return json.Marshal(s)
}

func (e *Engine) Restore(data []byte) error {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("解析快照失败: %w", err)
	}
	if err := e.cur.Restore(s.Cursor); err != nil {
		return err
	}
	for _, key := range e.keys {
		saved, ok := s.Indicators[key]
		if !ok {
			return fmt.Errorf("快照缺少指标 %q —— 策略声明的指标与快照不一致", key)
		}
		m := make(map[mktdata.InstrumentID]indicator.Indicator, len(saved))
		for idStr, st := range saved {
			var raw int32
			if _, err := fmt.Sscan(idStr, &raw); err != nil {
				return fmt.Errorf("快照标的 ID %q 无法解析: %w", idStr, err)
			}
			ind := e.factories[key]()
			if err := ind.Restore(st); err != nil {
				return fmt.Errorf("恢复指标 %s/%s 失败: %w", key, idStr, err)
			}
			m[mktdata.InstrumentID(raw)] = ind
		}
		e.indicators[key] = m
	}
	e.pf.Restore(s.Portfolio)
	e.pending = append([]trading.PendingOrder(nil), s.Pending...)
	e.steps = s.Steps
	if ss, ok := e.strategy.(StatefulStrategy); ok {
		if len(s.Strategy) == 0 {
			return fmt.Errorf("策略 %s 声明了跨步状态，但快照中没有 —— "+
				"该快照可能由另一个策略产生", e.strategy.Name())
		}
		if err := ss.RestoreState(s.Strategy); err != nil {
			return fmt.Errorf("恢复策略状态失败: %w", err)
		}
	}
	return nil
}

// ---- StepContext 实现 ----

type stepCtx struct {
	e        *Engine
	time     mktdata.TimePoint
	universe []mktdata.InstrumentID
}

func (e *Engine) ctxFor(tp mktdata.TimePoint, u []mktdata.InstrumentID) *stepCtx {
	e.ctx.time, e.ctx.universe = tp, u
	return e.ctx
}

func (c *stepCtx) Time() mktdata.TimePoint          { return c.time }
func (c *stepCtx) Universe() []mktdata.InstrumentID { return c.universe }
func (c *stepCtx) History(id mktdata.InstrumentID) mktdata.History {
	return c.e.cur.History(id)
}
func (c *stepCtx) Bar(id mktdata.InstrumentID) (mktdata.Bar, bool) { return c.e.cur.Bar(id) }
func (c *stepCtx) Instrument(id mktdata.InstrumentID) *mktdata.Instrument {
	return c.e.uni.Get(id)
}
func (c *stepCtx) Portfolio() *trading.Portfolio      { return c.e.pf }
func (c *stepCtx) Pending() []trading.PendingOrder    { return c.e.pending }
func (c *stepCtx) Fills() []trading.Fill              { return c.e.fills }
func (c *stepCtx) Rejections() []trading.Rejection    { return c.e.rejects }
func (c *stepCtx) EquityCents() int64                 { return c.e.EquityCents() }

func (c *stepCtx) Available(id mktdata.InstrumentID) int64 {
	return c.e.pf.Available(id, c.time.TsClose)
}

func (c *stepCtx) Indicator(id mktdata.InstrumentID, key string) (indicator.Indicator, bool) {
	m, ok := c.e.indicators[key]
	if !ok {
		return nil, false
	}
	ind, ok := m[id]
	return ind, ok
}

func (c *stepCtx) AdjClose(id mktdata.InstrumentID, back int, mode mktdata.AdjMode) (int64, bool) {
	if mode == mktdata.AdjQFQ {
		return 0, false
	}
	h := c.e.cur.History(id)
	raw, ok := h.Close(back)
	if !ok {
		return 0, false
	}
	if mode == mktdata.AdjNone || c.e.adj == nil {
		return raw, true
	}
	day, _ := h.TradingDay(back)
	return c.e.adj.Adjust(id, day, raw, mode), true
}

// ---- 本步明细 ----
//
// 单步运行要「详细记录」（ROADMAP 需求 3），而记录的消费者是引擎外部：
// CLI、Web、海选汇总。故成交与拒单必须在 Engine 上可读，
// 不能只经 StepContext 暴露给策略 —— 策略之外没人能拿到。
//
// 返回的切片在下一次 Step() 时被复用，调用方要留存须自行拷贝。

// Fills 返回最近一步的成交。
func (e *Engine) Fills() []trading.Fill { return e.fills }

// Rejections 返回最近一步的拒单，每条带结构化原因与数值 detail。
func (e *Engine) Rejections() []trading.Rejection { return e.rejects }

// LastCounts 最近一步的成交数与拒单数。
func (e *Engine) LastCounts() (fills, rejects int) { return len(e.fills), len(e.rejects) }
