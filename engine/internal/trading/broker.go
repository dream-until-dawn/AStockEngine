package trading

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// OrderType 订单类型。
type OrderType int8

const (
	OrderMarket OrderType = iota
	OrderLimit
)

// Order 是策略发出的订单。
type Order struct {
	Instrument mktdata.InstrumentID
	Side       Side
	Type       OrderType
	Qty        int64
	LimitPrice int64 // 仅限价单，定点
	// Reduce 平仓单。**双向持仓下缺了它就无法判断意图**：
	//
	//	买 + 开 = 开多      买 + 平 = 平空
	//	卖 + 开 = 开空      卖 + 平 = 平多
	//
	// 现货账本忽略它（卖出永远是平多）。由 Sizer 从
	// Signal.Kind == SignalExit 设置 —— 信号层本来就区分开平，
	// 只是这个区分在折算成订单时被丢掉了。
	Reduce bool
	Tag    string // 策略自定，用于归因
}

// PendingOrder 是已定价、等待成交的订单。
//
// 订单必须**跨时点存活**：主板 T+1、创业板/科创板可 T 日盘后、加密零间隔，
// 信号与成交发生在不同时点（设计第 1 节）。
type PendingOrder struct {
	Order
	SignalAt  mktdata.TimePoint
	NotBefore int64 // UTC 毫秒
	PriceRef  PriceRef
	// Tried 已尝试撮合的次数，达到 MaxSteps 即过期
	Tried    int
	MaxSteps int
}

// RejectReason 是结构化的拒单原因。
//
// **绝不允许用「成交量为 0」笼统表示失败** —— v0.4 单步调试的核心价值
// 就是让用户看到「为什么没成交」，而不只是「没成交」。
type RejectReason int8

const (
	RejectNone                 RejectReason = iota
	RejectSuspended                         // 停牌
	RejectNotListed                         // 未上市 / 已退市
	RejectOneWordBoard                      // 一字板：全天一个价，无法成交
	RejectLimitUpNoBuy                      // 涨停买不进
	RejectLimitDownNoSell                   // 跌停卖不出
	RejectLimitPriceNotReached              // 限价单价格未触及
	RejectInvalidQty                        // 申报数量不合规
	RejectInsufficientCash                  // 现金不足
	RejectInsufficientPosition              // 可卖数量不足（T+1 未解冻）
	RejectVolumeCap                         // 超出当日成交量占比上限
	RejectNoPrice                           // 无有效参考价（如成交量为 0 导致 VWAP 无法计算）
	RejectExpired                           // 超过有效期未成交
	// RejectRisk 风控拦截。**具体是哪条规则看 Rejection.Rule** ——
	// 每加一条风控就加一个枚举值，枚举会退化成规则清单的镜像。
	RejectRisk
)

var rejectNames = map[RejectReason]string{
	RejectNone:                 "none",
	RejectSuspended:            "停牌",
	RejectNotListed:            "未上市或已退市",
	RejectOneWordBoard:         "一字板无法成交",
	RejectLimitUpNoBuy:         "涨停买不进",
	RejectLimitDownNoSell:      "跌停卖不出",
	RejectLimitPriceNotReached: "限价未触及",
	RejectInvalidQty:           "申报数量不合规",
	RejectInsufficientCash:     "现金不足",
	RejectInsufficientPosition: "可卖数量不足",
	RejectVolumeCap:            "超出成交量占比上限",
	RejectNoPrice:              "无有效参考价",
	RejectExpired:              "超过有效期",
	RejectRisk:                 "风控拦截",
}

func (r RejectReason) String() string {
	if s, ok := rejectNames[r]; ok {
		return s
	}
	return "unknown"
}

// Fill 是一笔成交。
type Fill struct {
	Order
	At mktdata.TimePoint
	// Price 是**市场参考价**，不含滑点。滑点单独记在 SlippageCents 里 ——
	// 混进价格会让「成交价」既不是市场上真实存在的价格，
	// 也无法与行情数据核对。
	Price int64 // 定点成交价
	Qty   int64
	// AmountCents 名义额，计价币种的最小单位（A 股 = 分，加密 = 0.01 USDT）。
	//
	// **由撮合器算好放进来**，账本与绩效不再各算一遍。理由有两个：
	// 一是换算要知道标的的 scale 与合约乘数，而账本手里没有标的表；
	// 二是同一笔成交的金额在三处各算一次，迟早会有一处口径不同。
	AmountCents int64
	Fee         FeeBreakdown
	// SlippageCents 执行摩擦（分）。它不是「费用」——
	// 佣金印花税过户费是付给第三方的真金白银，滑点是执行质量的损耗，
	// 两者分开记，费用合计才能与券商对账单对得上。
	SlippageCents int64
}

