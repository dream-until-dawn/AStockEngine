# AStockEngine

中国 A 股交易策略回测系统。数据层 Python，回测引擎 Go。

**当前版本 v0.2** —— 引擎核心闭环已跑通：单步状态机 + A 股交易规则 + 费用账本，
三个样例策略端到端可跑。

## 当前范围

| 维度 | 范围 |
|---|---|
| 市场 | **仅 A 股** |
| 频率 | **仅日线** |
| 标的 | 个股（含已退市）+ ETF |
| 因子 | **纯技术面**（量价），不含市盈率等基本面 |

范围是刻意收窄的，但接口按多市场 / 多频率设计（约束 C9），
远期接入美股、日经、加密货币分钟线时不需要重构核心。

## 快速开始

### 1. 环境

```bash
python -m venv .venv
```

```bash
.\.venv\Scripts\python.exe -m pip install --no-cache-dir -r etl/requirements.txt
```

> ETL 脚本**必须用 `.venv` 里的解释器**跑。直接 `python etl/xxx.py` 会用到系统
> Python 从而缺依赖 —— `etl/_venv_guard.py` 会拦下这种情况并提示正确命令。

### 2. 拉数据

`data/` 不入 git，需自行拉取。全量约 323 MB，首次跑满需数小时，
断点续传与每日增量都已内建，可无人值守运行。

```bash
.\.venv\Scripts\python.exe etl\build_instruments.py
```

```bash
.\.venv\Scripts\python.exe etl\sync_bars.py --workers 12 --batch 300
```

```bash
.\.venv\Scripts\python.exe etl\build_etf_factors.py
```

```bash
.\.venv\Scripts\python.exe etl\build_corporate_actions.py
```

再次运行 `sync_bars.py` 即为增量更新，只补新交易日。

加密货币（OKX 永续合约日线 + 资金费率，可选，约 2 分钟）。
**不需要 API key** —— 行情端点是公开的：

```bash
.\.venv\Scripts\python.exe etl\build_crypto.py
```

> 加密数据目前**只到数据层**：可以在核对台里看 K 线与指标，
> 但还跑不了回测 —— 交易规则（T+0、无涨跌停、资金费率计费）尚未实现，
> 引擎会明确拒绝而不是套用 A 股规则给出假结果。
>
> 资金费率**只有约 3 个月**（OKX 公开端点的保留期），
> 对 6.6 年行情的覆盖率 3.9%，详见 [SCHEMA.md 6.3](docs/SCHEMA.md)。

### 3. 打开数据核对台

在改策略之前，先确认数据是对的。核对台把四张非行情表、日线行情
与**引擎算出的指标**摆在同一个页面上：

```bash
cd engine && go run ./cmd/server
```

```bash
cd web && npm install && npm run build
```

构建完前端后访问 http://127.0.0.1:8123 。开发前端时改用 `npm run dev`
（热更新，把 `/api` 代理到后端），访问 http://localhost:5173 。

后端首次启动约 30 秒 —— 它把全部 1,767 万行日线一次性载入内存（1.25 GB）。
原因和用法见 [WEB.md](docs/WEB.md)。

K 线页上的 MACD / KDJ / 均线**由后端引擎算出**，走的是与回测逐字节相同的
代码路径。切换复权模式时，读数面板会把「所选基准」与「回测基准（后复权）」
两组指标值并排给出。

### 4. 跑回测

**一次运行由一份 JSON 描述**，换 sizer、加风控、改费率都不需要重新编译：

```bash
cd engine && go run ./cmd/backtest -config ../configs/backtest/macd_cross.json
```

看有哪些模块可选、各自收什么参数：

```bash
cd engine && go run ./cmd/backtest -modules
```

命令行只剩三个开关：`-config`（配置在哪）、`-equity-out`（净值序列写哪）、
`-snapshot-at`（在第 N 步快照并验证往返）。配置示例见 `configs/backtest/`：

```json
{
  "data":     { "root": "../../data", "from": 20200101,
                "universe": { "type": "stock", "require_factor": true, "limit": 300 } },
  "fee":      { "impl": "config", "params": { "path": "../fee/ashare_default.json" } },
  "slippage": { "impl": "fixed_bps", "params": { "bps": 5 } },
  "sizer":    { "impl": "equal_weight", "params": { "slots": 10, "base": "initial" } },
  "risk":     [ { "impl": "max_position_pct", "params": { "pct": 15 } } ],
  "strategy": { "impl": "macd_cross", "params": { "short": 12, "long": 26, "signal": 9 } },
  "metrics":  { "benchmark": "510300" }
}
```

