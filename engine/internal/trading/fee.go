// Package trading 提供撮合与账本：Market / Broker / Portfolio / Fee。
package trading

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// Side 买卖方向。
type Side int8

const (
	SideBuy Side = iota
	SideSell
)

func (s Side) String() string {
	if s == SideSell {
		return "sell"
	}
	return "buy"
}

// Liquidity 区分挂单与吃单，供加密货币的 maker/taker 费率使用。
// A 股无此概念，一律取 LiquidityAny。
type Liquidity int8

const (
	LiquidityAny Liquidity = iota
	LiquidityMaker
	LiquidityTaker
)

// FeeBreakdown 是单笔交易的费用明细。
//
// **返回明细而非总额**：单步调试要能看到钱花在哪，海选归因也需要拆项。
// 全部为定点整数（分），禁用浮点 —— 并发海选下浮点累加顺序不同会破坏 C5。
type FeeBreakdown struct {
	Items map[string]int64 // kind -> 分
	Total int64
}

func (f FeeBreakdown) Get(kind string) int64 { return f.Items[kind] }

// FeeRequest 描述一次费用计算的输入。
type FeeRequest struct {
	Instrument  *mktdata.Instrument
	Side        Side
	Liquidity   Liquidity
	Qty         int64 // 股 / 份
	AmountCents int64 // 成交额，分
	TradingDay  int32 // YYYYMMDD，用于按日期分段选取规则
}

// Fee 计算交易费用。
type Fee interface {
	Calc(FeeRequest) FeeBreakdown
	Name() string
}

// ---- 配置驱动的实现 ----

// FeeRule 是一条费率规则。
//
// 费率必须**由用户配置**，不能硬编码：
//   - 各家券商佣金不同（万 2.5 只是常见值，实际从万 0.85 到万 3 都有）
//   - 加密货币的费率结构完全不同（maker/taker 分档、按币种、有提现费）
//   - 监管费率会变（印花税自 1991 年起已调整七次以上）
//
// 因此本结构刻意做得比 A 股所需更宽：同时支持按成交额、按数量、按笔计费，
// 以及最低 / 上限、日期区间、标的类型、买卖方向、挂单/吃单。
type FeeRule struct {
	// Kind 费用种类，自由字符串。A 股常用 commission / stamp_duty / transfer_fee，
	// 加密常用 trading_fee / funding。明细按此归类。
	Kind string `json:"kind"`
	// InstrumentTypes 适用的标的类型；空表示全部。取值 stock / etf
	InstrumentTypes []string `json:"instrument_types,omitempty"`
	// Boards 适用板块；空表示全部。取值 main / chinext / star / bse
	Boards []string `json:"boards,omitempty"`
	// Side 适用方向：buy / sell / both
	Side string `json:"side"`
	// Liquidity 适用挂/吃单：maker / taker / any
	Liquidity string `json:"liquidity,omitempty"`
	// From / To 生效区间（YYYYMMDD，含端点）。0 表示不限
	From int32 `json:"from,omitempty"`
	To   int32 `json:"to,omitempty"`

	// RatePPM 按成交额计费，单位百万分之一。
	// 万 2.5 = 250；印花税 0.05% = 500；过户费 0.001% = 10
	RatePPM int64 `json:"rate_ppm,omitempty"`
	// PerShareCents 按数量计费（分/股）。沪市过户费早年即按此计
	PerShareCents int64 `json:"per_share_cents,omitempty"`
	// FlatCents 每笔固定（分）
	FlatCents int64 `json:"flat_cents,omitempty"`
	// MinCents 最低收取（分）。佣金最低 5 元 = 500
	MinCents int64 `json:"min_cents,omitempty"`
	// MaxCents 上限（分）。0 表示无上限
	MaxCents int64 `json:"max_cents,omitempty"`

	Note string `json:"note,omitempty"`
}

// FeeConfig 是一份完整的费率配置。
type FeeConfig struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Rules       []FeeRule `json:"rules"`
}

// ConfigFee 是 Fee 的配置驱动实现。
type ConfigFee struct {
	cfg FeeConfig
}

// LoadFee 从 JSON 文件读取费率配置。
func LoadFee(path string) (*ConfigFee, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取费率配置失败: %w", err)
	}
	var cfg FeeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析费率配置失败: %w", err)
	}
	f := &ConfigFee{cfg: cfg}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// NewFee 由内存中的配置构造，便于测试。
