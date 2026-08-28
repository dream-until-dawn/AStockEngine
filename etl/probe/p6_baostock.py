"""探针 P6：BaoStock 作为备用/主力数据源。

引入动机来自 P1/P2 的实测结果：

  - 东财是退市股历史行情的**唯一**来源，但 API 限流极严（约 10 次请求即封 IP）
  - 新浪原始价与复权因子质量优秀，但**完全没有退市股数据**

若全量 ETL 必须依赖一个 10 次就封禁的接口，v0.1 的数据地基就是不可行的。
BaoStock 免费、无需注册，且其字段设计恰好覆盖 AkShare 的三个已知缺口：

  tradestatus  —— 停牌标记（AkShare 只能靠「日线缺行」间接推断）
  isST         —— 逐日 ST 状态（AkShare 只有当前 ST 快照，无历史区间）
  query_all_stock(day=) —— point-in-time 股票列表（对应约束 C3）

本探针验证这些字段是否真实可用，以及退市股与分钟线的覆盖情况。
"""

from __future__ import annotations

import baostock as bs
import pandas as pd

from common import Probe

DAILY_FIELDS = (
    "date,code,open,high,low,close,preclose,volume,amount,"
    "adjustflag,turn,tradestatus,pctChg,isST"
)
MINUTE_FIELDS = "date,time,code,open,high,low,close,volume,amount,adjustflag"


def _to_df(rs) -> pd.DataFrame:
    """BaoStock 返回的是游标对象，统一转成 DataFrame。"""
    rows = []
    while rs.error_code == "0" and rs.next():
        rows.append(rs.get_row_data())
    return pd.DataFrame(rows, columns=rs.fields)


