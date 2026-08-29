# v0.3 设计草案：模块化装配

> 状态：**已确认**（2026-08-29 评审通过，第 10 节五项均按推荐拍板）
> 目标：换 JSON 配置即可改变引擎行为；同配置两次运行结果逐笔一致。

## 0. 这一刀做什么、不做什么

**做**：registry、配置装配、`Sizer` / `Risk` 两个新模块、`Recorder` 三档、
完整指标集、结果指纹。

**不做**（明确划给后面的版本）：

| 事 | 归属 | 理由 |
|---|---|---|
| HTTP / WebSocket 单步 API | v0.4 | 本刀只保证引擎**可配置**，不管怎么远程驱动 |
| 参数网格展开、并发海选 | v0.5 | 需要先有稳定的配置格式与结果指纹 |
| Recorder 流式落盘 | v0.4 | 落盘格式该由 API 的消费方式决定，现在定会白定 |
| DuckDB 结果库 | v0.5 | 海选才有「大量结果需要查询」的问题 |

## 1. 现状盘点

v0.2 已经是接口、可替换的：

| 模块 | 接口 | 内置实现 |
|---|---|---|
| `Market` | ✅ | `AShareMarket` |
| `Fee` | ✅ | `ConfigFee`（JSON 驱动，按日期分段） |
| `Slippage` | ✅ | `NoSlippage` / `BpsSlippage` |
| `Strategy` | ✅ | `BuyAndHold` / `MACross` / `MACDCross` |
| `Indicator` | ✅ | `SMA` / `EMA` / `MACD` / `KDJ` |

还硬编码、这一刀要拆出来的：

| 现状 | 问题 |
|---|---|
| **仓位大小由策略自己算** —— 三个样例都写着 `slotCents * 10 / bar.Close` | 换一种仓位方法要改策略源码。海选没法把「信号逻辑」和「仓位方法」当两个维度扫 |
| **没有风控层** | 单票集中度、回撤熔断无处可放，塞进策略就和信号逻辑纠缠在一起 |
| **标的池在 `cmd/backtest` 里手挑** —— 取前 N 个有因子的个股 | 不可复现（换个 N 就是另一次实验），也无法写进配置 |
| **装配全靠 Go 代码** —— `eng.New(Deps{...}, mk(), Config{...})` | 换任何一件事都要重新编译 |
| **绩效只有峰值回撤**，还是在 `cmd/backtest` 的 main 里现算的 | 没有 Metrics 模块 |

## 2. registry：泛型容器，各域自持

ROADMAP 写的是 `init()` 自注册。直接照做会踩一个 Go 的坑：

```
trading 包里的 ConfigFee 要注册 → trading import registry
registry 的 map 值类型是 trading.Fee  → registry import trading
                                          ↑ 循环
```

解法是让 registry **不认识任何领域类型** —— 用泛型：

```go
package registry

// Factory 从配置片段构造一个实现。参数是该模块自己那段 JSON，
// 由各实现自行解析 —— registry 不该知道 bps 是什么。
type Factory[T any] func(json.RawMessage) (T, error)

type Registry[T any] struct {
    mu sync.RWMutex
    m  map[string]entry[T]
}

func New[T any](kind string) *Registry[T]
func (r *Registry[T]) Register(name string, specs []ParamSpec, f Factory[T])
func (r *Registry[T]) Build(name string, cfg json.RawMessage) (T, error)
func (r *Registry[T]) Names() []string
func (r *Registry[T]) Specs(name string) []ParamSpec   // 喂 Web 表单
```

各域在自己包里持有 registry 变量并自注册，没有循环也不需要空导入：

```go
// internal/trading/fee.go
var Fees = registry.New[Fee]("fee")

func init() {
    Fees.Register("config", configFeeSpecs, func(raw json.RawMessage) (Fee, error) { ... })
}
```

