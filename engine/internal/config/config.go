// Package config 把一份 JSON 变成一台装配好的引擎。
//
// 这是 v0.3 的核心：换配置即可改变引擎行为，不必重新编译。
//
// **装配路径上任何遍历 map 的地方都要先排序。** Go 的 map 遍历顺序是随机的，
// 这是 C5（可复现性）最容易被忽略的入口 —— 它不会报错，
// 只会让两次运行的标的池悄悄不同，然后结果对不上却查不出原因。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/record"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Module 是「按名字选实现 + 给它一段参数」。
type Module struct {
	Impl   string          `json:"impl"`
	Params json.RawMessage `json:"params,omitempty"`
	// Sources 仅 strategy.impl == "composite" 时有意义：组合的各个决策源。
	//
	// 放在 Module 上而不是塞进 params：源本身也是 {impl, params}，
	// 塞进 params 就得在 JSON 里嵌一层无类型的东西，校验也跟着失效。
	Sources []Module `json:"sources,omitempty"`
	// Mode 仅 composite 有意义：union / confirm / veto
	Mode string `json:"mode,omitempty"`
}

// Universe 描述标的池。
//
// **只允许静态条件。** 类型、板块、交易所可以进；`is_st` 不能 ——
// 它是逐 bar 时变的（ETL.md 6.6），当天是不是 ST 只有当天才知道，
// 把它写成 universe 过滤器等于用了未来信息。需要避开 ST 的策略
// 应当在 OnBar 里看 bar.IsST，或写成一条 Risk 规则。
//
// 也没有 point_in_time 开关：bar 表天生是 PIT 的（上市前无行、退市后无行），
// C3 是结构保证而不是配置项。
type Universe struct {
	// Symbols 显式列表。给了就只用它，其余过滤条件忽略 ——
	// 「就在这几只上跑」是最直接的意图，不该再被别的条件二次过滤掉。
	Symbols []string `json:"symbols,omitempty"`
	// Market ashare / us / jp / crypto，空表示不限。
	//
	// 当前只有 A 股，这个过滤器现在等于不过滤。留着是因为它属于**标的属性**，
	// 与 data.market（选分区路径）不是一回事 —— 远期一份数据里同时有
	// 多个市场时，两者会分开起作用（C9）。
	Market []string `json:"market,omitempty"`
	// Type stock / etf / all
	Type string `json:"type,omitempty"`
	// Board main / chinext / star / bse，空表示不限
	Board []string `json:"board,omitempty"`
	// Exchange sse / szse / bse，空表示不限
	Exchange []string `json:"exchange,omitempty"`
	// Status listed / delisted / all。默认 all —— 排除退市股会引入幸存者偏差（C3）
	Status string `json:"status,omitempty"`
	// RequireFactor 只要有复权因子事件的标的。调试除权路径时有用
	RequireFactor bool `json:"require_factor,omitempty"`
	// Limit 取前 N 个，0 表示不限。**按 instrument_id 升序取**，
	// 否则同一份配置两次运行的标的池就不同，C5 直接失守
	Limit int `json:"limit,omitempty"`
}

// Data 描述数据来源与区间。
type Data struct {
	Root    string   `json:"root"`
	Market  string   `json:"market"`
	Freq    string   `json:"freq"`
	From    int32    `json:"from"`
	To      int32    `json:"to"`
	Univers Universe `json:"universe"`
}

// BrokerCfg 撮合参数。
type BrokerCfg struct {
	VolumeCapPPM     int64 `json:"volume_cap_ppm"`
	AllowPartialFill *bool `json:"allow_partial_fill,omitempty"`
}

