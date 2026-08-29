"""数据目录布局的单一事实源。

`data/` 不进 git，因此目录结构不能只靠文档描述 —— 必须由代码定义并可一键重建。
ETL、探针与将来的 Go 引擎都以本模块声明的路径为准，不在各处硬编码字符串。

直接运行可创建全部目录并打印结构：

    python etl/layout.py
"""

from __future__ import annotations

import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[1]
DATA_ROOT = PROJECT_ROOT / "data"
CONFIG_ROOT = PROJECT_ROOT / "configs"

# --- 一级目录 ---------------------------------------------------------------

BAR_ROOT = DATA_ROOT / "bar"            # 行情时序，Hive 风格分区
META_ROOT = DATA_ROOT / "meta"          # 元数据表（含 CSV 镜像）
RESULTS_ROOT = DATA_ROOT / "results"    # 海选结果，由 Go 引擎写入
SNAPSHOT_ROOT = DATA_ROOT / "snapshots" # 引擎状态快照（v0.4）
CACHE_ROOT = DATA_ROOT / "cache"        # ETL 中间缓存，可安全删除
BENCH_ROOT = DATA_ROOT / "_bench"       # 探针基准产物，可安全删除

# --- 元数据表 ---------------------------------------------------------------

# 全部元数据表同时输出 CSV 镜像：Parquet 为准，CSV 仅供人眼检查与 diff。
# bar 表体量太大（2000 万行级），不做镜像。
META_TABLES = (
    "instruments",       # 标的静态属性
    "calendar",          # 交易日历
    "adj_factor",        # 复权因子（事件式）
    "corporate_action",  # 分红送配
)

MARKETS = ("ashare", "crypto")    # 远期扩展：us / hk / futures
FREQS = ("1d",)          # 远期扩展：1m / 5m / 1h


def bar_dir(market: str = "ashare", freq: str = "1d", year: int | None = None) -> Path:
    """行情分区目录。Hive 风格，DuckDB / Spark / Athena 均可自动识别。"""
    p = BAR_ROOT / f"market={market}" / f"freq={freq}"
    if year is not None:
        p = p / f"year={year}"
    return p


def meta_path(table: str, fmt: str = "parquet") -> Path:
    """元数据表路径。fmt 取 parquet（权威）或 csv（人眼镜像）。"""
    if table not in META_TABLES:
        raise ValueError(f"未知元数据表：{table}（可选 {META_TABLES}）")
    if fmt not in ("parquet", "csv"):
        raise ValueError(f"未知格式：{fmt}")
    return META_ROOT / f"{table}.{fmt}"


def write_meta(df, table: str) -> tuple[Path, Path]:
    """写出元数据表及其 CSV 镜像，返回两个路径。

    Parquet 是权威副本，引擎只读它；CSV 仅为调试便利，任何程序都不得依赖。
    """
    META_ROOT.mkdir(parents=True, exist_ok=True)
    pq_path = meta_path(table, "parquet")
    csv_path = meta_path(table, "csv")
    df.to_parquet(pq_path, index=False, compression="zstd")
    df.to_csv(csv_path, index=False, encoding="utf-8-sig")
    return pq_path, csv_path


def all_dirs() -> list[Path]:
    dirs = [
        DATA_ROOT, BAR_ROOT, META_ROOT, RESULTS_ROOT,
        RESULTS_ROOT / "equity", SNAPSHOT_ROOT, CACHE_ROOT,
        CONFIG_ROOT, CONFIG_ROOT / "fee", CONFIG_ROOT / "market",
        CONFIG_ROOT / "strategy",
    ]
    for m in MARKETS:
        for f in FREQS:
            dirs.append(bar_dir(m, f))
    return dirs


def ensure() -> list[Path]:
    created = []
    for d in all_dirs():
        if not d.exists():
            d.mkdir(parents=True, exist_ok=True)
            created.append(d)
    return created


def main() -> None:
    created = ensure()
    print(f"数据根目录：{DATA_ROOT}")
    print(f"配置根目录：{CONFIG_ROOT}")
    print(f"新建 {len(created)} 个目录\n")
    for d in sorted(all_dirs()):
        rel = d.relative_to(PROJECT_ROOT)
        mark = "  (新建)" if d in created else ""
        print(f"  {rel.as_posix()}/{mark}")
    print("\n元数据表（Parquet 权威 + CSV 镜像）：")
    for t in META_TABLES:
        print(f"  {meta_path(t).relative_to(PROJECT_ROOT).as_posix()}"
              f"  +  {meta_path(t, 'csv').name}")


if __name__ == "__main__":
    sys.exit(main())
