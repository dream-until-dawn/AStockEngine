"""解释器与依赖的前置检查。

必须在导入任何第三方包**之前**执行 —— 用错解释器时，失败点会落在
`import pandas` 上，报出来的是一堆看不懂的栈，掩盖了真正的原因。

无人值守场景下这一步尤其重要：若夜里用错解释器启动，早上会发现什么都没跑。
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[1]
VENV_DIR = PROJECT_ROOT / ".venv"
VENV_PY_WIN = VENV_DIR / "Scripts" / "python.exe"
VENV_PY_POSIX = VENV_DIR / "bin" / "python"

REQUIRED = ("pandas", "pyarrow", "baostock", "akshare")


def _venv_python() -> Path:
    return VENV_PY_WIN if VENV_PY_WIN.exists() else VENV_PY_POSIX


def _hint() -> str:
    py = _venv_python()
    try:
        rel = py.relative_to(Path.cwd())
    except ValueError:
        rel = py
    script = Path(sys.argv[0]).name if sys.argv and sys.argv[0] else "etl/xxx.py"
    args = " ".join(sys.argv[1:])
    return (
        f"\n请改用项目虚拟环境的解释器：\n\n"
        f"  PowerShell:  .\\{rel} etl\\{script} {args}\n"
        f"  Bash:        ./{Path(rel).as_posix()} etl/{script} {args}\n\n"
        f"若虚拟环境尚未创建：\n\n"
        f"  python -m venv .venv\n"
        f"  .\\.venv\\Scripts\\python.exe -m pip install --no-cache-dir -r etl/requirements.txt\n"
        f"（用户名含非 ASCII 字符时 pip 缓存会报 PermissionError，故须带 --no-cache-dir）\n"
    )


def ensure() -> None:
    """确认当前解释器具备全部依赖；不具备则给出可直接复制的命令后退出。"""
    missing = [m for m in REQUIRED if importlib.util.find_spec(m) is None]
    if not missing:
        return

    in_venv = VENV_DIR.exists() and str(Path(sys.prefix).resolve()).startswith(
        str(VENV_DIR.resolve()))

    print("=" * 68, file=sys.stderr)
    print("依赖缺失，无法启动", file=sys.stderr)
    print("=" * 68, file=sys.stderr)
    print(f"当前解释器: {sys.executable}", file=sys.stderr)
    print(f"缺少的包  : {', '.join(missing)}", file=sys.stderr)
    if not in_venv and _venv_python().exists():
        print("\n原因：当前用的是系统 Python，不是项目虚拟环境。", file=sys.stderr)
        print(_hint(), file=sys.stderr)
    else:
        print(f"\n请先安装依赖：\n\n  {sys.executable} -m pip install "
              f"--no-cache-dir -r etl/requirements.txt\n", file=sys.stderr)
    raise SystemExit(78)  # EX_CONFIG


ensure()
