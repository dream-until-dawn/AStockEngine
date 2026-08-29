package trading

import (
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"math"
)

// PriceRef 指定成交价的基准。
type PriceRef int8

const (
	PriceOpen  PriceRef = iota // 开盘价：次日开盘成交
	PriceClose                 // 收盘价：盘后固定价格交易按此成交
	PriceVWAP                  // 成交额 / 成交量
)

func (p PriceRef) String() string {
	switch p {
	case PriceClose:
		return "close"
	case PriceVWAP:
		return "vwap"
	}
	return "open"
}

// ExecWindow 描述一笔信号最早何时、按什么价基准可以成交。
//
// 用「不早于某个绝对时刻」而非「延迟 N 步」来表达，是因为 Market 并不知道
// 时点序列 —— 它只掌握规则，日历在数据里。引擎负责找到第一个满足
// NotBefore 的时点。这样多市场、24×7、休市日都无需特殊分支（C9）。
type ExecWindow struct {
	NotBefore int64 // UTC 毫秒。<= 信号时刻表示可在同一时点成交
	PriceRef  PriceRef
	// MaxSteps 订单最多在多少个可成交时点内有效，超出即过期。
	// 默认 1 = 当日有效（A 股实情）。
	//
	// **过期机制不可省**：涨停买不进的订单若一直挂着，会在几个月后突然成交，
	// 这是隐蔽但严重的回测失真。
	MaxSteps int
}

// Market 封装一个市场的全部交易规则。
//
// **引擎核心不得内建任何 `T+1`、`10%`、`100 股` 这类常量**（C9）——
// 它们全部住在这里，换市场只换实现。
type Market interface {
	Name() string
	// LimitPrices 返回涨停价与跌停价（定点，与 bar 同 scale）。
	// ok 为 false 表示该标的当日无涨跌幅限制（如新股上市初期）。
	LimitPrices(inst *mktdata.Instrument, b mktdata.Bar) (up, down int64, ok bool)
	// NextExecutable 由信号时点推出最早可成交时点与价格基准
	NextExecutable(inst *mktdata.Instrument, signalAt mktdata.TimePoint) (ExecWindow, bool)
	// SellableFrom 返回买入的股份最早何时可卖（UTC 毫秒）
	SellableFrom(inst *mktdata.Instrument, filledAt mktdata.TimePoint) int64
	// NormalizeQty 把申报数量修正为合规值。ok 为 false 表示无法修正
	NormalizeQty(inst *mktdata.Instrument, qty int64, side Side, held int64) (int64, bool)
	// Tradable 报告该标的当日是否可交易
	Tradable(inst *mktdata.Instrument, b mktdata.Bar) bool
	// AnnualDays 返回该市场一年有多少个「步」，用于年化收益与波动。
	//
	// **放在 Market 而不是 Calendar 上**：A 股由日历实测（含节假日、
	// 临时休市），而 24×7 市场压根没有日历表 —— 让 Calendar 去回答
	// 「加密一年几天」只能得到兜底值 252，而正确值是 365。
	// 45% 的偏差会直接改变「这个策略行不行」的结论。
	//
	// cal 可以为 nil（不依赖日历的市场直接返回常数）。
	AnnualDays(cal *mktdata.Calendar, from, to int32) float64
}

// ---- A 股实现 ----

// 涨跌幅规则的时间维度。与 etl/build_bars.py: limit_ratio 是**同一份规则的两处实现**，
// 任何变更都是双边改动。
//
// 两个生效日均由 v0.1 的板块自洽校验从数据中发现后查证确认（ETL.md 6.6）——
// 其中 ST 那条发生在编写者的知识截止之后，只能由数据揭示。
const (
	stMainRelaxedFrom  int32 = 20260706 // 主板风险警示股由 5% 放宽至 10%
	chiNextRelaxedFrom int32 = 20200824 // 创业板由 10% 放宽至 20%
)

// 涨跌幅以百万分之一表示，全程整数运算（C5）。
const (
	limit5  int64 = 50_000
	limit10 int64 = 100_000
	limit20 int64 = 200_000
	limit30 int64 = 300_000
)

