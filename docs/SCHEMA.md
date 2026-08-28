# 数据 Schema 定义

> **状态：待评审**（v0.1 开工前需定稿）
> 最后更新：2026-08-28
> 依据：[ROADMAP.md](ROADMAP.md) 的「数据存储设计」章节与 [v0.0 探针结论](probe/REPORT-v0.0.md)

本文档是 **Python ETL 与 Go 引擎唯一的契约**。两侧实现必须严格一致，
任何字段变更都是双边改动，因此本文档定稿后的修改需同步升级 `schema_version`。

---

## 0. 通用约定

### 0.1 类型选择原则

沿用 ROADMAP 的**保守子集原则**：只使用各语言 Parquet 实现都成熟支持的类型。

- 整数一律用**有符号**类型（`INT8` / `INT32` / `INT64`）。
  无符号类型在部分实现中支持不一致，而 `INT32` 的 21 亿容量远超本项目需求。
- 不使用 `DECIMAL` 逻辑类型 —— 跨语言支持参差，改用**定点整数 + scale**。
- 不使用 `DATE` / `TIMESTAMP` 逻辑类型 —— 时区语义在各实现间存在分歧，
  改用显式的 `INT32 (YYYYMMDD)` 与 `INT64 (UTC 毫秒)`。
- 布尔语义用 `INT8`（0/1）而非 `BOOLEAN`，便于与多值枚举统一处理。

### 0.2 定点整数约定

| 概念 | 表示 | scale 来源 |
|---|---|---|
| 价格 | `INT64` | `instruments.price_scale`（A 股 = 1000，即最小单位 0.001 元） |
| 数量 | `INT64` | `instruments.qty_scale`（A 股 = 1，即最小单位 1 股/份） |
| 金额 | `INT64` | 固定为计价币种最小单位（A 股 = 分，即 1e-2 元） |
| 比率 | `INT32` | 固定 1e6（如换手率 0.256% 存为 `256000`） |

> 价格用定点整数**不是为了体积**（实测仅比 float64 省 6%），
> 而是为了约束 C5 可复现性：浮点累加顺序不同会导致结果不同，
> 并发海选下无法保证逐笔一致。详见 ROADMAP「关键类型决策」。

### 0.3 时间约定

| 字段 | 类型 | 定义 |
|---|---|---|
| `ts` | `INT64` | **UTC 毫秒**，bar 的**结束时刻**。A 股日线 = 该交易日 15:00 CST = 07:00 UTC |
| `trading_day` | `INT32` | `YYYYMMDD`，业务语义的交易日 |
| 其余日期字段 | `INT32` | `YYYYMMDD` |

两者并存是刻意冗余：`ts` 用于跨市场对齐与远期 24×7 市场，
`trading_day` 用于业务语义。delta 编码后代价近乎为零。

### 0.4 枚举定义

枚举值的含义以本文档为准，Python 与 Go 两侧各自实现为编译期常量。
**0 一律保留为「未知/无效」**，便于识别未初始化数据。

```
market      1=ashare                     远期：2=us  3=hk  4=futures  5=crypto
exchange    1=SSE(上交所)  2=SZSE(深交所)  3=BSE(北交所)
type        1=stock(个股)  2=etf
board       1=主板  2=创业板  3=科创板  4=北交所
quote_ccy   1=CNY                        远期：2=USD  3=USDT
status      1=在市  2=已退市
```

### 0.5 排序保证

`bar` 表的每个文件内部必须按 **`(instrument_id, ts)` 严格升序**。

这不是可选优化：`DELTA_BINARY_PACKED` 编码的压缩效果依赖于此，
引擎的顺序扫描也假定此前提。ETL 必须在写出前排序，质检需校验。

---

## 1. `bar` —— 行情时序

**路径**：`data/bar/market={market}/freq={freq}/year={YYYY}/part-*.parquet`
**主键**：`(instrument_id, ts)`
**规模**：A 股日线全量约 2000 万行 / 约 350 MB
**编码**：定点整数 + `DELTA_BINARY_PACKED` + zstd(level 3)

### 1.1 核心列 —— 全市场严格统一

引擎核心循环只读这 9 列。**其名称与语义不可变更**，新增市场必须能填满它们。

