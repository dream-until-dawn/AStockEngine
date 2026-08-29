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
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Module 是「按名字选实现 + 给它一段参数」。
type Module struct {
	Impl   string          `json:"impl"`
	Params json.RawMessage `json:"params,omitempty"`
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
	// Symbols 显式列表。给了就只用它，其余过滤条件忽略。
	Symbols []string `json:"symbols,omitempty"`
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
	InitialCashCents int64 `json:"initial_cash_cents"`
	DividendTaxPPM   int64 `json:"dividend_tax_ppm"`
}

// EngineCfg 引擎参数。
type EngineCfg struct {
	// IndicatorAdj 指标喂入的复权方式。**拒绝 qfq** —— 前复权锚定末日，
	// 用于计算即构成未来函数（C1）且不可复现（C5）
	IndicatorAdj string `json:"indicator_adj"`
	// ImplySplitFromFactor 指针是为了区分「没写」与「显式写了 false」，
	// 默认 true（约 6,770 个因子事件缺分红记录，不处理会失真）
	ImplySplitFromFactor *bool `json:"imply_split_from_factor,omitempty"`
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
	Name      string       `json:"name,omitempty"`
	Data      Data         `json:"data"`
	Market    Module       `json:"market"`
	Fee       Module       `json:"fee"`
	Slippage  Module       `json:"slippage"`
	Sizer     Module       `json:"sizer"`
	Risk      []Module     `json:"risk,omitempty"`
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
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	// 拒绝未知字段：把 "sizer" 写成 "sizor" 时若静默忽略，
	// 引擎会用默认值跑完并给出看似正常的结果 —— 比报错难查得多
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	c.dir = filepath.Dir(path)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 校验失败: %w", path, err)
	}
	return &c, nil
}

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
	if c.Market.Impl == "" {
		c.Market.Impl = "ashare"
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
		c.Portfolio.InitialCashCents = 100_000_000 // 100 万元
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
	if !eng.Strategies.Has(c.Strategy.Impl) {
		return fmt.Errorf("未知 strategy 实现 %q，可选：%s",
			c.Strategy.Impl, strings.Join(eng.Strategies.Names(), " / "))
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
	// 策略参数按 Specs 校验
	if specs, ok := eng.Strategies.Specs(c.Strategy.Impl); ok {
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
	if _, err := eng.Strategies.Build(c.Strategy.Impl, nil); err != nil {
		return fmt.Errorf("strategy: %w", err)
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

// RecordLevel 记录级别。
type RecordLevel int8

const (
	RecordNone RecordLevel = iota
	RecordSummary
	RecordFull
)

func parseLevel(s string) (RecordLevel, error) {
	switch strings.ToLower(s) {
	case "none":
		return RecordNone, nil
	case "summary":
		return RecordSummary, nil
	case "full":
		return RecordFull, nil
	}
	return 0, fmt.Errorf("未知的 recorder.level %q，可选：none / summary / full", s)
}

// Level 返回解析后的记录级别。调用前配置须已通过 Validate。
func (c *Config) Level() RecordLevel {
	l, _ := parseLevel(c.Recorder.Level)
	return l
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

// sortedIDs 返回排序后的 ID 切片。装配路径上凡是从 map 出来的集合都要过它。
func sortedIDs(m map[mktdata.InstrumentID]bool) []mktdata.InstrumentID {
	out := make([]mktdata.InstrumentID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