// AShareMarket 是 A 股的规则实现。
type AShareMarket struct {
	// AfterHoursBoards 支持盘后固定价格交易的板块。
	//
	// 实测确认：15:05-15:30 按当日收盘价成交、时间优先撮合，
	// **仅适用于科创板与创业板，主板没有**。
	// 因此这些板块的 T 日收盘信号可在 T 日当天成交，主板则要等 T+1。
	AfterHoursBoards map[mktdata.Board]bool
	// OrderValidSteps 订单有效的可成交时点数，默认 1（当日有效）
	OrderValidSteps int
}

// NewAShareMarket 创建默认配置的 A 股市场规则。
func NewAShareMarket() *AShareMarket {
	return &AShareMarket{
		AfterHoursBoards: map[mktdata.Board]bool{
			mktdata.BoardSTAR:    true,
			mktdata.BoardChiNext: true,
		},
		OrderValidSteps: 1,
	}
}

func (m *AShareMarket) Name() string { return "ashare" }

// AnnualDays 由交易日历实测，而不是用「252」这个约定俗成的数。
//
// 252 是美股的口径。A 股因春节、国庆等长假，实测年均交易日在 242~245，
// 用 252 会让年化收益虚低约 3%、年化波动虚高约 2%。
func (m *AShareMarket) AnnualDays(cal *mktdata.Calendar, from, to int32) float64 {
	if cal == nil {
		return 252
	}
	return cal.TradingDaysPerYear(mktdata.MarketAShare, from, to)
}

// limitPPM 返回该标的当日的涨跌幅限制（百万分之一）。
//
// 用 TrackedBoard 而非 Board：ETF 自身不属于任何板块，其涨跌停由跟踪的
// 指数决定（SCHEMA.md 2.5）。个股的两者相同。
func (m *AShareMarket) limitPPM(inst *mktdata.Instrument, b mktdata.Bar) int64 {
	switch inst.TrackedBoard {
	case mktdata.BoardChiNext:
		if b.TradingDay >= chiNextRelaxedFrom {
			return limit20
		}
		return limit10
	case mktdata.BoardSTAR:
		return limit20
	case mktdata.BoardBSE:
		return limit30
	}
	// 主板。ST 的 5% 仅适用于主板，且 2026-07-06 起放宽至 10%
	if b.IsST == 1 && b.TradingDay < stMainRelaxedFrom {
		return limit5
	}
	return limit10
}

// LimitPrices 计算涨跌停价。
//
// 基准是 preclose（**除权调整后的前收盘价**），不是前一交易日的实际收盘价 ——
// 除权日两者相差极大，用后者会让 Broker 在每个大比例送转日误判（C8.1）。
//
// 取整是**四舍五入到分**。这与佣金的向上取整是不同规则：
// 前者是交易所定价规则，后者是券商计费惯例，不可混用。
func (m *AShareMarket) LimitPrices(inst *mktdata.Instrument, b mktdata.Bar) (int64, int64, bool) {
	if b.PreClose <= 0 {
		return 0, 0, false
	}
	// 别的市场没有涨跌停这回事，返回 false 而不是算一个数出来。
	//
	// 这不只是语义问题，也是溢出问题：roundToCent 里 preclose × 1.3e6
	// 在 A 股的价格量级（≤3e6 厘）上安全，而加密的定点价可到 1.25e13
	// （scale 1e8），乘完 1.6e19 **超过 int64**，会静默回绕成负数。
	// 见 SCHEMA.md 0.6。
	//
	// **market 未知时按 A 股处理**（走下面的量级兜底），而不是当成别的市场 ——
	// 未知市场若直接放行「无涨跌停」，等于在数据有问题时把风控关掉，
	// 回测结果会偏乐观。宁可用更严的规则，也不要静默放宽。
	if inst != nil && inst.Market != mktdata.MarketAShare &&
		inst.Market != mktdata.MarketUnknown {
		return 0, 0, false
	}
	// 量级兜底：market 填错时也不至于算出一个负的涨停价。
	// 上界由 roundToCent 的最坏情形反推（北交所 30% → factorPPM = 1.3e6）。
	if b.PreClose > maxPreCloseForLimit {
		return 0, 0, false
	}
	ppm := m.limitPPM(inst, b)
	return roundToCent(b.PreClose, 1_000_000+ppm),
		roundToCent(b.PreClose, 1_000_000-ppm), true
}

// maxPreCloseForLimit 是 roundToCent 不溢出的 preclose 上界。
//
// 最坏情形 factorPPM = 1e6 + 3e5（北交所 30%），再加上四舍五入的 5e6：
//
//	preclose × 1.3e6 + 5e6 ≤ 9.22e18   →   preclose ≤ 7.09e12
//
// A 股的价格上限约 3e6 厘，离这条线还有 200 万倍；
// 它拦的是「scale 不是 1000 的标的被当成了 A 股」这种情况。
const maxPreCloseForLimit = (math.MaxInt64 - 5_000_000) / 1_300_000

