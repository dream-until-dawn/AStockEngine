# AStockEngine

多市场量价策略回测系统。**数据层 Python，回测引擎 Go，核对台 React。**

三层之间只靠 [`docs/SCHEMA.md`](docs/SCHEMA.md) 定义的 Parquet 文件契约耦合 ——
不共享代码、不共享进程，换掉任意一层不影响另外两层。

---

## 它现在能做什么

下面每一条都是**当前代码里跑得通的**，不是排期。想验证哪一条，
直接跑对应那节给的命令。

### 两个市场，规则各自独立

| | A 股 | 加密永续（OKX） |
|---|---|---|
| 结算 | T+1，当日买入不可卖 | T+0 |
| 涨跌停 | 10% / 20%（按跟踪指数判定）| 无 |
| 方向 | 只做多（现货账本） | **双向持仓**，逐仓保证金，可强平 |
| 费用 | 佣金（含每笔最低）+ 印花税**按六个历史时段分段** + 过户费；ETF 免印花税与过户费 | maker / taker 分档 |
| 杠杆 | 无 | 1~100x 可配 |
| 年化系数 | 由交易日历实测（约 243） | 365 |

**市场规则不是开关，是模块。** 把做空策略配到 A 股上会在引擎装配时直接报错：

```
错误: 策略 grid 要开空，但市场规则 "ashare" 不支持做空 ——
      「仅做空」与「双向持仓」只在支持做空的市场（如 crypto）可用
```

不拦的话是**静默失效**：开空信号被 Sizer 当成减仓，而手上没有多头可减，
订单被丢掉 —— 实测「信号 606 条、成交 0 笔、拒单 0 笔、总收益 +0.00%」，
报告上看不出任何异常。

### 数据

本机当前一份完整数据：**1,767 万行日线 / 7,202 个标的**
（A 股 5,549 + ETF 1,651 + 加密永续 2），落盘 336 MB。
含已退市股票（C3 幸存者偏差防护），复权因子 + 分红送配 + 交易日历齐备。

### 7 个策略、5 个仓位模块、6 个风控、3 个离场规则

```bash
cd engine && go run ./cmd/backtest -modules
```

会列出全部模块与**每个参数的取值域和说明** —— 同一份自描述同时喂
Web 表单、海选参数网格和配置校验，不会出现「表单能填但引擎不认」。

策略里三个值得单独说：

- **`rule_tree` 规则树** —— 三棵决策树（买入 / 卖出 / 有效性）组合，
  被有效性树挡下的信号记为**虚拟持仓**：不占资金、不计胜率，但会算出
  「如果当时买了会怎样」。那棵树到底帮忙还是帮倒忙，只有这个量答得了
- **`grid` 网格** —— 做多 / 做空 / **双向**三种模式。双向是 0 线空仓、
  跌了做多、涨了做空，两端各一条止损线
- **`composite` 组合** —— 把多个决策源按 `union` / `confirm` / `veto`
  合成一个策略。它**不在 `-modules` 的注册表清单里**（它要嵌套 `sources`，
  而注册表只认「一个名字 + 一段扁平参数」），写法见
  `configs/backtest/crypto_multi_ruletree_hedge.json`：

  ```json
  "strategy": {
    "impl": "composite", "mode": "confirm",
    "sources": [ { "impl": "ma_cross", "params": { "fast": 5, "slow": 20 } },
                 { "impl": "donchian_breakout", "params": { "entry": 20, "exit": 10 } } ]
  }
  ```

  `confirm` 要求各源同时同意，所以**给它两个结构上对立的源会零成交**
  （追势的 `ma_cross` 配反转的 `rsi_reversion`，实测 0 轮）。
  这种时候用 `union`。

### 策略海选

不是「跑一堆参数排个名」。它按**可信度链条**输出：

1. **噪声基线** —— 同一组参数只把初始资金扰动 ±0.1%，重复跑，量出结果本身的抖动
2. **判定** —— 全网格散布 ÷ 噪声基线 < 1.5 时，结论是「这些参数没有可辨别的影响」，**不出排名**
3. **归因** —— 强平 / 熔断清仓 / 止损各几轮、摩擦占比、未平仓占比。
   光看收益分不出「策略有边际」和「强平替你止损」