`ParamSpec` 复用 v0.2 已有的那个（`engine.ParamSpec`），但它得从 `engine` 挪到
一个更底层的包 —— 否则 `trading` 要 import `engine`，又是循环。
建议挪到 `internal/spec`，`engine` 与各域都从那儿取。

> **这不是「热插拔」。** ROADMAP 已经定过：Go 的 `plugin` 在 Windows 上不可用，
> 本项目走「接口 + registry + 配置装配」，新增实现需重新编译（约 2 秒）。
> registry 买到的是**运行时按字符串选实现**，不是运行时加载新代码。

## 3. 配置文件：一份 JSON 描述一次运行

```json
{
  "name": "macd 十只等权",
  "data": {
    "root": "../data", "market": "ashare", "freq": "1d",
    "from": 20200101, "to": 0,
    "universe": { "type": "stock", "board": ["main", "chinext"], "limit": 300 }
  },
  "market":    { "impl": "ashare" },
  "fee":       { "impl": "config", "params": { "path": "configs/fee/ashare_default.json" } },
  "slippage":  { "impl": "fixed_bps", "params": { "bps": 5 } },
  "sizer":     { "impl": "equal_weight", "params": { "slots": 10 } },
  "risk": [
    { "impl": "max_position_pct", "params": { "pct": 15 } },
    { "impl": "drawdown_halt",    "params": { "pct": 30 } }
  ],
  "broker":    { "volume_cap_ppm": 100000, "allow_partial_fill": true },
  "portfolio": { "initial_cash_cents": 100000000, "dividend_tax_ppm": 0 },
  "engine":    { "indicator_adj": "hfq", "imply_split_from_factor": true },
  "strategy":  { "impl": "macd_cross", "params": { "short": 12, "long": 26, "signal": 9 } },
  "metrics":   { "benchmark": "510300", "risk_free_ppm": 0 },
  "recorder":  { "level": "summary" }
}
```

几个刻意的形状：

**`risk` 是数组，不是对象。** 风控是若干条独立规则叠加，不是单选。
顺序执行，任一条拒绝即拒绝，通过的订单可被前一条缩量后传给下一条。

**`universe` 只有静态条件。** 类型、板块、交易所可以进；
`is_st` 不能 —— 它是**逐 bar 时变**的（ETL.md 6.6），当天是不是 ST 只有当天才知道。
把时变条件写成 universe 过滤器等于用了未来信息。
需要「不碰 ST」的策略应当在 `OnBar` 里看 `bar.IsST`，或写成一条 Risk 规则。

**没有 `point_in_time` 开关。** 它不是选项：bar 表天生是 PIT 的（上市前无行、退市后无行），
`ctx.Universe()` 每步返回的就是当天有 bar 的标的。C3 是结构保证，不是配置项。

**`limit` 需要一个确定的取法。** 现在 `cmd/backtest` 取的是「前 N 个有因子的个股」，
依赖 map 遍历顺序之外的偶然性。配置化之后必须固定为**按 instrument_id 升序取前 N**，
否则同一份配置两次运行标的池就不同，C5 直接失守。

> 装配路径上任何遍历 map 的地方都要先排序。Go 的 map 遍历顺序是随机的，
> 这是 C5 最容易被忽略的入口 —— 它不会报错，只会让两次运行悄悄不同。

## 4. Sizer：策略只出信号，仓位交给可换的模块

### 4.1 为什么必须改 Strategy 的签名

现在 `OnBar` 返回 `[]trading.Order`，`Qty` 由策略算好。只要 `Qty` 还由策略给，
Sizer 就只能是个摆设 —— 它没有可以决定的东西。

而海选真正想扫的是两个**正交**维度：

```
信号逻辑：MACD / 双均线 / KDJ / ...
仓位方法：等权 / 固定金额 / 按信心加权 / 波动率目标 / ...
```

写在一起，这两个维度就绑死了：想试「MACD + 固定金额」得再写一个策略。

所以本刀提议改成：

