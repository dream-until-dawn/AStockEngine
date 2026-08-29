# v0.2 第二刀设计草案：撮合与账本

> **状态：已实现并验收**
> 最后更新：2026-08-29
> 前置：[第一刀（市场数据流）](DESIGN-v0.2-dataflow.md) 已验收
> 依据：[ROADMAP.md](ROADMAP.md) 的 C2 / C4 / C5 / C6 / C8 / C9

本刀交付 `Market` / `Broker` / `Portfolio` / `Fee` 四个模块，
产出一个**能真正下单成交、账目自洽**的引擎。

第一刀的 `OnBar(StepContext) error` 将改为返回订单。

---

## 1. 核心问题：信号、订单、成交发生在**不同时点**

这是本刀最难的部分，也是第一刀评审中被推翻的那条假设的直接后果。

初版曾以为「T 日信号 T+1 执行」是全市场规则，实为主板规则：
创业板/科创板有盘后固定价格交易（T 日可成交），加密货币则完全无间隔。
因此**订单必须跨时点存活**，引擎需要一个待撮合队列。

### 1.1 单步内的执行顺序

顺序不是随意的，每一步都有理由：

```
Step(t):
  1. 更新指标           ← 当日 bar 已收盘，信息可得
  2. 公司行动入账        ← 除权除息在开盘前生效，必须先于撮合
  3. 撮合到期订单        ← execAt <= t 的订单，用 t 的 bar 撮合
  4. 交收与估值          ← 解冻到期股份，按当日收盘价重估权益
  5. 调用策略 OnBar      ← 策略看到的是「已成交后」的持仓
  6. 新订单定价与入队     ← 由 Market.NextExecutable 算出 execAt
```

三个不可颠倒之处：

- **2 先于 3**：除权日若先撮合再除权，成交价与持仓基准会错配
- **3 先于 5**：策略必须看到已成交的结果，否则会重复下单
- **1 先于 5**：策略读到的指标必须**已含当前 bar**（第一刀已确立）

### 1.2 待撮合队列

```go
type PendingOrder struct {
    Order
    ExecAt   mktdata.TimePoint // Market.NextExecutable 算出
    ExpireAt mktdata.TimePoint // 超时未成交则撤单
}
```

**必须有过期机制**：涨停买不进的订单若一直挂着，会在几个月后突然成交，
这是隐蔽但严重的回测失真。默认「当日有效」（A 股实情），可配置。

---

## 2. Market —— 交易规则

### 2.1 规则有时间维度（沿用 C8.2）

第一刀已经在 ETL 侧发现涨跌幅规则会变。**费率同样会变**，
且历史变更点比涨跌幅更多。因此 `Market` 与 `Fee` 都必须按日期分段实现。

已知的印花税变更（**均需在实现前核实，不得凭记忆写死**）：

| 生效日 | 变更 | 状态 |
|---|---|---|
| 2023-08-28 | 减半至 **0.5‰** | ✅ 已核实 |
| 2008-09-19 | 由双边改为**单边**（仅卖出），税率维持 1‰ | ✅ 已核实 |
| 2008-04-24 | 由 3‰ 降至 1‰ | ✅ 已核实 |
| 2007-05-30 | 上调至 3‰ | ✅ 已核实（数据范围内，草案初版漏了） |
| 2005-01-23 | 由 2‰ 降至 1‰ | ✅ 已核实（数据范围内，草案初版漏了） |

来源：人民网、新浪财经对财政部公告的报道。数据起点 2005-01-04，
故 2005-01-23 之前还需一段 2‰ 的规则 —— 草案初版只列了三条，
实际落在数据范围内的有**六段**。

> 本项目已有先例：ST 涨跌幅由 5% 放宽至 10% 这条规则变更发生在
> 编写者的知识截止之后，是**由数据自己揭示的**（ETL.md 6.6）。
> 费率同理 —— 凭印象写死迟早出错，实现时必须逐条查证并记录来源。

### 2.2 接口

