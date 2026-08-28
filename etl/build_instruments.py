"""构建 instruments 与 calendar 两张元数据表（全量）。

instrument_id 的分配规则见 SCHEMA.md 2.4，是本模块最关键的部分：
按 (market, symbol) 首次出现分配、单调递增、**永不复用**。
若该映射丢失，全部 bar 分区文件随之失效，因此每次运行前自动备份。

用法：
    python etl/build_instruments.py
    python etl/build_instruments.py --calendar-start 2005-01-01
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入 —— 用错解释器时失败点会落在 import pandas，
# 报出的栈掩盖真正原因。见 etl/_venv_guard.py
import sys as _sys
from pathlib import Path as _Path
_sys.path.insert(0, str(_Path(__file__).resolve().parent))
import _venv_guard  # noqa: F401,E402

import argparse
import shutil
import sys
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402
from sources import BaoStockSource  # noqa: E402


def _backup(path: Path) -> Path | None:
    """instruments.parquet 是整个数据集里最关键的单个文件，覆盖前先备份。"""
    if not path.exists():
        return None
    bak_dir = path.parent / "_backup"
    bak_dir.mkdir(parents=True, exist_ok=True)
    bak = bak_dir / f"{path.stem}.bak{path.suffix}"
    shutil.copy2(path, bak)
    return bak


def load_existing_ids() -> tuple[dict[tuple[int, str], int], int]:
    """读取已有的 (market, symbol) -> instrument_id 映射与下一个可用 ID。"""
    path = layout.meta_path("instruments")
    if not path.exists():
        return {}, 1
    df = pd.read_parquet(path)
    mapping = {
        (int(m), str(s)): int(i)
        for m, s, i in zip(df["market"], df["symbol"], df["instrument_id"])
    }
    return mapping, (int(df["instrument_id"].max()) + 1 if len(df) else 1)


def build_instruments(src: BaoStockSource) -> pd.DataFrame:
    raw = src.instruments()
    print(f"  数据源返回 {len(raw)} 个标的", flush=True)

    existing, next_id = load_existing_ids()
    if existing:
        print(f"  复用已有 ID 映射 {len(existing)} 条，下一个可用 ID = {next_id}", flush=True)

    rows = []
    new_count = 0
    for r in raw.itertuples(index=False):
        symbol = str(r.symbol)
        itype = sc.InstrumentType.ETF if r.type == "etf" else sc.InstrumentType.STOCK
        exchange = sc.infer_exchange(symbol)
        board = sc.infer_board(symbol, itype)
        min_qty, qty_step = sc.order_qty_rule(board, itype)

        key = (int(sc.Market.ASHARE), symbol)
        iid = existing.get(key)
        if iid is None:
            iid = next_id
            next_id += 1
            new_count += 1

        rows.append({
            "instrument_id": iid,
            "market": int(sc.Market.ASHARE),
            "symbol": symbol,
            "exchange": int(exchange),
            "name": r.name,
            "type": int(itype),
            "board": int(board),
            "price_scale": sc.PRICE_SCALE_ASHARE,
            "qty_scale": sc.QTY_SCALE_ASHARE,
            "quote_ccy": int(sc.Currency.CNY),
            "min_order_qty": min_qty,
            "qty_step": qty_step,
            "list_date": int(r.list_date),
            "delist_date": int(r.delist_date) or None,
            "status": int(sc.Status.LISTED if r.listed else sc.Status.DELISTED),
            "attrs": None,
        })

    df = pd.DataFrame(rows).sort_values("instrument_id").reset_index(drop=True)
    print(f"  新分配 ID {new_count} 个", flush=True)
    return df


def check_instruments(df: pd.DataFrame) -> list[str]:
    """质检：ID 唯一性、(market,symbol) 唯一性、枚举完整性、日期合理性。"""
    problems = []
    if df["instrument_id"].duplicated().any():
        problems.append("instrument_id 存在重复")
    if df.duplicated(subset=["market", "symbol"]).any():
        problems.append("(market, symbol) 存在重复")

    for col, enum in (("exchange", sc.Exchange), ("board", sc.Board),
                      ("type", sc.InstrumentType)):
        bad = df[df[col] == 0]
        if len(bad):
            sample = bad["symbol"].head(5).tolist()
            problems.append(f"{col} 未能识别 {len(bad)} 个（{enum.__name__}=0），样本 {sample}")

    if (df["list_date"] <= 0).any():
        n = int((df["list_date"] <= 0).sum())
        problems.append(f"list_date 缺失 {n} 个")

    both = df[(df["status"] == int(sc.Status.DELISTED)) & df["delist_date"].isna()]
    if len(both):
        problems.append(f"标记为已退市但无 delist_date：{len(both)} 个")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser(description="构建 instruments 与 calendar")
    ap.add_argument("--calendar-start", default="2005-01-01")
    ap.add_argument("--calendar-end", default="2026-12-31")
    args = ap.parse_args()

    layout.ensure()
    bak = _backup(layout.meta_path("instruments"))
    if bak:
        print(f"已备份既有 instruments -> {bak}", flush=True)

    with BaoStockSource() as src:
        print("构建 instruments ...", flush=True)
        inst = build_instruments(src)

        print("构建 calendar ...", flush=True)
        cal_raw = src.calendar(args.calendar_start, args.calendar_end)
        cal = pd.DataFrame({
            "market": int(sc.Market.ASHARE),
            "date": cal_raw["date"].astype("int32"),
            "is_trading_day": cal_raw["is_trading_day"].astype("int8"),
        })

    problems = check_instruments(inst)

    # 类型对齐 schema 后写出
    inst = inst.astype({
        "instrument_id": "int32", "market": "int8", "exchange": "int8",
        "type": "int8", "board": "int8", "price_scale": "int32",
        "qty_scale": "int32", "quote_ccy": "int8", "min_order_qty": "int32",
        "qty_step": "int32", "list_date": "int32", "delist_date": "Int32",
        "status": "int8",
    })
    sc.validate_columns(inst, "instruments")
    sc.validate_columns(cal, "calendar")

    p1, c1 = layout.write_meta(inst, "instruments")
    p2, c2 = layout.write_meta(cal, "calendar")

    print(f"\ninstruments  {len(inst):>6} 行 -> {p1.name} (+{c1.name})")
    print(f"calendar     {len(cal):>6} 行 -> {p2.name} (+{c2.name})")

    print("\n--- 标的构成 ---")
    summary = inst.assign(
        类型=inst["type"].map({1: "个股", 2: "ETF"}),
        板块=inst["board"].map({1: "主板", 2: "创业板", 3: "科创板", 4: "北交所", 0: "未识别"}),
        状态=inst["status"].map({1: "在市", 2: "已退市"}),
    )
    print(summary.groupby(["类型", "板块", "状态"]).size().to_string())

    trading = int((cal["is_trading_day"] == 1).sum())
    print(f"\n交易日历：{len(cal)} 个自然日，其中交易日 {trading} 个")

    if problems:
        print("\n--- 质检问题 ---")
        for p in problems:
            print(f"  ! {p}")
        return 1
    print("\n质检通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
