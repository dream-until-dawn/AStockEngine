"""bar 全量拉取与每日增量更新。面向**无人值守**运行设计。

同一个入口同时承担两件事，靠状态文件区分：
  - 首次运行 = 全量拉取
  - 之后运行 = 只拉每只标的 last_day 之后的新数据

无人值守下的失败模式与对策：

  内存耗尽      全量约 2000 万行，攒在内存里最后一次性写必然 OOM 且前功尽弃
                → 每 --batch 只标的落盘一次，落盘后立即清空
  会话过期      BaoStock 是长连接，跑几小时会掉线
                → worker 捕获后重新登录并重试，不中断整轮
  单只标的失败  网络抖动、数据源缺该标的、字段异常
                → 分类重试（瞬时错误退避重试，永久错误直接跳过），
                  失败只记录不抛出，绝不中断整轮
  意外中止      断电、Ctrl+C、被杀进程
                → 状态文件在每批**落盘之后**立即更新，重跑最多丢一批
  重复数据      落盘成功但状态未及写入时重跑，会产生重复行
                → compact 阶段按 (instrument_id, ts_close) 去重，系统自愈

用法：
    python etl/sync_bars.py --limit 8              # 试跑 8 只
    python etl/sync_bars.py --reset                # 首次全量（清空重来）
    python etl/sync_bars.py                        # 每日增量
    python etl/sync_bars.py --status               # 只看进度
    python etl/sync_bars.py --retry-failed         # 重试失败标的
    python etl/sync_bars.py --compact              # 合并碎片并去重
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import shutil
import signal
import sys
import time
from datetime import datetime, timedelta
from multiprocessing import Pool
from pathlib import Path

import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402
from build_bars import fill_etf_preclose, to_bar_frame  # noqa: E402
from sources import BaoStockSource, SinaSource, SourceError  # noqa: E402

STATE_PATH = layout.META_ROOT / "_sync_state.json"
LOG_DIR = layout.CACHE_ROOT / "logs"
DEFAULT_START = "2005-01-01"

_EXCHANGE_TAG = {
    int(sc.Exchange.SSE): "sh",
    int(sc.Exchange.SZSE): "sz",
    int(sc.Exchange.BSE): "bj",
}

# 判定为「永久性」的错误：重试无意义，直接标记跳过。
# 其余一律视为瞬时错误，走退避重试。
_PERMANENT_HINTS = ("no data", "不存在", "无此", "empty")

_stop_requested = False
log = logging.getLogger("sync")


# --------------------------------------------------------------------------
# 状态
# --------------------------------------------------------------------------


def load_state() -> dict:
    if not STATE_PATH.exists():
        return {"instruments": {}, "runs": []}
    try:
        return json.loads(STATE_PATH.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        # 状态文件损坏（例如写入过程中断电）时退回备份，而非丢失全部进度
        bak = STATE_PATH.with_suffix(".json.bak")
        if bak.exists():
            log.warning("状态文件损坏，回退到备份 %s", bak)
            return json.loads(bak.read_text(encoding="utf-8"))
        log.error("状态文件损坏且无备份，将从头开始")
        return {"instruments": {}, "runs": []}


def save_state(state: dict) -> None:
    """先写临时文件再原子替换，避免写到一半被中断导致状态文件损坏。"""
    STATE_PATH.parent.mkdir(parents=True, exist_ok=True)
    if STATE_PATH.exists():
        shutil.copy2(STATE_PATH, STATE_PATH.with_suffix(".json.bak"))
    tmp = STATE_PATH.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(state, ensure_ascii=False, indent=1), encoding="utf-8")
    os.replace(tmp, STATE_PATH)


# --------------------------------------------------------------------------
# worker（子进程）
# --------------------------------------------------------------------------

_SRC = None


def _init_worker(kind: str) -> None:
    global _SRC
    logging.basicConfig(level=logging.WARNING)
    _SRC = BaoStockSource() if kind == "baostock" else SinaSource()
    _SRC.open()


def _relogin() -> None:
    """BaoStock 长连接掉线后重建会话。"""
    global _SRC
    try:
        _SRC.close()
    except Exception:  # noqa: BLE001
        pass
    time.sleep(2.0)
    _SRC.open()


def _fetch_one(task: dict) -> dict:
    """拉取单只标的。**任何异常都不得逃逸** —— 一只标的失败不能中断整轮。"""
    iid = task["iid"]
    started = time.perf_counter()
    last_err = ""
    for attempt in range(1, task["retries"] + 1):
        try:
            raw = _SRC.daily_bars(task["symbol"], task["exchange"],
                                  task["start"], task["end"])
            df = to_bar_frame(raw, iid, task["price_scale"])
            filled = 0
            if task["is_etf"] and not df.empty:
                filled = fill_etf_preclose(df)
            return {
                "iid": iid, "symbol": task["symbol"], "status": "done",
                "rows": len(df), "filled": filled,
                "last_day": int(df["trading_day"].max()) if len(df) else task["prev_last_day"],
                "elapsed": time.perf_counter() - started,
                "data": df if len(df) else None,
                "attempts": attempt, "error": "",
            }
        except Exception as exc:  # noqa: BLE001
            last_err = f"{type(exc).__name__}: {exc}"
            low = last_err.lower()
            if any(h in low for h in _PERMANENT_HINTS):
                break  # 永久性错误，重试无意义
            if isinstance(exc, SourceError) or "connect" in low or "socket" in low:
                try:
                    _relogin()
                except Exception as re_exc:  # noqa: BLE001
                    last_err = f"重登录失败 {type(re_exc).__name__}: {re_exc}"
            if attempt < task["retries"]:
                time.sleep(min(2.0 * attempt, 10.0))

    return {
        "iid": iid, "symbol": task["symbol"], "status": "failed",
        "rows": 0, "filled": 0, "last_day": task["prev_last_day"],
        "elapsed": time.perf_counter() - started, "data": None,
        "attempts": task["retries"], "error": last_err[:400],
    }


# --------------------------------------------------------------------------
# 落盘
# --------------------------------------------------------------------------


def flush(frames: list[pd.DataFrame], shard: int) -> tuple[int, int]:
    """把一批 bar 写入年份分区。返回 (行数, 字节数)。

    每个分片文件内部按 (instrument_id, ts_close) 排序即满足 SCHEMA.md 0.5
    —— 该约定是按文件而非按分区要求的。
    """
    if not frames:
        return 0, 0
    df = pd.concat(frames, ignore_index=True)
    if df.empty:
        return 0, 0
    df = df.astype({
        "instrument_id": "int32", "trading_day": "int32",
        "turn": "int32", "tradestatus": "int8", "is_st": "int8",
    })
    rows = nbytes = 0
    for year, g in df.groupby(df["trading_day"] // 10000):
        d = layout.bar_dir("ashare", "1d", int(year))
        d.mkdir(parents=True, exist_ok=True)
        path = d / f"part-{shard:05d}.parquet"
        g = g.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
        sc.validate_columns(g, "bar")
        pq.write_table(pa.Table.from_pandas(g, schema=sc.BAR_SCHEMA, preserve_index=False),
                       path, **sc.parquet_write_options("bar"))
        rows += len(g)
        nbytes += path.stat().st_size
    return rows, nbytes


def compact() -> None:
    """合并每个年份分区内的碎片并去重。

    去重是本流程的自愈机制：若某次运行在「分片已落盘、状态未及更新」之间中止，
    重跑会产生重复行，此处按主键消除。
    """
    root = layout.bar_dir("ashare", "1d")
    if not root.exists():
        log.warning("无 bar 数据可合并")
        return
    for ydir in sorted(root.glob("year=*")):
        files = sorted(ydir.glob("*.parquet"))
        if not files:
            continue
        df = pd.concat([pd.read_parquet(f) for f in files], ignore_index=True)
        before = len(df)
        df = df.drop_duplicates(subset=["instrument_id", "ts_close"], keep="last")
        df = df.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
        tmp = ydir / "_compacted.parquet"
        pq.write_table(pa.Table.from_pandas(df, schema=sc.BAR_SCHEMA, preserve_index=False),
                       tmp, **sc.parquet_write_options("bar"))
        for f in files:
            f.unlink()
        os.replace(tmp, ydir / "part-00000.parquet")
        log.info("%s: %d 个分片 %d 行 -> 1 个文件 %d 行（去重 %d）",
                 ydir.name, len(files), before, len(df), before - len(df))


# --------------------------------------------------------------------------
# 主流程
# --------------------------------------------------------------------------


def build_tasks(inst: pd.DataFrame, state: dict, args) -> tuple[list[dict], list[dict], int]:
    """按状态生成待办任务。已是最新的标的直接跳过（每日增量的核心）。"""
    end_ymd = int(args.end.replace("-", ""))
    stocks, etfs, skipped = [], [], 0

    for r in inst.itertuples(index=False):
        iid = int(r.instrument_id)
        st = state["instruments"].get(str(iid), {})
        if st.get("status") == "failed" and not args.retry_failed:
            skipped += 1
            continue

        prev_last = int(st.get("last_day", 0))
        if prev_last:
            # 增量：从已有数据的次日开始
            nxt = sc.int_to_date(prev_last) + timedelta(days=1)
            start = nxt.strftime("%Y-%m-%d")
            if int(nxt.strftime("%Y%m%d")) > end_ymd:
                skipped += 1
                continue
        else:
            # 首次：从上市日与全局起点的较晚者开始，避免拉取大段空数据
            lst = int(r.list_date) if r.list_date else 0
            base = max(lst, int(args.start.replace("-", ""))) if lst else int(args.start.replace("-", ""))
            start = f"{base//10000:04d}-{base//100%100:02d}-{base%100:02d}"

        is_etf = int(r.type) == int(sc.InstrumentType.ETF)
        task = {
            "iid": iid, "symbol": r.symbol,
            "exchange": _EXCHANGE_TAG.get(int(r.exchange), "sh"),
            "price_scale": int(r.price_scale), "is_etf": is_etf,
            "start": start, "end": args.end,
            "retries": args.retries, "prev_last_day": prev_last,
        }
        (etfs if is_etf else stocks).append(task)

    if args.limit:
        stocks = stocks[:args.limit]
        etfs = etfs[:max(1, args.limit // 2)]
    return stocks, etfs, skipped


def run_group(tasks: list[dict], kind: str, workers: int, state: dict,
              args, shard_start: int) -> tuple[int, int, int, int]:
    """跑一组任务。返回 (完成数, 失败数, 行数, 下一个分片号)。"""
    global _stop_requested
    if not tasks:
        return 0, 0, 0, shard_start

    log.info("=== %s：%d 只标的，%d 并发 ===", kind, len(tasks), workers)
    done = failed = total_rows = 0
    shard = shard_start
    buf: list[pd.DataFrame] = []
    started = time.perf_counter()

    with Pool(processes=workers, initializer=_init_worker, initargs=(kind,)) as pool:
        for i, res in enumerate(pool.imap_unordered(_fetch_one, tasks), 1):
            if res["status"] == "done":
                done += 1
                if res["data"] is not None:
                    buf.append(res["data"])
                    total_rows += res["rows"]
            else:
                failed += 1
                log.warning("失败 %s: %s", res["symbol"], res["error"][:160])

            state["instruments"][str(res["iid"])] = {
                "symbol": res["symbol"], "status": res["status"],
                "last_day": res["last_day"], "rows": res["rows"],
                "attempts": res["attempts"], "error": res["error"],
                "updated_at": datetime.now().isoformat(timespec="seconds"),
            }

            # 落盘 -> 再更新状态。顺序不可颠倒：反过来会在中断时丢数据且状态显示已完成
            if len(buf) >= args.batch or i == len(tasks) or _stop_requested:
                rows, nbytes = flush(buf, shard)
                buf.clear()
                shard += 1
                save_state(state)
                elapsed = time.perf_counter() - started
                rate = i / elapsed if elapsed else 0
                eta = (len(tasks) - i) / rate if rate else 0
                log.info("[%s] %d/%d  完成%d 失败%d  本批%d行/%.1fMB  "
                         "%.2f只/秒  剩余约%s",
                         kind, i, len(tasks), done, failed, rows, nbytes / 1024 / 1024,
                         rate, str(timedelta(seconds=int(eta))))

            if _stop_requested:
                log.warning("收到停止信号，已保存进度后退出（重跑即从此处继续）")
                pool.terminate()
                break

    return done, failed, total_rows, shard


def verify_all(inst: pd.DataFrame, cal: pd.DataFrame) -> int:
    """按年份分区流式质检，返回问题条数。

    全量约 2600 万行，一次性载入 pandas 约需 3 GB —— 无人值守下不能这么干。
    逐年检查把内存压在单年（约 300 万行）以内。

    代价：`check_bars` 中「排除上市后前 N 日」的逻辑按分区内的行序生效，
    逐年调用会多排除每年头几行，因此本模式只会漏报、不会误报，够用作总体体检。
    """
    from build_bars import check_bars

    root = layout.bar_dir("ashare", "1d")
    years = sorted(root.glob("year=*"))
    if not years:
        log.warning("无 bar 数据可校验")
        return 0

    total_rows = total_problems = 0
    for ydir in years:
        files = sorted(ydir.glob("*.parquet"))
        if not files:
            continue
        df = pd.concat([pd.read_parquet(f) for f in files], ignore_index=True)
        df = df.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
        total_rows += len(df)
        problems = check_bars(df, inst, cal)
        if problems:
            total_problems += len(problems)
            for p in problems:
                log.warning("[%s] %s", ydir.name, p)
        else:
            log.info("[%s] %d 行 / %d 标的  质检通过",
                     ydir.name, len(df), df["instrument_id"].nunique())
        del df

    log.info("质检合计：%d 年份 / %d 行 / %d 项问题", len(years), total_rows, total_problems)
    return total_problems


def print_status(inst: pd.DataFrame, state: dict) -> None:
    recs = state["instruments"]
    total = len(inst)
    done = sum(1 for v in recs.values() if v.get("status") == "done")
    failed = sum(1 for v in recs.values() if v.get("status") == "failed")
    rows = sum(int(v.get("rows", 0)) for v in recs.values())
    print(f"标的总数   {total}")
    print(f"  已完成   {done}")
    print(f"  失败     {failed}")
    print(f"  未开始   {total - len(recs)}")
    print(f"累计行数   {rows}")
    if failed:
        print("\n失败样本：")
        n = 0
        for k, v in recs.items():
            if v.get("status") == "failed":
                print(f"  {v['symbol']}  {v.get('error','')[:110]}")
                n += 1
                if n >= 10:
                    break
    root = layout.bar_dir("ashare", "1d")
    if root.exists():
        files = list(root.rglob("*.parquet"))
        size = sum(f.stat().st_size for f in files)
        print(f"\n落盘文件   {len(files)} 个 / {size/1024/1024:.1f} MB")


def setup_logging(quiet: bool) -> Path:
    LOG_DIR.mkdir(parents=True, exist_ok=True)
    path = LOG_DIR / f"sync_{datetime.now():%Y%m%d_%H%M%S}.log"
    handlers: list[logging.Handler] = [logging.FileHandler(path, encoding="utf-8")]
    if not quiet:
        sh = logging.StreamHandler(sys.stdout)
        handlers.append(sh)
    logging.basicConfig(
        level=logging.INFO, handlers=handlers,
        format="%(asctime)s %(levelname)-7s %(message)s", datefmt="%H:%M:%S",
    )
    return path


def _on_signal(signum, frame):  # noqa: ARG001
    global _stop_requested
    _stop_requested = True
    log.warning("收到信号 %s，将在当前批次结束后安全退出 ...", signum)


def main() -> int:
    ap = argparse.ArgumentParser(description="bar 全量拉取 / 每日增量更新")
    ap.add_argument("--start", default=DEFAULT_START, help="全局起始日（首次拉取用）")
    ap.add_argument("--end", default=datetime.now().strftime("%Y-%m-%d"))
    ap.add_argument("--workers", type=int, default=4, help="并发进程数")
    ap.add_argument("--batch", type=int, default=200, help="每多少只标的落盘一次")
    ap.add_argument("--retries", type=int, default=3, help="单只标的最大尝试次数")
    ap.add_argument("--limit", type=int, default=0, help="只跑前 N 只（试跑用）")
    ap.add_argument("--only", choices=["stocks", "etfs"], help="只跑其中一类")
    ap.add_argument("--reset", action="store_true", help="清空既有 bar 与状态后重来")
    ap.add_argument("--retry-failed", action="store_true", help="把失败标的重新纳入")
    ap.add_argument("--status", action="store_true", help="只打印进度")
    ap.add_argument("--compact", action="store_true", help="只做分片合并与去重")
    ap.add_argument("--verify", action="store_true", help="只做逐年质检")
    ap.add_argument("--no-verify", action="store_true", help="结束后不自动质检")
    ap.add_argument("--no-compact", action="store_true", help="结束后不自动合并")
    ap.add_argument("--quiet", action="store_true", help="只写日志文件不输出到终端")
    args = ap.parse_args()

    layout.ensure()
    log_path = setup_logging(args.quiet)

    inst = pd.read_parquet(layout.meta_path("instruments"))
    inst = inst[inst["type"].isin([int(sc.InstrumentType.STOCK), int(sc.InstrumentType.ETF)])]

    if args.status:
        print_status(inst, load_state())
        return 0
    if args.compact:
        compact()
        return 0
    if args.verify:
        cal = pd.read_parquet(layout.meta_path("calendar"))
        return 0 if verify_all(inst, cal) == 0 else 2

    if args.reset:
        root = layout.bar_dir("ashare", "1d")
        if root.exists():
            shutil.rmtree(root)
        for p in (STATE_PATH, STATE_PATH.with_suffix(".json.bak")):
            if p.exists():
                p.unlink()
        log.info("已清空既有 bar 数据与同步状态")

    signal.signal(signal.SIGINT, _on_signal)
    if hasattr(signal, "SIGTERM"):
        signal.signal(signal.SIGTERM, _on_signal)

    state = load_state()
    stocks, etfs, skipped = build_tasks(inst, state, args)
    if args.only == "stocks":
        etfs = []
    elif args.only == "etfs":
        stocks = []

    log.info("日志文件：%s", log_path)
    log.info("待办：个股 %d，ETF %d；已最新/跳过 %d", len(stocks), len(etfs), skipped)
    if not stocks and not etfs:
        log.info("全部已是最新，无需更新")
        return 0

    run_started = time.perf_counter()
    shard = int(time.time()) % 100000  # 分片号取时间派生，避免与既有文件重名
    d1, f1, r1, shard = run_group(stocks, "baostock", args.workers, state, args, shard)
    # 新浪是 HTTP 接口，并发过高易被判为异常流量，取较低并发
    d2 = f2 = r2 = 0
    if not _stop_requested:
        d2, f2, r2, shard = run_group(etfs, "sina", max(2, args.workers // 2),
                                      state, args, shard)

    elapsed = time.perf_counter() - run_started
    state.setdefault("runs", []).append({
        "at": datetime.now().isoformat(timespec="seconds"),
        "done": d1 + d2, "failed": f1 + f2, "rows": r1 + r2,
        "elapsed_sec": round(elapsed, 1), "interrupted": _stop_requested,
    })
    save_state(state)

    problems = 0
    if not _stop_requested and not args.no_compact:
        log.info("=== 合并分片并去重 ===")
        compact()
    if not _stop_requested and not args.no_verify:
        log.info("=== 逐年质检 ===")
        cal = pd.read_parquet(layout.meta_path("calendar"))
        problems = verify_all(inst, cal)

    log.info("=== 汇总 ===")
    log.info("完成 %d，失败 %d，新增 %d 行，耗时 %s",
             d1 + d2, f1 + f2, r1 + r2, str(timedelta(seconds=int(elapsed))))
    if f1 + f2:
        log.info("失败标的可用 --retry-failed 重试")
    if _stop_requested:
        log.warning("本轮被中断，重跑同一命令即从断点继续")
        return 130
    if problems:
        log.warning("质检发现 %d 项问题，详见上方日志", problems)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
