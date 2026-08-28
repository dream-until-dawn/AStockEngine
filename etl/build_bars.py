"""构建 bar 表与 adj_factor 表。

个股走 BaoStock，ETF 走新浪（BaoStock 无 ETF 行情）—— 两者经同一套适配器接口
归一化后进入同一张表，这是约束 C9 的实地检验。

抽试用法（默认）：
    python etl/build_bars.py --stocks 40 --etfs 20 --start 2018-01-01

全量用法（v0.1 后续）：
    python etl/build_bars.py --all
"""

from __future__ import annotations

# 依赖检查必须先于第三方包导入 —— 用错解释器时失败点会落在 import pandas，
# 报出的栈掩盖真正原因。见 etl/_venv_guard.py
import sys as _sys
from pathlib import Path as _Path
_sys.path.insert(0, str(_Path(__file__).resolve().parent))
import _venv_guard  # noqa: F401,E402

import argparse
import shutil
import sys
import time
from decimal import ROUND_HALF_UP, Decimal
from pathlib import Path

import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq

sys.path.insert(0, str(Path(__file__).resolve().parent))

import layout  # noqa: E402
import schema as sc  # noqa: E402
from sources import BaoStockSource, SinaSource  # noqa: E402

_EXCHANGE_TAG = {
    int(sc.Exchange.SSE): "sh",
    int(sc.Exchange.SZSE): "sz",
    int(sc.Exchange.BSE): "bj",
}

# 板块 -> 单日涨跌幅上限（比例）。用于板块自洽校验，不是撮合规则本身。
_BOARD_LIMIT = {
    int(sc.Board.MAIN): 0.10,
    int(sc.Board.CHINEXT): 0.20,
    int(sc.Board.STAR): 0.20,
    int(sc.Board.BSE): 0.30,
}
# ST 的 5% 限制**只适用于主板**。创业板与科创板的 ST 股仍为 20%。
_ST_LIMIT_MAIN = 0.05

# --- 涨跌幅规则的时间维度 ---
#
# 涨跌幅限制不是常量，会随监管规则变更。以下生效日均由**数据实测定位**后
# 查证确认，不是凭记忆写的：
#
#   20260706  沪深北新版交易规则实施，主板风险警示股（ST/*ST）涨跌幅
#             由 5% 放宽至 10%，与主板普通股一致
#   20200824  创业板注册制改革，涨跌幅由 10% 放宽至 20%
#
# v0.2 的 Market 模块必须同样按日期分段实现，不能写成静态常量 ——
# 否则回测 2026-07-06 之前的 ST 股会放行本该被拦截的成交。
_ST_MAIN_LIMIT_RELAXED_FROM = 20260706
_CHINEXT_LIMIT_RELAXED_FROM = 20200824


def limit_ratio(board: int, is_st: int, trading_day: int) -> float:
    """给定板块、ST 状态与交易日，返回当日涨跌幅限制比例。"""
    if board == int(sc.Board.CHINEXT):
        return 0.20 if trading_day >= _CHINEXT_LIMIT_RELAXED_FROM else 0.10
    if board == int(sc.Board.STAR):
        return 0.20
    if board == int(sc.Board.BSE):
        return 0.30
    # 主板
    if is_st == 1 and trading_day < _ST_MAIN_LIMIT_RELAXED_FROM:
        return _ST_LIMIT_MAIN
    return 0.10

# 退市整理期首日无涨跌幅限制，其后仍受限。缺少「进入退市整理期」的准确日期，
# 以退市前若干交易日近似圈定，该区间内的越界降级为提示而非错误。
_DELIST_WINDOW = 20

# 新股上市初期不设涨跌幅限制的交易日数。全面注册制后主板亦为 5 日，
# 首版校验只排除上市首日，导致新股次日的正常波动被误报。
_IPO_FREE_DAYS = 5


