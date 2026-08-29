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
	col, st, err := mktdata.Load(mktdata.LoadOptions{
		Root: filepath.Join(root, "bar",
			"market="+c.Data.Market, "freq="+c.Data.Freq),
		Instruments: ids,
		FromDay:     c.Data.From, ToDay: c.Data.To,
	})
	if err != nil {
		return nil, err
	}
	return &DataSet{
		Columns: col, Universe: uni, Adjuster: adj, CorpAct: corp,
		Calendar: cal, Stats: st, Root: root,
	}, nil
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

	picked := make([]mktdata.InstrumentID, 0, 1024)
	for _, in := range uni.All() {
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
	strat, err := eng.Strategies.Build(c.Strategy.Impl, nil)
	if err != nil {
		return nil, err
	}
	specs, _ := eng.Strategies.Specs(c.Strategy.Impl)
	params, err := decodeStrategyParams(specs, c.Strategy.Params)
	if err != nil {
		return nil, fmt.Errorf("strategy.params: %w", err)
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
		Market: market,
		Broker: trading.NewBroker(market, fee, slip, trading.BrokerConfig{
			VolumeCapPPM: c.Broker.VolumeCapPPM, AllowPartialFill: allowPartial,
		}),
		Portfolio: trading.NewPortfolio(c.Portfolio.InitialCashCents),
		Sizer:     sizer,
		Risk:      chain,
	}, strat, eng.Config{
		Params:               params,
		IndicatorAdjMode:     adjMode,
		InitialCashCents:     c.Portfolio.InitialCashCents,
		DividendTaxPPM:       c.Portfolio.DividendTaxPPM,
		ImplySplitFromFactor: implySplit,
	})
}

// narrow 把已载入的列式数据裁到配置要求的标的与区间。
//
// 若已经恰好匹配就原样返回 —— 全量数据 1.25 GB，无谓地 Subset 一次
// 就是又一个 1.25 GB。
func (c *Config) narrow(
	col *mktdata.Columns, ids []mktdata.InstrumentID,
) (*mktdata.Columns, error) {
	have := col.Instruments()
	sameSet := len(have) == len(ids)
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
func (c *Config) feeParams() []byte {
	if c.Fee.Impl != "config" {
		return c.Fee.Params
	}
	path, err := decodeFeePath(c.Fee.Params)
	if err != nil || path == "" {
		return c.Fee.Params
	}
	resolved := c.resolvePath(path)
	return []byte(fmt.Sprintf(`{"path":%q}`, resolved))
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
