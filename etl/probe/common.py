"""探针公共设施：统一的计时、异常捕获与报告输出。

v0.0 的探针只做一件事：把「AkShare 到底能不能拿到我们需要的数据」这个问题
从假设变成实测结论。因此每个探针都必须在失败时也留下可读的证据，而不是抛栈退出。
"""

from __future__ import annotations

import inspect
import json
import sys
import time
import traceback
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

PROJECT_ROOT = Path(__file__).resolve().parents[2]
REPORT_DIR = PROJECT_ROOT / "docs" / "probe"

# Windows 控制台默认 GBK，中文字段名会乱码；探针输出以可读为先
for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, "reconfigure"):
        _stream.reconfigure(encoding="utf-8", errors="replace")


def retry(fn: Callable[[], Any], attempts: int = 3, delay: float = 2.0) -> Any:
    """东财接口连续请求会返回 RemoteDisconnected，退避重试后基本可恢复。

    探针必须把「接口不支持」和「被限流」区分开——前者是设计约束，后者只是调用姿势问题。
    """
    last: Exception | None = None
    for i in range(attempts):
        try:
            return fn()
        except Exception as exc:  # noqa: BLE001
            last = exc
            if i < attempts - 1:
                time.sleep(delay * (i + 1))
    raise last  # type: ignore[misc]


def polite_sleep(seconds: float = 0.8) -> None:
    """相邻接口调用之间的固定间隔，降低触发限流的概率。"""
    time.sleep(seconds)


@dataclass
class CheckResult:
    """单个检查项的结果。"""

    name: str
    desc: str
    ok: bool = False
    elapsed: float = 0.0
    detail: dict[str, Any] = field(default_factory=dict)
    error: str | None = None

    def line(self) -> str:
        flag = "PASS" if self.ok else "FAIL"
        return f"[{flag}] {self.name:<34} {self.elapsed:6.2f}s  {self.desc}"


class Probe:
    """一组检查项的容器，负责计时、捕获异常与落盘。"""

    def __init__(self, probe_id: str, title: str) -> None:
        self.probe_id = probe_id
        self.title = title
        self.results: list[CheckResult] = []

    def check(self, name: str, desc: str, fn: Callable[[], dict[str, Any]]) -> CheckResult:
        """执行一个检查项。fn 返回 detail 字典；抛异常即记为失败。"""
        started = time.perf_counter()
        result = CheckResult(name=name, desc=desc)
        try:
            detail = fn() or {}
            result.detail = _jsonable(detail)
            # 检查项可通过返回 {"ok": False, ...} 主动判负（接口通了但数据不满足要求）
            result.ok = bool(detail.get("ok", True))
        except Exception as exc:  # noqa: BLE001 - 探针需要吞掉一切并留证据
            result.error = f"{type(exc).__name__}: {exc}"
            result.detail = {"traceback": traceback.format_exc(limit=6)}
        result.elapsed = time.perf_counter() - started
        self.results.append(result)
        print(result.line(), flush=True)
        if result.error:
            print(f"       └─ {result.error}", flush=True)
        return result

    @property
    def passed(self) -> int:
        return sum(1 for r in self.results if r.ok)

    def save(self) -> Path:
        REPORT_DIR.mkdir(parents=True, exist_ok=True)
        path = REPORT_DIR / f"{self.probe_id}.json"
        payload = {
            "probe_id": self.probe_id,
            "title": self.title,
            "run_at": datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds"),
            "passed": self.passed,
            "total": len(self.results),
            "results": [asdict(r) for r in self.results],
        }
        path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"\n{self.title}: {self.passed}/{len(self.results)} 通过 -> {path}", flush=True)
        return path


def describe_df(df: Any, sample_rows: int = 2) -> dict[str, Any]:
    """把 DataFrame 压成可写进报告的摘要。"""
    if df is None:
        return {"empty": True, "reason": "返回 None"}
    if not hasattr(df, "shape"):
        return {"type": type(df).__name__, "repr": str(df)[:400]}
    if len(df) == 0:
        return {"empty": True, "columns": list(df.columns)}
    return {
        "rows": int(len(df)),
        "columns": list(df.columns),
        "head": df.head(sample_rows).to_dict(orient="records"),
        "tail": df.tail(sample_rows).to_dict(orient="records"),
    }


def signature_of(fn: Callable[..., Any]) -> str:
    """记录接口的真实签名——AkShare 接口签名变动频繁，留档便于后续排查。"""
    try:
        return f"{fn.__name__}{inspect.signature(fn)}"
    except (TypeError, ValueError):
        return f"{getattr(fn, '__name__', '?')}(<signature unavailable>)"


def _jsonable(obj: Any) -> Any:
    """把 numpy / pandas / datetime 等类型转成可 JSON 序列化的形式。"""
    if isinstance(obj, dict):
        return {str(k): _jsonable(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [_jsonable(v) for v in obj]
    if isinstance(obj, (str, bool, int, float)) or obj is None:
        return obj
    if hasattr(obj, "item"):  # numpy 标量
        try:
            return obj.item()
        except (ValueError, AttributeError):
            pass
    return str(obj)
