"""探针 P9：跨市场统一 schema 的体积成本。

范围明确要求「后续兼容美股、ETF、期货、加密货币」，这对 bar 表的字段类型
提出了比 A 股更宽的要求：

  价格精度  A 股 2~3 位小数（×1e3 后 int32 够）；加密需 1e8 甚至更细
            -> 统一方案必须用 int64 价格
  数量      A 股/美股是整数股；加密是小数（0.001 BTC）
            -> 统一方案 volume 也需 int64 + scale
  时间      A 股单一时区固定时段；加密 24×7；美股有夏令时
            -> 统一方案用 UTC 毫秒时间戳而非日期
  标的标识  各市场 symbol 可能冲突（A 股 000001 与其他市场同名）
            -> 引擎内部用整数 instrument_id，symbol 只在 instruments 表

问题是：为了兼容将来的市场，A 股当前的数据要多付多少存储代价？
本探针用 P8 缓存的真实样本量化这个代价，避免「为了通用性而通用」。
"""

from __future__ import annotations

import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq

from common import PROJECT_ROOT, Probe

SAMPLE = PROJECT_ROOT / "data" / "_bench_sample" / "sample.parquet"
OUT_DIR = PROJECT_ROOT / "data" / "_bench_unified"

PRICE_SCALE = 1000
DELTA_COLS = ("open", "high", "low", "close", "preclose", "volume", "amount")


def build_ashare_specific(raw: pd.DataFrame) -> pd.DataFrame:
    """A 股专用 schema：价格 int32、标的用字符串代码。"""
    df = pd.DataFrame()
    df["date"] = pd.to_datetime(raw["date"]).dt.date
    df["code"] = raw["code"].astype("string")
    for c in ("open", "high", "low", "close", "preclose"):
        df[c] = (pd.to_numeric(raw[c], errors="coerce") * PRICE_SCALE).round().astype("int32")
    df["volume"] = pd.to_numeric(raw["volume"], errors="coerce").fillna(0).astype("int64")
    df["amount"] = (pd.to_numeric(raw["amount"], errors="coerce").fillna(0) * 100).round().astype("int64")
    df["turn"] = (pd.to_numeric(raw["turn"], errors="coerce").fillna(0) * 1_000_000).round().astype("int32")
    df["tradestatus"] = pd.to_numeric(raw["tradestatus"], errors="coerce").fillna(0).astype("int8")
    df["isST"] = pd.to_numeric(raw["isST"], errors="coerce").fillna(0).astype("int8")
    return df.sort_values(["code", "date"]).reset_index(drop=True)


