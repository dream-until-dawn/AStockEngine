package trading

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// Ledger 是账本：记现金与持仓，回答「买得起吗 / 平得掉吗」，并在每步重估。
//
// v0.3 之前它是个 struct（`Portfolio`），字段全部导出，谁都能 `pf.Cash = 999`。
// 抽成接口不是为了好看 —— 它是**唯一还写死了单一市场语义的核心模块**：
//
//	持仓数量恒非负        期货 / 加密的空头无从表达
//	「买得起」= 现金够    保证金账户不是这么算的
//	没有逐步重估的位置    强平只能发生在重估之后
//
// 所以这里的签名刻意留出了三处：`Exposure` 分多空、`BuyingPowerCents` 而非
// 现金、以及 `Mark` 这个每步钩子。**当前只有现货一个实现**（A 股），
// 它的空头恒为 0、可用资金就是现金、Mark 什么也不做。
//
// 这是 C9 的原话：接口按多市场设计，实现只做 A 股。保证金与强平等到
// 有真实数据能验证时再写 —— 照文档写一个验不了的保证金引擎，
// 只会得到一个看起来对的抽象，而那是这个项目一路上最不愿意留下的东西。
type Ledger interface {
	Name() string

	// ---- 账户状态 ----

	// InitialCashCents 初始资金，用于算总收益
	InitialCashCents() int64
	// CashCents 账面现金
	CashCents() int64
	// BuyingPowerCents 本次下单可动用的资金上限。
	//
	// 现货账户 = 现金；保证金账户 = 现金 + 可用保证金。
	// **撮合必须问它而不是问现金** —— 问现金就等于假定了现货。
	BuyingPowerCents() int64
	// RealizedCents 累计已实现盈亏
	RealizedCents() int64
	// EquityCents 按给定标记价重估的总权益
	EquityCents(marks map[mktdata.InstrumentID]int64) int64

	// ---- 持仓 ----

	// Exposure 单个标的的敞口。不存在时返回零值
	Exposure(id mktdata.InstrumentID) Exposure
	// EachExposure 遍历非空敞口。fn 返回 false 即停止
	EachExposure(fn func(id mktdata.InstrumentID, e Exposure) bool)
	// NumPositions 有非空敞口的标的数
	NumPositions() int
	// Available 在给定时刻可**减少**该标的敞口的数量。
	// 现货是「可卖」（A 股 T+1），合约是「可平」
	Available(id mktdata.InstrumentID, nowMs int64) int64

	// ---- 可行性 ----

	// CanFill 在记账之前判断这笔成交能否落地。
	//
	// 抽到账本里而不是留在 Broker：「买得起吗」的答案取决于账户类型，
	// Broker 不该知道现货与保证金的区别。
	CanFill(f Fill) (reason RejectReason, detail string, ok bool)

	// ---- 记账 ----

	ApplyFill(f Fill, sellableFrom int64) error
	ApplyCorporateAction(ca CorporateAction, taxPPM int64, nowMs int64)
	ApplyImpliedSplit(id mktdata.InstrumentID, exDate int32, ratio float64, nowMs int64)

	// Mark 每步用最新价重估，返回本步发生的强平。
	//
	// 现货账户什么也不做、恒返回 nil。保证金账户在这里判断维持保证金率、
	// 产生强平。**它必须在策略之前调用** —— 强平是市场施加的，
	// 不是策略的决定。
	Mark(marks map[mktdata.InstrumentID]int64, now mktdata.TimePoint) []Liquidation

	// ---- 观测 ----

	// FeeCents 按 kind 拆分的累计费用。**只含付给第三方的真金白银**
	FeeCents() map[string]int64
	TotalFeeCents() int64
	// SlippageCents 累计滑点。执行质量的损耗，不是费用，故与上面分开
	SlippageCents() int64
	Warnings() []string

	// ---- 快照（C6）----

	SnapshotLedger() ([]byte, error)
	RestoreLedger([]byte) error
}

// Exposure 是账本对外暴露的持仓视图。
//
// **分多空两边而不是用一个带符号的数**：有些市场允许同时持有多空两个方向
// （双向持仓），净额会把这个信息抹掉。现货实现里 Short 恒为 0。
type Exposure struct {
	Long  int64 `json:"long"`
	Short int64 `json:"short"`
	// LongCost / ShortCost 各方向的累计成本（分），**含开仓时的费用与滑点**
	LongCost  int64 `json:"long_cost"`
	ShortCost int64 `json:"short_cost"`
}

// Net 净敞口：多头为正，空头为负。
func (e Exposure) Net() int64 { return e.Long - e.Short }

// IsEmpty 报告是否无任何敞口。
func (e Exposure) IsEmpty() bool { return e.Long == 0 && e.Short == 0 }

// AvgLongCostCents 多头每股平均成本。向下取整 —— 保守，避免虚增成本。
func (e Exposure) AvgLongCostCents() int64 {
	if e.Long <= 0 {
		return 0
	}
	return e.LongCost / e.Long
}

// Liquidation 是一次强制平仓。
//
// 现货账户永远不产生它。留在接口里是因为强平**必须能被记录与展示** ——
// 一次回测里出现强平却只表现为「权益突然掉了一截」，是最难查的那种失真。
// 成交上的标准 Tag。策略与规则自定的 tag 不受限，
// 但这几个由引擎/风控产生的必须是固定值 —— 报告按它们分类。
const (
	// TagLiquidation 强平。**不是策略的决定**，是市场施加的
	TagLiquidation = "liquidation"
)

type Liquidation struct {
	Instrument mktdata.InstrumentID
	At         mktdata.TimePoint
	// Side 被平掉的方向
	Side Side
	Qty  int64
	// Price 强平成交价（定点）
	Price int64
	// NotionalCents 被强平仓位的名义额（计价币种最小单位）。
	//
	// **数量与价格都是定点的**，脱离标的的 scale 读不出意义
	// （加密的 253000000 是 2.53 张，不是 2.53 亿）。
	// 名义额不需要 scale 就能读，所以它才是消息里该出现的量。
	NotionalCents int64
	// LostMarginCents 这次强平损失掉的保证金。逐仓下它就是这个仓位
	// 亏掉的全部 —— 不倒扣余额
	LostMarginCents int64
	// Reason 人读的原因，如「逐仓权益 −11.24 低于维持保证金 0.78」
	Reason string
}

func (l Liquidation) String() string {
	return fmt.Sprintf("%d 强平标的 %d：%s 仓位，名义额 %.2f，损失保证金 %.2f（%s）",
		l.At.TradingDay, l.Instrument, sideWord(l.Side),
		cents(l.NotionalCents), cents(l.LostMarginCents), l.Reason)
}

// 编译期确认现货账本满足接口。少一个方法就在这里报错。
var _ Ledger = (*Portfolio)(nil)
