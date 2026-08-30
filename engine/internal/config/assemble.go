package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/record"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// DataSet 是已载入内存的全部数据。
//
// 拆出来是为了让**多次装配共用一份数据**：海选要跑几百组配置，
// 数据只该载入一次（ROADMAP：N 个 worker 共享同一份只读内存，零拷贝）。
// 数据核对服务也可以把自己预载的那份直接递进来。
type DataSet struct {
	Columns  *mktdata.Columns
	Universe *mktdata.Universe
	Adjuster *mktdata.Adjuster
	CorpAct  *mktdata.CorpActions
	Calendar *mktdata.Calendar
	Stats    mktdata.LoadStats
	Root     string

	// BenchmarkID 基准标的。它**不在标的池里** —— 否则策略会把它当成
	// 可交易标的，超额收益就成了自己跟自己比
	BenchmarkID  mktdata.InstrumentID
	HasBenchmark bool
	// BenchDays / BenchEquity 基准的后复权净值序列，**在裁子集之前算好**。
	//
	// 不能等到用的时候再从 Columns 取：调用方可能已经把 Columns 裁成
	// 不含基准的子集了（服务端为了复用就是这么做的），那时再取只会取到空 ——
	// 而「基准区块整个消失」这种失败是静悄悄的，报告里只是少了一段。
	BenchDays   []int32
	BenchEquity []int64
}

// LoadDataSet 按配置载入数据。
//
// 先从 instruments.parquet 解出标的池（毫秒级），再只加载那些标的的 bar ——
// 直接全量载入是 1.25 GB，跑一个 300 只标的的回测不必付这个代价。
func LoadDataSet(c *Config) (*DataSet, error) {
	root := c.resolvePath(c.Data.Root)
	meta := filepath.Join(root, "meta")

	uni, err := mktdata.LoadUniverse(filepath.Join(meta, "instruments.parquet"))
	if err != nil {
		return nil, err
	}
	adj, err := mktdata.LoadAdjuster(filepath.Join(meta, "adj_factor.parquet"))
	if err != nil {
		return nil, err
	}
	corp, err := mktdata.LoadCorpActions(filepath.Join(meta, "corporate_action.parquet"))
	if err != nil {
		return nil, err
	}
	cal, err := mktdata.LoadCalendar(filepath.Join(meta, "calendar.parquet"))
	if err != nil {
		return nil, err
	}

	ids, err := c.ResolveUniverse(uni, adj)
	if err != nil {
		return nil, err
	}
	// 基准标的一并载入，但**不进标的池** —— Assemble 会把它裁掉。
	// 不然策略会把基准当成可交易标的，超额收益就成了自己跟自己比。
	benchID, hasBench := mktdata.InstrumentID(0), false
	if c.Metrics.Benchmark != "" {
		in := uni.BySymbol(c.Metrics.Benchmark)
		if in == nil {
			return nil, fmt.Errorf("metrics.benchmark: 未找到标的 %q", c.Metrics.Benchmark)
		}
		benchID, hasBench = in.ID, true
		inPool := false
		for _, id := range ids {
			if id == benchID {
				inPool = true
				break
			}
		}
		if !inPool {
			ids = append(ids, benchID)
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		}
	}
	col, st, err := mktdata.Load(mktdata.LoadOptions{
		Root: filepath.Join(root, "bar",
			"market="+c.Data.Market, "freq="+c.Data.Freq),
		Instruments: ids,
		FromDay:     c.Data.From, ToDay: c.Data.To,
	})
	if err != nil {
		return nil, err
	}
	ds := &DataSet{
		Columns: col, Universe: uni, Adjuster: adj, CorpAct: corp,
		Calendar: cal, Stats: st, Root: root,
	}
	if hasBench && !ds.SetBenchmark(col, benchID) {
		return nil, fmt.Errorf("metrics.benchmark: %q 在该区间内没有行情",
			c.Metrics.Benchmark)
	}
	return ds, nil
}

// BenchmarkCurve 返回基准的净值序列。
func (ds *DataSet) BenchmarkCurve() (days []int32, equity []int64, ok bool) {
	if !ds.HasBenchmark || len(ds.BenchDays) == 0 {
		return nil, nil, false
	}
	return ds.BenchDays, ds.BenchEquity, true
}