| 字段 | Parquet | Go | 可空 | 说明 |
|---|---|---|---|---|
| `instrument_id` | INT32 | `int32` | 否 | 引擎内部 ID，见 `instruments` |
| `ts` | INT64 | `int64` | 否 | UTC 毫秒，bar 结束时刻 |
| `trading_day` | INT32 | `int32` | 否 | YYYYMMDD |
| `open` | INT64 | `int64` | 否 | 定点价格 |
| `high` | INT64 | `int64` | 否 | 定点价格 |
| `low` | INT64 | `int64` | 否 | 定点价格 |
| `close` | INT64 | `int64` | 否 | 定点价格 |
| `volume` | INT64 | `int64` | 否 | 定点数量 |
| `amount` | INT64 | `int64` | 否 | 成交额，单位为分 |

> **均为不可空**。停牌日的处理不是「置空」而是写入零成交行并由
> `tradestatus` 标记 —— 见 1.3。

### 1.2 A 股扩展列

直接附于同表。Go 侧用只含核心列的 `CoreBar` struct 亦可读取
（已由 `engine/cmd/subsetread` 实测验证）。

| 字段 | Parquet | Go | 可空 | 说明 |
|---|---|---|---|---|
| `preclose` | INT64 | `int64` | 否 | **除权调整后的前收盘价**，涨跌停基准（C8.1） |
| `turn` | INT32 | `int32` | 否 | 换手率，百分数 ×1e6 |
| `tradestatus` | INT8 | `int8` | 否 | 1=正常交易 0=停牌 |
| `is_st` | INT8 | `int8` | 否 | 1=ST 0=非 ST。**影响涨跌停幅度**（ST 为 5%） |

远期其他市场的扩展列（不在 v0.1 实现）：
`us` → `pre_market_volume`；`futures` → `open_interest` / `settlement`；
`crypto` → `funding_rate`。

### 1.3 停牌行的语义

BaoStock 在停牌日**照常返回行**，因此 `bar` 表与交易日历对齐，
且天然具备 point-in-time 语义：

```
某日标的池 = SELECT DISTINCT instrument_id FROM bar WHERE trading_day = d
```

停牌行的取值约定：

- `tradestatus = 0`
- `volume = 0`、`amount = 0`
- `open/high/low/close` = 停牌前最后收盘价（保持价格序列连续，避免指标计算出现空洞）
- `preclose` 照常给出

> ⚠️ **待确认**：BaoStock 停牌行的 OHLC 实际取值需在 v0.1 实测核对，
> 若其返回 0 或空值，ETL 需按上述约定回填。

### 1.4 ETF 的字段缺口

新浪 `fund_etf_hist_sina` **不提供 `preclose`**（仅少数标的带 `prevclose`），
也不提供 `turn` / `tradestatus` / `is_st`。ETF 行的处理约定：

| 字段 | ETF 取值 |
|---|---|
| `preclose` | 由 `corporate_action` 推算；无除权则取前一交易日 `close` |
| `turn` | 0（新浪不提供流通份额，无法计算） |
| `tradestatus` | 1（新浪无停牌标记，缺行即视为停牌/未上市） |
| `is_st` | 0（ETF 无 ST 概念） |

> ⚠️ 这是**已知的数据质量降级**，必须在质检报告中单独计数，
> 且 Web 端在回测 ETF 时应提示 `turn` 不可用。

---

## 2. `instruments` —— 标的静态属性

**路径**：`data/meta/instruments.parquet`（+ `instruments.csv` 镜像）
**主键**：`instrument_id`
**唯一约束**：`(market, symbol)`
**规模**：约 7200 行（A 股个股 5549 + ETF 1651）

| 字段 | Parquet | Go | 可空 | 说明 |
|---|---|---|---|---|
| `instrument_id` | INT32 | `int32` | 否 | 主键，从 1 开始 |
| `market` | INT8 | `int8` | 否 | 枚举，见 0.4 |
| `symbol` | STRING | `string` | 否 | 市场内唯一。A 股为 6 位数字，**不含交易所前缀** |
| `exchange` | INT8 | `int8` | 否 | 枚举 |
| `name` | STRING | `string` | 否 | 当前/最终名称。**不做时变**，见下方说明 |
| `type` | INT8 | `int8` | 否 | 1=个股 2=ETF |
| `board` | INT8 | `int8` | 否 | 决定涨跌停幅度，Market 模块使用 |
| `price_scale` | INT32 | `int32` | 否 | A 股 = 1000 |
| `qty_scale` | INT32 | `int32` | 否 | A 股 = 1 |
| `quote_ccy` | INT8 | `int8` | 否 | A 股 = 1 (CNY) |
| `min_order_qty` | INT32 | `int32` | 否 | 最小申报数量 |
| `qty_step` | INT32 | `int32` | 否 | 申报数量递增单位 |
| `list_date` | INT32 | `int32` | 否 | 上市日 YYYYMMDD |
| `delist_date` | INT32 | `int32` | **是** | 退市日；在市为 null |
| `status` | INT8 | `int8` | 否 | 1=在市 2=已退市 |
| `attrs` | STRING | `string` | **是** | 市场特定属性，JSON |