```go
type Strategy interface {
    Name() string
    Specs() []spec.ParamSpec
    Init(InitContext) error
    OnBar(StepContext) ([]Signal, error)   // ← 从 []trading.Order 改成 []Signal
}
```

### 4.2 Signal

```go
type SignalKind int8

const (
    SignalEnter  SignalKind = iota // 建仓 / 加仓，数量由 Sizer 定
    SignalExit                     // 清仓，数量明确 = 当前可卖
    SignalTarget                   // 调仓到目标权重
)

type Signal struct {
    Instrument mktdata.InstrumentID
    Kind       SignalKind
    Side       trading.Side
    Strength   float64 // [0,1] 策略对该信号的信心；等权 Sizer 忽略它
    Weight     float64 // Kind==SignalTarget 时的目标权重
    Tag        string  // 归因用，会一路带到 Fill
    LimitPrice int64   // 0 表示市价

    // Qty 非零表示策略坚持自己定量，绕过 Sizer。
    //
    // 这条口子会被 Recorder 记成 sizer_overridden。用得多了本身就是信号：
    // 说明 Sizer 的抽象没选对，该改抽象而不是继续绕。
    Qty int64
}
```

`SignalExit` 单独成一类，而不是「Side=Sell 的 Enter」——
清仓的数量是确定的（当前可卖全部），不需要也不应该交给 Sizer 算。

### 4.3 Sizer 接口

```go
type Sizer interface {
    Name() string
    // Size 把整批信号转成订单。
    //
    // **拿整批而不是逐条**：等权分配、按信心归一化、总仓位上限
    // 都需要看到本步的全部信号。逐条调用做不了这些。
    Size(sigs []Signal, ctx SizeContext) []trading.Order
}

type SizeContext interface {
    Time() mktdata.TimePoint
    Portfolio() *trading.Portfolio
    EquityCents() int64
    Available(id mktdata.InstrumentID) int64
    Bar(id mktdata.InstrumentID) (mktdata.Bar, bool)
    Instrument(id mktdata.InstrumentID) *mktdata.Instrument
    Pending() []trading.PendingOrder
}
```

内置实现：

| impl | 参数 | 行为 |
|---|---|---|
| `equal_weight` | `slots` | 权益 / slots 为一份，每个 Enter 信号一份 |
| `fixed_cash` | `cents` | 每笔固定金额 |
| `fixed_qty` | `qty` | 每笔固定股数（配合最小申报单位取整） |
| `pct_equity` | `pct` | 每笔占当前权益的 x% |
| `strength_weighted` | `total_pct` | 按 `Strength` 归一化分配总仓位 |

所有 Sizer 出的数量都要过 `Market.NormalizeQty` —— 100 股整数倍、零股卖出等
规则属于 Market 而非 Sizer，不该在五个实现里各写一遍。

### 4.4 三个样例策略怎么改

改动很小，因为它们本来就是「等权 N 只」：

```go
// 改前
qty := s.slotCents * 10 / bar.Close
if qty < 100 { continue }
orders = append(orders, trading.Order{Instrument: id, Side: SideBuy, Qty: qty, Tag: "macd_golden"})

// 改后
sigs = append(sigs, eng.Signal{Instrument: id, Kind: eng.SignalEnter,
    Side: trading.SideBuy, Tag: "macd_golden"})
```

`cash_cents` / `max_hold` 两个参数从策略挪到 Sizer（`equal_weight.slots`）。
策略只剩自己的信号参数，这本身就说明分层对了。

**回归验收**：配 `equal_weight{slots:10, base:initial}` 后与 v0.2 对照。

`buy_and_hold` **逐分位一致**（它没有卖出，故不受下述计数差异影响，
是这次重构的对照组）：