// roundToCent 计算 preclose × factorPPM / 1e6，并**四舍五入到分**。
//
// 价格以厘（1/1000 元）为单位，一分 = 10 厘，故要取整到 10 的倍数。
// 全程整数：preclose 上限约 3e6 厘，× 1.3e6 = 3.9e12，远在 int64 范围内。
//
// 校验：4.35 元 ×1.10 = 4.785 → 4.79（而非银行家舍入的 4.78）
//
//	5.35 元 ×1.10 = 5.885 → 5.89（而非 5.88）
func roundToCent(preclose, factorPPM int64) int64 {
	n := preclose * factorPPM // 厘 × 1e6
	// 除以 1e7 得到「分」，四舍五入；再乘 10 回到「厘」
	return ((n + 5_000_000) / 10_000_000) * 10
}

// NextExecutable 由信号时点推出最早可成交时点。
//
// 这是第一刀评审推翻的那条假设的落地：**引擎不得内建 T+1**。
//
//	主板              T+1（严格晚于信号时点的第一个时点）
//	创业板 / 科创板    T 日盘后固定价格交易，按当日收盘价成交
//	（远期）加密货币    零间隔
func (m *AShareMarket) NextExecutable(inst *mktdata.Instrument,
	signalAt mktdata.TimePoint) (ExecWindow, bool) {

	valid := m.OrderValidSteps
	if valid < 1 {
		valid = 1
	}
	if m.AfterHoursBoards[inst.TrackedBoard] {
		// 盘后定价：同一时点即可成交，价格取当日收盘价
		return ExecWindow{
			NotBefore: signalAt.TsClose,
			PriceRef:  PriceClose,
			MaxSteps:  valid,
		}, true
	}
	// 主板：严格晚于信号时点，按次日开盘价成交
	return ExecWindow{
		NotBefore: signalAt.TsClose + 1,
		PriceRef:  PriceOpen,
		MaxSteps:  valid,
	}, true
}

// SellableFrom 返回买入股份最早可卖的时刻。
//
// A 股股份 T+1：严格晚于成交时点的下一个时点。
// **资金则是 T+0** —— 当日卖出所得可当日买入，故 Portfolio 不冻结现金。
//
// 盘后定价买入的股份同样 T+1，不因成交时段不同而改变。
func (m *AShareMarket) SellableFrom(inst *mktdata.Instrument,
	filledAt mktdata.TimePoint) int64 {
	return filledAt.TsClose + 1
}

// NormalizeQty 把申报数量修正为合规值。
//
// held 是当前持仓，用于处理**卖出零股**：不足最小申报单位的余股
// 必须一次性全部卖出，这是 A 股的明文规则，也是唯一允许突破 qty_step 的情形。
func (m *AShareMarket) NormalizeQty(inst *mktdata.Instrument, qty int64,
	side Side, held int64) (int64, bool) {
	if qty <= 0 {
		return 0, false
	}
	step := int64(inst.QtyStep)
	if step < 1 {
		step = 1
	}
	minQty := int64(inst.MinOrderQty)

	if side == SideSell {
		if qty > held {
			qty = held
		}
		if qty <= 0 {
			return 0, false
		}
		// 全部卖出时允许零股；否则向下取整到申报单位
		if qty == held {
			return qty, true
		}
		qty = qty / step * step
		if qty < minQty {
			return 0, false
		}
		return qty, true
	}

	// 买入：向下取整到申报单位，且不得低于最小申报量
	qty = qty / step * step
	if qty < minQty {
		return 0, false
	}
	return qty, true
}

// Tradable 报告当日是否可交易。停牌行由 tradestatus 标记 ——
// 其 OHLC 全等于停牌前收盘价，若不拦截会「成交」在一个并不存在的价位上。
func (m *AShareMarket) Tradable(inst *mktdata.Instrument, b mktdata.Bar) bool {
	if b.Suspended() {
		return false
	}
	if inst.Status == mktdata.StatusDelisted && inst.DelistDate > 0 &&
		b.TradingDay > inst.DelistDate {
		return false
	}
	return b.TradingDay >= inst.ListDate
}
