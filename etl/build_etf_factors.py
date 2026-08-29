"""构建 ETF 复权因子，并修正 ETF 在事件日的 preclose。

个股的复权因子由新浪 `hfq-factor` 直接给出（16 位精度），ETF 没有对应接口 ——
v0.0 起这一直是 🟡 未收口项。本模块用「事件日期 + 分红金额」重建因子：

  事件日期   `fund_etf_dividend_sina` 提供（非东财，东财已因限流弃用）
  现金分红   由「累计分红」差分得到每次金额 —— **精确**，
             factor_ratio = close(d-1) / (close(d-1) - D)
  份额折算   分红金额为 0 的事件。价格比值只是近似，须取整到干净比例：
             159527 于 2026-07-22 折算，价格推出 3.0931，
             而深交所份额数据显示 (168,100,450 - 4,000,000) × 3 = 492,301,350，
             真实比例是 **3.0**，价格估计偏离 3.1%

为何事件日期是关键：同样是 -66%，落在事件日就是折算，不在事件日就是真跌。
没有权威事件日期，任何纯价格启发式都可能把暴跌误判成折算，反之亦然。

用法：
    python etl/build_etf_factors.py               # 全部 ETF
    python etl/build_etf_factors.py --limit 20    # 抽试
    python etl/build_etf_factors.py --symbol 159527
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入，见 etl/_venv_guard.py
import sys as _sys
from pathlib import Path as _Path
_sys.path.insert(0, str(_Path(__file__).resolve().parent))
import _venv_guard  # noqa: F401,E402

import argparse
import sys
import time
from decimal import Decimal
from pathlib import Path

import akshare as ak
import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402

_EXCHANGE_PREFIX = {int(sc.Exchange.SSE): "sh", int(sc.Exchange.SZSE): "sz"}

# 份额折算的常见比例。价格推出的比值落在容差内即取整到这些值。
_CLEAN_RATIOS = (1.5, 2.0, 2.5, 3.0, 4.0, 5.0, 8.0, 10.0, 20.0, 100.0)
_SNAP_TOLERANCE = 0.06   # 6%：实测 159527 的价格估计偏离真实比例 3.1%
_MIN_EVENT_RATIO = 1.001  # 低于此视为无实质调整，跳过


def snap_ratio(raw: float) -> tuple[float, bool]:
    """把价格推出的比值取整到干净比例。返回 (比例, 是否取整成功)。"""
    for c in _CLEAN_RATIOS:
        if abs(raw / c - 1.0) <= _SNAP_TOLERANCE:
            return c, True
    return raw, False


def load_etf_bars(inst: pd.DataFrame) -> pd.DataFrame:
    etf_ids = set(inst.loc[inst["type"] == int(sc.InstrumentType.ETF), "instrument_id"])
    root = layout.bar_dir("ashare", "1d")
    parts = []
    for f in sorted(root.rglob("*.parquet")):
        df = pd.read_parquet(f, columns=["instrument_id", "trading_day", "open",
                                         "close", "preclose"])
        parts.append(df[df["instrument_id"].isin(etf_ids)])
    if not parts:
        raise SystemExit("未找到 ETF bar 数据，请先运行 sync_bars.py")
    return pd.concat(parts, ignore_index=True).sort_values(
        ["instrument_id", "trading_day"]).reset_index(drop=True)


def events_for(symbol: str, exchange: int, retries: int = 3) -> pd.DataFrame:
    """取事件日期与每次分红金额。返回列：ex_date / cash。"""
    pref = _EXCHANGE_PREFIX.get(int(exchange))
    if pref is None:
        return pd.DataFrame(columns=["ex_date", "cash"])
    last = None
    for i in range(retries):
        try:
            df = ak.fund_etf_dividend_sina(symbol=f"{pref}{symbol}")
            break
        except Exception as exc:  # noqa: BLE001
            last = exc
            if i < retries - 1:
                time.sleep(1.5 * (i + 1))
    else:
        raise RuntimeError(f"{symbol} 事件查询失败: {last}")

    if df is None or len(df) == 0:
        return pd.DataFrame(columns=["ex_date", "cash"])
    out = pd.DataFrame()
    out["ex_date"] = pd.to_datetime(df["日期"]).dt.strftime("%Y%m%d").astype(int)
    cum = pd.to_numeric(df["累计分红"], errors="coerce").fillna(0.0)
    # 接口给的是累计值，差分还原每次金额；首行即其自身
    out["cash"] = cum.diff().fillna(cum).clip(lower=0.0)
    return out.sort_values("ex_date").reset_index(drop=True)


def build_factors(bars: pd.DataFrame, events: pd.DataFrame, iid: int) -> tuple[list[dict], list[str]]:
    """由事件序列累积出后复权因子。返回 (因子行, 告警)。"""
    x = bars[bars["instrument_id"] == iid].reset_index(drop=True)
    if x.empty or events.empty:
        return [], []

    day_to_pos = {int(d): i for i, d in enumerate(x["trading_day"])}
    factor = Decimal(1)
    rows, warns = [], []

    for e in events.itertuples(index=False):
        d = int(e.ex_date)
        pos = day_to_pos.get(d)
        if pos is None or pos == 0:
            # 事件日无行情（上市前 / 数据缺口），无法定位调整点
            continue
        prev_close = x["close"].iloc[pos - 1] / sc.PRICE_SCALE_ASHARE
        today_open = x["open"].iloc[pos] / sc.PRICE_SCALE_ASHARE
        if prev_close <= 0 or today_open <= 0:
            continue

        cash = float(e.cash)
        if cash > 0:
            # 现金分红：金额权威，比例可精确计算
            if cash >= prev_close:
                warns.append(f"{d} 分红 {cash} 不小于前收 {prev_close}，跳过")
                continue
            ratio = Decimal(str(prev_close)) / Decimal(str(prev_close - cash))
            kind = "dividend"
        else:
            # 份额折算：价格比值只是近似，取整到干净比例
            raw = prev_close / today_open
            if raw < _MIN_EVENT_RATIO:
                continue
            snapped, ok = snap_ratio(raw)
            if not ok:
                warns.append(f"{d} 折算比例 {raw:.4f} 无法取整到常见比例，按原值使用")
            ratio = Decimal(str(snapped))
            kind = "split"

        factor = factor * ratio
        rows.append({
            "instrument_id": iid,
            "ex_date": d,
            "hfq_factor": int((factor * sc.FACTOR_SCALE).quantize(Decimal("1"))),
            "hfq_factor_raw": f"{factor:.16f}",
            "_kind": kind,
        })
    return rows, warns


def main() -> int:
    ap = argparse.ArgumentParser(description="构建 ETF 复权因子并修正事件日 preclose")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--symbol", help="只处理单只 ETF")
    ap.add_argument("--pause", type=float, default=0.35, help="请求间隔秒")
    ap.add_argument("--no-fix-preclose", action="store_true",
                    help="不修正 bar 表中事件日的 preclose")
    args = ap.parse_args()

    inst = pd.read_parquet(layout.meta_path("instruments"))
    etfs = inst[inst["type"] == int(sc.InstrumentType.ETF)]
    if args.symbol:
        etfs = etfs[etfs["symbol"] == args.symbol]
    if args.limit:
        etfs = etfs.head(args.limit)
    if etfs.empty:
        raise SystemExit("没有匹配的 ETF")

    print(f"ETF {len(etfs)} 只，加载 bar ...", flush=True)
    bars = load_etf_bars(inst)
    print(f"  ETF bar {len(bars)} 行 / {bars['instrument_id'].nunique()} 只", flush=True)

    all_rows, all_warns, stats = [], [], {"dividend": 0, "split": 0, "no_event": 0, "failed": 0}
    started = time.perf_counter()
    for i, r in enumerate(etfs.itertuples(index=False), 1):
        try:
            ev = events_for(r.symbol, int(r.exchange))
        except Exception as exc:  # noqa: BLE001
            stats["failed"] += 1
            all_warns.append(f"{r.symbol}: {exc}")
            continue
        time.sleep(args.pause)
        if ev.empty:
            stats["no_event"] += 1
            continue
        rows, warns = build_factors(bars, ev, int(r.instrument_id))
        for w in warns:
            all_warns.append(f"{r.symbol} {w}")
        for row in rows:
            stats[row["_kind"]] += 1
        all_rows.extend(rows)
        if i % 100 == 0:
            el = time.perf_counter() - started
            print(f"  {i}/{len(etfs)}  {el:.0f}s  因子 {len(all_rows)} 行", flush=True)

    print(f"\n事件统计：现金分红 {stats['dividend']}，份额折算 {stats['split']}，"
          f"无事件 {stats['no_event']} 只，查询失败 {stats['failed']} 只")

    if not all_rows:
        print("无因子可写出")
        return 0

    fac = pd.DataFrame(all_rows).drop(columns=["_kind"])
    fac = fac.astype({"instrument_id": "int32", "ex_date": "int32", "hfq_factor": "int64"})

    existing = layout.meta_path("adj_factor")
    if existing.exists():
        old = pd.read_parquet(existing)
        # ETF 因子整体重算，先剔除同标的旧值再合并，避免新旧混杂
        old = old[~old["instrument_id"].isin(set(fac["instrument_id"]))]
        fac = pd.concat([old, fac], ignore_index=True)
    fac = fac.sort_values(["instrument_id", "ex_date"]).reset_index(drop=True)
    sc.validate_columns(fac, "adj_factor")
    p, c = layout.write_meta(fac, "adj_factor")
    print(f"adj_factor  {len(fac)} 行 / {fac['instrument_id'].nunique()} 只标的 "
          f"-> {p.name} (+{c.name})")

    if not args.no_fix_preclose:
        n = fix_event_preclose(inst, pd.DataFrame(all_rows))
        print(f"已修正 ETF 事件日 preclose {n} 行")

    if all_warns:
        print(f"\n--- 告警 {len(all_warns)} 条（前 15 条）---")
        for w in all_warns[:15]:
            print(f"  ! {w}")
    return 0


def fix_event_preclose(inst: pd.DataFrame, rows: pd.DataFrame) -> int:
    """把 ETF 事件日的 preclose 由「前一日收盘」改为「除权调整后的前收」。

    ETF 的 preclose 原本是用前一日 close 直接补齐的（新浪不提供该字段），
    在除权/折算日必然偏高，导致当日涨跌停判定失真（SCHEMA.md 1.4 已知降级 #2）。
    有了因子即可还原：调整后前收 = 前一日收盘 × factor(前) / factor(当日)。
    """
    if rows.empty:
        return 0
    keys = {(int(r.instrument_id), int(r.ex_date)) for r in rows.itertuples(index=False)}
    ratio_map = {}
    for iid, g in rows.groupby("instrument_id"):
        g = g.sort_values("ex_date")
        prev = sc.FACTOR_SCALE
        for r in g.itertuples(index=False):
            ratio_map[(int(iid), int(r.ex_date))] = int(r.hfq_factor) / prev
            prev = int(r.hfq_factor)

    root = layout.bar_dir("ashare", "1d")
    fixed = 0
    for f in sorted(root.rglob("*.parquet")):
        df = pd.read_parquet(f)
        mask = pd.Series(
            [(int(a), int(b)) in keys for a, b in zip(df["instrument_id"], df["trading_day"])],
            index=df.index)
        if not mask.any():
            continue
        idx = df.index[mask]
        for i in idx:
            iid, day = int(df.at[i, "instrument_id"]), int(df.at[i, "trading_day"])
            ratio = ratio_map.get((iid, day))
            if not ratio or ratio <= 0:
                continue
            df.at[i, "preclose"] = int(round(df.at[i, "preclose"] / ratio))
            fixed += 1
        df = df.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
        sc.validate_columns(df, "bar")
        pq.write_table(pa.Table.from_pandas(df, schema=sc.BAR_SCHEMA, preserve_index=False),
                       f, **sc.parquet_write_options("bar"))
    return fixed


if __name__ == "__main__":
    sys.exit(main())