4. **逐维边际** —— 每个轴的每个取值各自的中位数，答「这个轴有没有用、往哪边调」
5. **稳健区域** —— 邻域整体好的一片，而不是排名第一的那个点

重复的维度可以是**时间窗口**（Walk-Forward）、**标的**（横截面），
或者两者的**叉乘**（标的少的时候，比如加密只有两只永续）。

### Web 核对台（8 个视图）

概览 / 回测 / 单步调试 / 海选 / 标的列表 / 交易日历 / 复权因子 / 分红送配，
外加从标的列表进入的 K 线页。

- **K 线页的 MACD / KDJ / 均线由后端引擎算出**，走的是与回测逐字节相同的代码路径。
  切换复权模式时，读数面板把「所选基准」与「回测基准（后复权）」两组值并排给出
- **单步调试**可以一根 bar 一根 bar 地走，随时看持仓、信号、被拒订单，
  并做状态快照 / 回滚
- **回测页**逐轮交易能看出这一轮是实仓还是虚拟、是被止盈止损还是被强平

### 可复现（C5）

全链路定点整数，同配置两次运行逐笔一致，由**输入/输出指纹**机器可验。

```bash
cd engine && go run ./cmd/backtest -config ../configs/backtest/macd_cross.json | tail -5
```

想让指纹跨构建可复现，用 `-ldflags "-X main.gitCommit=$(git rev-parse HEAD)"` 构建 ——
`go run` 拿不到 commit，报告会明确标注「dev 构建，指纹不保证跨构建可复现」。

### 说清楚什么**没算**

每份回测报告末尾有「本次回测未计入」一段，逐条列出已知缺口
（资金费率、集合竞价、融资融券……）。**漏算的成本不报错，只让结果一致地偏乐观**，
所以它必须印出来。

### 测试不依赖数据

```bash
cd engine && go test ./...
```

**整个测试套件不读 `data/`**，clone 完就能跑。所以它测的是逻辑
（涨跌停判定、轮次配对、复权、开平方向表、网格档位……），
而不是「某只股票在某一天的收益是多少」—— 后者会随数据更新而失败，
那种测试第二天就会被人加 `-skip`。

---

## 部署运行

### 0. 前置

| | 版本 | 说明 |
|---|---|---|
| Python | 3.14 | 只有 ETL 用 |
| Go | 1.26.1 | 引擎与服务端 |
| Node | 18+（Vite 6 的要求） | 只有前端构建用 |

系统主要在 **Windows** 上开发，命令按 PowerShell 给出。
Linux / macOS 把 `.\.venv\Scripts\python.exe` 换成 `./.venv/bin/python`、
反斜杠换成正斜杠即可，代码本身无平台相关逻辑。

```bash
git clone <repo> && cd AStockEngine
```

### 1. Python 环境

```bash
python -m venv .venv
```

```bash
.\.venv\Scripts\python.exe -m pip install --no-cache-dir -r etl/requirements.txt
```

> **必须用 `.venv` 里的解释器**跑 ETL。直接 `python etl/xxx.py` 会用到系统 Python
> 从而缺依赖 —— `etl/_venv_guard.py` 会拦下来并提示正确命令。
>
> `--no-cache-dir` 不能省：用户名含非 ASCII 字符时 pip 缓存目录会触发 PermissionError。

### 2. 拉数据

`data/` **不入 git**，需自行拉取。全量约 336 MB，首次跑满需数小时；
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

加密永续（OKX 日线 + 资金费率，可选，约 2 分钟）。
**不需要 API key** —— 行情端点是公开的，`etl/sources/okx_src.py` 不碰任何凭据：

```bash
.\.venv\Scripts\python.exe etl\build_crypto.py
```

> 资金费率**只有约 3 个月**（OKX 公开端点的保留期），
> 对多年行情的覆盖率不足 4%，因此**引擎目前不计提资金费率**，
> 并在每份加密回测报告里显式披露。做多为主的结果会因此偏乐观。

想确认数据是对的，先跑一遍抽样核对：

```bash
.\.venv\Scripts\python.exe etl\sample_check.py
```

### 3. 起服务（Web 核对台 + API）

```bash
cd web && npm install && npm run build
```

```bash
cd engine && go run ./cmd/server
```

访问 <http://127.0.0.1:8123> 。