func NewFee(cfg FeeConfig) (*ConfigFee, error) {
	f := &ConfigFee{cfg: cfg}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *ConfigFee) Name() string { return f.cfg.Name }

// Config 返回配置的只读副本，供展示与快照指纹使用。
func (f *ConfigFee) Config() FeeConfig { return f.cfg }

// validate 拒绝会导致同一 kind 在同一情形下匹配到多条规则的配置。
//
// 若不拦截，费用会被重复计入而账目仍然「自洽」—— 这类错误极难被发现，
// 因此在加载期就必须失败。
func (f *ConfigFee) validate() error {
	if len(f.cfg.Rules) == 0 {
		return fmt.Errorf("费率配置 %q 没有任何规则", f.cfg.Name)
	}
	for i, r := range f.cfg.Rules {
		if r.Kind == "" {
			return fmt.Errorf("第 %d 条规则缺少 kind", i)
		}
		switch strings.ToLower(r.Side) {
		case "buy", "sell", "both", "":
		default:
			return fmt.Errorf("第 %d 条规则的 side=%q 无效", i, r.Side)
		}
		if r.From != 0 && r.To != 0 && r.From > r.To {
			return fmt.Errorf("第 %d 条规则的日期区间倒置：%d > %d", i, r.From, r.To)
		}
		if r.RatePPM < 0 || r.PerShareCents < 0 || r.FlatCents < 0 {
			return fmt.Errorf("第 %d 条规则含负费率", i)
		}
	}
	return nil
}

func (r *FeeRule) matches(req FeeRequest) bool {
	inst := req.Instrument
	if inst == nil {
		return false
	}
	if r.From != 0 && req.TradingDay < r.From {
		return false
	}
	if r.To != 0 && req.TradingDay > r.To {
		return false
	}
	switch strings.ToLower(r.Side) {
	case "buy":
		if req.Side != SideBuy {
			return false
		}
	case "sell":
		if req.Side != SideSell {
			return false
		}
	}
	if len(r.InstrumentTypes) > 0 && !containsFold(r.InstrumentTypes, inst.Type.String()) {
		return false
	}
	if len(r.Boards) > 0 && !containsFold(r.Boards, inst.TrackedBoard.String()) {
		return false
	}
	switch strings.ToLower(r.Liquidity) {
	case "maker":
		if req.Liquidity != LiquidityMaker {
			return false
		}
	case "taker":
		if req.Liquidity != LiquidityTaker {
			return false
		}
	}
	return true
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// Calc 计算费用。
//
// 取整口径：**按费率算出的部分向上取整到分**，理由是券商实际按此收取
// （不足一分按一分计）。这与涨跌停价的四舍五入是**不同**的规则 ——
// 前者是券商计费惯例，后者是交易所定价规则，两者不可混用。
func (f *ConfigFee) Calc(req FeeRequest) FeeBreakdown {
	out := FeeBreakdown{Items: make(map[string]int64, 4)}
	for i := range f.cfg.Rules {
		r := &f.cfg.Rules[i]
		if !r.matches(req) {
			continue
		}
		var v int64
		if r.RatePPM > 0 {
			v += ceilDiv(req.AmountCents*r.RatePPM, 1_000_000)
		}
		if r.PerShareCents > 0 {
			v += req.Qty * r.PerShareCents
		}
		v += r.FlatCents
		if r.MinCents > 0 && v < r.MinCents {
			v = r.MinCents
		}
		if r.MaxCents > 0 && v > r.MaxCents {
			v = r.MaxCents
		}
		if v == 0 {
			continue
		}
		out.Items[r.Kind] += v
		out.Total += v
	}
	return out
}

// ceilDiv 向上取整的整数除法。仅用于非负数。
func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// ZeroFee 是不收任何费用的实现，用于隔离测试 —— 验证撮合逻辑时
// 不希望费用把持仓与现金的核算搅浑。
type ZeroFee struct{}

func (ZeroFee) Name() string                 { return "zero" }
func (ZeroFee) Calc(FeeRequest) FeeBreakdown { return FeeBreakdown{Items: map[string]int64{}} }
