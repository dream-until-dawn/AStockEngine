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
