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
	// CostCents 含买入时分摊的费用与滑点
	CostCents int64
	// ProceedCents 已扣卖出时分摊的费用与滑点
	ProceedCents int64
	PnLCents     int64
	HoldDays     int
	// FromBonus 该轮的建仓份额来自送股 / 转增（零成本），而非买入。
	//
	// 送股不产生成交记录，但确实增加了可卖份额。若把这部分算成
	// 「零成本买入」，盈亏比会被虚高的收益率带偏 —— 所以单独标出来，
	// 让报告能把它剔开看。
	FromBonus bool
}

// openLot 是 FIFO 队列里的一层。
type openLot struct {
	day   int32
	qty   int64
	cost  int64 // 该层剩余份额对应的成本（分）
	bonus bool
}

// MatchRoundTrips 把成交流配成一轮轮交易。
//
// fills 必须按时间升序。返回的轮次也按平仓日升序。
//
// **卖出量超过已配对的买入量时，多出的部分按零成本入队。** 那是送股 /
// 转增来的份额 —— 它们不产生成交记录，成本已经付在原有份额上，
// 再计一次成本会让盈亏比虚高。这些轮次会被标上 FromBonus。
func MatchRoundTrips(fills []trading.Fill) (trips []RoundTrip, openQty map[mktdata.InstrumentID]int64) {
	queues := make(map[mktdata.InstrumentID][]openLot, 256)
	trips = make([]RoundTrip, 0, len(fills)/2+1)

	for _, f := range fills {
		if f.Qty <= 0 {
			continue
		}
		// 摩擦成本（费用 + 滑点）随成交一并计入这一轮
		friction := f.Fee.Total + f.SlippageCents
		amount := trading.AmountCents(f.Price, f.Qty)

		if f.Side == trading.SideBuy {
			queues[f.Instrument] = append(queues[f.Instrument], openLot{
				day: f.At.TradingDay, qty: f.Qty, cost: amount + friction,
			})
			continue
		}

		// 卖出：从队首逐层配对
		remain := f.Qty
		proceedTotal := amount - friction
		q := queues[f.Instrument]
		for remain > 0 {
			if len(q) == 0 {
				// 没有对应的买入层 —— 这是送股 / 转增来的份额
				q = append(q, openLot{day: f.At.TradingDay, qty: remain, cost: 0, bonus: true})
			}
			lot := &q[0]
			take := lot.qty
			if take > remain {
				take = remain
			}
			// 成本与收入都按份额比例分摊，保证逐层加总等于总额
			costPart := lot.cost * take / lot.qty
			proceedPart := proceedTotal * take / f.Qty

			trips = append(trips, RoundTrip{
				Instrument: f.Instrument,
				OpenDay:    lot.day, CloseDay: f.At.TradingDay,
				Qty: take, CostCents: costPart, ProceedCents: proceedPart,
				PnLCents:  proceedPart - costPart,
				HoldDays:  dayDiff(lot.day, f.At.TradingDay),
				FromBonus: lot.bonus,
			})

			lot.qty -= take
			lot.cost -= costPart
			remain -= take
			if lot.qty == 0 {
				q = q[1:]
			}
		}
		queues[f.Instrument] = q
	}

	// 回测结束时仍持有的份额不计入胜率 —— 未平仓既不是赢也不是输。
	// 但要把数量报出来，藏起来会让胜率失真。
	openQty = make(map[mktdata.InstrumentID]int64, len(queues))
	for id, q := range queues {
		var n int64
		for _, l := range q {
			n += l.qty
		}
		if n > 0 {
			openQty[id] = n
		}
	}

	sort.SliceStable(trips, func(i, j int) bool {
		if trips[i].CloseDay != trips[j].CloseDay {
			return trips[i].CloseDay < trips[j].CloseDay
		}
		return trips[i].Instrument < trips[j].Instrument
	})
	return trips, openQty
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