def limit_price(preclose_fixed: int, pct: float, up: bool) -> int:
    """按交易所规则计算涨/跌停价，返回定点整数。

    取整必须是**四舍五入到分**（ROUND_HALF_UP）。numpy / pandas 的 `round()`
    是银行家舍入（四舍六入五成双），在 x.xx5 上会偏差一分 ——
    4.35 × 1.10 = 4.785，银行家舍入得 4.78，而实际涨停价是 4.79。

    v0.2 的 Market 模块实现涨跌停时必须遵循同一规则，
    Go 侧不可用朴素浮点取整。
    """
    prev = Decimal(preclose_fixed) / sc.PRICE_SCALE_ASHARE
    rate = Decimal("1") + (Decimal(str(pct)) if up else -Decimal(str(pct)))
    yuan = (prev * rate).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)
    return int(yuan * sc.PRICE_SCALE_ASHARE)


def to_bar_frame(raw: pd.DataFrame, iid: int, price_scale: int) -> pd.DataFrame:
    """适配器的归一化输出 → 符合 SCHEMA.md 的 bar 行。"""
    if raw.empty:
        return pd.DataFrame(columns=[f.name for f in sc.BAR_SCHEMA])

    df = pd.DataFrame()
    df["instrument_id"] = [iid] * len(raw)
    day = raw["trading_day"].astype("int64")
    ts = [sc.session_ts(int(d)) for d in day]
    df["ts_open"] = [t[0] for t in ts]
    df["ts_close"] = [t[1] for t in ts]
    df["trading_day"] = day

    for c in ("open", "high", "low", "close"):
        df[c] = [sc.to_fixed(v, price_scale) for v in raw[c]]
    # 各列一律走 to_fixed / to_numeric 兜底：新浪 ETF 存在整行缺失字段，
    # BaoStock 停牌行的 turn 为空串，直接 astype 会抛 NaN 转换错误
    df["volume"] = pd.to_numeric(raw["volume"], errors="coerce").fillna(0).astype("int64")
    df["amount"] = [sc.to_fixed(v, sc.AMOUNT_SCALE) for v in raw["amount"]]
    df["preclose"] = [sc.to_fixed(v, price_scale) for v in raw["preclose"]]
    df["turn"] = [sc.to_fixed(v, sc.RATIO_SCALE) for v in raw["turn"]]
    df["tradestatus"] = pd.to_numeric(
        raw["tradestatus"], errors="coerce").fillna(1).astype("int8")
    df["is_st"] = pd.to_numeric(raw["is_st"], errors="coerce").fillna(0).astype("int8")
    return df


def fill_etf_preclose(df: pd.DataFrame) -> int:
    """新浪多数 ETF 不提供 prevclose，用前一交易日 close 补齐。

    返回补齐的行数。这是 SCHEMA.md 1.4 记录的**已知数据质量降级**：
    未考虑 ETF 除权，除权日的 preclose 会偏高，进而使当日涨跌停判定失真。
    ETF 复权方案收口后需重做。
    """
    missing = df["preclose"] <= 0
    n = int(missing.sum())
    if n:
        prev = df["close"].shift(1)
        # 首行无前值，退化为用当日 open 兜底
        df.loc[missing, "preclose"] = prev[missing].fillna(df["open"][missing]).astype("int64")
    return n