// PortfolioCfg 账户参数。
type PortfolioCfg struct {
	// InitialCashCents 初始资金，**计价币种的最小单位**。
	//
	// A 股 = 分（默认 20,000 元 = 2,000,000）
	// 加密 = 0.01 USDT（默认 1,000 USDT = 100,000）
	//
	// 默认值随 data.market 变 —— 把 100 万元的默认值原样搬到
	// 加密上就是 100 万 USDT，那不是一个散户的账户
	InitialCashCents int64 `json:"initial_cash_cents"`
	DividendTaxPPM   int64 `json:"dividend_tax_ppm"`
	// Ledger 账本实现：spot（现货，仅多）/ margin（逐仓双向）。
	// 空表示按市场选：A 股 spot，加密 margin
	Ledger string `json:"ledger,omitempty"`
	// Leverage 杠杆倍数，仅 margin 账本有意义。默认 1
	Leverage int64 `json:"leverage,omitempty"`
	// MaintMarginPPM 维持保证金率（百万分之一），仅 margin 有意义。
	// 默认 5000（0.5%），与主流交易所 BTC 永续的量级一致
	MaintMarginPPM int64 `json:"maint_margin_ppm,omitempty"`
}

// EngineCfg 引擎参数。
type EngineCfg struct {
	// IndicatorAdj 指标喂入的复权方式。**拒绝 qfq** —— 前复权锚定末日，
	// 用于计算即构成未来函数（C1）且不可复现（C5）
	IndicatorAdj string `json:"indicator_adj"`
	// ImplySplitFromFactor 指针是为了区分「没写」与「显式写了 false」，
	// 默认 true（约 6,770 个因子事件缺分红记录，不处理会失真）
	ImplySplitFromFactor *bool `json:"imply_split_from_factor,omitempty"`
	// TradeFrom 这一天之前只喂指标不交易（YYYYMMDD，0 表示不设限）。
	// 供 Walk-Forward 的预热前缀使用，见 engine.Config.TradeFrom
	TradeFrom int32 `json:"trade_from,omitempty"`
}

// MetricsCfg 绩效参数。
type MetricsCfg struct {
	// Benchmark 基准标的代码。数据里**没有指数**，只能用 ETF 代理
	// （510300 沪深300ETF 最早到 2012-05-28）。空表示不算超额
	Benchmark string `json:"benchmark,omitempty"`
	// RiskFreePPM 无风险利率（年化，百万分之一）。默认 0 并在报告里显式印出 ——
	// 写死一个值会让不同年份的夏普失去可比性
	RiskFreePPM int64 `json:"risk_free_ppm"`
}

// RecorderCfg 记录级别。
type RecorderCfg struct {
	Level string `json:"level"` // none / summary / full
}

// Config 是一次运行的完整描述。
type Config struct {
	Name     string   `json:"name,omitempty"`
	Data     Data     `json:"data"`
	Market   Module   `json:"market"`
	Fee      Module   `json:"fee"`
	Slippage Module   `json:"slippage"`
	Sizer    Module   `json:"sizer"`
	Risk     []Module `json:"risk,omitempty"`
	// Exit 离场规则链：止损 / 止盈 / 移动止损。
	//
	// **与 risk 是两回事**：risk 过滤订单（只能拦截或缩量），
	// exit 产生订单（平掉已有持仓）。止损塞不进 risk，
	// 因为 Risk.Check 的形状是「订单进、订单出」
	Exit      []Module     `json:"exit,omitempty"`
	Broker    BrokerCfg    `json:"broker"`
	Portfolio PortfolioCfg `json:"portfolio"`
	Engine    EngineCfg    `json:"engine"`
	Strategy  Module       `json:"strategy"`
	Metrics   MetricsCfg   `json:"metrics"`
	Recorder  RecorderCfg  `json:"recorder"`

	// dir 是配置文件所在目录，用于把相对路径（如 fee.params.path）解成绝对路径。
	// 不进 JSON —— 它是加载现场的信息，不是配置内容
	dir string
}

// Load 读入并校验一份配置。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	c, err := Parse(b, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("配置 %s：%w", path, err)
	}
	return c, nil
}

// Parse 解析并校验一份配置。dir 是相对路径的解析基准
// （通常是配置文件所在目录；配置由网络传来时用一个约定的基准目录）。
//
// 与 Load 分开是为了让 Web 端能把改过参数的配置直接 POST 回来跑，
// 而不必先落盘。
func Parse(b []byte, dir string) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	// 拒绝未知字段：把 "sizer" 写成 "sizor" 时若静默忽略，
	// 引擎会用默认值跑完并给出看似正常的结果 —— 比报错难查得多
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	c.dir = dir
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("校验失败: %w", err)
	}
	return &c, nil
}

