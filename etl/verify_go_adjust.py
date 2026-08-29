r"""逐位比对 Go 引擎与 Python 侧的复权结果。

对应 docs/DESIGN-v0.2-dataflow.md 第 7 节待测项 8，是 **C5 的第一道关**：
若两侧算出的复权价不一致，「同配置两次运行逐笔一致」从第一步就破了。

比对的是**整数**，不是浮点打印值 —— 浮点格式化会掩盖低位差异。

Python 侧用 Decimal 做无限精度运算再四舍五入，作为「标准答案」；
Go 侧用整数拆分法（避免 int64 溢出）。两者若一致，说明 Go 的拆分推导正确。

用法：
    cd engine
    go run ./cmd/adjcheck > ../data/cache/go_adjust.csv
    cd ..
    .\.venv\Scripts\python.exe etl\verify_go_adjust.py data/cache/go_adjust.csv
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入，见 etl/_venv_guard.py
import sys as _sys
from pathlib import Path as _Path
_sys.path.insert(0, str(_Path(__file__).resolve().parent))
import _venv_guard  # noqa: F401,E402

import argparse
import sys
from decimal import ROUND_HALF_UP, Decimal
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402

FACTOR_SCALE = Decimal(sc.FACTOR_SCALE)
ONE = Decimal("1")


def hfq(raw: int, factor: int) -> int:
    """后复权 = 原始价 × 因子 / FactorScale，四舍五入到定点单位。"""
    return int((Decimal(raw) * Decimal(factor) / FACTOR_SCALE).quantize(
        ONE, rounding=ROUND_HALF_UP))


def qfq(raw: int, factor: int, last_factor: int) -> int:
    """前复权 = 后复权价 × FactorScale / 末日因子。

    刻意分两步、每步各自取整 —— 与 Go 侧的实现顺序完全一致。
    若合成一步做无限精度运算，结果可能差 1 个定点单位，
    那不是「谁对谁错」，而是**两侧口径必须一模一样**。
    """
    h = hfq(raw, factor)
    return int((Decimal(h) * FACTOR_SCALE / Decimal(last_factor)).quantize(
        ONE, rounding=ROUND_HALF_UP))


def main() -> int:
    ap = argparse.ArgumentParser(description="比对 Go 与 Python 的复权结果")
    ap.add_argument("csv", help="go run ./cmd/adjcheck 的输出")
    ap.add_argument("--show", type=int, default=10, help="最多展示多少条不一致")
    args = ap.parse_args()

    go = pd.read_csv(args.csv)
    need = {"instrument_id", "trading_day", "factor", "raw_close", "hfq_close", "qfq_close"}
    missing = need - set(go.columns)
    if missing:
        raise SystemExit(f"CSV 缺少列：{sorted(missing)}")
    print(f"Go 侧样本 {len(go)} 行 / {go.instrument_id.nunique()} 只标的")

    fac = pd.read_parquet(layout.meta_path("adj_factor"))
    last = fac.sort_values("ex_date").groupby("instrument_id")["hfq_factor"].last().to_dict()

    # ---- 1. 因子取值是否一致 ----
    # Go 侧用二分找「最后一个 ex_date <= day」；这里用 numpy searchsorted
    # 独立复算，刻意不共用同一段逻辑，以免两边同时错。
    import numpy as np

    fac_by_id: dict[int, tuple] = {}
    for iid, g in fac.sort_values("ex_date").groupby("instrument_id"):
        fac_by_id[int(iid)] = (g["ex_date"].to_numpy(dtype="int64"),
                               g["hfq_factor"].to_numpy(dtype="int64"))

    expect = np.empty(len(go), dtype="int64")
    for i, (iid, day) in enumerate(zip(go["instrument_id"], go["trading_day"])):
        pair = fac_by_id.get(int(iid))
        if pair is None:
            expect[i] = sc.FACTOR_SCALE
            continue
        dates, factors = pair
        k = int(np.searchsorted(dates, int(day), side="right")) - 1
        expect[i] = sc.FACTOR_SCALE if k < 0 else int(factors[k])
    go["expect_factor"] = expect
    bad_factor = go[go["factor"] != go["expect_factor"]]

    # ---- 2. 复权结果是否逐位一致 ----
    rows = []
    for r in go.itertuples(index=False):
        lf = int(last.get(int(r.instrument_id), sc.FACTOR_SCALE))
        rows.append((hfq(int(r.raw_close), int(r.factor)),
                     qfq(int(r.raw_close), int(r.factor), lf)))
    go["py_hfq"] = [x[0] for x in rows]
    go["py_qfq"] = [x[1] for x in rows]
    bad_hfq = go[go["hfq_close"] != go["py_hfq"]]
    bad_qfq = go[go["qfq_close"] != go["py_qfq"]]

    print()
    print(f"因子取值不一致  {len(bad_factor)} / {len(go)}")
    print(f"后复权不一致    {len(bad_hfq)} / {len(go)}")
    print(f"前复权不一致    {len(bad_qfq)} / {len(go)}")

    for name, bad, cols in (
        ("因子", bad_factor, ["instrument_id", "trading_day", "factor", "expect_factor"]),
        ("后复权", bad_hfq, ["instrument_id", "trading_day", "raw_close", "hfq_close", "py_hfq"]),
        ("前复权", bad_qfq, ["instrument_id", "trading_day", "raw_close", "qfq_close", "py_qfq"]),
    ):
        if len(bad):
            print(f"\n--- {name}不一致样本 ---")
            print(bad[cols].head(args.show).to_string(index=False))

    ok = len(bad_factor) == 0 and len(bad_hfq) == 0 and len(bad_qfq) == 0
    print()
    if ok:
        print("✅ Go 与 Python 的复权结果逐位一致 —— C5 第一道关通过")
        return 0
    print("❌ 存在不一致，可复现性无法保证")
    return 1


if __name__ == "__main__":
    sys.exit(main())
