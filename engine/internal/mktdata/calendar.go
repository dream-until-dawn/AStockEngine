package mktdata

import (
	"fmt"
	"sort"

	"github.com/parquet-go/parquet-go"
)

// CalendarDay 是交易日历的一行，对应 SCHEMA.md 第 3 节。
//
// 日历**含非交易日**（IsTradingDay=0）—— 这不是冗余：
// 校验「某标的在某日没有 bar」时，必须先能区分「停牌/未上市」与「休市」。
type CalendarDay struct {
	Market       Market
	Date         int32
	IsTradingDay bool
}

type calendarRow struct {
	MarketV      int8  `parquet:"market"`
	Date         int32 `parquet:"date"`
	IsTradingDay int8  `parquet:"is_trading_day"`
}

// Calendar 是交易日历的内存索引。
type Calendar struct {
	days    []CalendarDay // 按 (market, date) 升序
	trading map[int64]bool
}

// LoadCalendar 从 calendar.parquet 读入交易日历。
func LoadCalendar(path string) (*Calendar, error) {
	rows, err := parquet.ReadFile[calendarRow](path)
	if err != nil {
		return nil, fmt.Errorf("读取 calendar 失败: %w", err)
	}
	cal := &Calendar{
		days:    make([]CalendarDay, 0, len(rows)),
		trading: make(map[int64]bool, len(rows)),
	}
	for i := range rows {
		d := CalendarDay{
			Market: Market(rows[i].MarketV), Date: rows[i].Date,
			IsTradingDay: rows[i].IsTradingDay != 0,
		}
		cal.days = append(cal.days, d)
		if d.IsTradingDay {
			cal.trading[key64(InstrumentID(d.Market), d.Date)] = true
		}
	}
	sort.Slice(cal.days, func(i, j int) bool {
		if cal.days[i].Market != cal.days[j].Market {
			return cal.days[i].Market < cal.days[j].Market
		}
		return cal.days[i].Date < cal.days[j].Date
	})
	return cal, nil
}

// Days 返回指定市场、指定区间内的日历行。from / to 为 0 表示该侧不限。
func (c *Calendar) Days(m Market, from, to int32) []CalendarDay {
	out := make([]CalendarDay, 0, 256)
	for _, d := range c.days {
		if d.Market != m {
			continue
		}
		if (from != 0 && d.Date < from) || (to != 0 && d.Date > to) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// IsTradingDay 报告某市场某日是否为交易日。
func (c *Calendar) IsTradingDay(m Market, date int32) bool {
	return c.trading[key64(InstrumentID(m), date)]
}

// Len 返回日历总行数。
func (c *Calendar) Len() int { return len(c.days) }

// TradingDays 返回交易日行数。
func (c *Calendar) TradingDays() int { return len(c.trading) }

// CountTradingDays 数指定市场、指定区间内的交易日数。from / to 为 0 表示该侧不限。
//
// **绩效指标的年化系数要用它算，不能写 252。** 252 是美股惯例；
// 本项目日历实测 A 股 2005–2025 年均 242.90 天（238–246）。
// 用 252 会让年化收益与年化波动同时偏高，夏普的分子分母各偏一点、
// 不会互相抵消干净。远期接美股（~252）、加密货币（365）时，
// 从日历数这条自动正确，写死常数则每接一个市场都要改一次。
func (c *Calendar) CountTradingDays(m Market, from, to int32) int {
	n := 0
	for _, d := range c.days {
		if d.Market != m || !d.IsTradingDay {
			continue
		}
		if (from != 0 && d.Date < from) || (to != 0 && d.Date > to) {
			continue
		}
		n++
	}
	return n
}

// TradingDaysPerYear 由日历实测该市场在给定区间内的年均交易日数。
//
// 区间不足一年时回退到整段日历，避免用几十天的样本外推出一个荒唐的年化系数。
func (c *Calendar) TradingDaysPerYear(m Market, from, to int32) float64 {
	n := c.CountTradingDays(m, from, to)
	span := yearSpan(from, to)
	if n < 240 || span < 1.0 {
		// 样本太短，改用整段日历
		n = c.CountTradingDays(m, 0, 0)
		var lo, hi int32
		for _, d := range c.days {
			if d.Market != m || !d.IsTradingDay {
				continue
			}
			if lo == 0 || d.Date < lo {
				lo = d.Date
			}
			if d.Date > hi {
				hi = d.Date
			}
		}
		span = yearSpan(lo, hi)
	}
	if span <= 0 || n <= 0 {
		return 252 // 兜底，且调用方应当把它当成「数不出来」的信号
	}
	return float64(n) / span
}

// yearSpan 由两个 YYYYMMDD 估算跨越的年数。
func yearSpan(from, to int32) float64 {
	if from == 0 || to == 0 || to < from {
		return 0
	}
	y1, m1, d1 := int(from/10000), int(from/100%100), int(from%100)
	y2, m2, d2 := int(to/10000), int(to/100%100), int(to%100)
	// 以 365.25 天折算，够用且不引入日期库
	days := float64(y2-y1)*365.25 + float64(m2-m1)*30.4375 + float64(d2-d1)
	return days / 365.25
}