// SetDataRoot 覆盖数据根目录。
//
// 服务端用它把配置指向自己已经载入的那份数据 —— 否则一份从浏览器传来的
// 配置可能指向别处，而服务端手里只有启动时载入的那一份，
// 用它跑却按配置里的路径算指纹，两者对不上。
func (c *Config) SetDataRoot(abs string) { c.Data.Root = abs }

func (c *Config) applyDefaults() {
	if c.Data.Root == "" {
		c.Data.Root = "../data"
	}
	if c.Data.Market == "" {
		c.Data.Market = "ashare"
	}
	if c.Data.Freq == "" {
		c.Data.Freq = "1d"
	}
	// **默认值随市场变。** 把 A 股的默认原样搬到加密上，
	// 得到的是「用 A 股规则跑 BTC」或「100 万 USDT 的账户」——
	// 两者都不报错，只是结果没有意义
	if c.Market.Impl == "" {
		c.Market.Impl = defaultMarketImpl(c.Data.Market)
	}
	if c.Fee.Impl == "" {
		c.Fee.Impl = "zero"
	}
	if c.Slippage.Impl == "" {
		c.Slippage.Impl = "none"
	}
	if c.Sizer.Impl == "" {
		c.Sizer.Impl = "equal_weight"
	}
	if c.Broker.VolumeCapPPM <= 0 {
		c.Broker.VolumeCapPPM = 100_000
	}
	if c.Portfolio.InitialCashCents <= 0 {
		c.Portfolio.InitialCashCents = defaultCashCents(c.Data.Market)
	}
	if c.Portfolio.Ledger == "" {
		c.Portfolio.Ledger = defaultLedger(c.Data.Market)
	}
	if c.Portfolio.Leverage <= 0 {
		c.Portfolio.Leverage = 1
	}
	if c.Portfolio.MaintMarginPPM <= 0 {
		c.Portfolio.MaintMarginPPM = 5_000 // 0.5%
	}
	if c.Engine.IndicatorAdj == "" {
		c.Engine.IndicatorAdj = "hfq"
	}
	if c.Recorder.Level == "" {
		c.Recorder.Level = "summary"
	}
	if c.Data.Univers.Type == "" {
		c.Data.Univers.Type = "all"
	}
	if c.Data.Univers.Status == "" {
		c.Data.Univers.Status = "all"
	}
}