**费率是用户配置项，不是常量** —— 不同券商佣金不同，远期加密货币的费率结构
（提现费、maker/taker）也与 A 股完全不同。见 `configs/fee/ashare_default.json`，
其中印花税按六个历史时间段分段，回测 2008 年的单子会用当时的税率。

## 目录结构

```
etl/           Python 数据管道
  layout.py      所有路径的唯一定义处
  schema.py      表结构（与 Go 侧的唯一耦合点）
  sources/       数据源适配器，每个源一个文件（BaoStock / 新浪 / OKX）
  probe/         v0.0 技术验证探针
engine/        Go 回测引擎
  internal/mktdata/     列式加载、时间游标、复权、公司行动
  internal/indicator/   增量指标（MA / MACD / KDJ）
  internal/trading/     Fee / Market / Portfolio / Broker
  internal/engine/      Step() 状态机与策略接口（含单步调试的只读入口）
  internal/strategies/  样例策略（趋势 / 均值回归 / 突破 / 网格）
  internal/spec/        参数自描述（喂 Web 表单 / 海选网格 / 配置校验）
  internal/registry/    泛型容器：按名字取实现
  internal/config/      一份 JSON 描述一次运行 + 未计入成本的显式披露
  internal/sweep/       策略海选：网格展开、并发驱动、噪声基线、高原判定
  internal/record/      三档记录（none / summary / full）
  internal/metrics/     绩效指标
  internal/fingerprint/ 结果指纹（C5）
  cmd/backtest          回测命令行
  cmd/server            数据核对服务（HTTP API）
web/           数据核对台前端（React + Vite）
  src/views/            五个视图：概览 / 标的 / K线 / 日历 / 因子 / 分红
  src/components/       图表与通用表格
configs/       费率 / 市场 / 策略配置
data/          数据与结果（**不入 git**）
docs/          设计文档
```

## 文档

| 文档 | 内容 |
|---|---|
| [ROADMAP.md](docs/ROADMAP.md) | **活文档**：10 条架构约束（C1–C10）、存储设计、版本排期 |
| [SCHEMA.md](docs/SCHEMA.md) | 表结构契约，Python 与 Go 都以此为准 |
| [ETL.md](docs/ETL.md) | 数据管道说明；**第 6 节「踩过的坑」是全项目信息密度最高的一节** |
| [WEB.md](docs/WEB.md) | 数据核对台：接口、五个视图、七条设计决策 |
| [DESIGN-v0.3-assembly.md](docs/DESIGN-v0.3-assembly.md) | 模块化装配：registry、Sizer/Risk、Metrics 的三个坑、结果指纹 |
| [DESIGN-v0.5-selection.md](docs/DESIGN-v0.5-selection.md) | 策略海选：**噪声基线 19 个百分点**、为什么单点收益率不可比、高原判定 |
| [DESIGN-v0.6-exit.md](docs/DESIGN-v0.6-exit.md) | 离场规则：止损/止盈/移动止损，以及**为什么它塞不进风控链** |
| [DESIGN-v0.2-dataflow.md](docs/DESIGN-v0.2-dataflow.md) | 行情数据流设计评审 |
| [DESIGN-v0.2-trading.md](docs/DESIGN-v0.2-trading.md) | 交易语义设计评审 |
| [probe/REPORT-v0.0.md](docs/probe/REPORT-v0.0.md) | 技术选型的实测依据 |

## 几条不可妥协的约束

完整 10 条见 ROADMAP，这里列最容易被写错的：

- **C1 未来函数防护** —— 策略拿不到全量数据切片，未来数据是*物理上不存在*，
  而非「有但禁止访问」
- **C2 复权** —— 存原始价 + 因子，引擎内动态复权。**前复权不得用于决策**：
  它锚定末日，本身即未来函数，且不可复现
- **C4 状态机** —— 核心是 `Step()`，**不能是内部 for 循环**。
  单步调试、批量海选、实盘增量三种模式共用同一核心
- **C5 可复现** —— 全链路定点整数，同配置两次运行逐笔一致。
  由**输入/输出指纹**机器可验，不靠肉眼比对几个汇总数。想跨构建复现，
  用 `-ldflags "-X main.gitCommit=$(git rev-parse HEAD)"` 构建 ——
  `go run` 拿不到 commit，报告会标注「dev 构建，指纹不保证跨构建可复现」
- **C8.1 涨跌停基准是 `preclose`** —— 不是前一日收盘价。用 `close.shift(1)`
  会在每个大比例送转的除权日误判

## 分支

`main` 为主干，`develop` 为日常开发，功能完成后 `--no-ff` 合入并打 tag。
