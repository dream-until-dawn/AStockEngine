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

### 3. 跑回测

```bash
cd engine && go run ./cmd/backtest -strategy macd_cross -equity-out ../data/results/macd.csv
```

常用参数：

| 参数 | 说明 |
|---|---|
| `-strategy` | `buy_and_hold` / `ma_cross` / `macd_cross` |
| `-instruments` | 抽样标的数，`0` 为全部 |
| `-from` / `-to` | 起止交易日（`20200101` 形式） |
| `-cash` | 初始资金（元） |
| `-fee` | 费率配置路径 |
| `-equity-out` | 净值序列 CSV 输出路径 |
| `-snapshot-at` | 在第 N 步快照并验证往返一致 |

**费率是用户配置项，不是常量** —— 不同券商佣金不同，远期加密货币的费率结构
（提现费、maker/taker）也与 A 股完全不同。见 `configs/fee/ashare_default.json`，
其中印花税按六个历史时间段分段，回测 2008 年的单子会用当时的税率。

## 目录结构

```
etl/           Python 数据管道
  layout.py      所有路径的唯一定义处
  schema.py      表结构（与 Go 侧的唯一耦合点）
  sources/       数据源适配器，每个源一个文件
  probe/         v0.0 技术验证探针
engine/        Go 回测引擎
  internal/mktdata/     列式加载、时间游标、复权、公司行动
  internal/indicator/   增量指标（MA / MACD / KDJ）
  internal/trading/     Fee / Market / Portfolio / Broker
  internal/engine/      Step() 状态机与策略接口
  internal/strategies/  样例策略
  cmd/                  命令行入口
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
- **C5 可复现** —— 全链路定点整数，同配置两次运行逐笔一致
- **C8.1 涨跌停基准是 `preclose`** —— 不是前一日收盘价。用 `close.shift(1)`
  会在每个大比例送转的除权日误判

## 分支

`main` 为主干，`develop` 为日常开发，功能完成后 `--no-ff` 合入并打 tag。