// Validate 在装配前把能查的都查掉。
//
// 校验分两层：这里查结构与取值域，registry.Build 查各模块自己的参数。
// 两层都在**跑之前**完成 —— 跑到第 3000 步才因为参数越界崩掉是最差的体验。
func (c *Config) Validate() error {
	if !trading.Markets.Has(c.Market.Impl) {
		return fmt.Errorf("未知 market 实现 %q，可选：%s",
			c.Market.Impl, strings.Join(trading.Markets.Names(), " / "))
	}
	if err := c.validateLeverage(); err != nil {
		return err
	}
	if !trading.Fees.Has(c.Fee.Impl) {
		return fmt.Errorf("未知 fee 实现 %q，可选：%s",
			c.Fee.Impl, strings.Join(trading.Fees.Names(), " / "))
	}
	if !trading.Slippages.Has(c.Slippage.Impl) {
		return fmt.Errorf("未知 slippage 实现 %q，可选：%s",
			c.Slippage.Impl, strings.Join(trading.Slippages.Names(), " / "))
	}
	if !trading.Sizers.Has(c.Sizer.Impl) {
		return fmt.Errorf("未知 sizer 实现 %q，可选：%s",
			c.Sizer.Impl, strings.Join(trading.Sizers.Names(), " / "))
	}
	if c.Strategy.Impl == "" {
		return fmt.Errorf("必须指定 strategy.impl，可选：%s",
			strings.Join(eng.Strategies.Names(), " / "))
	}
	if c.Strategy.Impl == compositeImpl {
		if err := validateComposite(c.Strategy); err != nil {
			return err
		}
	} else if !eng.Strategies.Has(c.Strategy.Impl) {
		return fmt.Errorf("未知 strategy 实现 %q，可选：%s（或 %s）",
			c.Strategy.Impl, strings.Join(eng.Strategies.Names(), " / "), compositeImpl)
	}
	seen := map[string]bool{}
	for i, r := range c.Risk {
		if !trading.Risks.Has(r.Impl) {
			return fmt.Errorf("risk[%d]: 未知实现 %q，可选：%s",
				i, r.Impl, strings.Join(trading.Risks.Names(), " / "))
		}
		// 同一条规则配两遍多半是复制粘贴出的错误，而且后一条会静默收紧前一条
		if seen[r.Impl] {
			return fmt.Errorf("risk[%d]: 规则 %q 重复", i, r.Impl)
		}
		seen[r.Impl] = true
	}
	if _, err := parseAdj(c.Engine.IndicatorAdj); err != nil {
		return err
	}
	if _, err := parseLevel(c.Recorder.Level); err != nil {
		return err
	}
	if c.Data.From != 0 && c.Data.To != 0 && c.Data.From > c.Data.To {
		return fmt.Errorf("data.from %d 晚于 data.to %d", c.Data.From, c.Data.To)
	}
	if c.Data.Univers.Limit < 0 {
		return fmt.Errorf("data.universe.limit 不能为负")
	}
	// 策略参数按 Specs 校验（组合策略的参数在各个源上，已由 validateComposite 查过）。
	//
	// **配置是结构而非标量的策略跳过这一段**：规则树的 params 是
	// 三棵树加一张指标表，按 map[string]float64 去解必然失败。
	// 它的校验在 dryBuild 里由 Configure 自己做，而且查得更细
	// （引用了不存在的指标、字段名写错、比较符不认识都会报出来）
	if specs, ok := eng.Strategies.Specs(c.Strategy.Impl); ok && !c.strategyIsStructured() {
		p, err := decodeStrategyParams(specs, c.Strategy.Params)
		if err != nil {
			return fmt.Errorf("strategy.params: %w", err)
		}
		if err := spec.ValidateAll(specs, p); err != nil {
			return fmt.Errorf("strategy.params: %w", err)
		}
	}
	return c.dryBuild()
}

// dryBuild 把每个模块真的构造一遍再丢掉，用来提前暴露参数错误。
//
// 光靠上面的结构校验不够：把 slots 写成 slot、base 写成不存在的取值，
// 都要等 registry.Build 才发现 —— 而那发生在**数据载入之后**。
// 全量载入要 30 秒，为一个拼写错误等 30 秒是最差的体验，
// 何况错误信息还会指向一个和拼写毫无关系的地方。
//
// 构造这些模块没有副作用（fee=config 会读一次费率文件，
// 而「费率文件在不在」本来就该在跑之前查）。
func (c *Config) dryBuild() error {
	if _, err := trading.Markets.Build(c.Market.Impl, c.Market.Params); err != nil {
		return fmt.Errorf("market: %w", err)
	}
	if _, err := trading.Fees.Build(c.Fee.Impl, c.feeParams()); err != nil {
		return fmt.Errorf("fee: %w", err)
	}
	if _, err := trading.Slippages.Build(c.Slippage.Impl, c.Slippage.Params); err != nil {
		return fmt.Errorf("slippage: %w", err)
	}
	if _, err := trading.Sizers.Build(c.Sizer.Impl, c.Sizer.Params); err != nil {
		return fmt.Errorf("sizer: %w", err)
	}
	for i, r := range c.Risk {
		if _, err := trading.Risks.Build(r.Impl, r.Params); err != nil {
			return fmt.Errorf("risk[%d]: %w", i, err)
		}
	}
	for i, r := range c.Exit {
		if _, err := trading.Exits.Build(r.Impl, r.Params); err != nil {
			return fmt.Errorf("exit[%d]: %w", i, err)
		}
	}
	if c.Strategy.Impl != compositeImpl {
		st, err := eng.Strategies.Build(c.Strategy.Impl, nil)
		if err != nil {
			return fmt.Errorf("strategy: %w", err)
		}
		// 结构化配置（规则树）也要在这里查一遍 —— 引用了不存在的指标、
		// 字段名写错、比较符不认识，都该在跑之前失败
		if cs, ok := st.(eng.ConfigurableStrategy); ok {
			if err := cs.Configure(c.Strategy.Params); err != nil {
				return fmt.Errorf("strategy.params: %w", err)
			}
		}
	}
	return nil
}