```go
type Market interface {
    // 涨跌停价。基准是 preclose（除权调整后的前收），不是前一日收盘（C8.1）
    LimitPrices(inst Instrument, b mktdata.Bar) (up, down int64)

    // 信号时点 → 最早可执行时点与执行价基准
    NextExecutable(inst Instrument, signalAt mktdata.TimePoint) (ExecWindow, bool)

    // 买入的股份何时可卖（A 股 T+1；跨境/黄金 ETF T+0；加密即时）
    SellableFrom(inst Instrument, filledAt mktdata.TimePoint) mktdata.TimePoint

    // 申报数量是否合规，返回修正后的数量
    NormalizeQty(inst Instrument, qty int64, side Side) (int64, bool)

    // 是否可交易（停牌、未上市、已退市）
    Tradable(inst Instrument, b mktdata.Bar) bool
}

type ExecWindow struct {
    At       mktdata.TimePoint
    PriceRef PriceRef
}

type PriceRef int8
const (
    PriceOpen      PriceRef = iota // 次日开盘价
    PriceClose                     // 当日/次日收盘价（盘后定价用此）
    PriceVWAP                      // 成交额/成交量
    PriceLimitOnly                 // 仅限价单，按限价与当日区间判定
)
```

`Instrument` 由 `instruments` 表提供：`board` / `type` / `min_order_qty` /
`qty_step` / `price_scale`。**接口中不得出现写死的 `T+1`、`10%`、`100 股`**（C9）。

### 2.3 ETF 的涨跌停仍是缺口

`instruments.board` 对 ETF 统一记为主板，但跟踪创业板/科创板指数的 ETF
实为 20%（ETL.md 已知缺口 #4）。本刀需要 `configs/market/etf_limits.json`
按 ETF 类别配置，或由 ETL 补一个 `tracked_board` 字段。

**倾向后者** —— 让数据回答数据的问题，比在引擎里维护一张硬编码表更耐久。

---

## 3. Broker —— 撮合

### 3.1 拒单原因必须结构化

这是 v0.4 单步调试的核心价值：用户要能看到「为什么没成交」。

```go
type RejectReason int8
const (
    RejectNone RejectReason = iota
    RejectSuspended      // 停牌
    RejectLimitUpNoBuy   // 涨停买不进
    RejectLimitDownNoSell// 跌停卖不出
    RejectOneWordBoard   // 一字板：全天一个价，无法成交
    RejectInsufficientCash
    RejectInsufficientPosition // 可卖不足（T+1 未解冻）
    RejectInvalidQty     // 申报数量不合规
    RejectVolumeCap      // 超出当日成交量占比上限
    RejectNotListed      // 未上市 / 已退市
    RejectExpired        // 超时未成交
)
```

**绝不允许用「成交量为 0」笼统表示失败** —— 那会让单步调试失去意义。

### 3.2 一字板的判定

一字板（全天一个价）意味着 `high == low`。若该价同时等于涨停价，
则买单无法成交；等于跌停价则卖单无法成交。

停牌行的 OHLC 也全等（SCHEMA.md 1.3），但由 `tradestatus == 0` 先行拦截，
两者不会混淆。

### 3.3 成交量约束

单笔成交不超过当日成交量的 X%，避免对流动性差的标的产生不真实的大额成交。

> ⚠️ **X 取多少需要有依据**，不能拍脑袋。倾向先取 10% 并做敏感性测试：
> 同一策略在 X = 5% / 10% / 20% 下的收益差多少。若差异很大，
> 说明策略的收益依赖于不现实的流动性假设 —— 那本身就是一个重要发现。

### 3.4 滑点可插拔

```go
type Slippage interface {
    Apply(side Side, refPrice int64, b mktdata.Bar) int64
}
```

实现：`none` / `fixed_bps` / `volume_ratio`（按下单量占当日成交量的比例放大）。

---

## 4. Portfolio —— 账本

### 4.1 股份与资金的交收规则不同

