"""探针 P5：东财 API 限流特征。

P1/P2 实测中发现：东财 push2his.eastmoney.com 在约 10 次请求后开始
返回 RemoteDisconnected，且封禁是 IP + 端点级别的（裸 requests 同样被拒，
但 quote.eastmoney.com 网页正常）。

这直接决定 v0.1 全量 ETL 的可行性：全 A 股 5551 只 + 约 370 只退市股，
若不能稳定拉取，数据地基就无从谈起。本探针测三件事：

  1. 封禁恢复时长
  2. 触发封禁的请求次数阈值
  3. 加入间隔后的可持续速率

用法：
    python p5_ratelimit.py recover     # 阶段一：等待并测量恢复时长
    python p5_ratelimit.py threshold   # 阶段二：无间隔连打，测触发阈值
    python p5_ratelimit.py sustain 1.5 # 阶段三：固定间隔下的可持续性
"""

from __future__ import annotations

import sys
import time

import akshare as ak

from common import Probe, polite_sleep

PROBE_INTERVAL = 60.0
MAX_WAIT = 45 * 60.0


def _hit() -> int:
    """一次最小成本的东财请求，返回行数。"""
    df = ak.stock_zh_a_hist(
        symbol="600519", period="daily",
        start_date="20240101", end_date="20240131", adjust="",
    )
    return len(df)


def _alive() -> bool:
    try:
        _hit()
        return True
    except Exception:  # noqa: BLE001
        return False


def phase_recover(probe: Probe) -> None:
    def run():
        started = time.perf_counter()
        attempts = 0
        while time.perf_counter() - started < MAX_WAIT:
            attempts += 1
            if _alive():
                waited = time.perf_counter() - started
                return {
                    "recovered": True,
                    "waited_seconds": round(waited, 1),
                    "waited_minutes": round(waited / 60, 1),
                    "attempts": attempts,
                }
            time.sleep(PROBE_INTERVAL)
        return {
            "ok": False,
            "recovered": False,
            "waited_minutes": round(MAX_WAIT / 60, 1),
            "note": "超过最长等待仍未恢复，需考虑更换数据源或代理",
        }

    probe.check("em.recover", "东财封禁恢复时长", run)


def phase_threshold(probe: Probe) -> None:
    def run():
        if not _alive():
            return {"ok": False, "note": "起始即处于封禁状态，无法测阈值"}
        ok_count = 1
        for _ in range(60):
            if _alive():
                ok_count += 1
            else:
                return {
                    "consecutive_ok_before_block": ok_count,
                    "note": "无间隔连续请求下触发封禁的次数",
                }
        return {"consecutive_ok_before_block": ok_count, "note": "60 次内未触发封禁"}

    probe.check("em.threshold", "无间隔连打触发封禁的次数", run)


def phase_sustain(probe: Probe, interval: float) -> None:
    def run():
        if not _alive():
            return {"ok": False, "note": "起始即处于封禁状态，无法测可持续速率"}
        ok_count = 1
        for _ in range(40):
            time.sleep(interval)
            if _alive():
                ok_count += 1
            else:
                return {
                    "ok": False,
                    "interval": interval,
                    "ok_before_block": ok_count,
                    "note": f"间隔 {interval}s 仍会触发封禁",
                }
        return {
            "interval": interval,
            "ok_count": ok_count,
            "note": f"间隔 {interval}s 下连续 {ok_count} 次未触发封禁",
        }

    probe.check(f"em.sustain_{interval}", f"间隔 {interval}s 的可持续性", run)


def main() -> None:
    mode = sys.argv[1] if len(sys.argv) > 1 else "recover"
    probe = Probe(f"p5_ratelimit_{mode}", f"P5 东财限流（{mode}）")

    if mode == "recover":
        phase_recover(probe)
    elif mode == "threshold":
        phase_threshold(probe)
    elif mode == "sustain":
        interval = float(sys.argv[2]) if len(sys.argv) > 2 else 1.5
        phase_sustain(probe, interval)
    else:
        raise SystemExit(f"未知模式：{mode}")

    probe.save()


if __name__ == "__main__":
    main()
