"""SCHEMA.md 的代码化实现：枚举、定点换算与 pyarrow schema。

本模块是 `docs/SCHEMA.md` 的唯一代码对应物。任何字段变更必须先改文档再改这里，
并同步升级 `SCHEMA_VERSION` —— 引擎侧 schema_version 不匹配时会拒绝启动。

所有整数一律有符号；不使用 DECIMAL / DATE / TIMESTAMP 逻辑类型（跨语言支持参差）。
"""

from __future__ import annotations

from datetime import date, datetime, timedelta, timezone
from decimal import Decimal, InvalidOperation
from enum import IntEnum
from typing import Iterable

import pyarrow as pa

SCHEMA_VERSION = "1.0.0"

# --- 枚举（0 一律保留为未知/无效） ------------------------------------------


class Market(IntEnum):
    UNKNOWN = 0
    ASHARE = 1
    # 远期：US = 2, HK = 3, FUTURES = 4, CRYPTO = 5


class Exchange(IntEnum):
    UNKNOWN = 0
    SSE = 1   # 上交所
    SZSE = 2  # 深交所
    BSE = 3   # 北交所


class InstrumentType(IntEnum):
    UNKNOWN = 0
    STOCK = 1
    ETF = 2


class Board(IntEnum):
    UNKNOWN = 0
    MAIN = 1      # 主板
    CHINEXT = 2   # 创业板
    STAR = 3      # 科创板
    BSE = 4       # 北交所


class Currency(IntEnum):
    UNKNOWN = 0
    CNY = 1


class Status(IntEnum):
    UNKNOWN = 0
    LISTED = 1
    DELISTED = 2


# --- 定点 scale --------------------------------------------------------------

PRICE_SCALE_ASHARE = 1000       # 价格最小单位 0.001 元
QTY_SCALE_ASHARE = 1            # 数量最小单位 1 股/份
AMOUNT_SCALE = 100              # 金额以分为单位
RATIO_SCALE = 1_000_000         # 比率（换手率等）固定 1e6
FACTOR_SCALE = 1_000_000_000_000  # 复权因子 1e12
PER_SHARE_SCALE = 1_000_000     # 每股分红/送转 1e6

# --- A 股交易时段（用于 ts_open / ts_close） ---------------------------------
# 不建模集合竞价（ROADMAP C8.2），故 ts_open 取连续竞价开始时刻 09:30 CST。

CST = timezone(timedelta(hours=8))
ASHARE_OPEN_HHMM = (9, 30)
ASHARE_CLOSE_HHMM = (15, 0)


# --- 类型换算 ----------------------------------------------------------------


def to_fixed(value, scale: int) -> int:
    """浮点/字符串 → 定点整数。缺失值一律归零。

    经 Decimal 中转，避免 float 二进制表示导致的 round-half 偏差
    （如 2.675 * 100 在 float 下为 267.49999...）。

    必须容忍 NaN 与空串：新浪的 ETF 行情存在整行缺失字段，BaoStock 的停牌行
    turn 为空串。让转换函数兜底，好过在每个调用点各写一遍防御。
    """
    if value is None or value == "":
        return 0
    try:
        d = Decimal(str(value))
    except (InvalidOperation, ValueError):
        return 0
    if not d.is_finite():
        return 0
    return int((d * scale).quantize(Decimal("1")))


def ymd_to_int(value) -> int:
    """日期 → YYYYMMDD 整数。接受 date / datetime / 'YYYY-MM-DD' / 'YYYYMMDD'。"""
    if value is None or value == "":
        return 0
    if isinstance(value, (date, datetime)):
        return value.year * 10000 + value.month * 100 + value.day
    s = str(value).strip()[:10].replace("-", "").replace("/", "")
    return int(s) if s.isdigit() else 0