> **首次启动约 30 秒** —— 它把全部日线一次性载入内存（实测 30.2 秒 / 1,247 MB，
> `cd engine && go run ./cmd/loadbench` 可复现）。
> 这是刻意的：核对台要随机访问任意标的的任意区间，按需读盘会让每次点击都卡。
>
> `web/dist` 不存在时服务端只提供 API 不提供页面，不会报错。
> 开发前端改用 `cd web && npm run dev`（热更新，`/api` 代理到后端），
> 访问 <http://localhost:5173> 。

服务端可调的四个参数：

```bash
cd engine && go run ./cmd/server -addr 127.0.0.1:8123 -data ../data -web ../web/dist -configs ../configs/backtest
```

### 4. 跑一次回测

**一次运行由一份 JSON 完整描述** —— 换 sizer、加风控、改费率都不需要重新编译：

```bash
cd engine && go run ./cmd/backtest -config ../configs/backtest/macd_cross.json
```

加密的：

```bash
cd engine && go run ./cmd/backtest -config ../configs/backtest/crypto_grid_both.json
```

命令行只有四个开关：`-config`（配置在哪）、`-modules`（列出可用模块）、
`-equity-out`（净值序列写哪）、`-snapshot-at`（在第 N 步快照并验证往返）。
配置示例见 `configs/backtest/`，形如：

```json
{
  "data":     { "root": "../../data", "market": "ashare", "from": 20200101,
                "universe": { "type": "stock", "require_factor": true, "limit": 300 } },
  "market":   { "impl": "ashare" },
  "fee":      { "impl": "config", "params": { "path": "../fee/ashare_default.json" } },
  "slippage": { "impl": "fixed_bps", "params": { "bps": 5 } },
  "portfolio":{ "initial_cash_cents": 2000000, "ledger": "spot" },
  "sizer":    { "impl": "equal_weight", "params": { "slots": 10, "base": "cost" } },
  "risk":     [ { "impl": "max_position_pct", "params": { "pct": 15 } } ],
  "exit":     [ { "impl": "trailing_stop", "params": { "pct": 12 } } ],
  "strategy": { "impl": "macd_cross", "params": { "short": 12, "long": 26, "signal": 9 } },
  "metrics":  { "benchmark": "510300" }
}
```

**费率是用户配置项，不是常量。** 不同券商佣金不同，加密的费率结构也与 A 股
完全不同。见 `configs/fee/`，其中 A 股印花税按六个历史时段分段 ——
回测 2008 年的单子会用当时的税率。

### 5. 跑一次海选

```bash
cd engine && go run ./cmd/sweep -config ../configs/sweep/etf_grid.json -workers 8
```

先看规模不跑：加 `-dry-run`。跑完之后换判据重新分析（不重跑回测）：

```bash
cd engine && go run ./cmd/sweep -config ../configs/sweep/etf_grid.json -report-only <sweep_id>
```

结果落在 `data/results/sweep=<id>/`，含 `manifest.json` —— 那份清单让结果目录
**自描述**：硬门槛、排序口径、参数集都在里面，Web 端只知道目录名也能读懂。
起服务后在「海选」页可以看同一份结果。

---

## 目录结构

```
etl/           Python 数据管道
  layout.py           所有路径的唯一定义处
  schema.py           表结构（与 Go 侧的唯一耦合点）
  sources/            数据源适配器，一源一文件（BaoStock / 新浪 / OKX）
  sync_bars.py        日线增量同步（断点续传）
  build_*.py          标的表 / ETF 复权因子 / 分红送配 / 加密
  etf_character.py    按形态给 ETF 分类，产出去重的横截面标的池
  probe/              v0.0 技术选型探针
ingest/        非 Python 数据源预留（契约是 Parquet，不是 Python 代码）
engine/        Go 回测引擎
  internal/mktdata/     列式加载、时间游标、复权、公司行动
  internal/indicator/   增量指标（MA / MACD / KDJ）
  internal/trading/     Market / Fee / Slippage / Sizer / Risk / Exit / 账本 / 撮合
  internal/engine/      Step() 状态机与策略接口（含单步调试的只读入口）
  internal/strategies/  策略实现（趋势 / 均值回归 / 突破 / 网格 / 规则树）
  internal/spec/        参数自描述（喂 Web 表单 / 海选网格 / 配置校验）
  internal/registry/    泛型容器：按名字取实现
  internal/config/      一份 JSON 描述一次运行 + 未计入成本的显式披露
  internal/sweep/       海选：网格展开、并发驱动、噪声基线、归因、高原判定
  internal/record/      三档记录（none / summary / full）
  internal/metrics/     绩效指标与轮次配对
  internal/fingerprint/ 结果指纹（C5）
  cmd/backtest          回测命令行
  cmd/sweep             海选命令行
  cmd/server            Web 服务与 API
  cmd/adjcheck 等       诊断小工具（复权核对、加载基准、Parquet 往返）
web/           核对台前端（React + Vite）
  src/views/          8 个视图 + K 线页
configs/       配置
  backtest/           回测样例（A 股 / 加密各若干）
  sweep/              海选配置，base/ 下是它们共享的基准回测配置
  fee/                费率模型（A 股 / OKX）
data/          数据与结果（**不入 git**）
docs/          设计文档
scripts/       一次性脚本（生成默认配置等）
```

