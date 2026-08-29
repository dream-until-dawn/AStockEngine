"""BaoStock 适配器 —— 个股日线、标的清单、交易日历、分红送配的主力源。

v0.0 实测结论（详见 docs/probe/REPORT-v0.0.md）：
  - 唯一同时提供 preclose / tradestatus / isST 的免费源
  - 退市股覆盖优于东财，且停牌日照常返回行（bar 表因此天然具备 PIT 语义）
  - 连续 30 次请求无限流（东财约 10 次即封 IP）

不提供 ETF 行情（query_history_k_data_plus 对 ETF 代码返回 0 行），
ETF 走 sina_src。
"""

from __future__ import annotations

import baostock as bs
import pandas as pd

from .base import Capability, DataSource, SourceError

_FIELDS = (
    "date,code,open,high,low,close,preclose,volume,amount,"
    "adjustflag,turn,tradestatus,pctChg,isST"
)

# BaoStock 的 type 取值
_TYPE_STOCK, _TYPE_ETF = "1", "5"

_EXCHANGE_PREFIX = {"sh": "sh", "sz": "sz", "bj": "bj"}


def _cursor_to_df(rs) -> pd.DataFrame:
    """BaoStock 返回游标对象，逐行读出。error_code 非 0 时抛错而非静默返回空表。"""
    if rs.error_code != "0":
        raise SourceError(f"baostock error_code={rs.error_code} msg={rs.error_msg}")
    rows = []
    while rs.next():
        rows.append(rs.get_row_data())
    return pd.DataFrame(rows, columns=rs.fields)


def _ymd(s) -> int:
    if not s or not isinstance(s, str):
        return 0
    t = s.strip().replace("-", "")
    return int(t) if len(t) == 8 and t.isdigit() else 0


class BaoStockSource(DataSource):
    name = "baostock"
    capabilities = (
        Capability.INSTRUMENTS
        | Capability.CALENDAR
        | Capability.STOCK_BARS
        | Capability.CORPORATE_ACTIONS
    )

    def __init__(self) -> None:
        self._logged_in = False

    def open(self) -> None:
        r = bs.login()
        if r.error_code != "0":
            raise SourceError(f"baostock 登录失败：{r.error_code} {r.error_msg}")
        self._logged_in = True

    def close(self) -> None:
        if self._logged_in:
            bs.logout()
            self._logged_in = False

    # --- 标的清单 ---

    def instruments(self) -> pd.DataFrame:
        raw = _cursor_to_df(bs.query_stock_basic())
        # type: 1=股票 5=ETF；2=指数 4=可转债 等一律排除（超出当前范围）
        raw = raw[raw["type"].isin([_TYPE_STOCK, _TYPE_ETF])].copy()

        out = pd.DataFrame()
        # BaoStock 的 code 形如 "sh.600519"，规范形态不含交易所前缀
        out["symbol"] = raw["code"].str.split(".").str[-1]
        out["name"] = raw["code_name"].fillna("").astype(str)
        out["type"] = raw["type"].map({_TYPE_STOCK: "stock", _TYPE_ETF: "etf"})
        out["list_date"] = raw["ipoDate"].map(_ymd)
        out["delist_date"] = raw["outDate"].map(_ymd)
        out["listed"] = raw["status"] == "1"
        return out.reset_index(drop=True)

    # --- 交易日历 ---

    def calendar(self, start: str, end: str) -> pd.DataFrame:
        raw = _cursor_to_df(bs.query_trade_dates(start_date=start, end_date=end))
        out = pd.DataFrame()
        out["date"] = raw["calendar_date"].map(_ymd)
        out["is_trading_day"] = raw["is_trading_day"] == "1"
        return out

    # --- 日线 ---

    def daily_bars(self, symbol: str, exchange: str, start: str, end: str) -> pd.DataFrame:
        code = f"{_EXCHANGE_PREFIX[exchange]}.{symbol}"
        raw = _cursor_to_df(bs.query_history_k_data_plus(
            code, _FIELDS, start_date=start, end_date=end,
            frequency="d", adjustflag="3",  # 3 = 不复权，符合约束 C2
        ))
        if raw.empty:
            return pd.DataFrame(columns=[
                "trading_day", "open", "high", "low", "close", "preclose",
                "volume", "amount", "turn", "tradestatus", "is_st",
            ])

        out = pd.DataFrame()
        out["trading_day"] = raw["date"].map(_ymd)
        # 价格保持字符串原样传递，由下游 Decimal 解析，避免 float 往返
        for c in ("open", "high", "low", "close", "preclose"):
            out[c] = raw[c]
        out["volume"] = pd.to_numeric(raw["volume"], errors="coerce").fillna(0).astype("int64")
        out["amount"] = raw["amount"]
        # 停牌行的 turn 为空字符串（v0.0 实测），必须容错
        out["turn"] = raw["turn"]
        out["tradestatus"] = pd.to_numeric(
            raw["tradestatus"], errors="coerce").fillna(0).astype("int8")
        out["is_st"] = pd.to_numeric(raw["isST"], errors="coerce").fillna(0).astype("int8")
        return out.reset_index(drop=True)

    # --- 分红送配 ---

    def corporate_actions(self, symbol: str, exchange: str, year: int) -> pd.DataFrame:
        code = f"{_EXCHANGE_PREFIX[exchange]}.{symbol}"
        raw = _cursor_to_df(bs.query_dividend_data(
            code=code, year=str(year), yearType="report"))
        cols = ["ex_date", "record_date", "pay_date",
                "cash_before_tax", "stock_dividend", "stock_transfer"]
        if raw.empty:
            return pd.DataFrame(columns=cols)

        out = pd.DataFrame()
        out["ex_date"] = raw["dividOperateDate"].map(_ymd)
        out["record_date"] = raw["dividRegistDate"].map(_ymd)
        out["pay_date"] = raw["dividPayDate"].map(_ymd)
        # 只取税前 —— 税后字段含 "27.7884或30.876" 这类非数值字符串（v0.0 实测），
        # 且红利税属规则不属数据，归 Fee 模块（SCHEMA.md 5.1）
        out["cash_before_tax"] = pd.to_numeric(
            raw["dividCashPsBeforeTax"], errors="coerce").fillna(0.0)
        out["stock_dividend"] = pd.to_numeric(
            raw["dividStocksPs"], errors="coerce").fillna(0.0)
        out["stock_transfer"] = pd.to_numeric(
            raw.get("dividReserveToStockPs", pd.Series(dtype=str)),
            errors="coerce").fillna(0.0)
        # 无除权日的记录（预案/未实施）对回测无意义，丢弃
        return out[out["ex_date"] > 0].reset_index(drop=True)
