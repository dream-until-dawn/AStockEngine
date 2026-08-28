"""探针 P1：股票池与退市股。

对应 ROADMAP 约束 C3（幸存者偏差）。若拿不到退市股的历史行情，
回测收益会系统性虚高，整套系统的结论不可信 —— 这是最关键的一项验证。

判定标准：
  - 能拿到退市股名单（含退市日期）
  - 能拿到退市股退市前的历史日线
两者缺一，就必须引入备用数据源（Tushare Pro / BaoStock）。
"""

from __future__ import annotations

import akshare as ak

from common import Probe, describe_df, signature_of

# 已知退市样本，覆盖沪深两市与不同退市年份
DELISTED_SAMPLES = [
    ("300104", "sz300104", "乐视网", "2020-07-21"),
    ("002680", "sz002680", "长生生物", "2019-11-27"),
    ("000979", "sz000979", "中弘股份", "2018-12-28"),
    ("600074", "sh600074", "保千里", "2019-08-27"),
]


def main() -> None:
    probe = Probe("p1_universe_delisted", "P1 股票池与退市股")

    def full_universe():
        df = ak.stock_info_a_code_name()
        info = describe_df(df)
        info["signature"] = signature_of(ak.stock_info_a_code_name)
        # 只有在读到实际数量后才知道是否只覆盖在市股票
        info["ok"] = info.get("rows", 0) > 4000
        return info

    probe.check("universe.a_code_name", "全 A 股代码名称列表", full_universe)

    def sh_delist():
        # 该接口签名在不同版本间变动过，两种调用方式都试
        try:
            df = ak.stock_info_sh_delist()
        except TypeError:
            df = ak.stock_info_sh_delist(symbol="终止上市公司")
        info = describe_df(df)
        info["signature"] = signature_of(ak.stock_info_sh_delist)
        info["ok"] = info.get("rows", 0) > 0
        return info

    probe.check("universe.sh_delist", "上交所终止上市公司名单", sh_delist)

    def sz_delist():
        try:
            df = ak.stock_info_sz_delist(symbol="终止上市公司")
        except TypeError:
            df = ak.stock_info_sz_delist()
        info = describe_df(df)
        info["signature"] = signature_of(ak.stock_info_sz_delist)
        info["ok"] = info.get("rows", 0) > 0
        return info

    probe.check("universe.sz_delist", "深交所终止上市公司名单", sz_delist)

    # 名单只是第一步，真正决定成败的是能否取到退市股的历史行情
    for code, sina_code, name, delist_date in DELISTED_SAMPLES:
        end = delist_date.replace("-", "")
        start = f"{int(end[:4]) - 1}{end[4:]}"

        def em_hist(code=code, start=start, end=end):
            df = ak.stock_zh_a_hist(
                symbol=code, period="daily", start_date=start, end_date=end, adjust=""
            )
            info = describe_df(df)
            info["ok"] = info.get("rows", 0) > 0
            return info

        probe.check(
            f"delisted.em.{code}", f"东财取 {name} 退市前一年日线", em_hist
        )

        def sina_hist(sina_code=sina_code, start=start, end=end):
            df = ak.stock_zh_a_daily(
                symbol=sina_code, start_date=start, end_date=end, adjust=""
            )
            info = describe_df(df)
            info["ok"] = info.get("rows", 0) > 0
            return info

        probe.check(
            f"delisted.sina.{code}", f"新浪取 {name} 退市前一年日线", sina_hist
        )

    def listing_date():
        df = ak.stock_individual_info_em(symbol="600519")
        info = describe_df(df, sample_rows=12)
        info["signature"] = signature_of(ak.stock_individual_info_em)
        return info

    probe.check("universe.listing_date", "个股基本信息（含上市时间）", listing_date)

    def current_st():
        df = ak.stock_zh_a_st_em()
        info = describe_df(df)
        # 只能拿到当前 ST 名单；历史 ST 区间是已知缺口，在报告中标注
        info["note"] = "仅当前 ST 快照，历史 ST 起止区间需另寻来源"
        info["ok"] = info.get("rows", 0) > 0
        return info

    probe.check("universe.current_st", "当前 ST 股名单", current_st)

    probe.save()


if __name__ == "__main__":
    main()