def check_bars(bars: pd.DataFrame, inst: pd.DataFrame, cal: pd.DataFrame) -> list[str]:
    """质检。每一项都对应 SCHEMA.md 或 ROADMAP 中的一条明文约定。"""
    problems: list[str] = []
    if bars.empty:
        return ["bar 表为空"]

    # 0.5 排序保证 —— delta 编码与引擎顺序扫描都依赖它
    key = bars[["instrument_id", "ts_close"]]
    if not key.equals(key.sort_values(["instrument_id", "ts_close"])):
        problems.append("未按 (instrument_id, ts_close) 升序排列")
    if key.duplicated().any():
        problems.append(f"主键重复 {int(key.duplicated().sum())} 行")

    # 价格关系自洽
    bad_hl = bars[(bars["high"] < bars["low"])
                  | (bars["high"] < bars["open"]) | (bars["high"] < bars["close"])
                  | (bars["low"] > bars["open"]) | (bars["low"] > bars["close"])]
    if len(bad_hl):
        problems.append(f"OHLC 关系越界 {len(bad_hl)} 行")

    if (bars[["open", "high", "low", "close", "preclose"]] <= 0).any().any():
        n = int((bars[["open", "high", "low", "close", "preclose"]] <= 0).any(axis=1).sum())
        problems.append(f"存在非正价格 {n} 行")
    if (bars[["volume", "amount", "turn"]] < 0).any().any():
        problems.append("存在负的成交量/额/换手率")

    # 停牌行必须零成交（SCHEMA.md 1.3）
    susp = bars[bars["tradestatus"] == 0]
    if len(susp) and (susp["volume"] != 0).any():
        problems.append(f"停牌行存在非零成交量 {int((susp['volume'] != 0).sum())} 行")

    # 交易日必须落在日历的交易日内
    trading_days = set(cal.loc[cal["is_trading_day"] == 1, "date"].tolist())
    off = bars[~bars["trading_day"].isin(trading_days)]
    if len(off):
        problems.append(f"{len(off)} 行的 trading_day 不是交易日，样本 "
                        f"{sorted(off['trading_day'].unique())[:5]}")

    # --- 板块与 ST 的自洽校验 ---
    # 涨跌幅超过板块上限，说明 board 或 is_st 标记有误。
    # v0.1 构建 instruments 时正是靠这条发现 302 段属创业板而非主板。
    #
    # 必须比**价格**而非百分比：涨跌停价四舍五入到分，低价股上一分钱就是数个
    # 百分点（0.24 元的票，涨停价 0.288 舍入为 0.29，百分比达 20.83%）。
    meta = inst.set_index("instrument_id")[["board", "symbol", "type", "status"]]
    j = bars.join(meta, on="instrument_id")
    j = j[(j["tradestatus"] == 1) & (j["preclose"] > 0)]
    # 复牌首日不设涨跌幅限制。2005-2007 股权分置改革期间尤为集中 ——
    # 对价送股造成的自然除权使复牌当日跌幅普遍在 -16% ~ -27%。
    # 实测：该区间 113 行越界中，113 行的前一交易日均为停牌，相关性 100%。
    prev_status = bars.groupby("instrument_id")["tradestatus"].shift(1)
    j = j[~(prev_status.reindex(j.index) == 0)]

    # 新股上市初期不设涨跌幅限制，整体排除：
    # 创业板 / 科创板一直如此；主板自 2023-04-10 全面注册制起同样为前 5 个交易日。
    # 保守起见对所有板块一律排除前 N 日，宁可漏报不可误报。
    j = j[j.groupby("instrument_id").cumcount() >= _IPO_FREE_DAYS]

    # 逐行按「板块 + ST + 交易日」取限制比例 —— 规则有时间维度，不能用静态映射
    pct = pd.Series(
        [limit_ratio(int(b), int(st), int(d))
         for b, st, d in zip(j["board"], j["is_st"], j["trading_day"])],
        index=j.index)

    limit_up = [limit_price(p, r, True) for p, r in zip(j["preclose"], pct)]
    limit_down = [limit_price(p, r, False) for p, r in zip(j["preclose"], pct)]
    over = j[(j["close"] > pd.Series(limit_up, index=j.index))
             | (j["close"] < pd.Series(limit_down, index=j.index))]

    # 退市整理期首日无涨跌幅限制；以退市前若干交易日近似圈定
    tail_rank = over.groupby("instrument_id")["trading_day"].rank(ascending=False)
    in_delist_window = (over["status"] == int(sc.Status.DELISTED)) & (tail_rank <= _DELIST_WINDOW)
    strict = over[~in_delist_window]
    relaxed = over[in_delist_window]

    over_stock = strict[strict["type"] == int(sc.InstrumentType.STOCK)]
    over_etf = strict[strict["type"] == int(sc.InstrumentType.ETF)]
    if len(over_stock):
        g = over_stock.groupby("symbol").size().sort_values(ascending=False).head(5)
        detail = g.to_string()
        problems.append(
            f"个股价格超板块涨跌停价 {len(over_stock)} 行"
            f"（board 或 is_st 可能有误）：\n{detail}"
        )
    if len(relaxed):
        print(f"  [提示] 退市整理期越界 {len(relaxed)} 行 —— "
              f"整理期首日无涨跌幅限制，符合预期")
    if len(over_etf):
        print(f"  [提示] ETF 越界 {len(over_etf)} 行 —— 跟踪创业板/科创板指数的 "
              f"ETF 为 20%，当前 board 统一记为主板，待 v0.2 Market 模块按类别配置")
    return problems