// SetBenchmark 记下基准标的并**立刻**算出它的后复权净值序列。
//
// 后复权而非原始价：基准的总回报要含分红再投，否则会系统性低估基准、
// 让策略的超额收益虚高。
//
// 必须在把 Columns 裁成子集**之前**调用 —— 基准不在标的池里，
// 裁完就取不到了。
//
// 覆盖不到的交易日直接缺席，由 metrics 按交集处理 ——
// 数据里没有指数（C10 纯技术面，ETL 没拉指数行情），只能用 ETF 代理，
// 而宽基 ETF 最早到 2012（510300 / 159919 都是 2012-05-28 起）。
func (ds *DataSet) SetBenchmark(col *mktdata.Columns, id mktdata.InstrumentID) bool {
	days, closes, ok := col.Series(id)
	if !ok {
		return false
	}
	equity := make([]int64, len(closes))
	for i, c := range closes {
		equity[i] = ds.Adjuster.Adjust(id, days[i], c, mktdata.AdjHFQ)
	}
	ds.BenchmarkID, ds.HasBenchmark = id, true
	ds.BenchDays, ds.BenchEquity = days, equity
	return true
}

// ResolveUniverse 把配置里的过滤条件解析成一组标的 ID。
//
// **结果一律按 ID 升序**：Limit 取前 N 个，若顺序不确定，
// 同一份配置两次运行的标的池就不同 —— C5 直接失守，而且不会报错。
func (c *Config) ResolveUniverse(
	uni *mktdata.Universe, adj *mktdata.Adjuster,
) ([]mktdata.InstrumentID, error) {
	u := c.Data.Univers

	// 显式列表优先，且顺序按配置写的来（用户写的顺序本身就是确定的）
	if len(u.Symbols) > 0 {
		out := make([]mktdata.InstrumentID, 0, len(u.Symbols))
		for _, sym := range u.Symbols {
			in := uni.BySymbol(sym)
			if in == nil {
				return nil, fmt.Errorf("universe.symbols: 未找到标的 %q", sym)
			}
			out = append(out, in.ID)
		}
		return out, nil
	}

	wantType, err := parseType(u.Type)
	if err != nil {
		return nil, err
	}
	wantBoard, err := parseBoards(u.Board)
	if err != nil {
		return nil, err
	}
	wantExch, err := parseExchanges(u.Exchange)
	if err != nil {
		return nil, err
	}
	wantStatus, err := parseStatus(u.Status)
	if err != nil {
		return nil, err
	}
	wantMarket, err := parseMarkets(u.Market)
	if err != nil {
		return nil, err
	}

	picked := make([]mktdata.InstrumentID, 0, 1024)
	for _, in := range uni.All() {
		if wantMarket != nil && !wantMarket[in.Market] {
			continue
		}
		if wantType >= 0 && in.Type != mktdata.InstrumentType(wantType) {
			continue
		}
		if wantBoard != nil && !wantBoard[in.Board] {
			continue
		}
		if wantExch != nil && !wantExch[in.Exchange] {
			continue
		}
		if wantStatus >= 0 && in.Status != mktdata.Status(wantStatus) {
			continue
		}
		if u.RequireFactor && len(adj.Factors(in.ID)) == 0 {
			continue
		}
		picked = append(picked, in.ID)
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i] < picked[j] })

	if u.Limit > 0 && len(picked) > u.Limit {
		picked = picked[:u.Limit]
	}
	if len(picked) == 0 {
		return nil, fmt.Errorf("universe 过滤后没有标的")
	}
	return picked, nil
}

