"""探针 P7：ETF 数据可用性。

范围新增项。ETF 在数据形态上与个股接近（同为二级市场逐日 OHLCV），
但在**交易规则**上有若干必须区分的差异，这些差异决定 Fee / Market 模块
不能对「标的类型」一视同仁：

  免印花税     卖出 ETF 不收 0.05% 印花税（个股要收）—— 影响最大的一条
  免过户费     场内基金无过户费（个股沪深双向 0.001%）
  T+0          跨境 / 黄金 / 债券 ETF 支持当日回转；股票型 ETF 仍是 T+1
  涨跌停       跟踪创业板 / 科创板指数的 ETF 为 20%，其余 10%
  最小单位     100 份起，以 100 份为单位

本探针只验证**数据侧**：ETF 的日线、列表、复权与分红能否拿到。
规则侧差异属于 Market/Fee 模块的实现，记录在 ROADMAP 而非探针。
"""

from __future__ import annotations

import akshare as ak
import baostock as bs
import pandas as pd

from common import Probe, describe_df, polite_sleep, retry, signature_of

# 覆盖不同类型：宽基、创业板（20% 涨跌停）、跨境（T+0）、商品（T+0）
ETFS = [
    ("510300", "sh.510300", "sh510300", "沪深300ETF", "宽基"),
    ("159915", "sz.159915", "sz159915", "创业板ETF", "20%涨跌停"),
    ("513100", "sh.513100", "sh513100", "纳指ETF", "跨境T+0"),
    ("518880", "sh.518880", "sh518880", "黄金ETF", "商品T+0"),
]

DAILY_FIELDS = (
    "date,code,open,high,low,close,preclose,volume,amount,"
    "adjustflag,turn,tradestatus,pctChg,isST"
)


def _to_df(rs) -> pd.DataFrame:
    rows = []
    while rs.error_code == "0" and rs.next():
        rows.append(rs.get_row_data())
    return pd.DataFrame(rows, columns=rs.fields)


def main() -> None:
    probe = Probe("p7_etf", "P7 ETF 数据可用性")

    login = bs.login()
    print(f"login: code={login.error_code} msg={login.error_msg}", flush=True)

    def bs_etf_daily():
        """BaoStock 已确认是个股的主力源，首先确认它是否同样覆盖 ETF。"""
        out = {}
        for _, bs_code, _, name, kind in ETFS:
            rs = bs.query_history_k_data_plus(
                bs_code, DAILY_FIELDS,
                start_date="2024-01-01", end_date="2024-03-31",
                frequency="d", adjustflag="3",
            )
            df = _to_df(rs)
            out[bs_code] = {
                "name": name,
                "kind": kind,
                "error_code": rs.error_code,
                "rows": int(len(df)),
                "head": df.head(1).to_dict(orient="records") if len(df) else [],
            }
        got = sum(1 for v in out.values() if v["rows"] > 0)
        return {"samples": out, "with_data": got, "ok": got == len(ETFS)}

    probe.check("bs.etf_daily", "BaoStock 是否覆盖 ETF 日线", bs_etf_daily)

    def bs_etf_basic():
        """type 字段用于区分股票/指数/ETF，是构建标的宇宙的分类依据。"""
        out = {}
        for _, bs_code, _, name, _ in ETFS:
            rs = bs.query_stock_basic(code=bs_code)
            df = _to_df(rs)
            out[bs_code] = {
                "name": name,
                "rows": int(len(df)),
                "record": df.head(1).to_dict(orient="records") if len(df) else [],
            }
        return {
            "samples": out,
            "note": "确认 ETF 在 query_stock_basic 中的 type 取值，用于标的分类",
            "ok": any(v["rows"] > 0 for v in out.values()),
        }

    probe.check("bs.etf_basic", "BaoStock ETF 基本信息与 type 分类", bs_etf_basic)

    bs.logout()

    def sina_etf_daily():
        """新浪是复权因子的来源，确认 ETF 是否同样支持。"""
        out = {}
        for _, _, sina_code, name, _ in ETFS:
            try:
                df = retry(lambda c=sina_code: ak.fund_etf_hist_sina(symbol=c))
                out[sina_code] = {
                    "name": name,
                    "rows": int(len(df)),
                    "columns": list(df.columns),
                    "earliest": str(df.iloc[0, 0]) if len(df) else None,
                    "latest": str(df.iloc[-1, 0]) if len(df) else None,
                }
            except Exception as exc:  # noqa: BLE001
                out[sina_code] = {"name": name, "error": f"{type(exc).__name__}: {exc}"}
            polite_sleep(0.6)
        got = sum(1 for v in out.values() if v.get("rows", 0) > 0)
        return {
            "samples": out,
            "signature": signature_of(ak.fund_etf_hist_sina),
            "with_data": got,
            "ok": got > 0,
        }

    probe.check("sina.etf_daily", "新浪 ETF 日线", sina_etf_daily)
    polite_sleep()

    def sina_etf_list():
        df = retry(lambda: ak.fund_etf_category_sina(symbol="ETF基金"))
        info = describe_df(df)
        info["signature"] = signature_of(ak.fund_etf_category_sina)
        info["ok"] = info.get("rows", 0) > 100
        return info

    probe.check("sina.etf_list", "新浪 ETF 全量列表", sina_etf_list)
    polite_sleep()

    def etf_adjust_factor():
        """ETF 也分红，同样需要复权。确认因子接口是否覆盖 ETF 代码。"""
        out = {}
        for _, _, sina_code, name, _ in ETFS[:2]:
            try:
                df = retry(lambda c=sina_code: ak.stock_zh_a_daily(symbol=c, adjust="hfq-factor"))
                out[sina_code] = {
                    "name": name,
                    "rows": int(len(df)),
                    "head": df.head(2).to_dict(orient="records") if len(df) else [],
                }
            except Exception as exc:  # noqa: BLE001
                out[sina_code] = {"name": name, "error": f"{type(exc).__name__}: {exc}"}
            polite_sleep(0.6)
        got = sum(1 for v in out.values() if v.get("rows", 0) > 0)
        return {
            "samples": out,
            "with_factor": got,
            "note": "若不支持，ETF 的复权需由 BaoStock 复权价反推或另寻分红数据",
            "ok": got > 0,
        }

    probe.check("sina.etf_factor", "ETF 复权因子可得性", etf_adjust_factor)

    probe.save()


if __name__ == "__main__":
    main()
