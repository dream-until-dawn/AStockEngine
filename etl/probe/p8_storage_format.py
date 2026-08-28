"""探针 P8：存储格式基准测试。

回答两个问题：

  1. **文件多大** —— 不同压缩、编码、列类型、排序方式下 bar_daily 的实际体积
  2. **跨语言能不能读** —— Python 写出的每个变体，Go 是否都能读回来
     （由 engine/probe/parquet_read 验证，本脚本只负责写出变体与记录尺寸）

关键假设需要实测推翻或确认：金融行情的价格列若用 float64 存储，
尾数位近乎随机，压缩器几乎无从下手；改用**定点整数**（以厘为单位）后
可以走 delta + bitpack，体积应显著下降。

样本取真实 A 股数据而非合成数据 —— 合成数据的压缩率没有参考价值。
"""

from __future__ import annotations

import json
import time
from pathlib import Path

import baostock as bs
import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq

from common import PROJECT_ROOT, Probe

OUT_DIR = PROJECT_ROOT / "data" / "_bench"
SAMPLE_DIR = PROJECT_ROOT / "data" / "_bench_sample"
FIELDS = (
    "date,code,open,high,low,close,preclose,volume,amount,"
    "turn,tradestatus,isST"
)
N_STOCKS = 120
START, END = "2016-01-01", "2026-08-28"

# 价格以厘（0.001 元）为单位存整数：A 股个股 2 位小数、ETF/可转债 3 位小数，
# 乘 1000 后为精确整数，无浮点误差。最大价约 3000 元 -> 300 万，int32 足够。
PRICE_SCALE = 1000


def _to_df(rs) -> pd.DataFrame:
    rows = []
    while rs.error_code == "0" and rs.next():
        rows.append(rs.get_row_data())
    return pd.DataFrame(rows, columns=rs.fields)


def fetch_sample() -> pd.DataFrame:
    """拉取真实样本并缓存，避免反复跑基准时重复请求。"""
    SAMPLE_DIR.mkdir(parents=True, exist_ok=True)
    cache = SAMPLE_DIR / "sample.parquet"
    if cache.exists():
        print(f"复用样本缓存 {cache}", flush=True)
        return pd.read_parquet(cache)

    bs.login()
    rs = bs.query_all_stock(day="2024-06-28")
    universe = _to_df(rs)
    codes = [c for c in universe["code"] if c.startswith(("sh.6", "sz.0", "sz.3"))]
    codes = codes[:N_STOCKS]

    frames = []
    started = time.perf_counter()
    for i, code in enumerate(codes, 1):
        r = bs.query_history_k_data_plus(
            code, FIELDS, start_date=START, end_date=END,
            frequency="d", adjustflag="3",
        )
        df = _to_df(r)
        if len(df):
            frames.append(df)
        if i % 20 == 0:
            print(f"  {i}/{len(codes)}  {time.perf_counter() - started:.0f}s", flush=True)
    bs.logout()

    out = pd.concat(frames, ignore_index=True)
    out.to_parquet(cache, index=False)
    print(f"样本已缓存：{len(out)} 行 -> {cache}", flush=True)
    return out


def normalize(raw: pd.DataFrame) -> pd.DataFrame:
    """转成 bar_daily 的规范形态（float64 版本，作为基线）。"""
    df = pd.DataFrame()
    df["date"] = pd.to_datetime(raw["date"]).dt.date
    df["code"] = raw["code"].astype("string")
    for c in ("open", "high", "low", "close", "preclose", "turn"):
        df[c] = pd.to_numeric(raw[c], errors="coerce").astype("float64")
    df["volume"] = pd.to_numeric(raw["volume"], errors="coerce").fillna(0).astype("int64")
    df["amount"] = pd.to_numeric(raw["amount"], errors="coerce").astype("float64")
    df["tradestatus"] = pd.to_numeric(raw["tradestatus"], errors="coerce").fillna(0).astype("int8")
    df["isST"] = pd.to_numeric(raw["isST"], errors="coerce").fillna(0).astype("int8")
    return df


def to_fixed_point(df: pd.DataFrame) -> pd.DataFrame:
    """价格转定点整数，成交额转分。"""
    out = df.copy()
    for c in ("open", "high", "low", "close", "preclose"):
        out[c] = (out[c] * PRICE_SCALE).round().astype("int32")
    # 换手率保留 6 位小数 -> 乘 1e6 存 int32
    out["turn"] = (out["turn"].fillna(0) * 1_000_000).round().astype("int32")
    out["amount"] = (out["amount"].fillna(0) * 100).round().astype("int64")
    return out


