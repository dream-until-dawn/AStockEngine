package engine

import (
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"fmt"
	"hash"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/record"
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
	// TradeFrom 这一天之前只喂指标不交易（YYYYMMDD，0 表示不设限）。
	//
	// 为 Walk-Forward 而加：窗口切开后每段开头的指标是**未预热**的
	// （MACD(26) 要 26 根 bar 才就绪），不处理的话每个窗口的头一个月
	// 被系统性地削掉信号 —— 那在 18 个窗口上是一致的**偏差**，不是噪声。
	//
	// 于是子集多裁一段预热前缀，前缀期内照常喂指标、处理公司行动、
	// 撮合在途单，只是不产生新的信号与订单。
	TradeFrom int32
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
	led      trading.Ledger
	strategy Strategy
	sizer    trading.Sizer
	risk     trading.RiskChain
	rec      record.Recorder
	exits    trading.ExitChain
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

	// stepEquity 本步开始撮合后的权益，Sizer 与 Risk 共用同一个数 ——
	// 各自重算会让「按权益的 10%」在同一步里指向不同的值
	stepEquity int64
	// peakEquity 历史峰值权益，回撤类风控的基准。**必须进快照** ——
	// 不进的话恢复后峰值归零，熔断规则立刻失效
	peakEquity int64

	// signals 本步策略发出的信号，供 Recorder 与单步调试读取
	signals []trading.Signal
	// sized 本步 Sizer 折算出的订单（风控之前）
	sized []trading.Order
	// liquidations 累计强平。现货账本永远为空 —— 留着是因为强平
	// **必须能被记录与展示**：权益突然掉一截却查不到原因是最糟的失真
	liquidations []trading.Liquidation

	// resultHash 逐笔成交的滚动哈希（C5）。在引擎内算，
	// 这样它不受 recorder.level 影响 —— 记录多少是给人看的事，
	// 不该改变「这次运行算出了什么」
	resultHash hash.Hash
}

