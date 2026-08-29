package mktdata

import (
	"fmt"

	"github.com/parquet-go/parquet-go"
)

// 枚举取值与 SCHEMA.md 0.4 一致。Python 侧 etl/schema.py 是同一份定义的另一实现，
// **两处必须同步**，任何变更都是双边改动。
type (
	Market         int8
	Exchange       int8
	InstrumentType int8
	Board          int8
	Currency       int8
	Status         int8
)

const (
	MarketUnknown Market = 0
	MarketAShare  Market = 1
)

const (
	ExchangeUnknown Exchange = 0
	ExchangeSSE     Exchange = 1
	ExchangeSZSE    Exchange = 2
	ExchangeBSE     Exchange = 3
)

const (
	TypeUnknown InstrumentType = 0
	TypeStock   InstrumentType = 1
	TypeETF     InstrumentType = 2
)

const (
	BoardUnknown Board = 0
	BoardMain    Board = 1
	BoardChiNext Board = 2
	BoardSTAR    Board = 3
	BoardBSE     Board = 4
)

const (
	CurrencyUnknown Currency = 0
	CurrencyCNY     Currency = 1
)

const (
	StatusUnknown  Status = 0
	StatusListed   Status = 1
	StatusDelisted Status = 2
)

func (t InstrumentType) String() string {
	switch t {
	case TypeStock:
		return "stock"
	case TypeETF:
		return "etf"
	}
	return "unknown"
}

func (b Board) String() string {
	switch b {
	case BoardMain:
		return "main"
	case BoardChiNext:
		return "chinext"
	case BoardSTAR:
		return "star"
	case BoardBSE:
		return "bse"
	}
	return "unknown"
}

// Instrument 是标的的静态属性，对应 SCHEMA.md 第 2 节。
type Instrument struct {
	ID       InstrumentID
	Market   Market
	Symbol   string
	Exchange Exchange
	Name     string
	Type     InstrumentType
	Board    Board
	// TrackedBoard 决定涨跌停幅度。ETF 与 Board 不同 —— ETF 自身不属于
	// 任何板块，其涨跌停由跟踪的指数决定（SCHEMA.md 2.5）。
	TrackedBoard Board
	PriceScale   int32
	QtyScale     int32
	QuoteCcy     Currency
	MinOrderQty  int32
	QtyStep      int32
	ListDate     int32
	DelistDate   int32 // 0 表示在市
	Status       Status
}

type instrumentRow struct {
	InstrumentID int32  `parquet:"instrument_id"`
	MarketV      int8   `parquet:"market"`
	Symbol       string `parquet:"symbol"`
	ExchangeV    int8   `parquet:"exchange"`
	Name         string `parquet:"name"`
	TypeV        int8   `parquet:"type"`
	BoardV       int8   `parquet:"board"`
	TrackedBoard int8   `parquet:"tracked_board"`
	PriceScale   int32  `parquet:"price_scale"`
	QtyScale     int32  `parquet:"qty_scale"`
	QuoteCcy     int8   `parquet:"quote_ccy"`
	MinOrderQty  int32  `parquet:"min_order_qty"`
	QtyStep      int32  `parquet:"qty_step"`
	ListDate     int32  `parquet:"list_date"`
	// delist_date 可空。parquet-go 用指针表达可空列。
	DelistDate *int32 `parquet:"delist_date,optional"`
	StatusV    int8   `parquet:"status"`
}

// Universe 是标的元数据的内存索引。
type Universe struct {
	byID map[InstrumentID]*Instrument
	all  []*Instrument
}

// LoadUniverse 从 instruments.parquet 读入标的元数据。
func LoadUniverse(path string) (*Universe, error) {
	rows, err := parquet.ReadFile[instrumentRow](path)
	if err != nil {
		return nil, fmt.Errorf("读取 instruments 失败: %w", err)
	}
	u := &Universe{
		byID: make(map[InstrumentID]*Instrument, len(rows)),
		all:  make([]*Instrument, 0, len(rows)),
	}
	for i := range rows {
		r := &rows[i]
		var delist int32
		if r.DelistDate != nil {
			delist = *r.DelistDate
		}
		inst := &Instrument{
			ID: InstrumentID(r.InstrumentID), Market: Market(r.MarketV),
			Symbol: r.Symbol, Exchange: Exchange(r.ExchangeV), Name: r.Name,
			Type: InstrumentType(r.TypeV), Board: Board(r.BoardV),
			TrackedBoard: Board(r.TrackedBoard),
			PriceScale:   r.PriceScale, QtyScale: r.QtyScale,
			QuoteCcy: Currency(r.QuoteCcy),
			MinOrderQty: r.MinOrderQty, QtyStep: r.QtyStep,
			ListDate: r.ListDate, DelistDate: delist, Status: Status(r.StatusV),
		}
		u.byID[inst.ID] = inst
		u.all = append(u.all, inst)
	}
	return u, nil
}

// Get 按 ID 取标的。不存在时返回 nil —— 调用方必须检查，
// 因为回测涉及已退市标的，「查不到」是需要显式处理的情形而非异常。
func (u *Universe) Get(id InstrumentID) *Instrument { return u.byID[id] }

// All 返回全部标的。
func (u *Universe) All() []*Instrument { return u.all }

// Len 返回标的数。
func (u *Universe) Len() int { return len(u.all) }

// BySymbol 按代码查找，主要供调试与测试使用。生产路径应始终用 ID。
func (u *Universe) BySymbol(symbol string) *Instrument {
	for _, i := range u.all {
		if i.Symbol == symbol {
			return i
		}
	}
	return nil
}
