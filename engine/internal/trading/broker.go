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
	LimitPrice int64  // 仅限价单，定点
	Tag        string // 策略自定，用于归因
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
	RejectNone RejectReason = iota
	RejectSuspended            // 停牌
	RejectNotListed            // 未上市 / 已退市
	RejectOneWordBoard         // 一字板：全天一个价，无法成交
	RejectLimitUpNoBuy         // 涨停买不进
	RejectLimitDownNoSell      // 跌停卖不出
	RejectLimitPriceNotReached // 限价单价格未触及
	RejectInvalidQty           // 申报数量不合规
	RejectInsufficientCash     // 现金不足
	RejectInsufficientPosition // 可卖数量不足（T+1 未解冻）
	RejectVolumeCap            // 超出当日成交量占比上限
	RejectNoPrice              // 无有效参考价（如成交量为 0 导致 VWAP 无法计算）
	RejectExpired              // 超过有效期未成交
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
	At    mktdata.TimePoint
	Price int64 // 定点成交价
	Qty   int64
	Fee   FeeBreakdown
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

// Slippage 滑点模型，可插拔。
type Slippage interface {
	Name() string
	// Apply 返回考虑滑点后的成交价
	Apply(side Side, refPrice int64, b mktdata.Bar, qty int64) int64
}

// NoSlippage 无滑点，用于隔离测试。
type NoSlippage struct{}

func (NoSlippage) Name() string { return "none" }
func (NoSlippage) Apply(_ Side, ref int64, _ mktdata.Bar, _ int64) int64 { return ref }

// BpsSlippage 固定基点滑点：买入价上浮、卖出价下压。
type BpsSlippage struct{ Bps int64 }

func (s BpsSlippage) Name() string { return "fixed_bps" }
func (s BpsSlippage) Apply(side Side, ref int64, _ mktdata.Bar, _ int64) int64 {
	adj := ref * s.Bps / 10_000
	if side == SideBuy {
		return ref + adj
	}
	v := ref - adj
	if v < 1 {
		v = 1
	}
	return v
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
	now mktdata.TimePoint, pf *Portfolio) (Fill, Rejection, bool) {

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
				fmt.Sprintf("一字涨停 %.3f，无法买入", float64(up)/1000))
		}
		if po.Side == SideSell && b.Low <= down {
			return rej(RejectOneWordBoard,
				fmt.Sprintf("一字跌停 %.3f，无法卖出", float64(down)/1000))
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
				fmt.Sprintf("参考价 %.3f 已达涨停 %.3f", float64(ref)/1000, float64(up)/1000))
		}
		if po.Side == SideSell && ref <= down {
			return rej(RejectLimitDownNoSell,
				fmt.Sprintf("参考价 %.3f 已达跌停 %.3f", float64(ref)/1000, float64(down)/1000))
		}
	}

	price := br.Slippage.Apply(po.Side, ref, b, po.Qty)
	// 滑点不得把成交价推出涨跌停区间 —— 那是不可能发生的成交
	if hasLimit {
		if price > up {
			price = up
		}
		if price < down {
			price = down
		}
	}

	// 限价单：买单要求成交价不高于限价，卖单不低于限价
	if po.Type == OrderLimit {
		if po.Side == SideBuy && price > po.LimitPrice {
			if b.Low <= po.LimitPrice {
				price = po.LimitPrice // 当日曾跌到限价，按限价成交
			} else {
				return rej(RejectLimitPriceNotReached,
					fmt.Sprintf("限价 %.3f，当日最低 %.3f",
						float64(po.LimitPrice)/1000, float64(b.Low)/1000))
			}
		}
		if po.Side == SideSell && price < po.LimitPrice {
			if b.High >= po.LimitPrice {
				price = po.LimitPrice
			} else {
				return rej(RejectLimitPriceNotReached,
					fmt.Sprintf("限价 %.3f，当日最高 %.3f",
						float64(po.LimitPrice)/1000, float64(b.High)/1000))
			}
		}
	}

	// 数量合规
	held := pf.Available(po.Instrument, now.TsClose)
	qty, ok := br.Market.NormalizeQty(inst, po.Qty, po.Side, held)
	if !ok {
		if po.Side == SideSell && held <= 0 {
			return rej(RejectInsufficientPosition,
				fmt.Sprintf("可卖 0 股（持仓 %d，T+1 未解冻）", positionTotal(pf, po.Instrument)))
		}
		return rej(RejectInvalidQty,
			fmt.Sprintf("申报 %d，最小 %d，步长 %d", po.Qty, inst.MinOrderQty, inst.QtyStep))
	}

	// 成交量占比上限
	cap := b.Volume * br.Cfg.VolumeCapPPM / 1_000_000
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
	amount := AmountCents(price, qty)
	fee := br.Fee.Calc(FeeRequest{
		Instrument: inst, Side: po.Side, Qty: qty,
		AmountCents: amount, TradingDay: b.TradingDay,
	})
	if po.Side == SideBuy {
		need := amount + fee.Total
		if need > pf.Cash {
			return rej(RejectInsufficientCash,
				fmt.Sprintf("需 %.2f 元，可用 %.2f 元",
					float64(need)/100, float64(pf.Cash)/100))
		}
	} else if qty > held {
		return rej(RejectInsufficientPosition,
			fmt.Sprintf("申报卖出 %d，可卖 %d", qty, held))
	}

	return Fill{Order: po.Order, At: now, Price: price, Qty: qty, Fee: fee},
		Rejection{}, true
}

func positionTotal(pf *Portfolio, id mktdata.InstrumentID) int64 {
	if p := pf.Position(id); p != nil {
		return p.Total
	}
	return 0
}
