"""探针 P3：分钟线历史深度。

对应 ROADMAP v0.5。选型时已确认要支持分钟线，但免费源普遍只提供近期数据。
本探针测各接口各周期的**实际可回溯深度**，据此决定：

  - 若深度足够（数年）→ 分钟线可像日线一样批量回补
  - 若深度很浅（数日/数周）→ 必须改为每日增量落盘、长期积累，
    且 v0.5 之前的分钟级回测只能在有限区间内进行

两个候选接口：
  新浪 stock_zh_a_minute       —— 不接受日期范围，返回其保留的全部数据
  东财 stock_zh_a_hist_min_em  —— 接受日期范围，但 1 分钟周期通常只留近 5 个交易日
"""

from __future__ import annotations

import akshare as ak

from common import Probe, describe_df, polite_sleep, retry, signature_of

SYMBOL_EM = "600519"
SYMBOL_SINA = "sh600519"
PERIODS = ["1", "5", "15", "30", "60"]
# 起点取得足够早，用于观察接口实际能给到多早
EM_START = "2015-01-01 09:30:00"
EM_END = "2026-08-28 15:00:00"


def _span(df, col_candidates=("day", "时间", "date", "datetime")) -> dict:
    """提取时间跨度摘要——不同接口的时间列名不一致，逐个尝试。"""
    if df is None or len(df) == 0:
        return {"empty": True}
    col = next((c for c in col_candidates if c in df.columns), df.columns[0])
    first, last = str(df[col].iloc[0]), str(df[col].iloc[-1])
    return {
        "rows": int(len(df)),
        "time_column": col,
        "earliest": first,
        "latest": last,
        "columns": list(df.columns),
    }


def main() -> None:
    probe = Probe("p3_minute_depth", "P3 分钟线历史深度")

    for period in PERIODS:
        def sina_minute(period=period):
            df = retry(lambda: ak.stock_zh_a_minute(
                symbol=SYMBOL_SINA, period=period, adjust=""
            ))
            info = _span(df)
            info["signature"] = signature_of(ak.stock_zh_a_minute)
            info["ok"] = not info.get("empty", False)
            return info

        probe.check(f"sina.min{period}", f"新浪 {period} 分钟线深度", sina_minute)
        polite_sleep()

    for period in PERIODS:
        def em_minute(period=period):
            df = retry(lambda: ak.stock_zh_a_hist_min_em(
                symbol=SYMBOL_EM, period=period, adjust="",
                start_date=EM_START, end_date=EM_END,
            ))
            info = _span(df)
            info["signature"] = signature_of(ak.stock_zh_a_hist_min_em)
            info["ok"] = not info.get("empty", False)
            return info

        probe.check(f"em.min{period}", f"东财 {period} 分钟线深度", em_minute)
        polite_sleep(1.5)

    def em_minute_adjusted():
        """分钟线是否支持复权——若不支持，分钟级回测需自行套用日线因子。"""
        df = retry(lambda: ak.stock_zh_a_hist_min_em(
            symbol=SYMBOL_EM, period="5", adjust="hfq",
            start_date="2024-01-02 09:30:00", end_date="2024-01-05 15:00:00",
        ))
        info = _span(df)
        info["ok"] = not info.get("empty", False)
        return info

    probe.check("em.min_adjust", "东财分钟线是否支持后复权", em_minute_adjusted)

    probe.save()


if __name__ == "__main__":
    main()
