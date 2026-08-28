"""外部数据源适配器 —— 把任意语言写的取数程序接入统一的 DataSource 接口。

对应 `ingest/README.md` 定义的集成契约。调用方拿到的是普通的 `DataSource`，
**无从得知底层是 Go、C++ 还是 Node 写的** —— 这正是约束 C9 的要求：
新增或替换数据源不得改动 DataGateway 以上的任何代码。

状态：接口已实现，但**尚未经真实外部源验证**。`ingest/` 目录当前为空，
A 股的数据需求由 BaoStock 与新浪两个 Python 适配器完全覆盖。
首次接入外部源时应预留调试时间，并补充针对性测试。
"""

from __future__ import annotations

import json
import subprocess
import tempfile
from pathlib import Path

import pandas as pd

from .base import Capability, DataSource, SourceError

_PLACEHOLDER_KEYS = ("symbol", "exchange", "start", "end", "out")


class ExternalSource(DataSource):
    """按 manifest.json 调用外部可执行程序的适配器。"""

    def __init__(self, manifest_path: str | Path) -> None:
        self.manifest_path = Path(manifest_path).resolve()
        self.root = self.manifest_path.parent
        with open(self.manifest_path, encoding="utf-8") as f:
            m = json.load(f)
        self.manifest = m

        self.name = m["name"]
        self.language = m.get("language", "unknown")
        self.mode = m.get("mode", "raw")
        if self.mode not in ("raw", "parquet"):
            raise ValueError(f"{self.name}: 未知 mode={self.mode}")
        self.output_format = m.get("output_format", "jsonl")

        caps = Capability.NONE
        for c in m.get("capabilities", []):
            if not hasattr(Capability, c):
                raise ValueError(f"{self.name}: 未知 capability {c!r}")
            caps |= getattr(Capability, c)
        self.capabilities = caps

    @classmethod
    def from_manifest(cls, path: str | Path) -> "ExternalSource":
        return cls(path)

    @classmethod
    def discover(cls, ingest_root: str | Path = "ingest") -> list["ExternalSource"]:
        """扫描 ingest/ 下所有 manifest.json。`_template` 是骨架，跳过。"""
        root = Path(ingest_root)
        if not root.exists():
            return []
        out = []
        for mf in sorted(root.glob("*/manifest.json")):
            if mf.parent.name.startswith("_"):
                continue
            out.append(cls(mf))
        return out

    # --- 生命周期 ---

    def open(self) -> None:
        build = self.manifest.get("build")
        if not build:
            return
        r = subprocess.run(build, cwd=self.root, capture_output=True, text=True)
        if r.returncode != 0:
            raise SourceError(
                f"{self.name} 构建失败（exit={r.returncode}）：\n{r.stderr[:2000]}")

    # --- 数据接口 ---

    def instruments(self):
        raise NotImplementedError(
            f"{self.name}: 外部源当前只支持行情，标的清单仍走 Python 适配器")

    def calendar(self, start: str, end: str):
        raise NotImplementedError(
            f"{self.name}: 外部源当前只支持行情，交易日历仍走 Python 适配器")

    def daily_bars(self, symbol: str, exchange: str, start: str, end: str) -> pd.DataFrame:
        if self.mode == "parquet":
            raise SourceError(
                f"{self.name}: mode=parquet 的源直接写出成品 Parquet，"
                f"不经由本接口读取；请用 build_bars.py --external 调度")

        with tempfile.TemporaryDirectory(prefix=f"ingest-{self.name}-") as tmp:
            out_path = Path(tmp) / f"out.{self.output_format}"
            cmd = self._render(self.manifest["run"], {
                "symbol": symbol, "exchange": exchange,
                "start": start, "end": end, "out": str(out_path),
            })
            r = subprocess.run(cmd, cwd=self.root, capture_output=True, text=True)
            if r.returncode != 0:
                raise SourceError(
                    f"{self.name} 运行失败（exit={r.returncode}）\n"
                    f"cmd: {' '.join(cmd)}\nstderr: {r.stderr[:2000]}")
            if not out_path.exists():
                raise SourceError(f"{self.name} 未产出 {out_path}")
            return self._read(out_path)

    # --- 内部 ---

    @staticmethod
    def _render(template: list[str], values: dict[str, str]) -> list[str]:
        """占位符替换。命令以数组形式声明并逐项替换，不经 shell，避免注入。"""
        rendered = []
        for part in template:
            for k in _PLACEHOLDER_KEYS:
                part = part.replace("{" + k + "}", str(values.get(k, "")))
            rendered.append(part)
        return rendered

    def _read(self, path: Path) -> pd.DataFrame:
        if self.output_format == "jsonl":
            df = pd.read_json(path, lines=True, dtype=False)
        elif self.output_format == "csv":
            df = pd.read_csv(path, dtype=str)
        else:
            raise SourceError(f"{self.name}: 未知 output_format={self.output_format}")

        required = ["trading_day", "open", "high", "low", "close"]
        missing = [c for c in required if c not in df.columns]
        if missing:
            raise SourceError(
                f"{self.name} 输出缺少必需列 {missing}；"
                f"契约见 ingest/README.md「mode=raw 的输出契约」")

        # 契约允许缺省的列在此补齐，使下游拿到与 Python 适配器一致的形态
        for col, default in (("preclose", None), ("volume", 0), ("amount", 0),
                             ("turn", 0), ("tradestatus", 1), ("is_st", 0)):
            if col not in df.columns:
                df[col] = default
        df["trading_day"] = pd.to_numeric(df["trading_day"], errors="coerce").fillna(0).astype("int64")
        return df.reset_index(drop=True)
