"""查看 Parquet 表内容的调试工具。

Parquet 是二进制，无法 `cat`。元数据表虽有 CSV 镜像，但 bar 表体量太大不做镜像，
且定点整数列（价格 ×1000、金额以分计）直接看不直观 —— 本工具负责还原成人类单位。

用法：
    python etl/dump.py instruments --limit 20
    python etl/dump.py instruments --where "board==3" --limit 10
    python etl/dump.py bar --symbol 600519 --limit 15
    python etl/dump.py adj_factor --symbol 600519
    python etl/dump.py --schema bar
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入 —— 用错解释器时失败点会落在 import pandas，
# 报出的栈掩盖真正原因。见 etl/_venv_guard.py
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

_ENUM_MAPS = {
    "market": {int(e): e.name for e in sc.Market},
    "exchange": {int(e): e.name for e in sc.Exchange},
    "type": {int(sc.InstrumentType.STOCK): "STOCK", int(sc.InstrumentType.ETF): "ETF"},
    "board": {int(e): e.name for e in sc.Board},
    "status": {int(e): e.name for e in sc.Status},
}

# 定点列 -> 还原除数
_FIXED_COLS = {
    "open": None, "high": None, "low": None, "close": None, "preclose": None,
    "amount": sc.AMOUNT_SCALE,
    "turn": sc.RATIO_SCALE,
    "hfq_factor": sc.FACTOR_SCALE,
    "cash_before_tax": sc.PER_SHARE_SCALE,
    "stock_dividend": sc.PER_SHARE_SCALE,
    "stock_transfer": sc.PER_SHARE_SCALE,
}


def load_bar() -> pd.DataFrame:
    root = layout.bar_dir("ashare", "1d")
    files = sorted(root.rglob("*.parquet"))
    if not files:
        raise SystemExit(f"未找到 bar 分区文件：{root}")
    return pd.concat([pd.read_parquet(f) for f in files], ignore_index=True)


def decode(df: pd.DataFrame, price_scale: int, raw: bool) -> pd.DataFrame:
    """定点整数与枚举还原为可读形式。--raw 可关闭。"""
    if raw:
        return df
    out = df.copy()
    for col, scale in _FIXED_COLS.items():
        if col in out.columns:
            out[col] = out[col] / (scale if scale else price_scale)
    for col, mapping in _ENUM_MAPS.items():
        if col in out.columns:
            out[col] = out[col].map(mapping).fillna(out[col])
    if "tradestatus" in out.columns:
        out["tradestatus"] = out["tradestatus"].map({1: "正常", 0: "停牌"})
    if "is_st" in out.columns:
        out["is_st"] = out["is_st"].map({1: "ST", 0: ""})
    if "is_trading_day" in out.columns:
        out["is_trading_day"] = out["is_trading_day"].map({1: "交易", 0: ""})
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description="查看 Parquet 表内容")
    ap.add_argument("table", nargs="?", default="instruments",
                    choices=["instruments", "calendar", "adj_factor",
                             "corporate_action", "bar"])
    ap.add_argument("--limit", type=int, default=20)
    ap.add_argument("--symbol", help="按标的代码过滤（bar / adj_factor 等）")
    ap.add_argument("--where", help="pandas query 表达式，作用于原始定点值")
    ap.add_argument("--cols", help="逗号分隔的列名")
    ap.add_argument("--raw", action="store_true", help="不还原定点与枚举")
    ap.add_argument("--schema", action="store_true", help="只打印 schema")
    args = ap.parse_args()

    if args.schema:
        print(sc.TABLE_SCHEMAS[args.table])
        return 0

    inst = pd.read_parquet(layout.meta_path("instruments"))
    price_scale = sc.PRICE_SCALE_ASHARE

    if args.table == "bar":
        df = load_bar()
    else:
        path = layout.meta_path(args.table)
        if not path.exists():
            raise SystemExit(f"表尚未构建：{path}")
        df = pd.read_parquet(path)

    if args.symbol:
        if "symbol" in df.columns:
            df = df[df["symbol"] == args.symbol]
        elif "instrument_id" in df.columns:
            hit = inst[inst["symbol"] == args.symbol]
            if hit.empty:
                raise SystemExit(f"instruments 中无此代码：{args.symbol}")
            iid = int(hit["instrument_id"].iloc[0])
            price_scale = int(hit["price_scale"].iloc[0])
            df = df[df["instrument_id"] == iid]
            print(f"{args.symbol}  {hit['name'].iloc[0]}  instrument_id={iid}")

    if args.where:
        df = df.query(args.where)

    total = len(df)
    out = decode(df.head(args.limit), price_scale, args.raw)
    if args.cols:
        keep = [c for c in args.cols.split(",") if c in out.columns]
        out = out[keep]

    pd.set_option("display.width", 220)
    pd.set_option("display.max_columns", 40)
    print(out.to_string(index=False))
    print(f"\n共 {total} 行，显示前 {min(args.limit, total)} 行"
          + ("（原始定点值）" if args.raw else "（已还原为人类单位）"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
