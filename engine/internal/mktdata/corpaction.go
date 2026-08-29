package mktdata

import (
	"fmt"
	"sort"

	"github.com/parquet-go/parquet-go"
)

// CorpAction 是除权日的分红送配，对应 SCHEMA.md 第 5 节。
// 各字段均为**每股**值，定点 scale 1e6。
type CorpAction struct {
	Instrument    InstrumentID
	ExDate        int32
	CashBeforeTax int64
	StockDividend int64
	StockTransfer int64
	RightsRatio   int64
	RightsPrice   int64
}

// HasEffect 报告该记录是否有实际的账户影响。
// 数据源里存在全零的记录（如「不分配」被误采），跳过可省去无谓的账本变动。
func (c CorpAction) HasEffect() bool {
	return c.CashBeforeTax > 0 || c.StockDividend > 0 ||
		c.StockTransfer > 0 || c.RightsRatio > 0
}

type corpActionRow struct {
	InstrumentID  int32 `parquet:"instrument_id"`
	ExDate        int32 `parquet:"ex_date"`
	CashBeforeTax int64 `parquet:"cash_before_tax"`
	StockDividend int64 `parquet:"stock_dividend"`
	StockTransfer int64 `parquet:"stock_transfer"`
	RightsRatio   int64 `parquet:"rights_ratio"`
	RightsPrice   int64 `parquet:"rights_price"`
}

// CorpActions 按交易日索引公司行动。
type CorpActions struct {
	byDay map[int32][]CorpAction
	keys  map[int64]bool // instrument<<32 | ex_date，用于判定「是否有记录」
	total int
}

// LoadCorpActions 从 corporate_action.parquet 读入。
func LoadCorpActions(path string) (*CorpActions, error) {
	rows, err := parquet.ReadFile[corpActionRow](path)
	if err != nil {
		return nil, fmt.Errorf("读取 corporate_action 失败: %w", err)
	}
	ca := &CorpActions{
		byDay: make(map[int32][]CorpAction, 4096),
		keys:  make(map[int64]bool, len(rows)),
	}
	for i := range rows {
		r := &rows[i]
		a := CorpAction{
			Instrument: InstrumentID(r.InstrumentID), ExDate: r.ExDate,
			CashBeforeTax: r.CashBeforeTax, StockDividend: r.StockDividend,
			StockTransfer: r.StockTransfer,
			RightsRatio:   r.RightsRatio, RightsPrice: r.RightsPrice,
		}
		ca.keys[key64(a.Instrument, a.ExDate)] = true
		if !a.HasEffect() {
			continue
		}
		ca.byDay[a.ExDate] = append(ca.byDay[a.ExDate], a)
		ca.total++
	}
	for d := range ca.byDay {
		sort.Slice(ca.byDay[d], func(i, j int) bool {
			return ca.byDay[d][i].Instrument < ca.byDay[d][j].Instrument
		})
	}
	return ca, nil
}

func key64(id InstrumentID, day int32) int64 {
	return int64(id)<<32 | int64(uint32(day))
}

// OnDay 返回该交易日的全部公司行动。
func (c *CorpActions) OnDay(day int32) []CorpAction { return c.byDay[day] }

// Has 报告某标的在某日是否**有记录**（哪怕内容全零）。
//
// 与 OnDay 的区别很重要：约 6,770 个复权因子事件根本没有对应记录
// （ETL.md 已知缺口），Portfolio 需要靠这个判断来决定是否按因子推算入账。
func (c *CorpActions) Has(id InstrumentID, day int32) bool {
	return c.keys[key64(id, day)]
}

// Total 返回有实际影响的记录数。
func (c *CorpActions) Total() int { return c.total }

// EventRatio 返回某标的在某除权日的因子跳变比例。
//
// ratio = factor(该日) / factor(前一事件)，即每股在该事件后相当于多少股。
// isEvent 为 false 表示该日不是除权日。
//
// Portfolio 用它处理「有因子事件但无分红记录」的情形 —— 按比例推算送转。
func (a *Adjuster) EventRatio(id InstrumentID, day int32) (float64, bool) {
	pts := a.byInstrument[id]
	if len(pts) == 0 {
		return 1, false
	}
	i := sort.Search(len(pts), func(k int) bool { return pts[k].exDate >= day })
	if i >= len(pts) || pts[i].exDate != day {
		return 1, false
	}
	prev := int64(FactorScale)
	if i > 0 {
		prev = pts[i-1].factor
	}
	if prev == 0 {
		return 1, false
	}
	return float64(pts[i].factor) / float64(prev), true
}

// ExDates 返回该标的的全部除权日，供质检与调试使用。
func (a *Adjuster) ExDates(id InstrumentID) []int32 {
	pts := a.byInstrument[id]
	out := make([]int32, len(pts))
	for i, p := range pts {
		out[i] = p.exDate
	}
	return out
}