def write_variant(df: pd.DataFrame, name: str, **kwargs) -> dict:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    path = OUT_DIR / f"{name}.parquet"
    table = pa.Table.from_pandas(df, preserve_index=False)
    pq.write_table(table, path, **kwargs)
    size = path.stat().st_size
    return {
        "variant": name,
        "path": str(path.relative_to(PROJECT_ROOT)),
        "bytes": size,
        "mb": round(size / 1024 / 1024, 2),
        "bytes_per_row": round(size / len(df), 2),
        "options": {k: str(v) for k, v in kwargs.items()},
    }


def main() -> None:
    probe = Probe("p8_storage_format", "P8 存储格式基准")

    raw = fetch_sample()
    base = normalize(raw)
    fixed = to_fixed_point(base)
    n = len(base)
    print(f"样本规模：{n} 行 / {base['code'].nunique()} 只标的", flush=True)

    # 排序对压缩率影响很大：同一标的的相邻行价格接近，delta 编码才有效
    base_sorted = base.sort_values(["code", "date"]).reset_index(drop=True)
    fixed_sorted = fixed.sort_values(["code", "date"]).reset_index(drop=True)

    variants = []

    def bench(name, desc, df, **kw):
        def run():
            info = write_variant(df, name, **kw)
            info["rows"] = n
            # 全市场 2000 万行的外推体积
            info["projected_full_mb"] = round(info["bytes"] / n * 20_000_000 / 1024 / 1024, 1)
            variants.append(info)
            return info

        probe.check(f"fmt.{name}", desc, run)

    bench("v1_f64_snappy", "float64 + snappy（朴素基线）",
          base_sorted, compression="snappy")
    bench("v2_f64_zstd", "float64 + zstd",
          base_sorted, compression="zstd", compression_level=3)
    bench("v3_fixed_zstd", "定点整数 + zstd",
          fixed_sorted, compression="zstd", compression_level=3)
    bench("v4_fixed_zstd_unsorted", "定点整数 + zstd + 未排序（对照）",
          fixed, compression="zstd", compression_level=3)
    bench("v5_fixed_zstd9", "定点整数 + zstd level 9",
          fixed_sorted, compression="zstd", compression_level=9)

    # 尝试 delta 编码；部分 Parquet 实现不支持，失败不影响主结论
    def bench_delta():
        try:
            info = write_variant(
                fixed_sorted, "v6_fixed_delta_zstd",
                compression="zstd", compression_level=3,
                column_encoding={
                    "open": "DELTA_BINARY_PACKED",
                    "high": "DELTA_BINARY_PACKED",
                    "low": "DELTA_BINARY_PACKED",
                    "close": "DELTA_BINARY_PACKED",
                    "preclose": "DELTA_BINARY_PACKED",
                    "volume": "DELTA_BINARY_PACKED",
                    "amount": "DELTA_BINARY_PACKED",
                },
                use_dictionary=["code"],
            )
            info["rows"] = n
            info["projected_full_mb"] = round(info["bytes"] / n * 20_000_000 / 1024 / 1024, 1)
            variants.append(info)
            return info
        except Exception as exc:  # noqa: BLE001
            return {"ok": False, "error": f"{type(exc).__name__}: {exc}",
                    "note": "该 pyarrow 版本不支持此编码组合"}

    probe.check("fmt.v6_delta", "定点整数 + DELTA_BINARY_PACKED + zstd", bench_delta)

    def summary():
        ranked = sorted(variants, key=lambda v: v["bytes"])
        baseline = next(v for v in variants if v["variant"] == "v1_f64_snappy")
        best = ranked[0]
        return {
            "rows": n,
            "ranking": [
                {
                    "variant": v["variant"],
                    "mb": v["mb"],
                    "bytes_per_row": v["bytes_per_row"],
                    "projected_full_mb": v["projected_full_mb"],
                    "vs_baseline": f"{v['bytes'] / baseline['bytes']:.2f}x",
                }
                for v in ranked
            ],
            "best": best["variant"],
            "best_projected_full_mb": best["projected_full_mb"],
        }

    probe.check("fmt.summary", "体积排名与全市场外推", summary)

    # 供 Go 侧回读校验：记录每个变体的行数与校验和
    checksum = {
        "rows": int(n),
        "sum_close_fixed": int(fixed_sorted["close"].sum()),
        "sum_volume": int(fixed_sorted["volume"].sum()),
        "distinct_codes": int(fixed_sorted["code"].nunique()),
    }
    (OUT_DIR / "checksum.json").write_text(
        json.dumps(checksum, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    print(f"\n校验基准已写出：{OUT_DIR / 'checksum.json'}", flush=True)
    print(json.dumps(checksum, ensure_ascii=False), flush=True)

    probe.save()


if __name__ == "__main__":
    main()
