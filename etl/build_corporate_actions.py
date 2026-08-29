"""构建 corporate_action 表（分红送配）。

Portfolio 模块据此把现金分红入账、送转股增加持仓（约束 C2）——
只调价格不调账户是回测的常见错误，会系统性低估收益。

数据源选择：

  个股  AkShare `stock_history_dividend_detail` —— **一次调用给全历史**。
        BaoStock 的 `query_dividend_data` 必须逐年查询（year="" 只返回当年），
        5549 只 × 22 年 ≈ 12 万次请求，约 5 小时，不可行。
  ETF   `fund_etf_dividend_sina` 的「累计分红」差分 —— 与 ETF 复权因子同源。

单位：接口给的送股/转增/派息均为**每 10 股**，本表统一折算为**每股**。
验证：比亚迪 2025-07-29 送 8 转 12 派 39.74（每 10 股），即每股送 0.8 转 1.2 派 3.974，
则 (337.00 - 3.974) / 3 = 111.0，与当日实际最低 109.77 吻合。

只采「进度 = 实施」且有除权除息日的记录 —— 预案与不分配对回测无意义。

用法：
    python etl/build_corporate_actions.py                # 全量
    python etl/build_corporate_actions.py --limit 20     # 抽试
    python etl/build_corporate_actions.py --symbol 002594
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
from multiprocessing import Pool
from pathlib import Path

import akshare as ak
import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402

_PER_TEN = 10.0  # 接口单位为每 10 股
_SINA_PREFIX = {int(sc.Exchange.SSE): "sh", int(sc.Exchange.SZSE): "sz"}


def _ymd(v) -> int:
    if v is None or (isinstance(v, float) and pd.isna(v)):
        return 0
    s = str(v)[:10].replace("-", "")
    return int(s) if len(s) == 8 and s.isdigit() else 0


def fetch_stock(task: dict) -> dict:
    """个股分红送配。异常不逃逸 —— 单只失败不能中断整轮。"""
    iid, symbol = task["iid"], task["symbol"]
    last = ""
    for attempt in range(1, task["retries"] + 1):
        try:
            df = ak.stock_history_dividend_detail(symbol=symbol, indicator="分红")
            if df is None or len(df) == 0:
                return {"iid": iid, "symbol": symbol, "status": "done", "rows": []}
            d = df[df["进度"].astype(str) == "实施"].copy()
            d["ex"] = d["除权除息日"].map(_ymd)
            d = d[d["ex"] > 0]
            rows = []
            for r in d.itertuples(index=False):
                rows.append({
                    "instrument_id": iid,
                    "ex_date": int(r.ex),
                    "record_date": _ymd(getattr(r, "股权登记日", None)) or None,
                    # 该接口不提供派息到账日；BaoStock 有但需逐年查询，不划算
                    "pay_date": None,
                    "cash_before_tax": sc.to_fixed(
                        float(pd.to_numeric(r.派息, errors="coerce") or 0) / _PER_TEN,
                        sc.PER_SHARE_SCALE),
                    "stock_dividend": sc.to_fixed(
                        float(pd.to_numeric(r.送股, errors="coerce") or 0) / _PER_TEN,
                        sc.PER_SHARE_SCALE),
                    "stock_transfer": sc.to_fixed(
                        float(pd.to_numeric(r.转增, errors="coerce") or 0) / _PER_TEN,
                        sc.PER_SHARE_SCALE),
                })
            return {"iid": iid, "symbol": symbol, "status": "done", "rows": rows}
        except Exception as exc:  # noqa: BLE001
            last = f"{type(exc).__name__}: {exc}"
            if attempt < task["retries"]:
                time.sleep(1.5 * attempt)
    return {"iid": iid, "symbol": symbol, "status": "failed", "rows": [], "error": last[:300]}


def fetch_etf(task: dict) -> dict:
    """ETF 分红。与 build_etf_factors 同源，仅取现金部分。

    「累计分红 = 0」意味着**金额未知**（见 ETL.md 6.9），此处记为 0 ——
    金额未知的分红在因子里已由价格估算修正，但账户入账金额无从得知。
    """
    iid, symbol = task["iid"], task["symbol"]
    pref = _SINA_PREFIX.get(task["exchange"])
    if pref is None:
        return {"iid": iid, "symbol": symbol, "status": "done", "rows": []}
    last = ""
    for attempt in range(1, task["retries"] + 1):
        try:
            df = ak.fund_etf_dividend_sina(symbol=f"{pref}{symbol}")
            if df is None or len(df) == 0:
                return {"iid": iid, "symbol": symbol, "status": "done", "rows": []}
            d = pd.DataFrame()
            d["ex"] = pd.to_datetime(df["日期"]).dt.strftime("%Y%m%d").astype(int)
            cum = pd.to_numeric(df["累计分红"], errors="coerce").fillna(0.0)
            d["cash"] = cum.diff().fillna(cum).clip(lower=0.0)
            d = d.sort_values("ex")
            rows = [{
                "instrument_id": iid, "ex_date": int(r.ex),
                "record_date": None, "pay_date": None,
                "cash_before_tax": sc.to_fixed(float(r.cash), sc.PER_SHARE_SCALE),
                "stock_dividend": 0, "stock_transfer": 0,
            } for r in d.itertuples(index=False)]
            return {"iid": iid, "symbol": symbol, "status": "done", "rows": rows}
        except Exception as exc:  # noqa: BLE001
            last = f"{type(exc).__name__}: {exc}"
            if attempt < task["retries"]:
                time.sleep(1.5 * attempt)
    return {"iid": iid, "symbol": symbol, "status": "failed", "rows": [], "error": last[:300]}


def _dispatch(task: dict) -> dict:
    return fetch_etf(task) if task["is_etf"] else fetch_stock(task)


def main() -> int:
    ap = argparse.ArgumentParser(description="构建 corporate_action 表")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--symbol")
    ap.add_argument("--workers", type=int, default=6)
    ap.add_argument("--retries", type=int, default=3)
    ap.add_argument("--only", choices=["stocks", "etfs"])
    args = ap.parse_args()

    inst = pd.read_parquet(layout.meta_path("instruments"))
    inst = inst[inst["type"].isin([int(sc.InstrumentType.STOCK), int(sc.InstrumentType.ETF)])]
    if args.symbol:
        inst = inst[inst["symbol"] == args.symbol]
    if args.only == "stocks":
        inst = inst[inst["type"] == int(sc.InstrumentType.STOCK)]
    elif args.only == "etfs":
        inst = inst[inst["type"] == int(sc.InstrumentType.ETF)]
    if args.limit:
        inst = inst.head(args.limit)
    if inst.empty:
        raise SystemExit("没有匹配的标的")

    tasks = [{"iid": int(r.instrument_id), "symbol": r.symbol,
              "exchange": int(r.exchange), "retries": args.retries,
              "is_etf": int(r.type) == int(sc.InstrumentType.ETF)}
             for r in inst.itertuples(index=False)]
    print(f"标的 {len(tasks)} 只（个股 {sum(1 for t in tasks if not t['is_etf'])}，"
          f"ETF {sum(1 for t in tasks if t['is_etf'])}），{args.workers} 并发")

    all_rows, failures = [], []
    started = time.perf_counter()
    with Pool(processes=args.workers) as pool:
        for i, res in enumerate(pool.imap_unordered(_dispatch, tasks), 1):
            if res["status"] == "done":
                all_rows.extend(res["rows"])
            else:
                failures.append(f"{res['symbol']}: {res.get('error', '')}")
            if i % 500 == 0 or i == len(tasks):
                el = time.perf_counter() - started
                print(f"  {i}/{len(tasks)}  {el:.0f}s  {i/el:.1f}只/秒  "
                      f"记录 {len(all_rows)} 行  失败 {len(failures)}", flush=True)

    if not all_rows:
        print("无记录可写出")
        return 0

    df = pd.DataFrame(all_rows)
    before = len(df)
    df = df.drop_duplicates(subset=["instrument_id", "ex_date"], keep="last")
    df = df.sort_values(["instrument_id", "ex_date"]).reset_index(drop=True)
    df = df.astype({
        "instrument_id": "int32", "ex_date": "int32",
        "record_date": "Int32", "pay_date": "Int32",
        "cash_before_tax": "int64", "stock_dividend": "int64",
        "stock_transfer": "int64",
    })
    sc.validate_columns(df, "corporate_action")
    p, c = layout.write_meta(df, "corporate_action")

    has_cash = int((df["cash_before_tax"] > 0).sum())
    has_stock = int(((df["stock_dividend"] + df["stock_transfer"]) > 0).sum())
    print(f"\ncorporate_action  {before} -> 去重后 {len(df)} 行 / "
          f"{df['instrument_id'].nunique()} 只标的 -> {p.name} (+{c.name})")
    print(f"  含现金分红 {has_cash} 行，含送转 {has_stock} 行")
    print(f"  除权日范围 {int(df['ex_date'].min())} ~ {int(df['ex_date'].max())}")

    # 与 adj_factor 交叉核对：有因子事件的日子原则上应有对应的分红送配记录
    fac_path = layout.meta_path("adj_factor")
    if fac_path.exists():
        fac = pd.read_parquet(fac_path)
        fk = set(zip(fac["instrument_id"], fac["ex_date"]))
        ck = set(zip(df["instrument_id"], df["ex_date"]))
        print(f"\n与 adj_factor 交叉核对：")
        print(f"  因子事件 {len(fk)}，分红记录 {len(ck)}，两者都有 {len(fk & ck)}")
        print(f"  有因子无分红记录 {len(fk - ck)}，有分红记录无因子 {len(ck - fk)}")

    if failures:
        print(f"\n--- 失败 {len(failures)} 只（前 10）---")
        for f in failures[:10]:
            print(f"  ! {f}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
