"""探针 P4：日线字段完整性、交易日历、停牌与涨跌停。

对应 ROADMAP 约束 C8（A 股交易规则）与 v0.1 的 schema 定义。

要回答的问题：
  1. 交易日历能否拿到，覆盖多久
  2. 日线接口给的字段够不够填满 bar_daily
  3. 涨跌停价：接口不直接提供，能否由前收盘价算出并与实盘一致
  4. 停牌：数据源是「停牌日不返回行」还是「返回零成交行」——决定
     bar_daily 是否需要独立的停牌标记表
"""

from __future__ import annotations

import akshare as ak
import pandas as pd

from common import Probe, describe_df, polite_sleep, retry, signature_of


def main() -> None:
    probe = Probe("p4_daily_fields", "P4 日线字段与交易规则")

    def calendar():
        df = retry(lambda: ak.tool_trade_date_hist_sina())
        info = describe_df(df)
        info["signature"] = signature_of(ak.tool_trade_date_hist_sina)
        if info.get("rows"):
            col = df.columns[0]
            info["earliest"] = str(df[col].iloc[0])
            info["latest"] = str(df[col].iloc[-1])
        info["ok"] = info.get("rows", 0) > 5000
        return info

    probe.check("calendar.sina", "交易日历覆盖范围", calendar)
    polite_sleep()

    def sina_daily_fields():
        """新浪是在市股票的原始价来源，确认其字段能否填满 bar_daily。"""
        df = retry(lambda: ak.stock_zh_a_daily(
            symbol="sh600519", start_date="20240101", end_date="20240331", adjust=""
        ))
        cols = set(df.columns)
        required = {"open", "high", "low", "close", "volume", "amount"}
        return {
            "columns": list(df.columns),
            "rows": int(len(df)),
            "has_required_ohlcv": required.issubset(cols),
            "missing": sorted(required - cols),
            "extra": sorted(cols - required),
            "note": "outstanding_share/turnover 为额外收益，可直接支撑换手率与市值计算",
            "ok": required.issubset(cols),
        }

    probe.check("fields.sina_daily", "新浪日线字段完整性", sina_daily_fields)
    polite_sleep()

    def em_daily_fields():
        df = retry(lambda: ak.stock_zh_a_hist(
            symbol="600519", period="daily",
            start_date="20240101", end_date="20240331", adjust="",
        ))
        cols = list(df.columns)
        return {
            "columns": cols,
            "rows": int(len(df)),
            "has_limit_price": any("涨停" in c or "跌停" in c for c in cols),
            "note": "东财日线不含涨跌停价，需由前收盘价按板块规则自行计算",
            "ok": len(df) > 0,
        }

    probe.check("fields.em_daily", "东财日线字段完整性", em_daily_fields)
    polite_sleep()

    def limit_price_rule():
        """用真实涨停日反推涨跌停价的计算规则是否为「前收 ×(1±幅) 四舍五入到分」。

        涨停日的特征是 最高 == 最低 == 收盘（一字板）或 收盘 == 涨停价。
        这里挑一段行情自行计算并与实际收盘比对，验证取整规则。
        """
        df = retry(lambda: ak.stock_zh_a_hist(
            symbol="600519", period="daily",
            start_date="20200101", end_date="20241231", adjust="",
        ))
        df = df.sort_values("日期").reset_index(drop=True)
        prev_close = df["收盘"].shift(1)
        # 主板 10%，四舍五入到分
        limit_up = (prev_close * 1.10).round(2)
        limit_down = (prev_close * 0.90).round(2)
        hit_up = df["收盘"].round(2) == limit_up
        hit_down = df["收盘"].round(2) == limit_down
        over = (df["收盘"] > limit_up + 1e-9).sum()
        under = (df["收盘"] < limit_down - 1e-9).sum()
        return {
            "days": int(len(df)),
            "limit_up_days": int(hit_up.sum()),
            "limit_down_days": int(hit_down.sum()),
            "close_above_computed_limit_up": int(over),
            "close_below_computed_limit_down": int(under),
            "note": "越界计数应为 0；非 0 说明取整规则或幅度判定有误",
            "ok": int(over) == 0 and int(under) == 0,
        }

    probe.check("rule.limit_price", "涨跌停价计算规则验证", limit_price_rule)
    polite_sleep()

    def suspension_representation():
        """停牌在数据里的表现形式，决定是否需要独立停牌表。

        取一只有长期停牌史的股票，把日线日期与交易日历求差集。
        """
        cal = retry(lambda: ak.tool_trade_date_hist_sina())
        polite_sleep()
        # 600074 保千里 2019-04 起停牌至退市，是明确的长停样本
        df = retry(lambda: ak.stock_zh_a_hist(
            symbol="600074", period="daily",
            start_date="20190101", end_date="20190827", adjust="",
        ))
        cal_col = cal.columns[0]
        cal_days = pd.to_datetime(cal[cal_col]).dt.date
        cal_days = set(d for d in cal_days if pd.Timestamp("2019-01-01").date() <= d <= pd.Timestamp("2019-08-27").date())
        bar_days = set(pd.to_datetime(df["日期"]).dt.date)
        missing = sorted(cal_days - bar_days)
        zero_volume = int((df["成交量"] == 0).sum())
        return {
            "calendar_days": len(cal_days),
            "bar_days": len(bar_days),
            "missing_days": len(missing),
            "missing_sample": [str(d) for d in missing[:5]],
            "zero_volume_rows": zero_volume,
            "note": "缺行而非零成交行 => bar_daily 需配独立停牌标记，否则无法区分停牌与数据缺失",
            "ok": True,
        }

    probe.check("rule.suspension", "停牌在数据中的表现形式", suspension_representation)
    polite_sleep()

    def suspension_table():
        df = retry(lambda: ak.stock_tfp_em(date="20240426"))
        info = describe_df(df)
        info["signature"] = signature_of(ak.stock_tfp_em)
        info["ok"] = info.get("rows", 0) > 0
        return info

    probe.check("rule.suspension_table", "东财停复牌接口可用性", suspension_table)

    probe.save()


if __name__ == "__main__":
    main()