A 股的实情：

| 项 | 规则 |
|---|---|
| 股份 | **T+1** —— 当日买入次日可卖 |
| 资金 | **T+0** —— 当日卖出所得可当日买入（T+1 才能取现，回测不涉及） |

因此 `Position` 需要区分总量与可卖量：

```go
type Position struct {
    Total     int64      // 总持仓
    lots      []lot      // 未解冻批次
    AvgCost   int64      // 移动加权成本（定点）
}

type lot struct {
    Qty          int64
    SellableFrom mktdata.TimePoint // 由 Market.SellableFrom 给出
}
```

用批次队列而非「今日买入量」计数器，是为了兼容多市场：
加密货币即时可卖、跨境 ETF T+0、A 股 T+1，同一套结构都能表达。
批次通常只有 1~2 个，内存代价可忽略。

### 4.2 公司行动入账（C2）

**只调价格不调账户是回测的常见错误，会系统性低估收益。**

`corporate_action` 表在除权日提供每股现金红利、送股、转增、配股。处理：

| 事项 | 账户变化 |
|---|---|
| 现金分红 | 现金 += 持股 × 每股税前红利 − 红利税 |
| 送股 / 转增 | 持股 += 持股 × (送 + 转)，成本价按比例摊薄 |
| 配股 | **需要显式决策**：参与则扣现金增持股，不参与则被稀释 |

三点必须明确：

1. **红利税属规则不属数据**（SCHEMA.md 5.3）：税率随持股期限变化，
   由 `configs/fee/*.json` 配置。本刀先实现固定税率，持股期限分档留待后续
2. **配股需要策略决策**。默认「不参与」最保守；`Strategy` 可选实现
   `OnCorporateAction` 来表态
3. `pay_date` 恒为 null（ETL 已知缺口 #8），故**按除权日入账**，
   实际到账有数日延迟 —— 这会略微高估资金可用性，需记录在案

### 4.3 已知缺口的传导

约 6,770 个复权因子事件没有对应的分红送配记录（ETL.md 6.11），
其中 1,270 个是 2005-2007 股改对价送股。

**这意味着这些日期的持仓无法正确入账**，而价格序列（经复权因子）却是连续的。
后果：回测在这些日期会出现「价格跳变但账户没变」的失真。

对策：Portfolio 在检测到**有因子事件但无 corporate_action 记录**时，
按因子比例推算送转比例并入账，同时**记录一条警告**。
这是有损的近似（分不清是送转还是分红），但优于完全不处理。

---

## 5. Fee —— 费用

### 5.0 费率必须由用户配置，不能硬编码 ⚠️

三个原因：

1. **券商佣金各不相同** —— 万 2.5 是常见值而非法定值，实际从万 0.85 到万 3 都有，
   部分券商已取消最低 5 元
2. **加密货币的费率结构完全不同** —— maker/taker 分档、按币种、另有提现费
3. **监管费率会变** —— 印花税自 1991 年起已调整七次以上

因此 `Fee` 是**配置驱动**的：规则写在 `configs/fee/*.json`，
引擎只负责按规则计算。规则模型刻意做得比 A 股所需更宽：

| 维度 | 支持 |
|---|---|
| 计费方式 | 按成交额（ppm）、按数量（分/股）、每笔固定，三者可叠加 |
| 边界 | 最低值、上限 |
| 适用范围 | 日期区间、标的类型、板块、买卖方向、**挂单/吃单**（crypto 的 maker/taker） |

费率以**百万分之一（ppm）为单位的整数**声明，而非浮点：
万 2.5 = 250、印花税 0.5‰ = 500、过户费 0.001% = 10。
这样全程整数运算，满足 C5。

> **配置校验必须在加载期失败**：若同一 kind 在同一情形下匹配到多条规则，
> 费用会被重复计入而账目仍然「自洽」—— 这类错误极难被发现。

按标的类型 × 日期分段（C8）：