def int_to_date(ymd: int) -> date:
    return date(ymd // 10000, ymd // 100 % 100, ymd % 100)


def session_ts(ymd: int) -> tuple[int, int]:
    """交易日 → (ts_open, ts_close)，UTC 毫秒。

    引擎的时间游标只用 ts_close —— 它才是该 bar 信息可得的时刻。
    """
    d = int_to_date(ymd)
    o = datetime(d.year, d.month, d.day, *ASHARE_OPEN_HHMM, tzinfo=CST)
    c = datetime(d.year, d.month, d.day, *ASHARE_CLOSE_HHMM, tzinfo=CST)
    return int(o.timestamp() * 1000), int(c.timestamp() * 1000)


# --- 标的属性推断 -------------------------------------------------------------


# 北交所代码段需先于上交所判断：920xxx 属北交所，而 900xxx 是上交所 B 股
_BSE_PREFIXES = ("43", "83", "87", "88", "920")


def infer_exchange(symbol: str) -> Exchange:
    if symbol.startswith(_BSE_PREFIXES):
        return Exchange.BSE
    # 上交所：5 开头为基金（50/51/52/53/55/56/58），6 开头为股票，900 为 B 股
    if symbol[:1] in ("5", "6") or symbol.startswith("900"):
        return Exchange.SSE
    # 深交所：0 主板、3 创业板、1 基金（15/16/18）、2 为 B 股
    if symbol[:1] in ("0", "1", "2", "3"):
        return Exchange.SZSE
    return Exchange.UNKNOWN


def infer_board(symbol: str, itype: InstrumentType) -> Board:
    """板块决定涨跌停幅度与申报单位，是 Market 模块的关键输入。

    代码段的归属**已用涨跌幅实测校验**，不靠推测 —— v0.1 构建 instruments 时
    发现两个易错段：

      689xxx  科创板 CDR（如 689009 九号公司），实测最大日涨 17.84%
      302xxx  创业板（如 302132 中航成飞，由创业板 300114 吸收合并后换段），
              实测最大日涨 20.01%，13 个交易日超过 10.5%

    若按「3 开头且非 300/301 即主板」推断，302 段会被误判为 10% 涨跌停。
    新代码段出现时，`build_bars.py` 的板块自洽校验会再次捕获。
    """
    if itype == InstrumentType.ETF:
        # ETF 的涨跌停由其跟踪指数决定（跟踪创业板/科创板指数者为 20%），
        # 无法由代码段可靠推断。此处统一记为主板，实际幅度由 v0.2 的 Market
        # 模块按 configs 中的 ETF 类别配置决定。
        return Board.MAIN
    if symbol.startswith(("688", "689")):
        return Board.STAR
    if symbol.startswith(("300", "301", "302")):
        return Board.CHINEXT
    if symbol.startswith(_BSE_PREFIXES):
        return Board.BSE
    if symbol.startswith(("60", "00")):
        return Board.MAIN
    return Board.UNKNOWN


def order_qty_rule(board: Board, itype: InstrumentType) -> tuple[int, int]:
    """返回 (min_order_qty, qty_step)。依据 SCHEMA.md 2.1。"""
    if itype == InstrumentType.ETF:
        return 100, 100
    if board == Board.STAR:
        return 200, 1      # 科创板：200 股起，1 股递增
    if board == Board.BSE:
        return 100, 1      # 北交所：100 股起，1 股递增（已核实）
    return 100, 100        # 主板 / 创业板


# --- pyarrow schema ----------------------------------------------------------

BAR_CORE_FIELDS = [
    pa.field("instrument_id", pa.int32(), nullable=False),
    pa.field("ts_open", pa.int64(), nullable=False),
    pa.field("ts_close", pa.int64(), nullable=False),
    pa.field("trading_day", pa.int32(), nullable=False),
    pa.field("open", pa.int64(), nullable=False),
    pa.field("high", pa.int64(), nullable=False),
    pa.field("low", pa.int64(), nullable=False),
    pa.field("close", pa.int64(), nullable=False),
    pa.field("volume", pa.int64(), nullable=False),
    pa.field("amount", pa.int64(), nullable=False),
]

BAR_ASHARE_EXT_FIELDS = [
    pa.field("preclose", pa.int64(), nullable=False),
    pa.field("turn", pa.int32(), nullable=False),
    pa.field("tradestatus", pa.int8(), nullable=False),
    pa.field("is_st", pa.int8(), nullable=False),
]

BAR_SCHEMA = pa.schema(BAR_CORE_FIELDS + BAR_ASHARE_EXT_FIELDS)

INSTRUMENTS_SCHEMA = pa.schema([
    pa.field("instrument_id", pa.int32(), nullable=False),
    pa.field("market", pa.int8(), nullable=False),
    pa.field("symbol", pa.string(), nullable=False),
    pa.field("exchange", pa.int8(), nullable=False),
    pa.field("name", pa.string(), nullable=False),
    pa.field("type", pa.int8(), nullable=False),
    pa.field("board", pa.int8(), nullable=False),
    pa.field("price_scale", pa.int32(), nullable=False),
    pa.field("qty_scale", pa.int32(), nullable=False),
    pa.field("quote_ccy", pa.int8(), nullable=False),
    pa.field("min_order_qty", pa.int32(), nullable=False),
    pa.field("qty_step", pa.int32(), nullable=False),
    pa.field("list_date", pa.int32(), nullable=False),
    pa.field("delist_date", pa.int32(), nullable=True),
    pa.field("status", pa.int8(), nullable=False),
    pa.field("attrs", pa.string(), nullable=True),
])

CALENDAR_SCHEMA = pa.schema([
    pa.field("market", pa.int8(), nullable=False),
    pa.field("date", pa.int32(), nullable=False),
    pa.field("is_trading_day", pa.int8(), nullable=False),
])

ADJ_FACTOR_SCHEMA = pa.schema([
    pa.field("instrument_id", pa.int32(), nullable=False),
    pa.field("ex_date", pa.int32(), nullable=False),
    pa.field("hfq_factor", pa.int64(), nullable=False),
    pa.field("hfq_factor_raw", pa.string(), nullable=False),
])

CORPORATE_ACTION_SCHEMA = pa.schema([
    pa.field("instrument_id", pa.int32(), nullable=False),
    pa.field("ex_date", pa.int32(), nullable=False),
    pa.field("record_date", pa.int32(), nullable=True),
    pa.field("pay_date", pa.int32(), nullable=True),
    pa.field("cash_before_tax", pa.int64(), nullable=False),
    pa.field("stock_dividend", pa.int64(), nullable=False),
    pa.field("stock_transfer", pa.int64(), nullable=False),
])

TABLE_SCHEMAS = {
    "bar": BAR_SCHEMA,
    "instruments": INSTRUMENTS_SCHEMA,
    "calendar": CALENDAR_SCHEMA,
    "adj_factor": ADJ_FACTOR_SCHEMA,
    "corporate_action": CORPORATE_ACTION_SCHEMA,
}

# bar 表中适用 delta 编码的列（实测该组合为最优：18.30 字节/行）
BAR_DELTA_COLUMNS = (
    "ts_open", "ts_close", "trading_day",
    "open", "high", "low", "close", "preclose", "volume", "amount",
)


def parquet_write_options(table: str) -> dict:
    """各表的 Parquet 写出参数。bar 表用 delta 编码，元数据表用默认。"""
    opts: dict = {"compression": "zstd", "compression_level": 3}
    if table == "bar":
        opts["column_encoding"] = {c: "DELTA_BINARY_PACKED" for c in BAR_DELTA_COLUMNS}
        opts["use_dictionary"] = False
    return opts


def validate_columns(df, table: str) -> None:
    """写出前校验列集合与 schema 完全一致 —— 多列少列都视为错误。"""
    expected = [f.name for f in TABLE_SCHEMAS[table]]
    actual = list(df.columns)
    if actual != expected:
        missing = set(expected) - set(actual)
        extra = set(actual) - set(expected)
        raise ValueError(
            f"{table} 列不匹配\n  期望: {expected}\n  实际: {actual}\n"
            f"  缺失: {sorted(missing)}\n  多余: {sorted(extra)}"
        )