| | v0.2 | v0.3 |
|---|---|---|
| 成交 / 拒单 | 10 / 0 | 10 / 0 |
| 权益 | 1,311,683.96（+31.17%） | 1,311,683.96（+31.17%） |
| 现金 | 206,647.96 | 206,647.96 |
| 费用 | 258.24（佣金 248.26 + 过户费 9.98） | 258.24（同） |
| 峰值回撤 | 26.80% | 26.80% |

对照组逐分位一致，说明整条新链路（Signal → Sizer → Risk → 定价入队）
**没有改变行为**。

`ma_cross` / `macd_cross` 有差异，原因是**一处有意的行为修正**：

> v0.2 的样例策略把「已持有」与「在途」**分别计数不去重**
> （`len(held) + len(inFlight)`）。于是一只标的挂着卖单时会占掉两个槽 ——
> 卖单意外收紧了买入上限。v0.3 的 `equal_weight` 按去重语义计数。

差异**已经过归因验证**：把 `equal_weight` 临时改回 v0.2 的计数语义后重跑，
两个策略的成交笔数、拒单笔数、三项费用明细、峰值回撤**全部逐分位复现 v0.2**
（`ma_cross` 1,242 / 7,668 / 80,716.24 / 59.37%；
`macd_cross` 1,686 / 552 / 108,997.69 / 24.89%）。
即差异**只**来自这一处修正，不含任何其他行为变化。

修正后的结果：

| 策略 | 成交 | 拒单 | 收益 | 峰值回撤 | 费用/初始 |
|---|---|---|---|---|---|
| `ma_cross` | 1,221 | 8,720 | −45.63% | 57.12% | 8.04% |
| `macd_cross` | 1,777 | 696 | +16.88% | 23.89% | 11.51% |

去重语义由 `TestEqualWeightSlotsCountDedup` 钉住。

> 上表是**滑点修正之前**的基线。修正后（滑点从单价挪到成交额）
> 三个策略的数字都变了，新基线见下节末尾 —— 之所以保留旧表，
> 是因为「重构无行为变化」这条结论是在旧基线上验证的，
> 换成新数字就看不出当时验证了什么。

## 5. Risk：链式，下单前拦截

```go
type Risk interface {
    Name() string
    // Check 在订单入队前拦截。
    // 返回调整后的订单（可缩量）；ok=false 表示拒绝。
    Check(o trading.Order, ctx RiskContext) (trading.Order, trading.Rejection, bool)
}
```

内置实现：

| impl | 参数 | 行为 |
|---|---|---|
| `max_position_pct` | `pct` | 单票市值占权益上限，超出则缩量 |
| `max_positions` | `n` | 最多同时持有 N 只，满了拒绝新开仓 |
| `drawdown_halt` | `pct` | 从峰值回撤超过 X% 后只许卖不许买 |
| `cash_reserve` | `pct` | 保留 X% 现金不参与买入 |

### 5.1 拒单原因：加字段而不是撑大枚举

现在 `RejectReason` 有 13 个取值。风控每加一条规则就加一个枚举值，
枚举会变成规则清单的镜像。改为：

```go
const RejectRisk RejectReason = ... // 「风控拦截」一个枚举值

type Rejection struct {
    Order
    At     mktdata.TimePoint
    Reason RejectReason
    Rule   string // ← 新增：风控规则名，如 "max_position_pct"
    Detail string
}
```

`Reason` 回答「哪一类障碍」，`Rule` 回答「哪一条规则」。
v0.4 的单步调试两个都要展示。

### 5.2 `drawdown_halt` 引入新的引擎状态

回撤熔断要知道**历史峰值权益**，那是引擎必须维护的跨步状态：

```go
type Engine struct {
    // ...
    peakEquityCents int64
}
```

**它必须进快照。** 不进的话，从快照恢复的引擎峰值是 0，
熔断规则立刻失效，账目随之偏离 —— 这与 v0.2 端到端暴露过的策略状态缺陷
是同一类问题（`StatefulStrategy` 那次）。

## 6. Recorder：三档

