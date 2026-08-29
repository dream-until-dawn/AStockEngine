package mktdata

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/parquet-go/parquet-go"
)

// AdjMode 复权方式。四条路径各用哪种，见 docs/DESIGN-v0.2-dataflow.md 6.1。
type AdjMode int8

const (
	// AdjNone 原始价：撮合成交、账户市值、涨跌停判定
	AdjNone AdjMode = iota
	// AdjHFQ 后复权：技术指标（序列必须连续，否则除权日产生假信号）
	AdjHFQ
	// AdjQFQ 前复权：**仅限展示路径**。它锚定末日，每次新除权改写全部历史，
	// 用于决策即构成未来函数（C1）且不可复现（C5）。
	AdjQFQ
)

func (m AdjMode) String() string {
	switch m {
	case AdjNone:
		return "none"
	case AdjHFQ:
		return "hfq"
	case AdjQFQ:
		return "qfq"
	}
	return "unknown"
}

type factorPoint struct {
	exDate int32
	factor int64 // 定点，scale = FactorScale
}

// Adjuster 按 adj_factor 表把原始价换算为指定复权方式下的价格。
type Adjuster struct {
	byInstrument map[InstrumentID][]factorPoint
	// lastFactor 是各标的最后一个因子值，前复权的锚点
	lastFactor map[InstrumentID]int64
}

type factorRow struct {
	InstrumentID int32 `parquet:"instrument_id"`
	ExDate       int32 `parquet:"ex_date"`
	HFQFactor    int64 `parquet:"hfq_factor"`
}

// LoadAdjuster 从 adj_factor.parquet 读入复权因子。
func LoadAdjuster(path string) (*Adjuster, error) {
	rows, err := parquet.ReadFile[factorRow](path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", filepath.Base(path), err)
	}
	a := &Adjuster{
		byInstrument: make(map[InstrumentID][]factorPoint, 8192),
		lastFactor:   make(map[InstrumentID]int64, 8192),
	}
	for i := range rows {
		id := InstrumentID(rows[i].InstrumentID)
		a.byInstrument[id] = append(a.byInstrument[id],
			factorPoint{exDate: rows[i].ExDate, factor: rows[i].HFQFactor})
	}
	for id, pts := range a.byInstrument {
		sort.Slice(pts, func(i, j int) bool { return pts[i].exDate < pts[j].exDate })
		a.lastFactor[id] = pts[len(pts)-1].factor
	}
	return a, nil
}

// NumInstruments 返回有因子记录的标的数。
func (a *Adjuster) NumInstruments() int { return len(a.byInstrument) }

// Factor 返回指定标的在指定交易日生效的后复权因子（定点）。
//
// 因子**自除权日当日起生效**（v0.0 实测确认，见 SCHEMA.md 4.2），
// 故取最后一个 exDate <= day 的因子；无匹配记录时返回 FactorScale（即 1.0）。
func (a *Adjuster) Factor(id InstrumentID, day int32) int64 {
	pts := a.byInstrument[id]
	if len(pts) == 0 {
		return FactorScale
	}
	// 二分找最后一个 exDate <= day
	i := sort.Search(len(pts), func(k int) bool { return pts[k].exDate > day })
	if i == 0 {
		return FactorScale
	}
	return pts[i-1].factor
}

// Adjust 把原始价换算为指定复权方式下的价格，返回同样的定点单位。
func (a *Adjuster) Adjust(id InstrumentID, day int32, rawPrice int64, mode AdjMode) int64 {
	switch mode {
	case AdjNone:
		return rawPrice
	case AdjHFQ:
		return mulDivFactor(rawPrice, a.Factor(id, day))
	case AdjQFQ:
		last, ok := a.lastFactor[id]
		if !ok || last == 0 {
			return rawPrice
		}
		// 前复权 = 原始价 × factor(d) / factor(末日)
		hfq := mulDivFactor(rawPrice, a.Factor(id, day))
		return mulDivRound(hfq, FactorScale, last)
	}
	return rawPrice
}

// mulDivFactor 计算 price * factor / FactorScale，四舍五入，**全程整数且不溢出**。
//
// 直接相乘会溢出：价格上限约 3e6（3000 元 × 1000），因子上限约 1e16
// （10000 × 1e12），乘积 3e22 远超 int64 的 9.2e18。
//
// 故把因子拆成整数部分与小数部分分别相乘：
//
//	price*q         ≤ 3e6 × 1e4 = 3e10        ✓
//	price*r         ≤ 3e6 × 1e12 = 3e18       ✓（int64 上限 9.2e18）
//
// 两部分都在安全范围内，且结果与「无限精度下四舍五入」完全一致 ——
// 这是 C5 可复现性的前提：换一台机器、换一种编译器，结果必须逐位相同，
// 浮点做不到这个保证。
func mulDivFactor(price, factor int64) int64 {
	if factor == FactorScale {
		return price
	}
	neg := false
	if price < 0 {
		price, neg = -price, true
	}
	q := factor / FactorScale
	r := factor % FactorScale
	out := price*q + (price*r+FactorScale/2)/FactorScale
	if neg {
		return -out
	}
	return out
}

// mulDivRound 计算 a * b / c，四舍五入。用于前复权的二次换算。
//
// 这里 a 已是后复权价（上限约 1e8 厘），b = FactorScale = 1e12，
// 乘积可达 1e20 —— 同样会溢出，故仍走拆分。
func mulDivRound(a, b, c int64) int64 {
	if c == 0 {
		return a
	}
	// b / c 拆成整数与小数部分
	q := b / c
	r := b % c
	return a*q + (a*r+c/2)/c
}

// AdjustBar 返回按指定方式复权后的 bar。量与额不复权 —— 送转会改变股数，
// 但成交量的复权语义各家不一，且策略极少需要复权成交量。
func (a *Adjuster) AdjustBar(id InstrumentID, b Bar, mode AdjMode) Bar {
	if mode == AdjNone {
		return b
	}
	out := b
	out.Open = a.Adjust(id, b.TradingDay, b.Open, mode)
	out.High = a.Adjust(id, b.TradingDay, b.High, mode)
	out.Low = a.Adjust(id, b.TradingDay, b.Low, mode)
	out.Close = a.Adjust(id, b.TradingDay, b.Close, mode)
	out.PreClose = a.Adjust(id, b.TradingDay, b.PreClose, mode)
	return out
}
