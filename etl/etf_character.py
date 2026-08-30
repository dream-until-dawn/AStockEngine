r"""按形态给 ETF 分类，并挑出一份去重的横截面标的池。

**不是 ETL 流水线的一环**，不进 sync/build。它是给海选备料的分析：
海选要问「同一套参数换一只标的还成立吗」，而这个问题只有在标的池
既够大、又没有影子标的时才问得出来。

产出两个文件（都在 data/cache/，可随时重算）：

  etf_character.csv   每只 ETF 在特征期与检验期的形态量
  etf_pool.json       去重后的标的池，直接贴进海选配置的 per_symbol

三条纪律，每一条都是踩过之后加的：

1. **特征算在前半段，网格考在后半段。** 用全期特征挑标的、再用全期收益
   报成绩，是把答案抄进了题目里。
2. **近似重复的标的要合并。** 沪深300 有八只壳（510050/159919/510330/
   510310/510180/510360/512990/159925 日收益相关都 ≥ 0.95），全放进横截面
   等于把同一个样本数了九遍，相关系数会被这批影子样本抬起来。
3. **货币 / 短债 ETF 必须挡在外面。** 它们价格几乎是常数，
   「净位移 ÷ 总路程」算出来接近 0，会排在「最震荡」榜的第一名 ——
   而它们连一格都走不到。不设波动率下限的话，
   「最适合网格的标的」前十全是货币基金。

用法：
    .\.venv\Scripts\python.exe etl\etf_character.py
"""


from __future__ import annotations

import glob
import json
import sys
from pathlib import Path

import numpy as np
import pandas as pd

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = Path(__file__).resolve().parents[1]

# 特征期 / 检验期的分界。特征期必须完全在检验期之前 —— 这是全部结论的地基
CHAR_FROM, CHAR_TO = 20170803, 20211231
TEST_FROM, TEST_TO = 20220104, 20260828

MIN_DAYS = 2100           # 全期至少这么多交易日
MIN_AMOUNT = 1e7          # 日成交额中位数下限（元）：太冷的 ETF 冲击成本不可忽略
# 年化波动率下限：**把货币 / 短债 ETF 挡在外面**。
# 它们的价格几乎是常数（货币 ETF 恒定 100 元、收益靠每日分红发出去），
# 于是「净位移 ÷ 总路程」算出来接近 0 —— 在效率比榜上排在最震荡的一端，
# 而实际上它们连一格都走不到，一笔网格都不会触发。
# 不设这条的话，「最适合网格的标的」前十名全是货币基金
MIN_VOL = 0.10
DUP_CORR = 0.95           # 日收益相关系数超过它就算同一个标的的不同壳


def load_bars() -> pd.DataFrame:
    inst = pd.read_parquet(ROOT / "data/meta/instruments.parquet")
    etf = inst[inst.type == 2][["instrument_id", "symbol", "name", "list_date"]]
    ids = set(etf.instrument_id)

    frames = []
    for f in sorted(glob.glob(str(ROOT / "data/bar/market=ashare/freq=1d/year=*/*.parquet"))):
        if int(f.split("year=")[1][:4]) < 2017:
            continue
        d = pd.read_parquet(f, columns=[
            "instrument_id", "trading_day", "close", "amount", "tradestatus"])
        frames.append(d[d.instrument_id.isin(ids)])
    b = pd.concat(frames, ignore_index=True)
    b = b[b.tradestatus == 1]  # 停牌日不参与

    # 后复权。**必须复权**：实测未复权价下 510500 的年化波动率是 72.6%，
    # 而同样跟踪中证500 的 510300 只有 21.8% —— 那个差是分红除权砸出来的坑，
    # 不是标的的波动
    adj = pd.read_parquet(ROOT / "data/meta/adj_factor.parquet")
    adj = adj[adj.instrument_id.isin(ids)][["instrument_id", "ex_date", "hfq_factor_raw"]]
    # merge_asof 要求**左右两边都按 on 键全局排序**，不是按 by 分组内排序
    adj = adj.rename(columns={"ex_date": "trading_day"}).sort_values(
        ["trading_day", "instrument_id"])
    b = b.sort_values(["trading_day", "instrument_id"])
    b = pd.merge_asof(b, adj, on="trading_day", by="instrument_id",
                      direction="backward")
    b["f"] = b.hfq_factor_raw.astype(float).fillna(1.0)
    b["adj_close"] = b.close * b.f
    return b.merge(etf, on="instrument_id")