// Deps 是引擎依赖的数据与模块。
type Deps struct {
	Columns  *mktdata.Columns
	Universe *mktdata.Universe
	Adjuster *mktdata.Adjuster
	CorpAct  *mktdata.CorpActions
	Market   trading.Market
	Broker   *trading.Broker
	// Ledger 账本。缺省为现货账本（A 股）；保证金账本换实现即可，
	// 引擎不需要知道两者的区别
	Ledger trading.Ledger
	// Sizer 缺省为 equal_weight{slots:10, base:initial}
	Sizer trading.Sizer
	// Risk 缺省为空链（不拦截）
	Risk trading.RiskChain
	// Exits 离场规则链（止损 / 止盈 / 移动止损）。缺省为空链。
	//
	// 它是**第三类决策来源**，与策略并列而非从属：策略产生 alpha，
	// 离场规则做风险管理。排在策略之前，且信号会覆盖策略在同一标的上的判断
	Exits trading.ExitChain
	// Recorder 缺省为 summary 级内存记录器。
	//
	// **记录由引擎发起而不是由驱动方发起**：单步驱动、批量海选、实盘增量
	// 三种模式共用同一个核心（C4），记录逻辑若写在驱动里就要写三遍。
	Recorder record.Recorder
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
	if d.Ledger == nil {
		d.Ledger = trading.NewPortfolio(cfg.InitialCashCents)
	}
	if d.Sizer == nil {
		sz, err := trading.Sizers.Build("equal_weight", nil)
		if err != nil {
			return nil, err
		}
		d.Sizer = sz
	}
	if d.Recorder == nil {
		d.Recorder = record.NewMemory(record.Summary, 0)
	}
	e := &Engine{
		col: d.Columns, cur: mktdata.NewCursor(d.Columns), adj: d.Adjuster,
		uni: d.Universe, corp: d.CorpAct, broker: d.Broker, market: d.Market,
		led: d.Ledger, strategy: s, sizer: d.Sizer, risk: d.Risk, exits: d.Exits,
		rec: d.Recorder, cfg: cfg,
		factories:  make(map[string]IndicatorFactory),
		indicators: make(map[string]map[mktdata.InstrumentID]indicator.Indicator),
		prices:     make(map[mktdata.InstrumentID]int64, 1024),
		resultHash: sha256.New(),
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

func (e *Engine) Done() bool             { return e.cur.Done() }
func (e *Engine) Steps() int             { return e.steps }
func (e *Engine) Ledger() trading.Ledger { return e.led }

// Market 返回本次运行的市场规则实现。
//
// 报告侧需要它来问「一年有多少个交易日」—— 那个答案是**市场相关**的
// （A 股 243、加密 365），以前写死成 mktdata.MarketAShare 去查日历，
// 换了市场就会静默给出错的年化系数。
func (e *Engine) Market() trading.Market { return e.market }

// Step 前进一个事件时点。
//
// 阶段顺序不可颠倒，每一处都有理由：
//
//  1. 推进游标           当前 bar 变为可见
//  2. 更新指标           策略读到的指标必须**已含当前 bar**
//  3. 公司行动入账        除权除息在开盘前生效，**必须先于撮合** ——
//     否则除权日的成交价与持仓基准会错配
//  4. 撮合到期订单        **必须先于策略** —— 否则策略看不到已成交结果会重复下单
//  5. 结算权益与峰值      Sizer 与 Risk 共用同一个权益快照，同一步里不会各算各的
//  6. 调用策略           策略此时看到的是最新持仓与指标，只出**信号**
//  7. Sizer 折算数量      信号 → 订单
//  8. Risk 链逐单把关 + 定价入队
//     由 Market 决定最早可执行时点；
//     可在本时点成交的（盘后定价）立即撮合
func (e *Engine) Step() (mktdata.TimePoint, error) {
	tp, updated, ok := e.cur.Advance()
	if !ok {
		return mktdata.TimePoint{}, fmt.Errorf("已到末尾")
	}
	e.steps++
	e.fills = e.fills[:0]
	e.rejects = e.rejects[:0]
	e.signals = e.signals[:0]
	e.sized = e.sized[:0]

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

	// 4.5 账本重估。保证金账本在这里判断强平 —— **必须先于策略**：
	//     强平是市场施加的，不是策略的决定。现货账本什么也不做
	if liq := e.led.Mark(e.marks(), tp); len(liq) > 0 {
		e.liquidations = append(e.liquidations, liq...)
	}

	// 5. 权益快照与峰值。放在撮合之后、策略之前：
	//    策略、Sizer、Risk 看到的是同一个已结算的账户状态
	e.ctxFor(tp, updated)
	e.stepEquity = e.EquityCents()
	if e.stepEquity > e.peakEquity {
		e.peakEquity = e.stepEquity
	}

	// 5.5~8 只在 TradeFrom 之后执行。预热期照常走完 2~5
	// （指标、公司行动、撮合、重估）—— 撮合也要走，否则预热期跨过来的
	// 在途单会永远挂着
	if e.cfg.TradeFrom == 0 || tp.TradingDay >= e.cfg.TradeFrom {
		// 5.5 离场规则 —— **在策略之前**。
		//
		// 止损必须压过策略的判断：策略说「继续持有」时止损也要能平掉。
		// 靠合并顺序来保证优先级太脆弱，所以直接排在前面，
		// 并在下面显式覆盖策略在同一标的上的信号
		exitSigs := e.exits.OnStep(e.ctx)

		// 6. 策略 —— 只出信号
		sigs, err := e.strategy.OnBar(e.ctx)
		if err != nil {
			return tp, fmt.Errorf("策略在 %d 报错: %w", tp.TradingDay, err)
		}
		sigs = mergeExits(exitSigs, sigs)
		e.signals = append(e.signals, sigs...)

		// 7. Sizer 折算数量
		orders := e.sizer.Size(sigs, e.ctx)
		e.sized = append(e.sized, orders...)

		// 8. Risk 把关、定价入队；可在本时点成交的立即撮合
		e.enqueue(orders, tp)
	}

	// 9. 记录。**放在最后** —— 盘后定价的单会在第 8 步就成交，
	//    提前记会漏掉它们
	e.rec.OnStep(record.Step{
		Time: tp, EquityCents: e.stepEquity, CashCents: e.led.CashCents(),
		Positions:  e.led.NumPositions(),
		NumSignals: len(e.signals), NumFills: len(e.fills), NumRejects: len(e.rejects),
		Signals: e.signals, Sized: e.sized, Fills: e.fills, Rejections: e.rejects,
	})
	return tp, nil
}

// mergeExits 把离场信号排在前面，并**丢掉策略在同一标的上的信号**。
//
// 不丢的话会出现两种荒唐情形：策略当天要加仓、止损当天要清仓，
// 两张单一起进 Sizer；或者策略也发了卖出，于是同一只标的被下两次卖单，
// 第二张必然因无券可卖被拒。
//
// **离场赢**。这就是把它排在策略之前的全部意义。
func mergeExits(exits, sigs []Signal) []Signal {
	if len(exits) == 0 {
		return sigs
	}
	blocked := make(map[mktdata.InstrumentID]bool, len(exits))
	for _, s := range exits {
		blocked[s.Instrument] = true
	}
	out := make([]Signal, 0, len(exits)+len(sigs))
	out = append(out, exits...)
	for _, s := range sigs {
		if blocked[s.Instrument] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// applyCorporateActions 处理当日的分红送配。
func (e *Engine) applyCorporateActions(tp mktdata.TimePoint, updated []mktdata.InstrumentID) {
	if e.corp != nil {
		for _, a := range e.corp.OnDay(tp.TradingDay) {
			if e.led.Exposure(a.Instrument).IsEmpty() {
				continue // 未持仓则无需入账
			}
			e.led.ApplyCorporateAction(trading.CorporateAction{
				Instrument: a.Instrument, ExDate: a.ExDate,
				CashBeforeTax: a.CashBeforeTax,
				StockDividend: a.StockDividend, StockTransfer: a.StockTransfer,
				RightsRatio: a.RightsRatio, RightsPrice: a.RightsPrice,
			}, e.cfg.DividendTaxPPM, tp.TsClose)
		}
	}

	// 有因子事件却无分红记录时按因子推算（评审决议 2）。
	// 只对**持仓中**的标的做，避免在全市场上做无谓的查表。
	if !e.cfg.ImplySplitFromFactor || e.adj == nil {
		return
	}
	e.led.EachExposure(func(id mktdata.InstrumentID, _ trading.Exposure) bool {
		ratio, isEvent := e.adj.EventRatio(id, tp.TradingDay)
		if !isEvent || ratio <= 1.0 {
			return true
		}
		if e.corp != nil && e.corp.Has(id, tp.TradingDay) {
			return true // 已有记录，按记录处理即可
		}
		e.led.ApplyImpliedSplit(id, tp.TradingDay, ratio, tp.TsClose)
		return true
	})
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
	fill, rej, ok := e.broker.Match(po, inst, bar, tp, e.led)
	if !ok {
		e.rejects = append(e.rejects, rej)
		return false
	}
	sellable := e.market.SellableFrom(inst, tp)
	if err := e.led.ApplyFill(fill, sellable); err != nil {
		// 撮合已校验过资金与持仓，走到这里说明有内部不一致，必须留痕
		e.rejects = append(e.rejects, trading.Rejection{
			Order: po.Order, At: tp, Reason: trading.RejectInsufficientCash,
			Detail: "记账失败: " + err.Error(),
		})
		return false
	}
	e.fills = append(e.fills, fill)
	fillDigest(e.resultHash, fill)
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
		// 风控在定价入队**之前**拦截：被拦下的单不该占用挂单队列，
		// 也不该在几天后突然冒出来成交
		if len(e.risk) > 0 {
			checked, rej, ok := e.risk.Check(o, e.ctx)
			if !ok {
				e.rejects = append(e.rejects, rej)
				continue
			}
			o = checked
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
	e.led.EachExposure(func(id mktdata.InstrumentID, _ trading.Exposure) bool {
		if b, ok := e.cur.Bar(id); ok {
			e.prices[id] = b.Close
		}
		return true
	})
	return e.led.EquityCents(e.prices)
}

// ---- 快照（C6）----

type snapshot struct {
	Steps int `json:"steps"`
	// PeakEquity 回撤类风控的基准。不存的话恢复后熔断规则立刻失效
	PeakEquity int64                                 `json:"peakEquity"`
	Cursor     mktdata.CursorState                   `json:"cursor"`
	Indicators map[string]map[string]indicator.State `json:"indicators"`
	Ledger     []byte                                `json:"ledger"`
	// Pending 必须进快照 —— 否则实盘恢复后，昨日挂出的未成交单会凭空消失
	Pending []trading.PendingOrder `json:"pending"`
	// Strategy 策略自身的跨步状态。实现 StatefulStrategy 的策略才有。
	Strategy []byte `json:"strategy,omitempty"`
	// Exits 离场规则链的跨步状态。移动止损的峰值在这里 ——
	// 不存的话，从快照恢复后峰值归零，移动止损**静默失效**：
	// 不报错、不异常，只是再也不会触发
	Exits []byte `json:"exits,omitempty"`
	// ResultHash 输出指纹的滚动哈希状态。
	//
	// 不存的话，从快照恢复后继续跑出来的指纹会缺掉快照之前的全部成交 ——
	// 而 C6 的实盘模式**每天都从快照恢复**，那样指纹就永远对不上全程运行，
	// 「同配置同结果」这条也就无从验证。
	ResultHash []byte `json:"result_hash,omitempty"`
}

func (e *Engine) Snapshot() ([]byte, error) {
	rh, err := e.resultHash.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("序列化输出指纹状态失败: %w", err)
	}
	ledSnap, err := e.led.SnapshotLedger()
	if err != nil {
		return nil, fmt.Errorf("序列化账本失败: %w", err)
	}
	s := snapshot{
		Steps: e.steps, PeakEquity: e.peakEquity, ResultHash: rh,
		Cursor:     e.cur.Snapshot(),
		Indicators: make(map[string]map[string]indicator.State, len(e.keys)),
		Ledger:     ledSnap,
		Pending:    append([]trading.PendingOrder(nil), e.pending...),
	}
	if ss, ok := e.strategy.(StatefulStrategy); ok {
		b, err := ss.SnapshotState()
		if err != nil {
			return nil, fmt.Errorf("策略状态快照失败: %w", err)
		}
		s.Strategy = b
	}
	if len(e.exits) > 0 {
		b, err := e.exits.SnapshotState()
		if err != nil {
			return nil, fmt.Errorf("离场规则状态快照失败: %w", err)
		}
		s.Exits = b
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
	if err := e.led.RestoreLedger(s.Ledger); err != nil {
		return err
	}
	e.pending = append([]trading.PendingOrder(nil), s.Pending...)
	e.steps = s.Steps
	e.peakEquity = s.PeakEquity
	if len(s.ResultHash) > 0 {
		if err := e.resultHash.(encoding.BinaryUnmarshaler).UnmarshalBinary(s.ResultHash); err != nil {
			return fmt.Errorf("恢复输出指纹状态失败: %w", err)
		}
	}
	if ss, ok := e.strategy.(StatefulStrategy); ok {
		if len(s.Strategy) == 0 {
			return fmt.Errorf("策略 %s 声明了跨步状态，但快照中没有 —— "+
				"该快照可能由另一个策略产生", e.strategy.Name())
		}
		if err := ss.RestoreState(s.Strategy); err != nil {
			return fmt.Errorf("恢复策略状态失败: %w", err)
		}
	}
	if len(e.exits) > 0 {
		if err := e.exits.RestoreState(s.Exits); err != nil {
			return fmt.Errorf("恢复离场规则状态失败: %w", err)
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
func (c *stepCtx) Ledger() trading.Ledger          { return c.e.led }
func (c *stepCtx) Pending() []trading.PendingOrder { return c.e.pending }
func (c *stepCtx) Fills() []trading.Fill           { return c.e.fills }
func (c *stepCtx) Rejections() []trading.Rejection { return c.e.rejects }

// EquityCents 返回**本步已结算的权益快照**，而不是每次调用都重算。
//
// 策略、Sizer、风控链在同一步里会各问好几次；若每次重算，
// 「按权益的 10%」在同一步内就可能指向不同的数 —— 那种偏差极难查。
func (c *stepCtx) EquityCents() int64 { return c.e.stepEquity }

// ---- SizeContext / RiskContext 补齐 ----

func (c *stepCtx) InitialCashCents() int64 { return c.e.cfg.InitialCashCents }
func (c *stepCtx) PeakEquityCents() int64  { return c.e.peakEquity }
func (c *stepCtx) Market() trading.Market  { return c.e.market }

func (c *stepCtx) Available(id mktdata.InstrumentID) int64 {
	return c.e.led.Available(id, c.time.TsClose)
}

func (c *stepCtx) Indicator(id mktdata.InstrumentID, key string) (indicator.Indicator, bool) {
	m, ok := c.e.indicators[key]
	if !ok {
		return nil, false
	}
	ind, ok := m[id]
	return ind, ok
}

// AdjBar 返回复权后的整根 bar。
//
// 与 AdjClose 同源，但条件表达式要用到 open / high / low，
// 只给收盘价不够。前复权同样被拒 —— 理由见 AdjClose。
func (c *stepCtx) AdjBar(id mktdata.InstrumentID, mode mktdata.AdjMode) (mktdata.Bar, bool) {
	bar, ok := c.e.cur.Bar(id)
	if !ok {
		return mktdata.Bar{}, false
	}
	if mode == mktdata.AdjNone || c.e.adj == nil {
		return bar, true
	}
	if mode == mktdata.AdjQFQ {
		// 前复权锚定末日，用于决策即构成未来函数（C1）且不可复现（C5）
		return mktdata.Bar{}, false
	}
	return c.e.adj.AdjustBar(id, bar, mode), true
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

// 编译期确认 stepCtx 同时满足三个上下文接口。
// 少一个方法就在这里报错，而不是等到运行时装配失败。
var (
	_ StepContext         = (*stepCtx)(nil)
	_ trading.SizeContext = (*stepCtx)(nil)
	_ trading.RiskContext = (*stepCtx)(nil)
	_ trading.ExitContext = (*stepCtx)(nil)
)

// ---- 本步中间产物 ----

// Signals 返回本步策略发出的信号。
func (e *Engine) Signals() []trading.Signal { return e.signals }

// Sized 返回本步 Sizer 折算出的订单（风控之前）。
//
// 与 Fills / Rejections 一起，四者构成单步调试要看的完整链条：
// 信号 → 定量 → 风控 → 成交。缺任何一环都会出现「为什么没买」答不上来的情况。
func (e *Engine) Sized() []trading.Order { return e.sized }

// PeakEquityCents 历史峰值权益。
func (e *Engine) PeakEquityCents() int64 { return e.peakEquity }

// Sizer 返回装配的仓位模块。
func (e *Engine) Sizer() trading.Sizer { return e.sizer }

// Risk 返回装配的风控链。
func (e *Engine) Risk() trading.RiskChain { return e.risk }

// Exits 返回装配的离场规则链。
func (e *Engine) Exits() trading.ExitChain { return e.exits }

// WarmupReporter 供**能报出预热情况**的策略实现。
//
// 预热是逐标的的：MACD(12,26,9) 要 35 根、MA200 要 200 根，
// 而每只标的从自己上市那天起算。Ready() 已经保证了未就绪的不参与决策，
// 但此前**没有任何地方把它显示出来** —— 用户只看到「信号少」，
// 看不出是条件不满足还是指标还没算出来。
type WarmupReporter interface {
	Warmup() (evaluated, notReady int)
}

// Warmup 返回上一步进入决策集合与仍在预热的标的数。
// ok 为 false 表示当前策略不报这个。
func (e *Engine) Warmup() (evaluated, notReady int, ok bool) {
	w, is := e.strategy.(WarmupReporter)
	if !is {
		return 0, 0, false
	}
	ev, nr := w.Warmup()
	return ev, nr, true
}

// Recorder 返回装配的记录器。
func (e *Engine) Recorder() record.Recorder { return e.rec }

// marks 收集当前时点各持仓标的的最新价，供账本重估使用。
//
// 只取**有敞口的**标的：无敞口的标的重估它没有意义，
// 而全市场取价在大标的池下是每步几千次查表。
func (e *Engine) marks() map[mktdata.InstrumentID]int64 {
	for id := range e.prices {
		delete(e.prices, id)
	}
	e.led.EachExposure(func(id mktdata.InstrumentID, _ trading.Exposure) bool {
		if b, ok := e.cur.Bar(id); ok {
			e.prices[id] = b.Close
		}
		return true
	})
	return e.prices
}

// Liquidations 返回累计的强平记录。现货账本下恒为空。
func (e *Engine) Liquidations() []trading.Liquidation { return e.liquidations }

// ---- 单步调试的只读入口（v0.4）----
//
// 这一组存在的理由只有一个：**回答「这一天为什么是这个决定」**。
//
// 批量回测只需要知道「发生了什么」，单步调试要知道「为什么」，
// 而「为什么」不在成交记录里 —— 它在指标值、在途队列、可卖数量里。
// 这些量此前只经 StepContext 给策略看，策略之外拿不到，
// 于是「今天 MACD 是多少」这种最基本的问题只能靠加日志。

// Pending 返回当前尚未成交的订单队列。
//
// **单步调试最常问的问题是「为什么今天没买」**，而最常见的答案是
// 「单还在队列里等着」—— T+1 的卖单、次日开盘执行的买单都在这里。
// 看不到这个队列，就只能得出「策略没发信号」这个错误结论。
func (e *Engine) Pending() []trading.PendingOrder { return e.pending }

// NumSteps 返回本次运行总共有多少个时点。
func (e *Engine) NumSteps() int { return e.col.NumSteps() }

// Now 返回当前所处的时点。尚未开始时 ok 为 false。
func (e *Engine) Now() (mktdata.TimePoint, bool) {
	if e.steps <= 0 || e.steps > e.col.NumSteps() {
		return mktdata.TimePoint{}, false
	}
	return e.col.StepAt(e.steps - 1), true
}

// PeekAt 返回第 i 个时点（0 起）。用于「跑到某日」前先算出目标步数。
func (e *Engine) PeekAt(i int) (mktdata.TimePoint, bool) {
	if i < 0 || i >= e.col.NumSteps() {
		return mktdata.TimePoint{}, false
	}
	return e.col.StepAt(i), true
}

// IndicatorKeys 返回本次装配声明了哪些指标（策略在 Init 里 Use 的那些）。
func (e *Engine) IndicatorKeys() []string {
	out := make([]string, len(e.keys))
	copy(out, e.keys)
	return out
}

// IndicatorAt 取某标的在当前时点的指标值。
//
// ready 为 false 时值是**垃圾**（预热未完成），调用方必须原样传给界面 ——
// 把未就绪的指标画成 0 会让人以为「指标是 0 所以没信号」，
// 而真相是「指标还没算出来」。这两件事在调试时天差地别。
func (e *Engine) IndicatorAt(id mktdata.InstrumentID, key string) (
	names []string, values []float64, ready, ok bool,
) {
	m, exists := e.indicators[key]
	if !exists {
		return nil, nil, false, false
	}
	ind, exists := m[id]
	if !exists {
		return nil, nil, false, false
	}
	v := ind.Values()
	cp := make([]float64, len(v))
	copy(cp, v) // Values 返回内部切片的视图，下一步会被改写
	return ind.Names(), cp, ind.Ready(), true
}

// BarAt 取某标的在当前时点的 bar（**原始价**，与撮合口径一致）。
func (e *Engine) BarAt(id mktdata.InstrumentID) (mktdata.Bar, bool) {
	return e.cur.Bar(id)
}

// AvailableAt 取某标的当前可卖数量（已考虑 T+1）。
func (e *Engine) AvailableAt(id mktdata.InstrumentID) int64 {
	tp, ok := e.Now()
	if !ok {
		return 0
	}
	return e.led.Available(id, tp.TsClose)
}