const compositeImpl = "composite"

// 按市场的默认值。**一处定义**，配置层与前端都从这里取 ——
// 抄一份到别处就会分叉，然后出现「界面显示 1000 USDT、引擎按 100 万跑」。
const (
	marketCrypto = "crypto"
	// defaultCashAShare 20,000 元。散户量级；
	// 太大会让「一手买不起」这类真实约束在回测里消失
	defaultCashAShare = int64(2_000_000)
	// defaultCashCrypto 1,000 USDT（单位 0.01 USDT）
	defaultCashCrypto = int64(100_000)
)

func defaultMarketImpl(market string) string {
	if market == marketCrypto {
		return marketCrypto
	}
	return "ashare"
}

func defaultCashCents(market string) int64 {
	if market == marketCrypto {
		return defaultCashCrypto
	}
	return defaultCashAShare
}

// defaultLedger 加密用逐仓双向，A 股用现货。
//
// A 股普通账户不能做空（融资融券本项目明确不建模），
// 给它一个保证金账本只会让策略以为能开空。
func defaultLedger(market string) string {
	if market == marketCrypto {
		return "margin"
	}
	return "spot"
}

// maxLeverage 杠杆上限。
//
// **加密是账户级可选的 1–100x**，与期货不同 —— 期货每个合约有各自
// 固定的保证金率，交易者选不了；加密交易所让你在开仓时任选倍数，
// 所以它是**配置项**而不是标的属性。
// 100 是主流交易所 BTC/ETH 永续对普通用户的常见上限
// （更高的档位要么限币种、要么限仓位规模，回测里假装有它没有意义）。
const maxLeverage = 100

// validateLeverage 查杠杆与维持保证金率的取值域，以及它们与账本的搭配。
func (c *Config) validateLeverage() error {
	lev := c.Portfolio.Leverage
	if lev < 1 || lev > maxLeverage {
		return fmt.Errorf("portfolio.leverage = %d 越界，可选 1–%d", lev, maxLeverage)
	}
	if mmr := c.Portfolio.MaintMarginPPM; mmr < 1 || mmr >= 1_000_000 {
		return fmt.Errorf("portfolio.maint_margin_ppm = %d 越界，"+
			"可选 1–999999（百万分之一，主流交易所约 5000 = 0.5%%）", mmr)
	}
	if c.Portfolio.Ledger == "margin" {
		// 维持保证金率高于开仓保证金率时，仓位一开出来就该被强平 ——
		// 这不是「风控严」，是参数写错了
		if openPPM := int64(1_000_000) / lev; c.Portfolio.MaintMarginPPM >= openPPM {
			return fmt.Errorf(
				"portfolio: %d 倍杠杆的开仓保证金率是 %d ppm，"+
					"而维持保证金率 %d ppm 不低于它 —— 仓位一开出来就会被强平",
				lev, openPPM, c.Portfolio.MaintMarginPPM)
		}
		return nil
	}
	// 现货账本上杠杆是没有意义的。**必须报错而不是忽略**：
	// 静默忽略的话，用户以为自己在用 5 倍杠杆回测，
	// 拿到的却是 1 倍的结果，而报告里看不出任何异常
	if lev != 1 {
		return fmt.Errorf("portfolio.leverage = %d 但 ledger = %q —— "+
			"现货账本没有杠杆。要用杠杆请把 ledger 设为 margin",
			lev, c.Portfolio.Ledger)
	}
	return nil
}

// DefaultBenchmark 各市场的默认基准标的。
//
// A 股没有指数数据（C10 纯技术面，ETL 没拉指数行情），只能用宽基 ETF
// 代理；加密用 BTC 永续本身 —— 它就是这个市场的「大盘」。
func DefaultBenchmark(market string) string {
	if market == marketCrypto {
		return "BTC-USDT-SWAP"
	}
	return ""
}