```go
type RecordLevel int8

const (
    RecordNone    RecordLevel = iota // 只累计最终权益。海选内层用，省内存
    RecordSummary                    // 每步：权益 / 现金 / 持仓数 / 成交数 / 拒单数
    RecordFull                       // 上面全部 + 每步的信号 / 订单 / 成交 / 拒单明细
)

type Recorder interface {
    Name() string
    Level() RecordLevel
    OnStep(StepRecord)
    Result() *RunResult
}
```

`RecordSummary` 就是现在 `cmd/backtest -equity-out` 输出的那条净值序列，
只是从 main 里挪进了模块。

**体积**：v0.2 的 demo（285 标的 × 1,614 步，成交 1,686、拒单 552）Full 级只有几 MB。
但全市场 7,175 标的 × 5,260 步、策略每步产生大量信号时，Full 可能上 GB。
本刀只做**内存** Recorder，并在超过阈值时报警而不是静默吃内存。
流式落盘留给 v0.4 —— 落盘格式该由 API 的消费方式决定，现在定会白定。

## 7. Metrics：九个指标，三个坑

| 指标 | 说明 |
|---|---|
| 总收益 / 年化收益 | |
| 年化波动 | 日收益标准差 × √(年交易日) |
| 夏普 | (年化收益 − 无风险利率) / 年化波动 |
| 索提诺 | 分母换成下行波动 |
| 最大回撤 | 含回撤区间与恢复天数 |
| 卡玛 | 年化收益 / 最大回撤 |
| 胜率 / 盈亏比 | 需要「一轮交易」的定义，见坑 2 |
| 换手率 | 单边成交额 / 平均权益 / 年数 |
| 基准超额 / Beta / Alpha / 信息比率 | 见坑 3 |

### 坑 1：年化交易日不是 252 ⚠️

252 是美股惯例。**A 股实测（本项目日历，2005–2025 完整年份）**：

```
均值 242.90    中位 243    范围 238–246
2005:242 2006:241 2007:242 2008:246 2009:244 2010:242 2011:244
2012:243 2013:238 2014:245 2015:244 2016:244 2017:244 2018:243
2019:244 2020:243 2021:243 2022:242 2023:242 2024:242 2025:243
```

用 252 会让年化收益与年化波动**同时**偏高（指数偏大 3.7%、波动偏大 1.8%），
夏普的分子分母各偏一点、不会互相抵消干净。

正确做法：**不写常数，从 `Calendar` 数**。日历已经在内存里，
`cal.TradingDays(market, from, to) / 年数` 就是这段样本真实的年交易日。
远期接美股、加密货币时这条自动正确（加密 365，美股 ~252）——
写死常数则每接一个市场就要改一次。

### 坑 2：胜率与盈亏比需要「一轮交易」的定义

成交是逐笔的，但「赢了几次」问的是**开仓到清仓的一轮**。
部分成交、加仓、送股都会打断朴素配对。

方案：按标的维护 FIFO 队列，卖出时逐层配对，产出：

```go
type RoundTrip struct {
    Instrument   mktdata.InstrumentID
    OpenDay      int32
    CloseDay     int32
    Qty          int64
    CostCents    int64 // 含买入费用
    ProceedCents int64 // 扣卖出费用
    PnLCents     int64
    HoldDays     int
}
```

几处必须想清楚的：

- **送股 / 转增**按 0 成本入队 —— 成本已经付在原有份额上，
  再计一次成本会让盈亏比虚高
- **现金分红**不产生 RoundTrip，计入持仓期收益（否则「只分红没卖过」的持仓永远不计胜负）
- **回测结束时仍持有的仓位**不计入胜率（未平仓不是「赢」也不是「输」），
  但要在报告里给出未平仓数量与浮盈 —— 藏起来会让胜率失真

### 坑 3：基准只能用 ETF 代理，且最早到 2012 ⚠️