// Rejection 是一次拒单。Detail 给出可读的数值依据，
// 让单步调试能回答「涨停价多少、我挂了多少」这类问题。
type Rejection struct {
	Order
	At     mktdata.TimePoint
	Reason RejectReason
	// Rule 风控规则名，仅 Reason==RejectRisk 时有值。
	// Reason 回答「哪一类障碍」，Rule 回答「哪一条规则」——
	// v0.4 的单步调试两个都要展示。
	Rule   string
	Detail string
}

// WithAmount 补上名义额，供**手工构造 Fill** 的地方使用（主要是测试）。
//
// 生产路径只有 Broker.Match 会造 Fill，它自己算好。但 AmountCents 漏填
// 就是 0，而 0 不会报错 —— 账目会安静地少掉这一笔。
// 与其靠每处记得填，不如给一个补齐的入口。
func (f Fill) WithAmount(in *mktdata.Instrument) Fill {
	f.AmountCents = NotionalCents(in, f.Price, f.Qty)
	return f
}

// Slippage 滑点模型，可插拔。
//
// **它返回成本（分）而不是调整后的价格。** 这一点换过一次，理由是实测出来的：
//
// 价格是定点整数，单位厘。5 bp 的滑点在 20 元以下的标的上不足一厘，
// 于是无论怎么取整都错得很离谱 —— 0.594 元的标的：向下取整 −100%、
// 向上取整 +237%；1.071 元：−100% / +87%；3.132 元：−36% / +28%。
//
// 实测（buy_and_hold，成交额约 99.3 万元，配置 5.00 bp）：
//
//	向下取整（原实现）  3.67 bp   −26.5%
//	向上取整            5.63 bp   +12.7%
//	四舍五入            5.34 bp    +6.7%
//	本实现（按成本）    5.00 bp     精确
//
// **问题不在取整方向，在于施加的位置。**
//
// 改成按成交总额计成本后，粒度从厘变成分、基数从单价变成总额，
// 相对误差降到可忽略（10 万元一笔 5 bp = 5000 分，取整误差 0.02%）。
// 附带的好处是滑点从此在账本里可见 —— 以前它藏在成交价里，
// 报告只看得到佣金印花税，看不到执行摩擦到底吃掉多少。
type Slippage interface {
	Name() string
	// CostCents 返回这笔成交因滑点付出的额外成本（分，恒为非负）。
	// 买入时加到成本上，卖出时从收入里扣掉。
	//
	// **收的是名义额而不是 (价格, 数量)**：换算成金额要知道标的的
	// scale 与合约乘数，滑点模型不该也去操心这个。
	CostCents(side Side, amountCents int64, b mktdata.Bar) int64
}

// NoSlippage 无滑点，用于隔离测试。
type NoSlippage struct{}

func (NoSlippage) Name() string { return "none" }

func (NoSlippage) CostCents(Side, int64, mktdata.Bar) int64 { return 0 }

// BpsSlippage 固定基点滑点：按成交额的万分之 Bps/10 计成本。
type BpsSlippage struct{ Bps int64 }

func (s BpsSlippage) Name() string { return "fixed_bps" }

func (s BpsSlippage) CostCents(_ Side, amount int64, _ mktdata.Bar) int64 {
	if s.Bps <= 0 || amount <= 0 {
		return 0
	}
	// 四舍五入到分。这里的取整误差是 1 分对上万分，无关紧要 ——
	// 与原来「1 厘对 1 厘」的困境完全不是一个量级。
	return roundHalfUp(amount*s.Bps, 10_000)
}

// roundHalfUp 计算 a/b 四舍五入，要求 a >= 0 且 b > 0。
func roundHalfUp(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b/2) / b
}

