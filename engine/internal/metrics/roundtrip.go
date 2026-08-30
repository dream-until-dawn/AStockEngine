package metrics

import (
	"sort"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// RoundTrip 是一轮完整交易：从建仓到平掉那部分仓位。
//
// 「胜率」问的是**一轮**的输赢，而成交是逐笔的。部分成交、加仓、送股
// 都会打断朴素配对，所以要按标的做 FIFO 逐层匹配。
type RoundTrip struct {
	Instrument mktdata.InstrumentID
	OpenDay    int32
	CloseDay   int32
	Qty        int64
	// Short 这一轮是做空。多空的盈亏方向相反，报告要能分开看
	Short bool
	// CostCents 这一轮付出去的钱：做多是买入成本，做空是买回花费。
	// 两者都含分摊到这一轮的费用与滑点
	CostCents int64
	// ProceedCents 这一轮收回来的钱：做多是卖出所得，做空是开仓所得。
	// 已扣除对应一侧的费用与滑点
	ProceedCents int64
	PnLCents     int64
	HoldDays     int
	// OpenTag / CloseTag 开仓与平仓成交的 tag，**这一轮是怎么开的、
	// 又是怎么结束的**。
	//
	// 没有它，逐轮表里所有的行长得一模一样：策略正常止盈平掉的、
	// 被止损砍掉的、被熔断清仓的、被强平爆掉的 —— 全都只是一行数字。
	// 而这四种结局对「策略行不行」的意义完全不同。
	OpenTag  string
	CloseTag string
	// FromBonus 该轮的建仓份额来自送股 / 转增（零成本），而非买入。
	//
	// 送股不产生成交记录，但确实增加了可卖份额。若把这部分算成
	// 「零成本买入」，盈亏比会被虚高的收益率带偏 —— 所以单独标出来，
	// 让报告能把它剔开看。
	FromBonus bool
}

// openLot 是 FIFO 队列里的一层。
type openLot struct {
	day int32
	qty int64
	// tag 开仓那笔成交的 tag，随份额一路带到轮次上
	tag string
	// value 该层剩余份额在**开仓那一侧**的金额（分），已含开仓摩擦：
	// 做多是付出去的成本，做空是收进来的所得
	value int64
	bonus bool
}

// lotKey 队列的键：标的 + 方向。
//
// **方向必须进键**：双向持仓下同一标的的多头与空头是两个独立的仓位，
// 合用一个队列会让「开空」去配对「已有的多头」——
// 那配出来的既不是一轮多头也不是一轮空头，只是两笔无关成交的差价。
type lotKey struct {
	id    mktdata.InstrumentID
	short bool
}

// MatchRoundTrips 把成交流配成一轮轮交易。
//
// fills 必须按时间升序。返回的轮次也按平仓日升序。
//
// # hedge：一笔成交到底是开还是平
//
// 现货（hedge=false）只有一种可能：**买即开、卖即平**，`Reduce` 不看。
// A 股的减仓卖出与清仓卖出都是平多，没有「卖出开仓」这回事。
//
// 双向合约（hedge=true）要看 `Reduce`，用的就是 trading.LegOf 那张表。
//
// 不区分的后果是：每一笔开空都会被当成「卖掉一个多头」——
// 手上正好有多头就配出一轮方向错了的轮次（盈亏符号相反），
// 没有就按送股零成本入队，凭空造出一轮。
// 平空则会被当成开多，永远留在队列里不平。
//
// 注意**轮次数多于平仓笔数是正常的**：一笔平仓可以吃掉 FIFO 队列里的
// 好几层，每层算一轮。所以「轮次 > 成交数 ÷ 2」本身不说明配对有问题。
//
// **卖出量超过已配对的买入量时，多出的部分按零成本入队。** 那是送股 /
// 转增来的份额 —— 它们不产生成交记录，成本已经付在原有份额上，
// 再计一次成本会让盈亏比虚高。这些轮次会被标上 FromBonus。
func MatchRoundTrips(
	fills []trading.Fill, hedge bool,
) (trips []RoundTrip, open map[mktdata.InstrumentID]OpenLeg) {

	queues := make(map[lotKey][]openLot, 256)
	trips = make([]RoundTrip, 0, len(fills)/2+1)

	for _, f := range fills {
		if f.Qty <= 0 {
			continue
		}
		// 摩擦成本（费用 + 滑点）随成交一并计入这一轮
		friction := f.Fee.Total + f.SlippageCents
		short, opening := legOf(f, hedge)
		key := lotKey{id: f.Instrument, short: short}

		if opening {
			// 开多是把钱付出去（成本 = 金额 + 摩擦），
			// 开空是把钱收进来（所得 = 金额 − 摩擦）—— 符号相反
			v := f.AmountCents + friction
			if short {
				v = f.AmountCents - friction
			}
			queues[key] = append(queues[key], openLot{
				day: f.At.TradingDay, qty: f.Qty, value: v, tag: f.Tag,
			})
			continue
		}

		// 平仓：从队首逐层配对
		remain := f.Qty
		// 平多是收钱（所得 = 金额 − 摩擦），平空是付钱（花费 = 金额 + 摩擦）
		closeTotal := f.AmountCents - friction
		if short {
			closeTotal = f.AmountCents + friction
		}
		q := queues[key]
		for remain > 0 {
			if len(q) == 0 {
				// 没有对应的开仓层 —— 现货下这是送股 / 转增来的份额。
				// 合约下不该出现，出现了说明开平判断错了，
				// FromBonus 会把它标出来
				q = append(q, openLot{day: f.At.TradingDay, qty: remain, bonus: true})
			}
			lot := &q[0]
			take := lot.qty
			if take > remain {
				take = remain
			}
			// 两侧都按份额比例分摊，保证逐层加总等于总额
			openPart := lot.value * take / lot.qty
			closePart := closeTotal * take / f.Qty

			cost, proceed := openPart, closePart
			if short {
				// 做空反过来：开仓那一侧是收入，平仓那一侧才是成本
				cost, proceed = closePart, openPart
			}
			trips = append(trips, RoundTrip{
				Instrument: f.Instrument,
				OpenDay:    lot.day, CloseDay: f.At.TradingDay,
				Qty: take, Short: short,
				CostCents: cost, ProceedCents: proceed,
				PnLCents: proceed - cost,
				HoldDays: dayDiff(lot.day, f.At.TradingDay),
				OpenTag:  lot.tag, CloseTag: f.Tag,
				FromBonus: lot.bonus,
			})

			lot.qty -= take
			lot.value -= openPart
			remain -= take
			if lot.qty == 0 {
				q = q[1:]
			}
		}
		queues[key] = q
	}

	sortTrips(trips)
	return trips, collectOpen(queues)
}

// legOf 判断一笔成交作用在哪个方向、是开还是平。
func legOf(f trading.Fill, hedge bool) (short, opening bool) {
	leg := trading.LegOf(f.Side, f.Reduce, hedge)
	return leg.Short, leg.Opening
}

// OpenLeg 回测结束时某个标的仍未平掉的敞口。
type OpenLeg struct {
	// LongQty / ShortQty 定点数量。**不同标的的 scale 不同，
	// 跨标的相加没有意义** —— 报告要报的是 CostCents
	LongQty  int64
	ShortQty int64
	// CostCents 这些未平仓位的开仓金额（分）。它是跨标的可加的量，
	// 也是「还有多少钱没结算」这个问题的答案
	CostCents int64
}

// collectOpen 汇总回测结束时仍未平掉的仓位。
//
// 未平仓不计入胜率 —— 既不是赢也不是输。但要报出来，藏起来会让胜率失真。
func collectOpen(queues map[lotKey][]openLot) map[mktdata.InstrumentID]OpenLeg {
	out := make(map[mktdata.InstrumentID]OpenLeg, len(queues))
	for k, q := range queues {
		var qty, val int64
		for _, l := range q {
			qty += l.qty
			val += l.value
		}
		if qty <= 0 {
			continue
		}
		leg := out[k.id]
		if k.short {
			leg.ShortQty += qty
		} else {
			leg.LongQty += qty
		}
		leg.CostCents += val
		out[k.id] = leg
	}
	return out
}

// sortTrips 把轮次按平仓日升序排好，同日按标的、再按方向排。
//
// **必须定序**：同一天平掉的多笔轮次来自不同队列，而队列存在 map 里，
// 不排的话同一份配置两次跑出的逐轮明细顺序不同 ——
// C5 就是在这类地方失守的。
//
// 这里排的是**给下游一个确定的顺序**，不是展示顺序。
// 展示怎么排由调用方决定 —— 服务端把虚拟轮次并进来之后按**开仓日**
// 重排（见 sortTripDTOs），因为那张表读的是「哪一天做了什么决定」。
func sortTrips(trips []RoundTrip) {
	sort.SliceStable(trips, func(i, j int) bool {
		if trips[i].CloseDay != trips[j].CloseDay {
			return trips[i].CloseDay < trips[j].CloseDay
		}
		if trips[i].Instrument != trips[j].Instrument {
			return trips[i].Instrument < trips[j].Instrument
		}
		return !trips[i].Short && trips[j].Short
	})
}

// dayDiff 由两个 YYYYMMDD 估算相隔的自然日数。
//
// 用自然日而非交易日：持有期是给人看的量级感，
// 差几天不影响判断，而按交易日算要把日历传进来，得不偿失。
func dayDiff(from, to int32) int {
	if from == 0 || to == 0 {
		return 0
	}
	return int(julian(to) - julian(from))
}

// julian 把 YYYYMMDD 折算成一个连续日号（不要求与真实儒略日一致，
// 只要求两个日号之差等于自然日之差）。
func julian(d int32) int32 {
	y, m, day := d/10000, d/100%100, d%100
	if m <= 2 {
		y -= 1
		m += 12
	}
	a := y / 100
	b := 2 - a + a/4
	return int32(365.25*float64(y+4716)) + int32(30.6001*float64(m+1)) + day + b - 1524
}