**我们的数据里没有指数。** `instruments` 只有个股和 ETF（C10 纯技术面，
ETL 也没拉指数行情）。实查可用的代理：

| 代理 | 名称 | bar 数 | 覆盖区间 |
|---|---|---|---|
| `510300` | 华泰柏瑞沪深300ETF | 3,466 | 2012-05-28 ~ 2026-08-28 |
| `159919` | 嘉实沪深300ETF | 3,464 | 2012-05-28 ~ 2026-08-28 |
| `510500` | 南方中证500ETF | 3,269 | 2013-03-15 ~ 2026-08-28 |
| `588000` | 华夏中证科创50ETF | 1,405 | 2020-11-16 ~ 2026-08-28 |

于是：**回测 2005–2012 段没有沪深 300 基准。**

处理方式：基准覆盖不到的区间**不计超额**，并在报告里明示
「基准覆盖 3,466 / 5,260 步」。当成 0 收益补齐是最糟的选择 ——
它会凭空造出一段超额收益。

> 另一面：ETF 代理其实比指数**更诚实**。指数不可投资，
> 而 ETF 的净值已经含了管理费与跟踪误差。跑赢 510300 才是真的跑赢了
> 「买指数」这个替代方案。

### 无风险利率

默认 **0**，可由 `metrics.risk_free_ppm` 配置为常数。
不写死 3%：利率是时变的，写死会让不同年份的夏普失去可比性。
报告里必须显式印出这次用了多少 —— 夏普是最容易被悄悄美化的数字。

时间序列形态的无风险利率留待需要时再加（数据源另说，属 C9 的新数据源问题）。

## 8. 结果指纹（C5）

两个指纹，缺一不可：

```
输入指纹 = sha256( 规范化配置 ‖ 数据指纹 ‖ 引擎版本 )
输出指纹 = sha256( 逐笔成交的规范化序列 )
```

**同输入指纹必须给出同输出指纹。** 这是 C5 可验证的形式。

### 8.1 规范化配置

键排序、数值统一为整数或定长小数、去掉**不影响结果**的字段。
哪些字段进指纹是设计决定，不是实现细节：

| 进指纹 | 不进 |
|---|---|
| `slippage.bps`、`fee.*`、`sizer.*`、`risk[*]`、`strategy.params` | `name`（人给的标签） |
| `data.from/to/universe`、`engine.*`、`portfolio.*` | `recorder.level`（只影响记多少，不影响算什么） |
| | 输出路径、日志级别 |

### 8.2 数据指纹

不能用文件 mtime —— 复制一次就变，跨机器必然不同。
用**内容哈希**：载入时顺带对定点列跑 xxhash64（数据本来就要过一遍内存），
结果缓存到 `data/.fingerprint.json`，缓存键是 `(相对路径, 大小, mtime)`。
缓存失效就重算，重算结果与原来一致 —— 因为哈希的是内容。

### 8.3 引擎版本

编译期用 `-ldflags -X main.gitCommit=$(git rev-parse HEAD)` 注入。

`go run` 下拿不到，退化为 `"dev"`，并在指纹里**标记为不可复现**。
这点必须诚实：dev 构建之间源码可能不同，指纹相同不代表结果可复现。
报告里印 `fingerprint: ...  (dev build, 不保证跨构建可复现)`。

## 9. 分刀与验收

延续 v0.2 的节奏，分三刀。

### 第一刀：registry + 配置装配 + Sizer + Risk ✅（2026-08-29 完成）

- `internal/spec`：`ParamSpec` 从 `engine` 挪出来（打破循环依赖）
- `internal/registry`：泛型容器
- 各域自注册内置实现
- `internal/config`：JSON 结构、校验、装配成 `Deps` + `Config`
- `Sizer` / `Risk` 接口 + 各四五个内置实现
- `Strategy.OnBar` 改签名，三个样例策略跟着改
- `cmd/backtest` 改成 `-config path.json`