// validateComposite 在跑之前把组合策略能查的都查掉。
func validateComposite(m Module) error {
	if _, err := eng.ParseCombineMode(m.Mode); err != nil {
		return fmt.Errorf("strategy.mode: %w", err)
	}
	if len(m.Sources) == 0 {
		return fmt.Errorf("strategy.sources 不能为空 —— 组合至少要有一个决策源")
	}
	for i, src := range m.Sources {
		if src.Impl == compositeImpl {
			// 嵌套组合能表达的东西，用一层加合适的 mode 都能表达，
			// 而嵌套会让「谁否决谁」变得难以推理
			return fmt.Errorf("strategy.sources[%d]: 不支持嵌套组合", i)
		}
		if !eng.Strategies.Has(src.Impl) {
			return fmt.Errorf("strategy.sources[%d]: 未知实现 %q，可选：%s",
				i, src.Impl, strings.Join(eng.Strategies.Names(), " / "))
		}
		// 结构化配置的源（规则树）自己校验，而且查得更细 ——
		// 它没有 ParamSpec，按标量去解必然失败
		if st, err := eng.Strategies.Build(src.Impl, nil); err == nil {
			if cs, ok := st.(eng.ConfigurableStrategy); ok {
				if err := cs.Configure(src.Params); err != nil {
					return fmt.Errorf("strategy.sources[%d].params: %w", i, err)
				}
				continue
			}
		}
		specs, _ := eng.Strategies.Specs(src.Impl)
		p, err := decodeStrategyParams(specs, src.Params)
		if err != nil {
			return fmt.Errorf("strategy.sources[%d].params: %w", i, err)
		}
		if err := spec.ValidateAll(specs, p); err != nil {
			return fmt.Errorf("strategy.sources[%d].params: %w", i, err)
		}
	}
	return nil
}

func parseAdj(s string) (mktdata.AdjMode, error) {
	switch strings.ToLower(s) {
	case "none", "raw":
		return mktdata.AdjNone, nil
	case "hfq":
		return mktdata.AdjHFQ, nil
	case "qfq":
		return 0, fmt.Errorf("engine.indicator_adj 不能是 qfq —— " +
			"前复权锚定末日，用于计算即构成未来函数（C1）且不可复现（C5）。" +
			"它只允许出现在展示路径上")
	}
	return 0, fmt.Errorf("未知的 indicator_adj %q，可选：none / hfq", s)
}

func parseLevel(s string) (record.Level, error) {
	return record.ParseLevel(strings.ToLower(s))
}

// Level 返回解析后的记录级别。调用前配置须已通过 Validate。
func (c *Config) Level() record.Level {
	l, _ := parseLevel(c.Recorder.Level)
	return l
}

// strategyIsStructured 报告当前策略的配置是不是「结构」而非标量。
func (c *Config) strategyIsStructured() bool {
	if c.Strategy.Impl == compositeImpl {
		return false
	}
	st, err := eng.Strategies.Build(c.Strategy.Impl, nil)
	if err != nil {
		return false
	}
	_, ok := st.(eng.ConfigurableStrategy)
	return ok
}

func decodeStrategyParams(specs []spec.ParamSpec, raw json.RawMessage) (spec.Params, error) {
	p := spec.Defaults(specs)
	if len(raw) == 0 {
		return p, nil
	}
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("策略参数必须是数值对象: %w", err)
	}
	for k, v := range m {
		p[k] = v
	}
	return p, nil
}

// resolvePath 把配置里的相对路径解成相对配置文件所在目录的路径。
//
// 相对进程 CWD 会让「同一份配置在不同目录下跑出不同结果」——
// 对一个把可复现性当硬约束的项目，那是不可接受的。
func (c *Config) resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

// readFile 单独抽出来只为让 fingerprint.go 不必再 import os。
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// sortedIDs 返回排序后的 ID 切片。装配路径上凡是从 map 出来的集合都要过它。
func sortedIDs(m map[mktdata.InstrumentID]bool) []mktdata.InstrumentID {
	out := make([]mktdata.InstrumentID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