| 项 | 个股 | ETF |
|---|---|---|
| 佣金 | 由用户配置（默认万 2.5，最低 5 元，双向） | 同左 |
| 印花税 | 卖出 0.5‰（2023-08-28 起，六段历史） | **免征** |
| 过户费 | 双向 0.001% | **免征** |

```go
type Fee interface {
    // 返回本笔的各项费用（定点，单位为分）
    Calc(inst Instrument, side Side, qty, price int64, day int32) FeeBreakdown
}

type FeeBreakdown struct {
    Commission int64
    StampDuty  int64
    Transfer   int64
}
```

**返回明细而非总额**：单步调试要能看到钱花在哪，海选归因也需要拆项。

### 5.1 取整口径必须定死

「佣金万 2.5，最低 5 元」在整数下有多种实现，结果差一分钱：

```
成交额 100,000 分（1000 元）
  × 25 / 100000 = 25 分  →  低于最低 500 分，取 500
```

约定：**先按费率计算并向上取整到分，再与最低值取大**。
向上取整是因为券商实际按此收取（不足一分按一分计）。

> 这与涨跌停价的四舍五入是**不同**的取整规则 —— 前者是券商计费惯例，
> 后者是交易所定价规则。两者不可混用，必须分别写明。

---

## 6. 可复现性：全整数账本（C5）

**现金、持仓、成本、费用一律用定点整数**，禁止 float。

理由与价格相同：并发海选下浮点累加顺序不同会导致结果不同，
「同配置两次运行逐笔一致」就破了。

- 现金：分（1e-2 元）
- 持仓：股 / 份
- 成本价：与价格同 scale
- 权益：分

移动加权成本的除法需定义取整：`(旧成本×旧量 + 新价×新量) / 总量`，
**向下取整**（保守，避免虚增成本导致少交税）。这条也要写死。

---

## 7. 快照（C6）

在第一刀的基础上扩展：

```go
type snapshot struct {
    Steps      int
    Cursor     mktdata.CursorState
    Indicators map[string]map[string]indicator.State
    Portfolio  PortfolioState   // 新增
    Pending    []PendingOrder   // 新增
}
```

待撮合订单必须进快照 —— 否则实盘恢复后，昨日挂出的未成交单会凭空消失。

---

## 8. 接口草案

```go
type Side int8
const (SideBuy Side = iota; SideSell)

type OrderType int8
const (OrderMarket OrderType = iota; OrderLimit)

type Order struct {
    Instrument mktdata.InstrumentID
    Side       Side
    Type       OrderType
    Qty        int64
    LimitPrice int64 // 仅限价单
    Tag        string // 策略自定，用于归因
}

type Fill struct {
    Order
    At       mktdata.TimePoint
    Price    int64
    Qty      int64
    Fee      FeeBreakdown
}

type Rejection struct {
    Order
    At     mktdata.TimePoint
    Reason RejectReason
    Detail string // 如「涨停价 10.45，限价 10.50」
}

// 策略接口的变化
type Strategy interface {
    Name() string
    Specs() []ParamSpec
    Init(InitContext) error
    OnBar(StepContext) ([]Order, error)          // 变化
    // 可选：对配股表态。不实现则默认不参与
    // OnCorporateAction(ctx, action) RightsDecision
}
```

`StepContext` 新增：`Portfolio()` / `Fills()` / `Rejections()` / `Pending()`。

---

## 9. 验收结果

| 项 | 标准 | 结果 |
|---|---|---|
| **手工核算** | 个股与 ETF 各核一笔，与引擎输出逐分位一致 | ✅ 见下 |
| 时序 | 主板 T+1、创业板可 T 日盘后，两条路径均有测试 | ✅ `TestNextExecutableDiffersByBoard` |
| T+1 | 当日买入当日卖出被拒 | ✅ `TestT1Settlement` |
| 涨跌停 | 一字板、涨停价买单被拒，理由结构化 | ✅ 13 种拒单原因全覆盖 |
| 公司行动 | 分红入账、送转增持、成本摊薄、缺记录告警 | ✅ 四项各有测试 |
| 快照 | 含待撮合订单，恢复后结果一致 | ✅ 端到端账本完全一致 |
| 全整数 | 账本无任何 float | ✅ |