**验收**：
1. 换 JSON 即可改变引擎行为（换 sizer / 加 risk 规则，不重新编译）
2. **回归**：三个样例策略在等价配置下与 v0.2 的数字逐分位一致（表见 §4.4）
3. 配置校验能报出人话错误：未知 impl 名、参数越界、`risk` 里重复规则

### 第二刀：Recorder + Metrics

- Recorder 三档
- RoundTrip FIFO 配对
- 九个指标 + 基准（ETF 代理，明示覆盖区间）
- `cmd/backtest` 输出完整报告

**验收**：手工核算一个小样本（约 20 个交易日、3 笔来回）的
年化收益 / 夏普 / 最大回撤 / 胜率 / 盈亏比，与实现**逐位一致**。
年交易日取自日历而非 252，用一段跨年样本验证。

### 第三刀：结果指纹

- 数据指纹（载入时算 + 缓存）
- 配置规范化
- 输出指纹

**验收**：
1. 同配置两次运行，两个指纹都相同
2. 只改 `slippage.bps` 一个数，输入指纹改变、输出指纹也改变
3. 只改 `recorder.level`，输入指纹**不变**、输出指纹不变
4. 把 `data/` 复制到另一个路径（mtime 全变），数据指纹不变

## 9.5 实施中发现的问题

### 零预算把卖单一起挡掉了（已修）

`EqualWeight.Size` 第一版在预算为零时直接 `return nil`，
连**清仓信号**一起丢了 —— 权益归零的账户将永远无法卖出。
由 `TestSizerExitUsesAvailable` 发现。

卖出从来不需要预算。现在预算检查只挡建仓分支（`PctEquity` 同）。

### 滑点施加的位置错了（已修）

原实现把滑点加在**单价**上：`ref * bps / 10_000`，整数除法向下取整。
第一反应是取整方向的问题（抹掉不足一厘的摩擦，偏向使用者），
于是照「成本模型的取整不得偏向使用者」改成向上取整。**再测更糟。**

真正的原因是**施加的位置**：价格的单位是厘，而 5 bp 在 20 元以下的标的上
不足一厘，任何取整方向都错得离谱：

| 价格 | 理论滑点 | 向下取整 | 四舍五入 | 向上取整 |
|---|---|---|---|---|
| 0.594 元 | 0.297 厘 | 0（−100%） | 0（−100%） | 1（+237%） |
| 1.071 元 | 0.535 厘 | 0（−100%） | 1（+87%） | 1（+87%） |
| 3.132 元 | 1.566 厘 | 1（−36%） | 2（+28%） | 2（+28%） |
| 9.000 元 | 4.500 厘 | 4（−11%） | 5（+11%） | 5（+11%） |
| 20 元以上 | ≥10 厘 | 精确 | 精确 | 精确 |

整体实测（`buy_and_hold`，成交额约 99.3 万元，配置 5.00 bp）：

| 实现 | 实际生效 | 偏差 |
|---|---|---|
| 向下取整（原） | 3.67 bp | −26.5% |
| 四舍五入 | 5.34 bp | +6.7% |
| 向上取整 | 5.63 bp | +12.7% |
| **按成交额计成本** | **5.00 bp** | **精确** |

改为**按成交总额计成本**（分），粒度细 1000 倍、基数从单价变成总额，
误差降到可忽略。接口随之从 `Apply(...) 价格` 改成 `CostCents(...) 成本`。

三个连带的好处：

1. **滑点在账本里可见了。** 以前它藏在成交价里，报告只看得到佣金印花税。
   实测 `macd_cross` 的滑点是 88,857.96 元（占初始 8.89%），
   与全部费用（11.56%）同一量级 —— 这么大一笔以前是隐形的。
2. **成交价重新变回市场上真实存在的价格**，可以直接与行情数据核对。
3. 不再需要「把滑点推出去的价格夹回涨跌停区间」那段特判 ——
   参考价本来就已经校验过不在涨跌停上。

