"""探针 P2：复权因子。

对应 ROADMAP 约束 C2。设计要求是「存原始价 + 复权因子」，
因此必须确认因子的获取路径。两条候选路径：

  路径 A（首选）：新浪 stock_zh_a_daily(adjust="hfq-factor") 直接给因子
  路径 B（退路）：东财 adjust="" 与 adjust="hfq" 两套价格相除反推因子

P1 中新浪接口对退市股全部报 JSONDecodeError，所以这里第一件事是确认
新浪接口对**在市股票**是否可用 —— 区分「接口整体不可用」与「仅退市股无数据」。

路径 B 的关键风险是精度：东财返回的复权价只有 2 位小数，
相除反推的因子会带量化噪声，需要实测噪声量级是否可接受。
"""

from __future__ import annotations

import akshare as ak

from common import Probe, describe_df, polite_sleep, retry, signature_of

LIQUID = "600519"  # 贵州茅台：长期在市、每年分红，适合观察除权跳变
LIQUID_SINA = "sh600519"
START, END = "20220101", "20251231"


def main() -> None:
    probe = Probe("p2_adjust_factor", "P2 复权因子")

    # ---- 路径 A：新浪直接给因子 ----

    def sina_baseline():
        """先用在市股票探接口本身是否活着。"""
        df = retry(lambda: ak.stock_zh_a_daily(
            symbol=LIQUID_SINA, start_date=START, end_date=END, adjust=""
        ))
        info = describe_df(df)
        info["signature"] = signature_of(ak.stock_zh_a_daily)
        info["ok"] = info.get("rows", 0) > 0
        return info

    probe.check("sina.baseline", "新浪日线接口对在市股票是否可用", sina_baseline)
    polite_sleep()

    for adj in ("hfq-factor", "qfq-factor"):
        def factor(adj=adj):
            df = retry(lambda: ak.stock_zh_a_daily(symbol=LIQUID_SINA, adjust=adj))
            info = describe_df(df, sample_rows=3)
            info["ok"] = info.get("rows", 0) > 0
            return info

        probe.check(f"sina.{adj}", f"新浪直接获取 {adj}", factor)
        polite_sleep()

    # ---- 路径 B：东财两套价格反推因子 ----

    def em_reverse_factor():
        """用 hfq/raw 相除反推因子，并量化 2 位小数带来的噪声。"""
        raw = retry(lambda: ak.stock_zh_a_hist(
            symbol=LIQUID, period="daily", start_date=START, end_date=END, adjust=""
        ))
        polite_sleep()
        hfq = retry(lambda: ak.stock_zh_a_hist(
            symbol=LIQUID, period="daily", start_date=START, end_date=END, adjust="hfq"
        ))

        merged = raw[["日期", "收盘"]].merge(
            hfq[["日期", "收盘"]], on="日期", suffixes=("_raw", "_hfq")
        )
        merged["factor"] = merged["收盘_hfq"] / merged["收盘_raw"]

        # 因子在两次除权之间应严格恒定；实测跳变次数与噪声量级
        f = merged["factor"]
        rel_change = (f.diff() / f.shift(1)).abs().dropna()
        # 真实除权跳变通常 >0.1%，量化噪声远小于此，以此为界区分两者
        real_jumps = int((rel_change > 1e-3).sum())
        noise = rel_change[rel_change <= 1e-3]

        info = {
            "rows": int(len(merged)),
            "factor_first": float(f.iloc[0]),
            "factor_last": float(f.iloc[-1]),
            "distinct_factors": int(f.round(6).nunique()),
            "real_jump_count": real_jumps,
            "noise_max_rel": float(noise.max()) if len(noise) else 0.0,
            "noise_mean_rel": float(noise.mean()) if len(noise) else 0.0,
            "sample": merged.head(3).to_dict(orient="records"),
        }
        # 噪声若超过 1e-5，说明反推因子不足以支撑逐笔可复现的回测
        info["ok"] = info["noise_max_rel"] < 1e-5
        info["verdict"] = (
            "反推因子精度可用" if info["ok"]
            else f"反推因子存在量化噪声（最大相对抖动 {info['noise_max_rel']:.2e}）"
        )
        return info

    probe.check("em.reverse_factor", "东财 raw/hfq 反推因子及精度", em_reverse_factor)
    polite_sleep()

    def em_qfq_drift():
        """验证 C2 的核心论断：前复权价格会随时间变化，不可用于存储。"""
        qfq = retry(lambda: ak.stock_zh_a_hist(
            symbol=LIQUID, period="daily", start_date=START, end_date=END, adjust="qfq"
        ))
        polite_sleep()
        raw = retry(lambda: ak.stock_zh_a_hist(
            symbol=LIQUID, period="daily", start_date=START, end_date=END, adjust=""
        ))
        m = qfq[["日期", "收盘"]].merge(raw[["日期", "收盘"]], on="日期", suffixes=("_qfq", "_raw"))
        # 前复权的锚点是最后一天：最后一天 qfq == raw，越早的日期偏离越大
        last_diff = abs(float(m["收盘_qfq"].iloc[-1]) - float(m["收盘_raw"].iloc[-1]))
        first_ratio = float(m["收盘_qfq"].iloc[0] / m["收盘_raw"].iloc[0])
        return {
            "last_day_qfq_equals_raw": last_diff < 0.01,
            "last_day_abs_diff": last_diff,
            "first_day_qfq_over_raw": first_ratio,
            "note": "锚定在最后一日 => 每次新除权都会改写全部历史前复权价，故只能存 raw+factor",
        }

    probe.check("em.qfq_drift", "确认前复权锚定末日（不可持久化）", em_qfq_drift)
    polite_sleep()

    def dividend_detail():
        df = retry(lambda: ak.stock_history_dividend_detail(
            symbol=LIQUID, indicator="分红"
        ))
        info = describe_df(df, sample_rows=3)
        info["signature"] = signature_of(ak.stock_history_dividend_detail)
        info["ok"] = info.get("rows", 0) > 0
        return info

    probe.check("em.dividend_detail", "分红送配明细（除权日/派息/送转）", dividend_detail)

    probe.save()


if __name__ == "__main__":
    main()