### 2.1 `min_order_qty` / `qty_step` 取值

| 板块 | `min_order_qty` | `qty_step` |
|---|---|---|
| 主板 / 创业板（个股） | 100 | 100 |
| 科创板（个股） | 200 | 1 |
| ETF | 100 | 100 |
| 北交所 | 100 | 1 |

> ⚠️ **待确认**：北交所的申报单位规则需在 v0.1 核实来源后确认。
> 当前范围内北交所标的极少，可先按上表实现并标记。
>
> 卖出零股（不足最小单位的余股）必须一次性全部卖出 —— 这是 Broker 的规则，
> 不在本表体现。

### 2.2 关于 `name` 不做时变

严格说名称是时变的（`乐视网` → `*ST视退`），标准做法是
`(instrument_id, name, valid_from, valid_to)` 区间表。本项目**刻意不做**：

- 策略不消费名称，它只影响展示
- BaoStock 不提供历史名称，做区间表需引入新数据源

代价：回看 2019 年的回测报告时，显示的是标的**当前/最终**名称。
若将来判定此代价不可接受，再补 `instrument_name` 区间表，不影响其他表。

### 2.3 `attrs` JSON

v0.1 的 A 股标的此列为 null。远期市场用于承载：

```json
// futures
{"contract_multiplier": 300, "expiry_date": 20240315, "underlying": "IF"}
// crypto
{"base_ccy": "BTC", "contract_type": "perpetual", "venue": "binance"}
```

instruments 仅数万行，全量载入内存后解析 JSON 无性能顾虑，
换来的是**新增市场不改表结构**。

### 2.4 `instrument_id` 分配规则

**这条规则关系到增量更新的正确性，必须严格遵守：**

1. 按 `(market, symbol)` **首次出现**时分配，单调递增，从 1 开始
2. **永不复用**，即使标的退市
3. ETL 每次运行必须先读取已有 `instruments.parquet` 取得现有映射，
   仅为新标的分配新 ID
4. 若 `instruments.parquet` 丢失，则所有 ID 会重新分配，
   **既有的 `bar` 分区文件随之全部失效**，必须整体重建

> 因此 `instruments.parquet` 是整个数据集里最关键的单个文件。
> 质检需校验其单调性与唯一性；建议 ETL 每次运行前自动备份该文件。

---

## 3. `calendar` —— 交易日历

**路径**：`data/meta/calendar.parquet`（+ CSV 镜像）
**主键**：`(market, date)`
**规模**：约 9000 行/市场

| 字段 | Parquet | Go | 可空 | 说明 |
|---|---|---|---|---|
| `market` | INT8 | `int8` | 否 | 枚举 |
| `date` | INT32 | `int32` | 否 | YYYYMMDD |
| `is_trading_day` | INT8 | `int8` | 否 | 1=交易日 0=非交易日 |

日历包含非交易日（`is_trading_day = 0`），便于直接做日期区间运算，
无需另行判断周末与节假日。

---

## 4. `adj_factor` —— 复权因子

**路径**：`data/meta/adj_factor.parquet`（+ CSV 镜像）
**主键**：`(instrument_id, ex_date)`
**规模**：约 20 万行（每标的数十行）
**语义**：**事件式**，仅在除权日给出一行；**自 `ex_date` 当日起生效**（前向填充）

| 字段 | Parquet | Go | 可空 | 说明 |
|---|---|---|---|---|
| `instrument_id` | INT32 | `int32` | 否 | |
| `ex_date` | INT32 | `int32` | 否 | 除权除息日 YYYYMMDD |
| `hfq_factor` | INT64 | `int64` | 否 | 后复权因子，定点 **scale = 1e12** |
| `hfq_factor_raw` | STRING | `string` | 否 | 数据源原始字符串，保留 16 位精度用于审计 |