滑点**不计入 `FeeCents`**：佣金印花税过户费是付给第三方的真金白银，
要能与券商对账单对得上；滑点是执行质量的损耗。两者分开记，
`Portfolio.SlippageCents` 单列。

> 一个记录在案的教训：先前算出的 −7.9% / +41% 是**错的**，
> 分母用了 `初始资金 − 期末现金`，漏掉了 6.6 年里收到的约 20 万元现金分红。
> 正确的分母要用成交额（可由佣金 ÷ 250 ppm 反推）。
> 结论方向没变，但幅度小报了 —— 原实现的偏差实际是 −26.5% 而非 −7.9%。
>
> 这类错误的共同形态：**用差额倒推分母时，忘了中间还有别的现金流进出。**
> 与 ETL.md 6.10「绝对误差与相对误差」是同一类 —— 分母取错，
> 结论的量级就全错，而方向的正确会掩盖它。

#### 修正后的基线（2026-08-29）

| 策略 | 成交 | 拒单 | 收益 | 峰值回撤 | 费用 | 滑点 |
|---|---|---|---|---|---|---|
| `buy_and_hold` | 10 | 0 | +31.15% | 26.81% | 258.11（0.03%） | 496.24（0.05%） |
| `ma_cross` | 1,438 | 5,722 | −49.14% | 49.40% | 93,387.58（9.34%） | 71,161.84（7.12%） |
| `macd_cross` | 1,787 | 738 | +10.76% | 27.34% | 115,644.39（11.56%） | 88,857.96（8.89%） |

**滑点与费用同一量级。** `macd_cross` 的摩擦合计 20.45 万元 —— 占初始资金
20.45%，其中近一半以前是隐形的。这一条本身就够说明为什么值得改。

## 10. 评审决议

以下五项已于 2026-08-29 确认，**均按推荐执行**。原选项保留在下方备查。

| | 决议 |
|---|---|
| ① | Strategy 改出 `Signal`，三个样例策略跟着改 |
| ② | 无风险利率：常数配置，默认 0，报告显式印出 |
| ③ | 基准覆盖不足的区间不计超额，报告明示覆盖比例 |
| ④ | Full 级 Recorder 只做内存 + 超阈值报警，落盘留 v0.4 |
| ⑤ | 引擎版本用 `-ldflags` 注入 git commit，`go run` 退化 dev 并标记不可复现 |

### 原议题

### ① Strategy 接口是否改成出 `Signal`？

这是**破坏性变更**，三个样例策略要改。

- **改**：Sizer 才有意义；海选能把「信号逻辑」与「仓位方法」当两个独立维度扫；
  策略变薄，只剩自己的信号参数
- **不改**：策略写起来直接一点；但 Sizer 就是个摆设

**我推荐改。** 不改的话这一刀的核心目标（模块可替换）就只完成了一半。

### ② 无风险利率

- **A. 常数配置，默认 0**（推荐）：报告里显式印出用了多少
- B. 写死一个值（如 3%）
- C. 支持时间序列

推荐 A：写死会让不同年份的夏普失去可比性；时间序列要新数据源，属另一件事。

### ③ 基准怎么处理覆盖不足

- **A. 不计超额，报告明示覆盖比例**（推荐）
- B. 覆盖不到的区间按 0 收益补齐
- C. 找更早的代理（更早的宽基 ETF 也在 2012 之后）

推荐 A：B 会凭空造出超额收益。

### ④ Full 级 Recorder 是否落盘

- **A. 只做内存，超阈值报警**（推荐）
- B. 现在就做流式落盘

推荐 A：落盘格式该由 v0.4 的 API 消费方式决定。

### ⑤ 引擎版本

- **A. `-ldflags` 注入 git commit，`go run` 退化为 dev 并标记不可复现**（推荐）
- B. 手写一个版本常量

推荐 A：B 会忘记改，然后指纹开始撒谎。