def build_unified(raw: pd.DataFrame) -> pd.DataFrame:
    """跨市场统一 schema：instrument_id uint32、ts int64(UTC ms)、价格与数量 int64。

    市场特定字段（tradestatus / isST / turn）不进核心表，放扩展表。
    这里保留它们只是为了与 A 股专用版做等价对比。
    """
    codes = sorted(raw["code"].unique())
    code_to_id = {c: i for i, c in enumerate(codes, start=1)}

    df = pd.DataFrame()
    df["instrument_id"] = raw["code"].map(code_to_id).astype("uint32")
    # A 股日线的时点取当日收盘 15:00 CST = 07:00 UTC
    ts = pd.to_datetime(raw["date"]) + pd.Timedelta(hours=7)
    df["ts"] = (ts.astype("int64") // 1_000_000).astype("int64")  # ns -> ms
    df["trading_day"] = pd.to_datetime(raw["date"]).dt.strftime("%Y%m%d").astype("int32")
    for c in ("open", "high", "low", "close", "preclose"):
        df[c] = (pd.to_numeric(raw[c], errors="coerce") * PRICE_SCALE).round().astype("int64")
    df["volume"] = pd.to_numeric(raw["volume"], errors="coerce").fillna(0).astype("int64")
    df["amount"] = (pd.to_numeric(raw["amount"], errors="coerce").fillna(0) * 100).round().astype("int64")
    df["turn"] = (pd.to_numeric(raw["turn"], errors="coerce").fillna(0) * 1_000_000).round().astype("int32")
    df["tradestatus"] = pd.to_numeric(raw["tradestatus"], errors="coerce").fillna(0).astype("int8")
    df["isST"] = pd.to_numeric(raw["isST"], errors="coerce").fillna(0).astype("int8")
    return df.sort_values(["instrument_id", "ts"]).reset_index(drop=True)


def build_unified_core(unified: pd.DataFrame) -> pd.DataFrame:
    """统一 schema 的**核心表**：只保留全市场共有字段，市场特定列移出。"""
    return unified[
        ["instrument_id", "ts", "trading_day", "open", "high", "low", "close", "volume", "amount"]
    ].copy()


def build_ashare_ext(unified: pd.DataFrame) -> pd.DataFrame:
    """A 股扩展表：市场特定字段，按 (instrument_id, ts) 与核心表对齐。"""
    return unified[["instrument_id", "ts", "preclose", "turn", "tradestatus", "isST"]].copy()


def write(df: pd.DataFrame, name: str, delta: bool = True) -> dict:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    path = OUT_DIR / f"{name}.parquet"
    kw: dict = {"compression": "zstd", "compression_level": 3}
    if delta:
        enc = {c: "DELTA_BINARY_PACKED" for c in DELTA_COLS if c in df.columns}
        if "ts" in df.columns:
            enc["ts"] = "DELTA_BINARY_PACKED"
        if "trading_day" in df.columns:
            enc["trading_day"] = "DELTA_BINARY_PACKED"
        if enc:
            kw["column_encoding"] = enc
        if "code" in df.columns:
            kw["use_dictionary"] = ["code"]
        elif "instrument_id" in df.columns:
            kw["use_dictionary"] = False
    pq.write_table(pa.Table.from_pandas(df, preserve_index=False), path, **kw)
    size = path.stat().st_size
    return {
        "name": name,
        "bytes": size,
        "mb": round(size / 1024 / 1024, 2),
        "bytes_per_row": round(size / len(df), 2),
        "projected_full_mb": round(size / len(df) * 20_000_000 / 1024 / 1024, 1),
        "columns": list(df.columns),
    }


def main() -> None:
    probe = Probe("p9_unified_schema", "P9 跨市场统一 schema 成本")

    if not SAMPLE.exists():
        raise SystemExit(f"缺少样本 {SAMPLE}，请先运行 p8_storage_format.py")
    raw = pd.read_parquet(SAMPLE)
    n = len(raw)
    print(f"样本：{n} 行 / {raw['code'].nunique()} 只标的", flush=True)

    ashare = build_ashare_specific(raw)
    unified = build_unified(raw)
    core = build_unified_core(unified)
    ext = build_ashare_ext(unified)

    sizes: dict[str, dict] = {}

    def bench(key, desc, df, delta=True):
        def run():
            info = write(df, key, delta=delta)
            sizes[key] = info
            return info

        probe.check(f"sch.{key}", desc, run)

    bench("a_specific", "A 股专用 schema（int32 价格 + 字符串代码）", ashare)
    bench("unified_wide", "统一 schema 单宽表（int64 价格 + instrument_id）", unified)
    bench("unified_core", "统一 schema 核心表（仅全市场共有字段）", core)
    bench("ashare_ext", "A 股扩展表（市场特定字段）", ext)

    def verdict():
        a = sizes["a_specific"]
        w = sizes["unified_wide"]
        split_bytes = sizes["unified_core"]["bytes"] + sizes["ashare_ext"]["bytes"]
        return {
            "rows": n,
            "a_specific_mb": a["mb"],
            "unified_wide_mb": w["mb"],
            "unified_split_mb": round(split_bytes / 1024 / 1024, 2),
            "wide_vs_specific": f"{w['bytes'] / a['bytes']:.3f}x",
            "split_vs_specific": f"{split_bytes / a['bytes']:.3f}x",
            "projected_full_specific_mb": a["projected_full_mb"],
            "projected_full_wide_mb": w["projected_full_mb"],
            "projected_full_split_mb": round(
                split_bytes / n * 20_000_000 / 1024 / 1024, 1
            ),
            "note": "统一 schema 的额外代价 = int64 价格 + ts 时间戳；"
                    "instrument_id 取代字符串代码则是净收益",
        }

    probe.check("sch.verdict", "统一 schema 的净成本", verdict)
    probe.save()


if __name__ == "__main__":
    main()