// Assemble 用给定数据装配一台引擎。
//
// ds 可以是刚好匹配的（LoadDataSet 的产物），也可以是更大的全量数据 ——
// 后者会被 Subset 裁到配置要求的范围。
func (c *Config) Assemble(ds *DataSet) (*eng.Engine, error) {
	ids, err := c.ResolveUniverse(ds.Universe, ds.Adjuster)
	if err != nil {
		return nil, err
	}
	col, err := c.narrow(ds.Columns, ids)
	if err != nil {
		return nil, err
	}

	if err := checkMarketRules(ds.Universe, ids, c.Market.Impl); err != nil {
		return nil, err
	}
	market, err := trading.Markets.Build(c.Market.Impl, c.Market.Params)
	if err != nil {
		return nil, err
	}
	fee, err := trading.Fees.Build(c.Fee.Impl, c.feeParams())
	if err != nil {
		return nil, err
	}
	slip, err := trading.Slippages.Build(c.Slippage.Impl, c.Slippage.Params)
	if err != nil {
		return nil, err
	}
	sizer, err := trading.Sizers.Build(c.Sizer.Impl, c.Sizer.Params)
	if err != nil {
		return nil, err
	}
	chain := make(trading.RiskChain, 0, len(c.Risk))
	for i, r := range c.Risk {
		rr, err := trading.Risks.Build(r.Impl, r.Params)
		if err != nil {
			return nil, fmt.Errorf("risk[%d]: %w", i, err)
		}
		chain = append(chain, rr)
	}
	strat, params, err := buildStrategy(c.Strategy)
	if err != nil {
		return nil, err
	}

	allowPartial := true
	if c.Broker.AllowPartialFill != nil {
		allowPartial = *c.Broker.AllowPartialFill
	}
	implySplit := true
	if c.Engine.ImplySplitFromFactor != nil {
		implySplit = *c.Engine.ImplySplitFromFactor
	}
	adjMode, err := parseAdj(c.Engine.IndicatorAdj)
	if err != nil {
		return nil, err
	}

	return eng.New(eng.Deps{
		Columns: col, Universe: ds.Universe, Adjuster: ds.Adjuster, CorpAct: ds.CorpAct,
		Recorder: record.NewMemory(c.Level(), 0),
		Market:   market,
		Broker: trading.NewBroker(market, fee, slip, trading.BrokerConfig{
			VolumeCapPPM: c.Broker.VolumeCapPPM, AllowPartialFill: allowPartial,
		}),
		Ledger: trading.NewPortfolio(c.Portfolio.InitialCashCents),
		Sizer:  sizer,
		Risk:   chain,
	}, strat, eng.Config{
		Params:               params,
		IndicatorAdjMode:     adjMode,
		InitialCashCents:     c.Portfolio.InitialCashCents,
		DividendTaxPPM:       c.Portfolio.DividendTaxPPM,
		ImplySplitFromFactor: implySplit,
		TradeFrom:            c.Engine.TradeFrom,
	})
}

// narrow 把已载入的列式数据裁到配置要求的标的与区间。
//
// 若已经恰好匹配就原样返回 —— 全量数据 1.25 GB，无谓地 Subset 一次
// 就是又一个 1.25 GB。
func (c *Config) narrow(
	col *mktdata.Columns, ids []mktdata.InstrumentID,
) (*mktdata.Columns, error) {
	// 判据是**「col 里的标的都在 ids 里」而不是「两者相等」**。
	//
	// 相等这个条件在含退市股的标的池上永远不成立：ResolveUniverse 按
	// instruments 表返回 3,485 只主板个股，而在 2020 年之后真有行情的
	// 只有 3,375 只 —— 差着 110 只从没进过这段区间的退市股。
	// 于是每次 Assemble 都重新 Subset 一份，实测**每次 358 MB**。
	// 海选 8 个 worker 就是 2.8 GB 的纯重复拷贝（cmd/enginebench 可复现）。
	//
	// 放宽成子集关系是安全的：ids 里没有行情的标的，Subset 本来也会丢掉，
	// 拷完的结果与 col 逐位相同。反过来 col 里有 ids 之外的标的
	// （典型是基准标的）时仍然必须拷 —— 基准不能进标的池。
	have := col.Instruments()
	sameSet := len(have) <= len(ids)
	if sameSet {
		want := make(map[mktdata.InstrumentID]bool, len(ids))
		for _, id := range ids {
			want[id] = true
		}
		for _, id := range have {
			if !want[id] {
				sameSet = false
				break
			}
		}
	}
	// 区间是否已经收窄过，只能从数据本身看
	inRange := true
	if col.NumSteps() > 0 {
		first := col.StepAt(0).TradingDay
		last := col.StepAt(col.NumSteps() - 1).TradingDay
		if c.Data.From != 0 && first < c.Data.From {
			inRange = false
		}
		if c.Data.To != 0 && last > c.Data.To {
			inRange = false
		}
	}
	if sameSet && inRange {
		return col, nil
	}
	return col.Subset(ids, c.Data.From, c.Data.To)
}