def write_bars(bars: pd.DataFrame, tag: str, clean: bool) -> list[Path]:
    root = layout.bar_dir("ashare", "1d")
    if clean and root.exists():
        shutil.rmtree(root)
    written = []
    for year, g in bars.groupby(bars["trading_day"] // 10000):
        d = layout.bar_dir("ashare", "1d", int(year))
        d.mkdir(parents=True, exist_ok=True)
        path = d / f"part-{tag}.parquet"
        g = g.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
        sc.validate_columns(g, "bar")
        table = pa.Table.from_pandas(g, schema=sc.BAR_SCHEMA, preserve_index=False)
        pq.write_table(table, path, **sc.parquet_write_options("bar"))
        written.append(path)
    return written


def main() -> int:
    ap = argparse.ArgumentParser(description="构建 bar 与 adj_factor（默认抽试）")
    ap.add_argument("--stocks", type=int, default=40, help="抽样个股数量")
    ap.add_argument("--etfs", type=int, default=20, help="抽样 ETF 数量")
    ap.add_argument("--start", default="2018-01-01")
    ap.add_argument("--end", default="2026-08-28")
    ap.add_argument("--tag", default="sample", help="分区文件名后缀")
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--no-clean", action="store_true", help="不清空既有分区")
    args = ap.parse_args()

    inst = pd.read_parquet(layout.meta_path("instruments"))
    cal = pd.read_parquet(layout.meta_path("calendar"))

    stocks = inst[inst["type"] == int(sc.InstrumentType.STOCK)]
    etfs = inst[inst["type"] == int(sc.InstrumentType.ETF)]
    # 抽样刻意覆盖已退市标的：C3 幸存者偏差的验证点
    delisted = stocks[stocks["status"] == int(sc.Status.DELISTED)]
    listed = stocks[stocks["status"] == int(sc.Status.LISTED)]
    n_del = min(len(delisted), max(1, args.stocks // 4))
    pick_stocks = pd.concat([
        delisted.sample(n=n_del, random_state=args.seed),
        listed.sample(n=max(0, args.stocks - n_del), random_state=args.seed),
    ])
    pick_etfs = etfs.sample(n=min(args.etfs, len(etfs)), random_state=args.seed)

    print(f"抽样：个股 {len(pick_stocks)}（含已退市 {n_del}）+ ETF {len(pick_etfs)}")
    print(f"区间：{args.start} ~ {args.end}\n")

    frames: list[pd.DataFrame] = []
    factors: list[pd.DataFrame] = []
    etf_filled = 0
    failures: list[str] = []
    started = time.perf_counter()

    with BaoStockSource() as bao:
        for i, r in enumerate(pick_stocks.itertuples(index=False), 1):
            ex = _EXCHANGE_TAG.get(int(r.exchange))
            try:
                raw = bao.daily_bars(r.symbol, ex, args.start, args.end)
                frames.append(to_bar_frame(raw, int(r.instrument_id), int(r.price_scale)))
            except Exception as exc:  # noqa: BLE001
                failures.append(f"个股 {r.symbol}: {type(exc).__name__}: {exc}")
            if i % 10 == 0:
                print(f"  个股 {i}/{len(pick_stocks)}  {time.perf_counter()-started:.0f}s", flush=True)

    sina = SinaSource()
    for i, r in enumerate(pick_etfs.itertuples(index=False), 1):
        ex = _EXCHANGE_TAG.get(int(r.exchange))
        try:
            raw = sina.daily_bars(r.symbol, ex, args.start, args.end)
            df = to_bar_frame(raw, int(r.instrument_id), int(r.price_scale))
            if not df.empty:
                etf_filled += fill_etf_preclose(df)
            frames.append(df)
        except Exception as exc:  # noqa: BLE001
            failures.append(f"ETF {r.symbol}: {type(exc).__name__}: {exc}")
        if i % 10 == 0:
            print(f"  ETF {i}/{len(pick_etfs)}  {time.perf_counter()-started:.0f}s", flush=True)

    # 复权因子只对个股取（新浪的 hfq-factor 不接受 ETF 代码，见 v0.0 报告 3.1）
    print("\n拉取复权因子 ...", flush=True)
    for r in pick_stocks.itertuples(index=False):
        ex = _EXCHANGE_TAG.get(int(r.exchange))
        try:
            f = sina.adj_factors(r.symbol, ex)
            if len(f):
                factors.append(pd.DataFrame({
                    "instrument_id": int(r.instrument_id),
                    "ex_date": f["ex_date"].astype("int32"),
                    "hfq_factor": [sc.to_fixed(v, sc.FACTOR_SCALE) for v in f["factor_raw"]],
                    "hfq_factor_raw": f["factor_raw"].astype(str),
                }))
        except Exception as exc:  # noqa: BLE001
            failures.append(f"因子 {r.symbol}: {type(exc).__name__}: {exc}")

    bars = pd.concat([f for f in frames if not f.empty], ignore_index=True)
    bars = bars.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
    bars = bars.astype({
        "instrument_id": "int32", "trading_day": "int32",
        "turn": "int32", "tradestatus": "int8", "is_st": "int8",
    })

    paths = write_bars(bars, args.tag, clean=not args.no_clean)

    if factors:
        fac = pd.concat(factors, ignore_index=True)
        fac = fac.sort_values(["instrument_id", "ex_date"]).reset_index(drop=True)
        fac = fac.astype({"instrument_id": "int32", "ex_date": "int32", "hfq_factor": "int64"})
        sc.validate_columns(fac, "adj_factor")
        fp, fc = layout.write_meta(fac, "adj_factor")
    else:
        fac, fp = pd.DataFrame(), None

    total_bytes = sum(p.stat().st_size for p in paths)
    print(f"\nbar          {len(bars):>8} 行 / {len(paths)} 个年份分区 / "
          f"{total_bytes/1024/1024:.2f} MB / {total_bytes/len(bars):.2f} 字节-行")
    print(f"adj_factor   {len(fac):>8} 行" + (f" -> {fp.name}" if fp is not None else ""))
    print(f"标的数       {bars['instrument_id'].nunique():>8}")
    print(f"交易日范围   {bars['trading_day'].min()} ~ {bars['trading_day'].max()}")
    print(f"停牌行       {int((bars['tradestatus']==0).sum()):>8}")
    print(f"ETF preclose 补齐 {etf_filled} 行（已知降级，见 SCHEMA.md 1.4）")

    if failures:
        print(f"\n--- 拉取失败 {len(failures)} 项 ---")
        for f in failures[:10]:
            print(f"  ! {f}")

    print("\n--- 质检 ---")
    problems = check_bars(bars, inst, cal)
    if problems:
        for p in problems:
            print(f"  ! {p}")
        return 1
    print("  全部通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
