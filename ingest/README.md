# ingest —— 非 Python 数据源

`etl/sources/` 下的适配器都是 Python 实现。但数据源的接入方式无法预设：
某些行情商只提供 C++ / Java SDK，某些交易所的 WebSocket 客户端在 Go 或 Rust
下更省事，某些站点只能用 Node 的浏览器自动化抓取。

本目录**为这些非 Python 数据源预留**。每个源一个子目录，语言自选。

---

## 为什么这样可行

整个系统的契约是 **`docs/SCHEMA.md` 定义的 Parquet 文件**，不是 Python 代码。
Python 与 Go 之间已经是这样解耦的（见 ROADMAP「数据存储设计」），
同一条原则对数据源同样成立：**只要最终产出符合契约，用什么语言写无所谓。**

因此本目录的存在不是为了「支持多语言」这个抽象目标，
而是为了把「用什么语言」这个决定，从架构问题降级为每个数据源各自的实现细节。

---

## 两种集成模式

### 模式 A：`raw` —— 外部程序只负责「取」，Python 负责「归一化」

外部程序把原始数据落成 JSONL 或 CSV，Python 侧读取后走既有的归一化、
定点转换与质检流程。

```
外部程序 → data/cache/raw/<source>/*.jsonl → Python 归一化 → Parquet
```

**多数情况下应选这个。** 难的部分（鉴权、SDK、协议、限流）用原生语言解决，
而归一化逻辑（字段映射、单位换算、定点转换、缺失补齐）**只在 Python 侧存在一份**。
归一化逻辑分散到多个语言里，是这类系统最容易失控的地方。

### 模式 B：`parquet` —— 外部程序直接产出成品

外部程序自行实现 SCHEMA.md 的全部约定，直接写出符合契约的 Parquet。

```
外部程序 → data/bar/market=.../*.parquet
```

仅在以下情况选它：数据量大到中间格式的读写开销不可接受，
或该语言已有成熟的 Parquet 写入能力且归一化逻辑非常简单。

代价是**该源必须自己实现并维护定点换算、枚举取值、排序保证与 schema 校验**，
且 SCHEMA.md 每次变更都要同步改它。

---

## 目录约定

```
ingest/
├── README.md              本文件
├── _template/             新增数据源的骨架，复制后改
│   └── manifest.json
└── <source-name>/         例：ibkr-cpp/  binance-go/  tdx-rust/
    ├── manifest.json      必需 —— 声明能力、模式与调用方式
    └── ...                该语言的源码、构建脚本、依赖清单
```

命名用 `<数据源>-<语言>`，便于一眼看出实现语言。

---

## `manifest.json`

Python 侧的 `etl/sources/external.py` 通过它发现并调用外部源，
**因此调用方无需知道该源是什么语言写的** —— 这正是约束 C9 的要求。

```json
{
  "name": "binance-go",
  "language": "go",
  "mode": "raw",
  "capabilities": ["STOCK_BARS"],
  "markets": ["crypto"],
  "build": ["go", "build", "-o", "bin/ingest", "./cmd/ingest"],
  "run": ["bin/ingest", "--symbol", "{symbol}", "--start", "{start}",
          "--end", "{end}", "--out", "{out}"],
  "output_format": "jsonl"
}
```

| 字段 | 说明 |
|---|---|
| `name` | 唯一标识，同时是 `DataSource.name` |
| `language` | 仅供人阅读与排障 |
| `mode` | `raw`（模式 A）或 `parquet`（模式 B） |
| `capabilities` | `etl/sources/base.py: Capability` 的成员名 |
| `markets` | 该源覆盖的市场，对应 `schema.Market` |
| `build` | 构建命令（数组形式，避免 shell 注入）；无需构建则省略 |
| `run` | 运行命令；占位符 `{symbol}` `{exchange}` `{start}` `{end}` `{out}` 会被替换 |
| `output_format` | `mode=raw` 时必填：`jsonl` 或 `csv` |

## `mode=raw` 的输出契约

每行一条记录，字段名与 `etl/sources/base.py` 中 `daily_bars()` 的
**归一化列名一致**（`trading_day` / `open` / `high` / `low` / `close` /
`preclose` / `volume` / `amount` / `turn` / `tradestatus` / `is_st`）。

- 价格**以字符串传递**，保留数据源原始精度 —— 转 float 再转回会丢位
- 缺失字段留 `null`，由 Python 侧按各表的降级约定补齐，外部程序不要自行猜测填充
- `trading_day` 为 `YYYYMMDD` 整数

---

## 新增一个数据源

1. `cp -r _template <source-name>` 并填写 `manifest.json`
2. 实现取数逻辑，按上述契约输出
3. 在 `etl/sources/__init__.py` 中注册：
   `ExternalSource.from_manifest("ingest/<source-name>/manifest.json")`
4. 用 `python etl/validate.py --source <name>` 校验产出
5. 在 `docs/ETL.md` 的「数据源分工」表中补一行，说明**为什么**需要这个源

第 5 步不要跳过。v0.0 的教训是数据源的选择会被实测推翻
（主力源从 AkShare 换成了 BaoStock），而当时的判断依据若不记下来，
半年后没人说得清为什么是这个源。

---

## 现状

**本目录当前为空。** A 股的全部数据需求由 `etl/sources/` 下的
BaoStock 与新浪两个 Python 适配器覆盖，没有引入其他语言的必要。

`etl/sources/external.py` 的接口已实现，但**尚未经真实外部源验证** ——
首次接入时应预留调试时间。