// feeParams 把 fee.params.path 解成相对配置文件的路径后再交给 registry。
//
// **只改 path，其余参数原样带过去。** 早先这里是直接重建一个
// `{"path": ...}` 交出去的 —— 那会把同一段里的其他参数
// （如 commission_ppm 佣金覆盖）**整个丢掉**，而且不报错：
// 引擎照常跑，只是覆盖没生效，费用还是文件里的默认值。
func (c *Config) feeParams() []byte {
	if c.Fee.Impl != "config" {
		return c.Fee.Params
	}
	path, err := decodeFeePath(c.Fee.Params)
	if err != nil || path == "" {
		return c.Fee.Params
	}
	var m map[string]json.RawMessage
	if len(c.Fee.Params) > 0 {
		if err := json.Unmarshal(c.Fee.Params, &m); err != nil {
			return c.Fee.Params
		}
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	resolved, err := json.Marshal(c.resolvePath(path))
	if err != nil {
		return c.Fee.Params
	}
	m["path"] = resolved
	out, err := json.Marshal(m)
	if err != nil {
		return c.Fee.Params
	}
	return out
}

func decodeFeePath(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var m struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	return m.Path, nil
}

// ---- 枚举解析 ----

func parseType(s string) (int, error) {
	switch strings.ToLower(s) {
	case "", "all":
		return -1, nil
	case "stock":
		return int(mktdata.TypeStock), nil
	case "etf":
		return int(mktdata.TypeETF), nil
	}
	return 0, fmt.Errorf("未知的 universe.type %q，可选：all / stock / etf", s)
}

func parseStatus(s string) (int, error) {
	switch strings.ToLower(s) {
	case "", "all":
		return -1, nil
	case "listed":
		return int(mktdata.StatusListed), nil
	case "delisted":
		return int(mktdata.StatusDelisted), nil
	}
	return 0, fmt.Errorf("未知的 universe.status %q，可选：all / listed / delisted", s)
}

// checkMarketRules 拦下「用 A 股规则跑非 A 股标的」。
//
// 数据层是市场无关的（C9），交易规则层不是。A 股规则带着 T+1、涨跌停、
// 印花税、252 日年化；把它套到加密货币上**不会报任何错**，只会静默给出
// 一份看着很正常的假结果 —— 这比崩溃危险得多。
//
// 加密的规则实现（T+0、无涨跌停、365 日年化、资金费率）还没写，
// 所以这里宁可拒绝。等 CryptoMarket 落地后，这个函数改成查表即可。
func checkMarketRules(uni *mktdata.Universe, ids []mktdata.InstrumentID, impl string) error {
	if strings.ToLower(strings.TrimSpace(impl)) != "ashare" {
		return nil
	}
	counts := make(map[mktdata.Market]int, 2)
	var sample string
	for _, id := range ids {
		in := uni.Get(id)
		if in == nil || in.Market == mktdata.MarketAShare {
			continue
		}
		if counts[in.Market] == 0 {
			sample = in.Symbol
		}
		counts[in.Market]++
	}
	if len(counts) == 0 {
		return nil
	}
	parts := make([]string, 0, len(counts))
	for m, n := range counts {
		parts = append(parts, fmt.Sprintf("%s %d 个", m, n))
	}
	sort.Strings(parts)
	return fmt.Errorf("market.impl=\"ashare\" 但标的池里有非 A 股标的（%s，如 %s）—— "+
		"A 股规则含 T+1、涨跌停、印花税，套到其他市场会静默给出错误结果。"+
		"请用 universe.market 限定市场；对应市场的规则实现尚未提供",
		strings.Join(parts, "、"), sample)
}

func parseMarkets(ss []string) (map[mktdata.Market]bool, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	m := make(map[mktdata.Market]bool, len(ss))
	for _, s := range ss {
		switch strings.ToLower(s) {
		case "ashare", "a", "cn":
			m[mktdata.MarketAShare] = true
		case "crypto", "okx":
			m[mktdata.MarketCrypto] = true
		default:
			return nil, fmt.Errorf("未知的 universe.market %q，可选：ashare / crypto", s)
		}
	}
	return m, nil
}

func parseBoards(ss []string) (map[mktdata.Board]bool, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	m := make(map[mktdata.Board]bool, len(ss))
	for _, s := range ss {
		switch strings.ToLower(s) {
		case "main":
			m[mktdata.BoardMain] = true
		case "chinext":
			m[mktdata.BoardChiNext] = true
		case "star":
			m[mktdata.BoardSTAR] = true
		case "bse":
			m[mktdata.BoardBSE] = true
		default:
			return nil, fmt.Errorf("未知的 universe.board %q，可选：main / chinext / star / bse", s)
		}
	}
	return m, nil
}

func parseExchanges(ss []string) (map[mktdata.Exchange]bool, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	m := make(map[mktdata.Exchange]bool, len(ss))
	for _, s := range ss {
		switch strings.ToLower(s) {
		case "sse":
			m[mktdata.ExchangeSSE] = true
		case "szse":
			m[mktdata.ExchangeSZSE] = true
		case "bse":
			m[mktdata.ExchangeBSE] = true
		default:
			return nil, fmt.Errorf("未知的 universe.exchange %q，可选：sse / szse / bse", s)
		}
	}
	return m, nil
}

// Describe 把装配结果摘要成一行行文字，供 CLI 与报告回显。
//
// **配置回显不是装饰**：跑完一看就知道用的是哪套费率、哪条风控，
// 省掉「这个结果到底是怎么跑出来的」这类事后考古。
func (c *Config) Describe(e *eng.Engine, ds *DataSet, load time.Duration) []string {
	risks := e.Risk().Names()
	riskStr := "无"
	if len(risks) > 0 {
		riskStr = strings.Join(risks, " → ")
	}
	name := c.Name
	if name == "" {
		name = "(未命名)"
	}
	return []string{
		fmt.Sprintf("配置    %s", name),
		fmt.Sprintf("数据    %s  %s/%s  %d ~ %d",
			ds.Root, c.Data.Market, c.Data.Freq, c.Data.From, c.Data.To),
		fmt.Sprintf("标的    %d 只 / %d 行 / %d 个时点 / 载入 %v",
			ds.Stats.Instruments, ds.Stats.Rows, ds.Stats.Steps,
			load.Round(time.Millisecond)),
		fmt.Sprintf("策略    %s %s", c.Strategy.Impl, rawOrEmpty(c.Strategy.Params)),
		fmt.Sprintf("仓位    %s %s", c.Sizer.Impl, rawOrEmpty(c.Sizer.Params)),
		fmt.Sprintf("风控    %s", riskStr),
		fmt.Sprintf("市场    %s   费率 %s   滑点 %s %s",
			c.Market.Impl, c.Fee.Impl, c.Slippage.Impl, rawOrEmpty(c.Slippage.Params)),
		fmt.Sprintf("撮合    成交量上限 %.2f%%  部分成交 %v",
			float64(c.Broker.VolumeCapPPM)/10_000, c.Broker.AllowPartialFill == nil ||
				*c.Broker.AllowPartialFill),
		fmt.Sprintf("账户    初始 %.2f 元  红利税 %.2f%%  指标复权 %s",
			float64(c.Portfolio.InitialCashCents)/100,
			float64(c.Portfolio.DividendTaxPPM)/10_000, c.Engine.IndicatorAdj),
	}
}

func rawOrEmpty(raw []byte) string {
	if len(raw) == 0 {
		return "(默认参数)"
	}
	return string(raw)
}

// buildStrategy 装配策略，组合策略走另一条路。
//
// 返回的 Params 是**引擎级**的那一份：普通策略就是它自己的参数；
// 组合策略返回空 —— 各源的参数由 Composite 自己在 Init 时分发下去，
// 因为一份 Params 装不下 N 个源的参数，硬装就得靠前缀，
// 那会让源看到一个和自己声明不一样的参数名。
func buildStrategy(m Module) (eng.Strategy, spec.Params, error) {
	if m.Impl != compositeImpl {
		s, err := eng.Strategies.Build(m.Impl, nil)
		if err != nil {
			return nil, nil, err
		}
		specs, _ := eng.Strategies.Specs(m.Impl)
		p, err := decodeStrategyParams(specs, m.Params)
		if err != nil {
			return nil, nil, fmt.Errorf("strategy.params: %w", err)
		}
		return s, p, nil
	}

	mode, err := eng.ParseCombineMode(m.Mode)
	if err != nil {
		return nil, nil, fmt.Errorf("strategy.mode: %w", err)
	}
	sources := make([]eng.Source, 0, len(m.Sources))
	for i, src := range m.Sources {
		s, err := eng.Strategies.Build(src.Impl, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("strategy.sources[%d]: %w", i, err)
		}
		specs, _ := eng.Strategies.Specs(src.Impl)
		p, err := decodeStrategyParams(specs, src.Params)
		if err != nil {
			return nil, nil, fmt.Errorf("strategy.sources[%d].params: %w", i, err)
		}
		sources = append(sources, eng.Source{Name: src.Impl, Strategy: s, Params: p})
	}
	comp, err := eng.NewComposite(mode, sources)
	if err != nil {
		return nil, nil, fmt.Errorf("strategy: %w", err)
	}
	return comp, spec.Params{}, nil
}