def main() -> None:
    probe = Probe("p6_baostock", "P6 BaoStock 备用数据源")

    login = bs.login()
    print(f"login: code={login.error_code} msg={login.error_msg}", flush=True)

    def daily_with_flags():
        """核心验证：一次请求同时拿到原始价、前收盘、停牌标记与 ST 标记。"""
        rs = bs.query_history_k_data_plus(
            "sh.600519", DAILY_FIELDS,
            start_date="2024-01-01", end_date="2024-03-31",
            frequency="d", adjustflag="3",  # 3=不复权
        )
        df = _to_df(rs)
        cols = set(df.columns)
        return {
            "error_code": rs.error_code,
            "rows": int(len(df)),
            "columns": list(df.columns),
            "has_tradestatus": "tradestatus" in cols,
            "has_isST": "isST" in cols,
            "has_preclose": "preclose" in cols,
            "head": df.head(2).to_dict(orient="records"),
            "ok": len(df) > 0 and {"tradestatus", "isST", "preclose"}.issubset(cols),
        }

    probe.check("bs.daily_flags", "日线含停牌/ST/前收盘标记", daily_with_flags)

    def delisted_coverage():
        """退市股覆盖：若 BaoStock 能给，就不必为退市股死磕限流严重的东财。"""
        samples = {
            "sz.300104": ("乐视网", "2019-07-01", "2020-07-21"),
            "sz.002680": ("长生生物", "2019-01-01", "2019-11-27"),
            "sh.600074": ("保千里", "2018-08-27", "2019-08-27"),
        }
        out = {}
        for code, (name, start, end) in samples.items():
            rs = bs.query_history_k_data_plus(
                code, DAILY_FIELDS, start_date=start, end_date=end,
                frequency="d", adjustflag="3",
            )
            df = _to_df(rs)
            out[code] = {
                "name": name,
                "error_code": rs.error_code,
                "rows": int(len(df)),
                "earliest": str(df["date"].iloc[0]) if len(df) else None,
                "latest": str(df["date"].iloc[-1]) if len(df) else None,
            }
        got = sum(1 for v in out.values() if v["rows"] > 0)
        return {"samples": out, "with_data": got, "ok": got > 0}

    probe.check("bs.delisted", "退市股历史行情覆盖", delisted_coverage)

    def stock_basic():
        """上市/退市日期与在市状态——构建完整股票宇宙的基础。"""
        rs = bs.query_stock_basic(code="sh.600519")
        df = _to_df(rs)
        rs_all = bs.query_stock_basic()
        df_all = _to_df(rs_all)
        delisted = df_all[df_all["status"] == "0"] if "status" in df_all.columns else pd.DataFrame()
        return {
            "single_columns": list(df.columns),
            "single_head": df.head(1).to_dict(orient="records"),
            "universe_rows": int(len(df_all)),
            "delisted_rows": int(len(delisted)),
            "delisted_sample": delisted.head(3).to_dict(orient="records") if len(delisted) else [],
            "ok": len(df_all) > 0,
        }

    probe.check("bs.stock_basic", "股票基本信息与在市状态", stock_basic)

    def point_in_time_universe():
        """约束 C3 要求 point-in-time 股票池：某个历史日期当天到底有哪些股票在交易。"""
        rs = bs.query_all_stock(day="2019-06-28")
        df = _to_df(rs)
        return {
            "error_code": rs.error_code,
            "rows": int(len(df)),
            "columns": list(df.columns),
            "head": df.head(3).to_dict(orient="records"),
            "note": "可按任意历史日回放当日股票池，直接支撑 C3 幸存者偏差防护",
            "ok": len(df) > 3000,
        }

    probe.check("bs.pit_universe", "历史某日的 point-in-time 股票池", point_in_time_universe)

    def minute_depth():
        """分钟线深度——对应 v0.5。BaoStock 无 1 分钟，最细为 5 分钟。"""
        out = {}
        for freq in ("5", "15", "30", "60"):
            rs = bs.query_history_k_data_plus(
                "sh.600519", MINUTE_FIELDS,
                start_date="2015-01-01", end_date="2026-08-28",
                frequency=freq, adjustflag="3",
            )
            df = _to_df(rs)
            out[f"{freq}min"] = {
                "error_code": rs.error_code,
                "rows": int(len(df)),
                "earliest": str(df["date"].iloc[0]) if len(df) else None,
                "latest": str(df["date"].iloc[-1]) if len(df) else None,
            }
        deep = sum(1 for v in out.values() if v["rows"] > 10000)
        return {"by_freq": out, "deep_freqs": deep, "ok": deep > 0}

    probe.check("bs.minute_depth", "分钟线历史深度（5/15/30/60）", minute_depth)

    def calendar_and_dividend():
        rs_cal = bs.query_trade_dates(start_date="2015-01-01", end_date="2026-12-31")
        cal = _to_df(rs_cal)
        rs_div = bs.query_dividend_data(code="sh.600519", year="2024", yearType="report")
        div = _to_df(rs_div)
        return {
            "calendar_rows": int(len(cal)),
            "calendar_columns": list(cal.columns),
            "trading_days": int((cal["is_trading_day"] == "1").sum()) if "is_trading_day" in cal.columns else None,
            "dividend_rows": int(len(div)),
            "dividend_columns": list(div.columns),
            "dividend_head": div.head(2).to_dict(orient="records"),
            "ok": len(cal) > 0,
        }

    probe.check("bs.calendar_dividend", "交易日历与分红数据", calendar_and_dividend)

    def rate_limit():
        """BaoStock 是长连接协议，重点确认连续请求是否会被限流。"""
        import time

        started = time.perf_counter()
        ok = 0
        for i in range(30):
            rs = bs.query_history_k_data_plus(
                "sh.600519", "date,close",
                start_date="2024-01-01", end_date="2024-01-31",
                frequency="d", adjustflag="3",
            )
            df = _to_df(rs)
            if rs.error_code == "0" and len(df) > 0:
                ok += 1
            else:
                return {"ok": False, "succeeded_before_block": ok, "error_code": rs.error_code}
        elapsed = time.perf_counter() - started
        return {
            "requests": 30,
            "succeeded": ok,
            "elapsed_seconds": round(elapsed, 2),
            "req_per_second": round(30 / elapsed, 2),
            "note": "无间隔连续 30 次请求的表现，与东财约 10 次即封禁对比",
            "ok": ok == 30,
        }

    probe.check("bs.rate_limit", "连续 30 次请求的限流表现", rate_limit)

    bs.logout()
    probe.save()


if __name__ == "__main__":
    main()
