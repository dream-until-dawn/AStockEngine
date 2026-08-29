"""推断 ETF 跟踪的板块，写回 instruments.tracked_board。

为什么需要这个字段：ETF 的涨跌停幅度由其**跟踪的指数**决定 ——
跟踪创业板 / 科创板指数的 ETF 为 20%，其余为 10%。而 `instruments.board`
对 ETF 统一记为主板（代码段无法可靠区分），这是 ETL.md 记录的已知缺口 #4。

v0.2 的 Market 模块需要这个字段才能正确判定 ETF 的涨跌停，
故由 ETL 补齐 —— **让数据回答数据的问题**，比在引擎里维护一张硬编码表更耐久。

判定方法（价格实证为主、名称为辅）：

  1. **价格实证**：该 ETF 历史上是否出现过 |涨跌幅| > 10.5% 的交易日。
     出现过即证明其限制不是 10%。这是最硬的证据 —— 本项目已有先例：
     302132 中航成飞的板块归属正是靠涨跌幅实证纠正的（ETL.md 6.4）。
  2. **名称关键词**：价格证据不足时（该 ETF 从未大幅波动）的兜底。
  3. **冲突报告**：两者不一致时逐条列出，由人工裁决。

排除项：上市前 5 个交易日与复权事件日 —— 前者不设涨跌幅限制，
后者的 preclose 经过调整，均不能作为证据。

用法：
    .\\.venv\\Scripts\\python.exe etl\\infer_etf_board.py
    .\\.venv\\Scripts\\python.exe etl\\infer_etf_board.py --dry-run
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入，见 etl/_venv_guard.py
import sys as _sys
from pathlib import Path as _Path
_sys.path.insert(0, str(_Path(__file__).resolve().parent))
import _venv_guard  # noqa: F401,E402

import argparse
import sys
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402

# 名称含这些词的 ETF 跟踪创业板 / 科创板指数，涨跌停为 20%。
# 刻意用精确词而非「创新」这类宽泛词 —— 后者会把「创新药 ETF」误判进来。
_STAR_WORDS = ("科创",)
_CHINEXT_WORDS = ("创业板", "创业成长", "创成长", "创50", "创200")
_DUAL_WORDS = ("双创",)  # 双创 = 创业板 + 科创板，同为 20%

# 涨跌幅超过该阈值即证明限制不是 10%。
# 留 0.5 个百分点容差：涨跌停价四舍五入到分，低价 ETF 上一分钱就是数个百分点
# （ETL.md 6.3）。
_OVER_10 = 10.5
# 上市初期不设涨跌幅限制，一律排除
_IPO_FREE_DAYS = 5


def name_hint(name: str) -> int:
    """由名称推断跟踪板块。返回 sc.Board 值，0 表示无法判断。"""
    if any(w in name for w in _STAR_WORDS):
        return int(sc.Board.STAR)
    if any(w in name for w in _CHINEXT_WORDS):
        return int(sc.Board.CHINEXT)
    if any(w in name for w in _DUAL_WORDS):
        # 双创同时含创业板与科创板成分，两者均为 20%，记为创业板即可
        return int(sc.Board.CHINEXT)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description="推断 ETF 跟踪板块")
    ap.add_argument("--dry-run", action="store_true", help="只报告不写回")
    ap.add_argument("--show", type=int, default=20, help="最多展示多少条冲突")
    args = ap.parse_args()

    inst = pd.read_parquet(layout.meta_path("instruments"))
    fac = pd.read_parquet(layout.meta_path("adj_factor"))
    etf_ids = set(inst.loc[inst["type"] == int(sc.InstrumentType.ETF), "instrument_id"])
    print(f"ETF {len(etf_ids)} 只，加载行情 ...", flush=True)

    parts = []
    for f in sorted(layout.bar_dir("ashare", "1d").rglob("*.parquet")):
        d = pd.read_parquet(f, columns=["instrument_id", "trading_day",
                                        "close", "preclose", "tradestatus"])
        parts.append(d[d["instrument_id"].isin(etf_ids)])
    bars = pd.concat(parts, ignore_index=True).sort_values(
        ["instrument_id", "trading_day"]).reset_index(drop=True)
    print(f"  ETF bar {len(bars)} 行", flush=True)

    # 排除复权事件日：那些日子的 preclose 经过调整，涨跌幅不可作为证据
    ev = set(zip(fac["instrument_id"], fac["ex_date"]))
    bars["is_event"] = [(i, d) in ev for i, d in
                        zip(bars["instrument_id"], bars["trading_day"])]
    # 排除上市初期
    bars["seq"] = bars.groupby("instrument_id").cumcount()

    ok = bars[(bars["tradestatus"] == 1) & (bars["preclose"] > 0)
              & (~bars["is_event"]) & (bars["seq"] >= _IPO_FREE_DAYS)].copy()
    ok["chg"] = (ok["close"] / ok["preclose"] - 1.0) * 100.0
    stat = ok.groupby("instrument_id")["chg"].agg(
        max_abs=lambda s: s.abs().max(), days="size")

    meta = inst[inst["type"] == int(sc.InstrumentType.ETF)][
        ["instrument_id", "symbol", "name", "board"]].copy()
    meta = meta.merge(stat, left_on="instrument_id", right_index=True, how="left")
    meta["max_abs"] = meta["max_abs"].fillna(0.0)
    meta["days"] = meta["days"].fillna(0).astype(int)

    meta["price_says_20"] = meta["max_abs"] > _OVER_10
    meta["name_says"] = meta["name"].map(name_hint)

    def decide(r) -> tuple[int, str]:
        if r.price_says_20:
            # 价格已证明不是 10%；用名称细分是创业板还是科创板
            if r.name_says:
                return int(r.name_says), "价格实证+名称细分"
            return int(sc.Board.CHINEXT), "价格实证（名称无法细分，按创业板计）"
        if r.name_says:
            return int(r.name_says), "名称（价格证据不足）"
        return int(sc.Board.MAIN), "默认主板"

    decided = [decide(r) for r in meta.itertuples(index=False)]
    meta["tracked_board"] = [d[0] for d in decided]
    meta["method"] = [d[1] for d in decided]

    print()
    print("=== 判定结果 ===")
    names = {1: "主板(10%)", 2: "创业板(20%)", 3: "科创板(20%)", 4: "北交所(30%)"}
    summary = meta.assign(板块=meta["tracked_board"].map(names)).groupby(
        ["板块", "method"]).size()
    print(summary.to_string())

    # 冲突：价格说 20% 但名称说不出所以然，或名称说 20% 但价格从未超 10.5%
    conflict = meta[
        (meta["price_says_20"] & (meta["name_says"] == 0))
        | ((~meta["price_says_20"]) & (meta["name_says"] != 0) & (meta["days"] > 250))
    ]
    print(f"\n=== 价格与名称不一致 {len(conflict)} 只 ===")
    if len(conflict):
        show = conflict[["symbol", "name", "max_abs", "days", "name_says",
                         "tracked_board", "method"]].head(args.show)
        print(show.to_string(index=False))
        print("\n  （名称说 20% 但价格从未超 10.5% 的，多为上市不久或波动小，"
              "按名称判定；价格说 20% 但名称无关键词的需人工确认）")

    if args.dry_run:
        print("\n--dry-run，未写回")
        return 0

    # 写回 instruments：个股的 tracked_board 等于 board 本身
    full = inst.copy()
    tb = dict(zip(meta["instrument_id"], meta["tracked_board"]))
    full["tracked_board"] = [
        int(tb.get(i, b)) if t == int(sc.InstrumentType.ETF) else int(b)
        for i, t, b in zip(full["instrument_id"], full["type"], full["board"])
    ]
    full = full.astype({"tracked_board": "int8"})
    # 字段顺序需与 schema 一致
    full = full[[f.name for f in sc.INSTRUMENTS_SCHEMA]]
    sc.validate_columns(full, "instruments")
    p, c = layout.write_meta(full, "instruments")
    n20 = int((full["tracked_board"].isin([int(sc.Board.CHINEXT), int(sc.Board.STAR)])
               & (full["type"] == int(sc.InstrumentType.ETF))).sum())
    print(f"\n已写回 {p.name}（+{c.name}）：{n20} 只 ETF 判定为 20% 涨跌停")
    return 0


if __name__ == "__main__":
    sys.exit(main())