// BrokerConfig 撮合参数。
type BrokerConfig struct {
	// VolumeCapPPM 单笔成交占当日成交量的上限（百万分之一）。
	// 默认 100000 = 10%。
	//
	// **取值需要实测支撑**：同一策略在 5%/10%/20% 下若收益差异很大，
	// 说明其收益依赖不现实的流动性假设 —— 那本身就是重要发现。
	VolumeCapPPM int64
	// AllowPartialFill 成交量受限时是否部分成交。false 则整单拒绝。
	AllowPartialFill bool
}

// DefaultBrokerConfig 返回默认撮合参数。
func DefaultBrokerConfig() BrokerConfig {
	return BrokerConfig{VolumeCapPPM: 100_000, AllowPartialFill: true}
}

// Broker 负责把订单撮合成成交或拒单。
type Broker struct {
	Market   Market
	Fee      Fee
	Slippage Slippage
	Cfg      BrokerConfig
}

// NewBroker 装配撮合器。
func NewBroker(m Market, f Fee, s Slippage, cfg BrokerConfig) *Broker {
	if s == nil {
		s = NoSlippage{}
	}
	if cfg.VolumeCapPPM <= 0 {
		cfg.VolumeCapPPM = 100_000
	}
	return &Broker{Market: m, Fee: f, Slippage: s, Cfg: cfg}
}

// refPrice 按价格基准取参考价。
func refPrice(ref PriceRef, b mktdata.Bar) (int64, bool) {
	switch ref {
	case PriceClose:
		if b.Close > 0 {
			return b.Close, true
		}
	case PriceVWAP:
		if b.Volume > 0 {
			// amount 是分、volume 是股，换算回厘：分×10/股
			return b.Amount * 10 / b.Volume, true
		}
		// 成交量为 0 时无法计算，回落到收盘价而非静默给出 0
		if b.Close > 0 {
			return b.Close, true
		}
	default:
		if b.Open > 0 {
			return b.Open, true
		}
	}
	return 0, false
}