---

## 文档

| 文档 | 内容 |
|---|---|
| [ROADMAP.md](docs/ROADMAP.md) | **活文档**：10 条架构约束（C1–C10）、存储设计、版本排期 |
| [SCHEMA.md](docs/SCHEMA.md) | 表结构契约，Python 与 Go 都以此为准 |
| [ETL.md](docs/ETL.md) | 数据管道；**第 6 节「踩过的坑」是全项目信息密度最高的一节** |
| [WEB.md](docs/WEB.md) | 核对台：接口、视图、设计决策 |
| [DESIGN-v0.2-dataflow.md](docs/DESIGN-v0.2-dataflow.md) | 行情数据流设计评审 |
| [DESIGN-v0.2-trading.md](docs/DESIGN-v0.2-trading.md) | 交易语义设计评审 |
| [DESIGN-v0.3-assembly.md](docs/DESIGN-v0.3-assembly.md) | 模块化装配：registry、Sizer/Risk、Metrics 的三个坑、结果指纹 |
| [DESIGN-v0.4-modularity.md](docs/DESIGN-v0.4-modularity.md) | 单步调试与状态快照 |
| [DESIGN-v0.5-selection.md](docs/DESIGN-v0.5-selection.md) | 海选方法论：**噪声基线**、为什么单点收益率不可比、高原判定 |
| [DESIGN-v0.5.1-sweep-redux.md](docs/DESIGN-v0.5.1-sweep-redux.md) | 海选重做 + **三次实测走查**（ETF 网格 / 按标的配参数 / 加密三模式） |
| [DESIGN-v0.6-exit.md](docs/DESIGN-v0.6-exit.md) | 离场规则，以及**为什么它塞不进风控链** |
| [DESIGN-v0.7-ruletree.md](docs/DESIGN-v0.7-ruletree.md) | 规则树 + 虚拟持仓，以及由它挖出的**假金叉 bug** |
| [DESIGN-v0.8-crypto.md](docs/DESIGN-v0.8-crypto.md) | 加密永续交易规则：双向持仓、逐仓保证金、强平 |
| [probe/REPORT-v0.0.md](docs/probe/REPORT-v0.0.md) | 技术选型的实测依据 |

---

## 几条不可妥协的约束

完整 10 条见 ROADMAP，这里列最容易被写错的：

- **C1 未来函数防护** —— 策略拿不到全量数据切片，未来数据是*物理上不存在*，
  而非「有但禁止访问」
- **C2 复权** —— 存原始价 + 因子，引擎内动态复权。**前复权不得用于决策**：
  它锚定末日，本身即未来函数，且不可复现
- **C3 幸存者偏差** —— 标的池含已退市股票，且是 point-in-time 的
- **C4 状态机** —— 核心是 `Step()`，**不能是内部 for 循环**。
  单步调试、批量海选、实盘增量三种模式共用同一核心
- **C5 可复现** —— 全链路定点整数，同配置两次运行逐笔一致，由指纹机器可验
- **C8.1 涨跌停基准是 `preclose`** —— 不是前一日收盘价。用 `close.shift(1)`
  会在每个大比例送转的除权日误判
- **C9 市场独立** —— 交易规则、费率、年化系数全部由 Market 模块给出，
  引擎里没有任何一处写死 A 股
- **C10 纯技术面** —— 只用量价，不含市盈率等基本面

---

## 分支

`main` 为主干，`develop` 为日常开发，功能完成后 `--no-ff` 合入并打 tag。
