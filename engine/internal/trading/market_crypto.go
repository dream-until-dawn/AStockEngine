package trading

import (
	"encoding/json"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// CryptoMarket 是加密货币永续合约的规则实现。
//
// 与 A 股逐条对照，差别都在这里：
//
//	                A 股                    加密永续
//	涨跌停          ±10% / ±20% / ±5%       **无**
//	可卖时点        T+1                     **T+0**（成交即可平）
//	申报单位        100 股 / 1 股           张，BTC/ETH 均 0.01 张
//	年化系数        由日历数出（约 243）     **365**（24×7）
//	停牌            有                      无（交易所维护表现为缺 bar）
//	方向            仅多                    **双向**（见 MarginLedger）
//
// # 成交时点仍然是「下一根」
//
// 24×7 没有隔夜，一根 bar 结束就是下一根开始（SCHEMA 0.3），
// 所以「次日开盘价」在时间轴上与「本根收盘价」是同一时刻、价格也几乎相同。
// 但结构上仍取**下一根的开盘价**，理由与 A 股一样：
// 在做出决定的那一根上成交，等于用了当根收盘价这个决策依据本身，
// 差一个 tick 就是未来函数（C1）。
//
// # 没有涨跌停不等于什么价都能成交
//
// 流动性约束由 Broker 的成交量上限与 min_turnover 风控负责，
// 与 A 股同一条路径。这里只是说「规则上没有价格上下限」。
type CryptoMarket struct {
	// OrderValidSteps 订单有效期（时点数）。
	// 加密没有「当日委托」的概念，但过期机制仍要有 ——
	// 否则一张挂不上的单会永远留在队列里
	OrderValidSteps int
}

var cryptoMarketSpecs = []spec.ParamSpec{
	{Name: "order_valid_steps", Kind: spec.ParamInt, Default: 1, Min: 1, Max: 30, Step: 1,
		Desc: "订单有效期（时点数），超过仍未成交即作废"},
}

func NewCryptoMarket() *CryptoMarket { return &CryptoMarket{OrderValidSteps: 1} }

func (m *CryptoMarket) Name() string { return "crypto" }

// LimitPrices 永续合约没有涨跌幅限制。
//
// 返回 ok=false 而不是给一对极大极小值：**「没有限制」与「限制很宽」
// 是两回事**，前者在报告里应当显示成「无限制」，后者会显示成一个数字。
func (m *CryptoMarket) LimitPrices(*mktdata.Instrument, mktdata.Bar) (int64, int64, bool) {
	return 0, 0, false
}

// NextExecutable 下一根的开盘价。理由见类型注释。
func (m *CryptoMarket) NextExecutable(_ *mktdata.Instrument,
	signalAt mktdata.TimePoint) (ExecWindow, bool) {

	valid := m.OrderValidSteps
	if valid < 1 {
		valid = 1
	}
	return ExecWindow{
		NotBefore: signalAt.TsClose + 1,
		PriceRef:  PriceOpen,
		MaxSteps:  valid,
	}, true
}

// AllowsShort 永续合约双向开仓。
func (m *CryptoMarket) AllowsShort() bool { return true }

// SellableFrom T+0：成交即可平仓。
func (m *CryptoMarket) SellableFrom(*mktdata.Instrument, mktdata.TimePoint) int64 { return 0 }

// NormalizeQty 按标的的申报单位规整张数。
//
// 与 A 股的关键差别：**没有「全部卖出允许零股」这一条**。
// A 股那条是为了处理送股产生的零股（不足 100 股必须一次卖光），
// 而永续合约不存在零股 —— 数量始终是 lotSz 的整数倍。
func (m *CryptoMarket) NormalizeQty(inst *mktdata.Instrument, qty int64,
	side Side, held int64) (int64, bool) {

	if qty <= 0 {
		return 0, false
	}
	step := int64(inst.QtyStep)
	if step < 1 {
		step = 1
	}
	minQty := int64(inst.MinOrderQty)

	// 平仓不得超过持有量。**开空不受此限** ——
	// 那由账本按可用保证金判断，不由申报单位规则判断
	if side == SideSell && held > 0 && qty > held {
		qty = held
	}
	qty = qty / step * step
	if qty < minQty || qty <= 0 {
		return 0, false
	}
	return qty, true
}

// Tradable 加密不停牌。
//
// 但 `tradestatus == 0` 仍要拦：OKX 早期有整天零成交的 bar，
// 其 OHLC 是拿上一根价格铺平的（SCHEMA 1.5）。
// 那种 bar 上没有对手盘，成交是假的。
func (m *CryptoMarket) Tradable(_ *mktdata.Instrument, b mktdata.Bar) bool {
	return !b.Suspended() && b.Close > 0
}

// Units 永续合约以 USDT 计价、以张为数量单位。
//
// 「张」不是「个 BTC」：BTC-USDT-SWAP 一张 = 0.01 BTC（ct_val），
// 两者差 100 倍。报告里印张数才与交易所的持仓页对得上。
func (m *CryptoMarket) Units() (string, string) { return "USDT", "张" }

// AnnualDays 24×7，一年 365 天。
//
// **不查日历**：加密在 calendar 表里没有行（SCHEMA 3），
// 查了只会拿到兜底值 252 —— 而 252 与 365 差 45%，
// 年化收益、年化波动、夏普会一起错，且不报任何错。
func (m *CryptoMarket) AnnualDays(*mktdata.Calendar, int32, int32) float64 { return 365 }

func init() {
	Markets.Register("crypto",
		"加密永续：无涨跌停、T+0、按张申报、一年 365 天",
		cryptoMarketSpecs, func(raw json.RawMessage) (Market, error) {
			p, err := registry.DecodeParams(cryptoMarketSpecs, raw)
			if err != nil {
				return nil, err
			}
			return &CryptoMarket{OrderValidSteps: p.Int("order_valid_steps", 1)}, nil
		})
}

var _ Market = (*CryptoMarket)(nil)