def characterize(px: pd.Series) -> dict:
    """一只标的在一段区间里的形态。全部是纯技术量，不用任何基本面。"""
    r = np.diff(np.log(px.values))
    if len(r) < 100:
        return {}
    total = px.values[-1] / px.values[0] - 1.0
    # 效率比（Kaufman）：净位移 ÷ 走过的总路程。
    # **这就是「单边 vs 震荡」的直接度量** —— 1 = 一路直线，0 = 原地折返跑。
    # 网格赚的是折返，所以这一项才是它该看的，而不是波动率
    path = np.abs(np.diff(px.values)).sum()
    er = abs(px.values[-1] - px.values[0]) / path if path > 0 else np.nan
    # 整段只算一个 ER，量到的其实是**那个时代**：2017~2021 与 2022~2026
    # 是两种完全不同的行情，同一只标的两段的 ER 差很远也不奇怪。
    # 要问「这只标的一般有多折返」，就得在**滚动窗口**上算再取中位数 ——
    # 那才是标的自己的属性，而不是它碰上了什么行情
    def roll(win: int, fn) -> float:
        vals = [fn(px.values[i - win:i]) for i in range(win, len(px) + 1, win // 4)]
        vals = [v for v in vals if np.isfinite(v)]
        return float(np.median(vals)) if vals else np.nan

    def _er(a):
        path = np.abs(np.diff(a)).sum()
        return abs(a[-1] - a[0]) / path if path > 0 else np.nan

    def _vol(a):
        return np.std(np.diff(np.log(a))) * np.sqrt(243)

    return {
        "ret": float(total),
        "vol": float(r.std() * np.sqrt(243)),
        "er": float(er),
        "rer": roll(243, _er),    # 滚动一年 ER 的中位数
        "rvol": roll(243, _vol),  # 滚动一年年化波动率的中位数
        "ac1": float(pd.Series(r).autocorr(1)),  # 负 = 均值回复
        "maxdd": float((px / px.cummax() - 1).min()),
        "days": int(len(px)),
    }


def main() -> None:
    b = load_bars()
    span = b.groupby("instrument_id").trading_day.agg(["min", "max", "size"])
    keep = span[(span["min"] <= CHAR_FROM) & (span["max"] >= TEST_TO - 300)
                & (span["size"] >= MIN_DAYS)].index
    liq = b[b.instrument_id.isin(keep)].groupby("instrument_id").amount.median()
    keep = [i for i in keep if liq.get(i, 0) >= MIN_AMOUNT]
    print(f"覆盖全期且有流动性的 ETF：{len(keep)} 只")

    b = b[b.instrument_id.isin(keep)]
    char_w = b[(b.trading_day >= CHAR_FROM) & (b.trading_day <= CHAR_TO)]
    test_w = b[(b.trading_day >= TEST_FROM) & (b.trading_day <= TEST_TO)]

    rows = []
    for iid, g in char_w.groupby("instrument_id"):
        c = characterize(g.set_index("trading_day").adj_close)
        if not c:
            continue
        t = characterize(test_w[test_w.instrument_id == iid]
                         .set_index("trading_day").adj_close)
        if c["vol"] < MIN_VOL:
            continue
        meta = b[b.instrument_id == iid].iloc[0]
        # **meta["name"] 不能写成 meta.name** —— meta 是 Series，
        # `.name` 是它自己的索引标签，取出来是行号不是标的名称
        rows.append({"id": int(iid), "symbol": meta["symbol"], "name": meta["name"],
                     "amount": float(liq[iid]),
                     **{f"c_{k}": v for k, v in c.items()},
                     **{f"t_{k}": v for k, v in t.items()}})
    df = pd.DataFrame(rows)
    print(f"特征期/检验期都算得出来、且年化波动率 ≥ {MIN_VOL:.0%} 的：{len(df)} 只")

    # ---- 合并影子标的 ----
    wide = char_w.pivot_table(index="trading_day", columns="instrument_id",
                              values="adj_close").pct_change().dropna(how="all")
    corr = wide[[i for i in df.id if i in wide.columns]].corr()
    order = df.sort_values("amount", ascending=False).id.tolist()
    chosen, dropped = [], {}
    for i in order:
        dup = next((c for c in chosen
                    if i in corr.index and c in corr.columns
                    and corr.loc[i, c] >= DUP_CORR), None)
        if dup is None:
            chosen.append(i)
        else:
            dropped.setdefault(dup, []).append(i)
    df["keep"] = df.id.isin(chosen)
    print(f"去重后：{len(chosen)} 只（合并掉 {len(df) - len(chosen)} 只影子标的，"
          f"日收益相关 ≥ {DUP_CORR}）")

    out = ROOT / "data/cache/etf_character.csv"
    df.to_csv(out, index=False, encoding="utf-8-sig")
    print(f"写入 {out}")

    k = df[df.keep].sort_values("c_er")
    pd.set_option("display.width", 220)
    print("\n特征期（%d~%d）最震荡的 12 只（效率比最低）：" % (CHAR_FROM, CHAR_TO))
    print(k[["symbol", "name", "c_ret", "c_vol", "c_rvol", "c_er", "c_rer"]].head(12).to_string(index=False))
    print("\n特征期最单边的 12 只（效率比最高）：")
    print(k[["symbol", "name", "c_ret", "c_vol", "c_rvol", "c_er", "c_rer"]].tail(12).to_string(index=False))

    print("\n特征在两段之间稳不稳（同一只标的，前半段 vs 后半段）：")
    for col in ["vol", "rvol", "er", "rer", "ac1"]:
        cc = k[f"c_{col}"].corr(k[f"t_{col}"])
        print(f"  {col:4s}  corr(特征期, 检验期) = {cc:+.3f}")

    json.dump({"chosen": [df[df.id == i].symbol.iloc[0] for i in chosen],
               "dropped": {df[df.id == k_].symbol.iloc[0]:
                           [df[df.id == v].symbol.iloc[0] for v in vs]
                           for k_, vs in dropped.items()}},
              open(ROOT / "data/cache/etf_pool.json", "w", encoding="utf-8"),
              ensure_ascii=False, indent=1)


if __name__ == "__main__":
    main()