// Match 尝试撮合一笔订单。返回 (成交, 拒单, 是否成交)。
//
// 检查顺序刻意从「最不可能成交」排到「最可能成交」，
// 这样拒单原因总是指向最根本的那个障碍 ——
// 停牌的股票不该报「现金不足」。
func (br *Broker) Match(po *PendingOrder, inst *mktdata.Instrument, b mktdata.Bar,
	now mktdata.TimePoint, led Ledger) (Fill, Rejection, bool) {

	rej := func(r RejectReason, detail string) (Fill, Rejection, bool) {
		return Fill{}, Rejection{Order: po.Order, At: now, Reason: r, Detail: detail}, false
	}

	if !br.Market.Tradable(inst, b) {
		if b.Suspended() {
			return rej(RejectSuspended, "该标的当日停牌")
		}
		return rej(RejectNotListed, fmt.Sprintf("交易日 %d 不在上市区间内", b.TradingDay))
	}

	up, down, hasLimit := br.Market.LimitPrices(inst, b)

	// 一字板：全天一个价。涨停一字板买不进，跌停一字板卖不出。
	// 停牌行的 OHLC 也全等，但已由 Tradable 先行拦截，两者不会混淆。
	if hasLimit && b.High == b.Low {
		if po.Side == SideBuy && b.High >= up {
			return rej(RejectOneWordBoard,
				fmt.Sprintf("一字涨停 %.4f，无法买入", priceOf(inst, up)))
		}
		if po.Side == SideSell && b.Low <= down {
			return rej(RejectOneWordBoard,
				fmt.Sprintf("一字跌停 %.4f，无法卖出", priceOf(inst, down)))
		}
	}

	ref, ok := refPrice(po.PriceRef, b)
	if !ok {
		return rej(RejectNoPrice, fmt.Sprintf("价格基准 %s 无有效取值", po.PriceRef))
	}

	// 参考价已在涨跌停上：买单在涨停价买不到，卖单在跌停价卖不掉
	if hasLimit {
		if po.Side == SideBuy && ref >= up {
			return rej(RejectLimitUpNoBuy,
				fmt.Sprintf("参考价 %.4f 已达涨停 %.4f", priceOf(inst, ref), priceOf(inst, up)))
		}
		if po.Side == SideSell && ref <= down {
			return rej(RejectLimitDownNoSell,
				fmt.Sprintf("参考价 %.4f 已达跌停 %.4f", priceOf(inst, ref), priceOf(inst, down)))
		}
	}

	// 成交价就是参考价。滑点不再推动价格（见 Slippage 接口注释）——
	// 因此也不再需要「把滑点推出去的价格夹回涨跌停区间」那一步：
	// ref 已经在上面校验过不在涨跌停上。
	price := ref

	// 限价单：买单要求成交价不高于限价，卖单不低于限价
	if po.Type == OrderLimit {
		px := func(v int64) float64 { return priceOf(inst, v) }
		if po.Side == SideBuy && price > po.LimitPrice {
			if b.Low <= po.LimitPrice {
				price = po.LimitPrice // 当日曾跌到限价，按限价成交
			} else {
				return rej(RejectLimitPriceNotReached,
					fmt.Sprintf("限价 %.4f，当日最低 %.4f", px(po.LimitPrice), px(b.Low)))
			}
		}
		if po.Side == SideSell && price < po.LimitPrice {
			if b.High >= po.LimitPrice {
				price = po.LimitPrice
			} else {
				return rej(RejectLimitPriceNotReached,
					fmt.Sprintf("限价 %.4f，当日最高 %.4f", px(po.LimitPrice), px(b.High)))
			}
		}
	}

	// 数量合规
	held := led.Available(po.Instrument, now.TsClose)
	qty, ok := br.Market.NormalizeQty(inst, po.Qty, po.Side, held)
	if !ok {
		if po.Side == SideSell && held <= 0 {
			return rej(RejectInsufficientPosition,
				fmt.Sprintf("可减仓 0（敞口 %+d，尚未解冻）",
					led.Exposure(po.Instrument).Net()))
		}
		return rej(RejectInvalidQty,
			fmt.Sprintf("申报 %d，最小 %d，步长 %d", po.Qty, inst.MinOrderQty, inst.QtyStep))
	}

	// 成交量占比上限
	// 走 128 位：BTC 的日成交量定点值到 1.25e17，直接乘 100000 ppm
	// 是 1.25e22，**回绕之后上限变成一个很小的数**，于是所有单子
	// 都被判成「超出成交量占比上限」—— 实测踩过这一脚
	cap := MulDiv(b.Volume, br.Cfg.VolumeCapPPM, 1_000_000)
	if cap <= 0 {
		return rej(RejectVolumeCap, "当日成交量为 0")
	}
	if qty > cap {
		if !br.Cfg.AllowPartialFill {
			return rej(RejectVolumeCap,
				fmt.Sprintf("申报 %d 超过当日成交量上限 %d", qty, cap))
		}
		qty, ok = br.Market.NormalizeQty(inst, cap, po.Side, held)
		if !ok {
			return rej(RejectVolumeCap,
				fmt.Sprintf("按上限 %d 截断后不满足申报单位", cap))
		}
	}

	// 资金 / 持仓校验
	// 名义额按**标的自己的 scale 与合约乘数**算，且走 128 位中间结果 ——
	// BTC 的 价格_fp × 数量_fp 实测到 1.25e21，直接相乘会静默回绕（SCHEMA 0.6）
	amount := NotionalCents(inst, price, qty)
	fee := br.Fee.Calc(FeeRequest{
		Instrument: inst, Side: po.Side, Qty: qty,
		AmountCents: amount, TradingDay: b.TradingDay,
	})
	slip := br.Slippage.CostCents(po.Side, amount, b)
	fill := Fill{
		Order: po.Order, At: now, Price: price, Qty: qty,
		AmountCents: amount, Fee: fee, SlippageCents: slip,
	}

	// **可行性由账本判断，不由撮合器判断。**
	//
	// 「买得起吗」的答案取决于账户类型：现货看现金，保证金账户看可用保证金；
	// 「卖得出吗」同理 —— 现货不能卖超持仓，保证金账户那叫开空。
	// 撮合器不该知道这个区别。滑点与费用一并计入需求额 ——
	// 漏掉会让判断偏松，成交之后账本才发现钱不够。
	if reason, detail, ok := led.CanFill(fill); !ok {
		return rej(reason, detail)
	}
	return fill, Rejection{}, true
}

// priceOf 把定点价按标的自己的 price_scale 转成人读值。
//
// 只用于拼消息。**不要拿它去算钱** —— 浮点一进来，
// 定点整数这条线就断了。
func priceOf(inst *mktdata.Instrument, v int64) float64 {
	scale := int64(1000)
	if inst != nil && inst.PriceScale > 0 {
		scale = int64(inst.PriceScale)
	}
	return float64(v) / float64(scale)
}
