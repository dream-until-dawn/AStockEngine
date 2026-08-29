"""导出人工校验样本：随机标的的若干交易日，同时给出未复权 / 前复权 / 后复权价。

本项目只存**原始价 + 复权因子**（约束 C2），三种价格的关系是：

    未复权 = 原始价
    后复权 = 原始价 × factor(d)
    前复权 = 原始价 × factor(d) / factor(最后一个交易日)

前复权之所以要除以末日因子，是因为它锚定在**最后一个交易日** ——
这也正是它不可持久化的原因：每来一次新的除权，全部历史前复权价都会改写。

与行情软件比对时务必注意：软件默认多为前复权，且其锚点是**软件当前的最后一日**，
若与本地数据的最后一日不同，前复权价会整体差一个比例。**未复权最适合比对。**

用法：
    python etl/sample_check.py                      # 随机 3 只个股 + 2 只 ETF
    python etl/sample_check.py --stocks 5 --etfs 3 --days 6 --seed 42
    python etl/sample_check.py --symbol 600519 --days 10
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入，见 etl/_venv_guard.py
import sys as _sys
from pathlib import Path as _Path
_sys.path.insert(0, str(_Path(__file__).resolve().parent))
import _venv_guard  # noqa: F401,E402

import argparse
import bisect
import sys
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402

_BOARD = {1: "主板", 2: "创业板", 3: "科创板", 4: "北交所", 0: "未识别"}
_EXCH = {1: "上交所", 2: "深交所", 3: "北交所"}


def load_bars(iids: set[int]) -> pd.DataFrame:
    parts = []
    for f in sorted(layout.bar_dir("ashare", "1d").rglob("*.parquet")):
        df = pd.read_parquet(f)
        hit = df[df["instrument_id"].isin(iids)]
        if len(hit):
            parts.append(hit)
    if not parts:
        raise SystemExit("未找到 bar 数据")
    return pd.concat(parts, ignore_index=True).sort_values(
        ["instrument_id", "trading_day"]).reset_index(drop=True)


def factor_lookup(fac: pd.DataFrame, iid: int):
    f = fac[fac["instrument_id"] == iid].sort_values("ex_date")
    eds, fv = list(f["ex_date"]), list(f["hfq_factor"])

    def get(day: int) -> float:
        i = bisect.bisect_right(eds, day) - 1
        return fv[i] / sc.FACTOR_SCALE if i >= 0 else 1.0

    return get, len(eds)


def _build(w: pd.DataFrame, scale: int, get_f, f_last: float) -> pd.DataFrame:
    out = pd.DataFrame()
    out["交易日"] = w["trading_day"].astype(int).values
    for col, label in (("open", "开"), ("high", "高"), ("low", "低"), ("close", "收")):
        out[f"{label}(未复权)"] = (w[col] / scale).round(3).values
    out["前收(未复权)"] = (w["preclose"] / scale).round(3).values
    f_col = [get_f(int(d)) for d in w["trading_day"]]
    out["复权因子"] = [round(v, 8) for v in f_col]
    out["收(后复权)"] = [round(c / scale * f, 4) for c, f in zip(w["close"], f_col)]
    out["收(前复权)"] = [round(c / scale * f / f_last, 4) for c, f in zip(w["close"], f_col)]
    out["成交量"] = w["volume"].astype("int64").values
    out["成交额(元)"] = (w["amount"] / sc.AMOUNT_SCALE).round(2).values
    out["状态"] = w["tradestatus"].map({1: "正常", 0: "停牌"}).values
    return out


def _render(w: pd.DataFrame, scale: int, get_f, f_last: float) -> str:
    return _build(w, scale, get_f, f_last).to_string(index=False)


def _indent(text: str, pad: str = "  ") -> str:
    return "\n".join(pad + line for line in text.splitlines())


def main() -> int:
    ap = argparse.ArgumentParser(description="导出人工校验样本")
    ap.add_argument("--stocks", type=int, default=3)
    ap.add_argument("--etfs", type=int, default=2)
    ap.add_argument("--days", type=int, default=5)
    ap.add_argument("--seed", type=int, default=None)
    ap.add_argument("--symbol", help="指定标的，忽略随机抽样")
    args = ap.parse_args()

    inst = pd.read_parquet(layout.meta_path("instruments"))
    fac = pd.read_parquet(layout.meta_path("adj_factor"))

    if args.symbol:
        picked = inst[inst["symbol"] == args.symbol]
        if picked.empty:
            raise SystemExit(f"instruments 中无此代码：{args.symbol}")
    else:
        # 只抽在市标的：退市标的的最后交易日久远，前复权锚点与行情软件不一致，不便比对
        listed = inst[inst["status"] == int(sc.Status.LISTED)]
        s = listed[listed["type"] == int(sc.InstrumentType.STOCK)]
        e = listed[listed["type"] == int(sc.InstrumentType.ETF)]
        picked = pd.concat([
            s.sample(n=min(args.stocks, len(s)), random_state=args.seed),
            e.sample(n=min(args.etfs, len(e)), random_state=args.seed),
        ])

    bars = load_bars(set(picked["instrument_id"].astype(int)))
    pd.set_option("display.width", 240)
    pd.set_option("display.max_columns", 40)

    for r in picked.itertuples(index=False):
        iid = int(r.instrument_id)
        x = bars[bars["instrument_id"] == iid]
        if x.empty:
            print(f"\n{r.symbol} {r.name}：无行情数据\n")
            continue
        get_f, n_events = factor_lookup(fac, iid)
        last_day = int(x["trading_day"].iloc[-1])
        f_last = get_f(last_day)

        # 抽最近的若干个交易日，便于与行情软件对照
        w = x.tail(args.days).copy()
        scale = int(r.price_scale)

        out = _build(w, scale, get_f, f_last)

        kind = "ETF" if int(r.type) == int(sc.InstrumentType.ETF) else "个股"
        print("=" * 118)
        print(f"{r.symbol}  {r.name}   [{kind} / {_EXCH.get(int(r.exchange), '?')} / "
              f"{_BOARD.get(int(r.board), '?')}]   上市 {int(r.list_date)}")
        print(f"  本地数据末日 {last_day}（前复权锚点，末日因子 {f_last:.8f}）  "
              f"复权事件 {n_events} 次  行情共 {len(x)} 个交易日")
        print(f"\n  [最近 {args.days} 个交易日]")
        print(_indent(out.to_string(index=False)))

        # 最近的一次除权前后 —— 窗口内因子发生变化，前复权与未复权才会不同，
        # 这才是能验证复权逻辑是否正确的地方；只看最近几日看不出任何差别。
        ev = fac[fac["instrument_id"] == iid].sort_values("ex_date")
        if len(ev):
            ex = int(ev["ex_date"].iloc[-1])
            pos = x.index[x["trading_day"] == ex]
            if len(pos):
                k = x.index.get_loc(pos[0])
                w2 = x.iloc[max(0, k - 3):k + 3]
                print(f"\n  [最近一次除权 {ex} 前后]")
                print(_indent(_render(w2, scale, get_f, f_last)))
            else:
                print(f"\n  [最近一次除权 {ex} 当日无行情，跳过]")
        print()

    print("=" * 118)
    print("比对说明：")
    print("  · 未复权 = 原始价，与行情软件的「不复权」对照，**最适合比对**")
    print("  · 后复权 = 原始价 × 因子")
    print("  · 前复权 = 原始价 × 因子 / 末日因子，锚定在本地数据末日")
    print("    行情软件的前复权锚点是软件自己的最后一日，若与本地末日不同，")
    print("    前复权价会整体差一个固定比例 —— 对不上属正常，看未复权即可")
    print("  · 停牌日 OHLC 全等于停牌前收盘价，成交量为 0（见 ETL.md 6.7）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