### 9.1 手工核算

```
买入 1000 股 @1700.000 元
  成交额 170,000,000 分  佣金 42,500  过户费 1,700  买入不收印花税
  成本 170,044,200 分 → 均价 1700.44 元/股
卖出 1000 股 @1800.000 元
  佣金 45,000  印花税 90,000（0.5‰）  过户费 1,800
  已实现 9,819,000 分 = 98,190 元
最终现金 = 初始 + 已实现，完全吻合
```

ETF 同额卖出费用 100 元 vs 个股 304 元 —— 免印花税与过户费的差异，
这正是「同一策略跑个股与跑 ETF 会得出不同结论」的原因。

### 9.2 端到端回测

`engine/cmd/backtest`，285 只个股、2020 年至今、MACD 金叉策略：

```
步数 1,614   耗时 188 ms
信号 1,962   买入成交 848   卖出成交 838
初始 100 万 → 权益 127.20 万（+27.20%）
费用合计 108,997.69 元
  佣金 41,986.17   印花税 65,324.12   过户费 1,687.40
快照往返：账本与全程完全一致
```

> 费用占初始资金的 **10.9%**，其中印花税占六成 —— 高换手策略的成本
> 远比直觉中大。这个数字本身就说明为什么费用模型不能省。

### 9.3 端到端才暴露的一个设计缺口 ⚠️

首次跑快照往返时账本对不上：全程现金 305,255 元、恢复后 24,922 元。

原因是**引擎的快照涵盖不了策略自己的字段**。恢复后的策略是全新实例，
不知道自己持有什么，于是重复下单。

修法分两层，两层都必要：

1. **多数状态本就不该由策略自己存**。「持有哪些标的」可由
   `ctx.Portfolio()` 导出、「哪些单在途」可由 `ctx.Pending()` 导出 ——
   这些状态归引擎管，策略再存一份必然在恢复时不一致。
   示例策略首版三项状态里有两项属于此类。
2. **真正的跨步记忆**（如「上一步 DIF 是否在 DEA 之上」）无法导出，
   为此新增可选的 `StatefulStrategy` 接口，其状态一并进快照。

修正后端到端账本完全一致。

> 这个缺口在单元测试里看不出来 —— 单测里策略状态简单到不影响结果。
> **只有跑完整链路才会暴露**。

---

## 10. 待核实与待实测

| # | 项 | 性质 |
|---|---|---|
| 1 | 印花税三次变更的生效日与税率 | **待核实**，不得凭记忆 |
| 2 | 日线 `volume` 是否含盘后固定价格交易成交量 | 待核实，影响成交量约束 |
| 3 | 佣金最低 5 元的取整方向 | 待核实券商惯例 |
| 4 | 成交量约束比例 X | 待实测敏感性（5%/10%/20%） |
| 5 | 红利税分档规则 | 待核实，本刀先用固定税率 |
| 6 | ETF 跟踪板块（决定 20% 涨跌停） | 倾向由 ETL 补 `tracked_board` 字段 |

---

## 11. 需要你拍板的

1. **配股默认行为** —— 默认不参与（保守）还是默认全额参与？
   我倾向不参与，且要求策略显式表态才参与。

2. **有因子无分红记录的 6,770 个事件**（4.3）—— 按因子推算入账并告警，
   还是直接跳过并告警？我倾向前者，有损近似优于完全不处理。

3. **订单默认有效期** —— 当日有效（A 股实情）还是可配置的 N 日？
   我倾向默认当日有效，配置项留出但不鼓励用。

4. **ETF 涨跌停** —— 引擎侧配置表，还是回头让 ETL 补 `tracked_board` 字段？
   我倾向后者，但那意味着本刀要等 ETL 改动。