### 4.1 为什么存两份

数据源（新浪）返回的因子是 **16 位精度的字符串**，直接 `float()` 会丢精度。

- `hfq_factor`：定点 int64，scale 1e12，供引擎直接使用，**无需 Go 端引入 Decimal 库**
- `hfq_factor_raw`：原样保留字符串，用于审计与将来重算

精度核算：因子实际范围约 1~10000，`10000 × 1e12 = 1e16 < 9.2e18`（int64 上限），
不会溢出；scale 1e12 提供 13 位有效数字，而 v0.0 实测的还原相对误差为 4.19e-07
（受限于数据源后复权价的 2 位小数），13 位有效数字远超所需。

### 4.2 使用方式

```
后复权价(d) = 原始价(d) × hfq_factor(最近一个 ex_date <= d) / 1e12
```

无匹配记录时（标的上市至今无除权）因子取 `1e12`（即 1.0）。

> **前复权价禁止持久化**（C2）：它锚定在最后一个交易日，
> 每次新除权都会改写全部历史值，既不可复现，本身也是未来函数。

---

## 5. `corporate_action` —— 分红送配

**路径**：`data/meta/corporate_action.parquet`（+ CSV 镜像）
**主键**：`(instrument_id, ex_date)`
**用途**：Portfolio 模块据此将现金分红入账、送转股增加持仓（C2）

| 字段 | Parquet | Go | 可空 | 说明 |
|---|---|---|---|---|
| `instrument_id` | INT32 | `int32` | 否 | |
| `ex_date` | INT32 | `int32` | 否 | 除权除息日 |
| `record_date` | INT32 | `int32` | 是 | 股权登记日 |
| `pay_date` | INT32 | `int32` | 是 | 派息日 |
| `cash_before_tax` | INT64 | `int64` | 否 | **每股**税前现金红利，定点 scale 1e6（元） |
| `stock_dividend` | INT64 | `int64` | 否 | **每股**送股数，定点 scale 1e6 |
| `stock_transfer` | INT64 | `int64` | 否 | **每股**转增股数，定点 scale 1e6 |

### 5.1 不存税后金额

BaoStock 的 `dividCashPsAfterTax` 存在 `"27.7884或30.876"` 这类
**非数值字符串**（税率分档所致）。

红利税属于**规则**而非数据：税率随持股期限变化，应由 `Fee` / `Portfolio`
模块按 `configs/fee/*.json` 计算。因此本表**只存税前金额**。

---

## 6. `_manifest.json` —— 数据版本指纹

**路径**：`data/meta/_manifest.json`

约束 C5 要求结果指纹包含**数据版本**。每次 ETL 产出后写出：

```json
{
  "schema_version": "1.0.0",
  "data_version": "2026-08-28T21:40:00+08:00",
  "generated_at": "2026-08-28T21:40:00+08:00",
  "sources": {
    "stock_bar": "baostock-0.9.3",
    "etf_bar": "akshare-1.18.94/fund_etf_hist_sina",
    "adj_factor": "akshare-1.18.94/stock_zh_a_daily:hfq-factor"
  },
  "row_counts": {"bar": 19923117, "instruments": 7200, "adj_factor": 198433},
  "trading_day_range": [20050104, 20260828],
  "content_hash": "sha256:..."
}
```

引擎启动时读取，并将 `data_version` + `content_hash` 纳入回测结果指纹。
`schema_version` 不匹配时引擎应**拒绝启动**，而非静默按旧 schema 解析。

---

## 7. 尚未定义的表

| 表 | 版本 | 说明 |
|---|---|---|
| `results` | v0.5 | 海选结果，由 Go 写入。字段依赖 Metrics 模块的最终指标集，届时定义 |
| `equity` | v0.5 | 净值曲线分片 |
| `snapshot` | v0.4 | 引擎状态快照，非 Parquet（JSON / gob） |

---

## 8. 评审要点

定稿前需重点确认的开放项：

1. **停牌行的 OHLC 取值**（1.3）—— 需 v0.1 实测 BaoStock 实际返回值
2. **北交所申报单位**（2.1）—— 需核实来源
3. **ETF 的 `preclose` 推算方式**（1.4）—— 依赖 ETF 复权方案的收口结果
4. **`name` 不做时变**（2.2）—— 确认展示层面的代价可接受
5. **不存税后分红**（5.1）—— 确认税率计算放在 Fee 模块的分工合理
