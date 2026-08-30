package mktdata

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
	// MarketCrypto 加密货币。5 起留给非股票市场，中间空出来给港股/美股
	MarketCrypto Market = 5
)

const (
	ExchangeUnknown Exchange = 0
	ExchangeSSE     Exchange = 1
	ExchangeSZSE    Exchange = 2
	ExchangeBSE     Exchange = 3
	// ExchangeOKX 10 起留给非 A 股交易所
	ExchangeOKX Exchange = 10
)

const (
	TypeUnknown InstrumentType = 0
	TypeStock   InstrumentType = 1
	TypeETF     InstrumentType = 2
	// TypeSwap 永续合约。10 起留给衍生品，免得与现货类型挤在一起
	TypeSwap InstrumentType = 10
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
	// CurrencyUSDT 稳定币计价。10 起留给非法币
	CurrencyUSDT Currency = 10
)

const (
	StatusUnknown  Status = 0
	StatusListed   Status = 1
	StatusDelisted Status = 2
)

func (m Market) String() string {
	switch m {
	case MarketAShare:
		return "ashare"
	case MarketCrypto:
		return "crypto"
	}
	return "unknown"
}

func (t InstrumentType) String() string {
	switch t {
	case TypeStock:
		return "stock"
	case TypeETF:
		return "etf"
	case TypeSwap:
		return "swap"
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

	// ContractMult 合约乘数，定点 ×1e8。**每张合约代表多少标的**。
	//
	// A 股恒为 1（1e8）。加密永续来自 `attrs.ct_val`：
	// 1 张 BTC-USDT-SWAP = 0.01 BTC，故为 1e6。
	//
	// 没有它，「数量」这一列就没有意义 —— 名义额 = 张数 × ct_val × 价格。
	ContractMult int64

	// PriceTick 价格最小变动，与价格同 scale。0 表示未知（不做规整）。
	//
	// 加密来自 `attrs.tick_sz`：BTC-USDT-SWAP 是 0.1 USDT、
	// ETH-USDT-SWAP 是 0.01。A 股是 0.01 元，但 ETL 没有这一列，
	// 故为 0 —— **未知就不规整**，猜一个 tick 比不规整更糟。
	//
	// 用途只有一个：滑点调整后的成交价必须仍是**市场上真实存在的价格**。
	// 行情里的 OHLC 天然落在 tick 上，加了滑点就不一定了。
	PriceTick int64
	// Attrs 市场特定属性的原始 JSON，可空。
	// 引擎只解出确实要用的字段（当前只有 ct_val），其余原样留着
	Attrs string
}

// NotionalRatio 返回把 (定点价格 × 定点数量) 换算成**计价币种最小单位**
// 的比值：
//
//	金额 = 价格_fp × 数量_fp × num / den
//
// 推导：金额(最小单位) = (价格_fp/price_scale) × (数量_fp/qty_scale)
//
//	× (乘数_fp/1e8) × 100
//
// A 股：num=1 den=**10** —— 与旧的 `price*qty/10` 完全一致。
// 加密：num=1 den=**1e16**。
//
// # 为什么要一步步约分
//
// 直接算 `price_scale × qty_scale × 1e8` 在加密上是 **1e24**，
// 它本身就溢出 int64。所以每乘一个因子就先约一次，
// 让中间量始终留在 int64 内。
//
// 约分之后分母落得下，**但被除数落不下** ——
// BTC 的 价格_fp × 数量_fp 实测到 1.25e21，远超 int64（SCHEMA 0.6）。
// 所以除法必须走 128 位中间结果，见 trading.NotionalCents。
func (i *Instrument) NotionalRatio() (num, den int64) {
	mult := i.ContractMult
	if mult <= 0 {
		mult = contractMultOne
	}
	ps, qs := int64(i.PriceScale), int64(i.QtyScale)
	if ps <= 0 {
		ps = 1000
	}
	if qs <= 0 {
		qs = 1
	}
	num, den = mult*100, int64(1)
	for _, d := range [...]int64{contractMultOne, ps, qs} {
		g := gcd(num, d)
		num /= g
		den *= d / g
	}
	return num, den
}

const contractMultOne = int64(100_000_000) // 1e8

// parseContractMult 从 attrs 里取 ct_val 并换成定点 ×1e8。
//
// ct_val 在 ETL 里**存成字符串**（与 OKX 返回一致，见 SCHEMA 2.3）——
// 转成 float 再存回去会引入一次没必要的精度损失，而这个值要参与金额计算。
// 这里也用字符串解析：`strconv.ParseFloat` 后乘 1e8 会在 0.01 上
// 得到 999999.9999999999，取整成 999999 —— 合约乘数差一，全部名义额跟着差。
func parseContractMult(attrs string) int64 {
	var m struct {
		CtVal string `json:"ct_val"`
	}
	if err := json.Unmarshal([]byte(attrs), &m); err != nil || m.CtVal == "" {
		return 0
	}
	return decimalToFixed(m.CtVal, contractMultOne)
}

// parsePriceTick 从 attrs 里取 tick_sz，按该标的的 price_scale 转成定点。
//
// 与 ct_val 同理走字符串解析：0.1 × 1e8 经 float64 是 10000000.000000002，
// 0.01 更差。tick 差一个单位，规整后的价格就会系统性地偏一侧。
func parsePriceTick(attrs string, priceScale int32) int64 {
	if priceScale <= 0 {
		return 0
	}
	var m struct {
		TickSz string `json:"tick_sz"`
	}
	if err := json.Unmarshal([]byte(attrs), &m); err != nil || m.TickSz == "" {
		return 0
	}
	return decimalToFixed(m.TickSz, int64(priceScale))
}

// decimalToFixed 把十进制字符串按 scale 转成定点整数，**不经过 float**。
//
// 0.01 × 1e8：走 float64 会得到 999999.9999999999。差一个单位，
// 名义额就跟着差 —— 而这类错不会报警，只会让账目一直偏一点点。
func decimalToFixed(s string, scale int64) int64 {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	intPart, frac, _ := strings.Cut(s, ".")
	digits := 0
	for scale > 1 {
		scale /= 10
		digits++
	}
	for len(frac) < digits {
		frac += "0"
	}
	if len(frac) > digits {
		frac = frac[:digits] // 超出 scale 的位直接截断，不四舍五入
	}
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil {
		return 0
	}
	if neg {
		return -v
	}
	return v
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
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
	// attrs 市场特定属性的 JSON。引擎**只解出确实要用的字段**
	// （当前只有加密永续的 ct_val 合约乘数），其余原样留着。
	//
	// 不另立一列 contract_mult：那要改 schema 版本、改 ETL、重建
	// instruments.parquet，而这里只是把已经写在 attrs 里的东西读出来。
	// 7,202 行解一次 JSON，一次性开销可忽略。
	Attrs *string `parquet:"attrs,optional"`
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
			QuoteCcy:    Currency(r.QuoteCcy),
			MinOrderQty: r.MinOrderQty, QtyStep: r.QtyStep,
			ListDate: r.ListDate, DelistDate: delist, Status: Status(r.StatusV),
			ContractMult: contractMultOne,
		}
		if r.Attrs != nil && *r.Attrs != "" {
			inst.Attrs = *r.Attrs
			if m := parseContractMult(*r.Attrs); m > 0 {
				inst.ContractMult = m
			}
			inst.PriceTick = parsePriceTick(*r.Attrs, r.PriceScale)
		}
		u.byID[inst.ID] = inst
		u.all = append(u.all, inst)
	}
	return u, nil
}

// NewUniverse 由一组标的构造索引，供测试与内存构造使用。
//
// 生产路径走 LoadUniverse —— 那里的数据来自 parquet。
func NewUniverse(insts []*Instrument) *Universe {
	u := &Universe{
		byID: make(map[InstrumentID]*Instrument, len(insts)),
		all:  make([]*Instrument, 0, len(insts)),
	}
	for _, in := range insts {
		if in == nil {
			continue
		}
		u.byID[in.ID] = in
		u.all = append(u.all, in)
	}
	return u
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
