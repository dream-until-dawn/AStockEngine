"""拉取加密货币永续合约日线，写入与 A 股同一套表。

    .\\.venv\\Scripts\\python.exe etl\\build_crypto.py
    .\\.venv\\Scripts\\python.exe etl\\build_crypto.py --instruments BTC-USDT-SWAP

「同一套表」是这件事的全部意义：加密标的进 `instruments`（拿新的
instrument_id），行情进 `bar/market=crypto/freq=1d/`，引擎不需要知道
它在读的是 A 股还是加密（C9）。

三处与 A 股不同、必须在这里处理掉的：

1. **按 UTC+8 切日**，`ts_close` 是次日 00:00（24×7，一根 bar 结束就是
   下一根开始）。A 股是当日 15:00。两者能排在同一条时间轴上，
   引擎的游标无需分支。
2. **数量单位是「张」**，1 张 BTC = 0.01 BTC（`ctVal`）。合约乘数进 `attrs`。
3. **没有交易日历、没有涨跌停、没有除权、没有停牌**。日历表不写，
   `adj_factor` / `corporate_action` 不写 —— 空表比假数据诚实。

**资金费率单独一张表**（`funding`，8 小时一条，不聚合）。它不是可选项：
永续合约靠这笔多空互付把合约价钉在现货附近，实测最近 96 天 BTC 有
82.4% 的结算是多头付钱、年化约 4.4% —— 比一个年换手 24 倍的 A 股策略的
全部摩擦还贵。少算它，任何长期做多的加密回测都会系统性偏乐观。

⚠ **两个缺口，都写在这里免得将来忘了：**

1. **OKX 只保留约 3 个月的资金费率**（实测 `after=2024-06-01` 返回 0 条）。
   6.6 年的行情，费率覆盖不到 4%。这是数据源限制，不是这里的 bug。
   引擎**不能对未覆盖区间静默按 0 计** —— 那比统一不计更糟，
   因为它让同一次回测的两段用了不同的成本模型。
2. **引擎侧尚未计费。** 表建好了、数据拉了，但 Ledger 还不会在
   持仓期间扣这笔钱 —— 那要等 CryptoMarket。
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import _venv_guard  # noqa: F401  必须最先导入：拿系统 Python 跑会缺依赖

import pandas as pd
import pyarrow as pa
import pyarrow.parquet as pq

import layout
import schema as sc
from sources.okx_src import OKXSource

# 用户指定：先只拉这两个
DEFAULT_INSTRUMENTS = ("BTC-USDT-SWAP", "ETH-USDT-SWAP")


def load_existing_instruments() -> tuple[pd.DataFrame, dict, int]:
    """读已有的 instruments 表，返回 (表, (market,symbol)->id 映射, 下一个可用 id)。

    **必须合并而不是覆盖**：A 股的 7,200 个标的已经在表里，
    而且它们的 instrument_id 被 bar 表引用着 —— 重新分配会让全部历史行情错位。
    """
    path = layout.meta_path("instruments", "parquet")
    if not path.exists():
        return pd.DataFrame(), {}, 1
    df = pd.read_parquet(path)
    mapping = {(int(m), str(s)): int(i)
               for m, s, i in zip(df["market"], df["symbol"], df["instrument_id"])}
    return df, mapping, int(df["instrument_id"].max()) + 1


def build_instrument_rows(src: OKXSource, inst_ids: list[str],
                          mapping: dict, next_id: int) -> tuple[list[dict], int]:
    meta = src.instruments(inst_ids)
    rows = []
    for r in meta.itertuples(index=False):
        key = (int(sc.Market.CRYPTO), r.symbol)
        iid = mapping.get(key)
        if iid is None:
            iid = next_id
            next_id += 1
        # 合约规格原样存进 attrs。**ctVal 是算名义价值的必需项**：
        # 名义额 = 张数 × ctVal × 价格，少了它数量就没有意义
        attrs = {
            "ct_val": r.ct_val, "ct_val_ccy": r.ct_val_ccy,
            "ct_mult": r.ct_mult, "ct_type": r.ct_type,
            "tick_sz": r.tick_sz, "lot_sz": r.lot_sz, "min_sz": r.min_sz,
            "settle_ccy": r.settle_ccy,
            "source": "okx",
            # 明示状态：数据已采集（funding 表），但引擎还没接上计费
            "funding_rate": "collected_not_charged",
        }
        rows.append({
            "instrument_id": iid,
            "market": int(sc.Market.CRYPTO),
            "symbol": r.symbol,
            "exchange": int(sc.Exchange.OKX),
            "name": r.name,
            "type": int(sc.InstrumentType.SWAP),
            # 加密货币没有板块 —— 给 UNKNOWN 而不是硬塞主板。
            # board 在引擎里决定涨跌停幅度，塞个假板块会让它按 A 股规则算
            "board": int(sc.Board.UNKNOWN),
            "tracked_board": int(sc.Board.UNKNOWN),
            "price_scale": sc.PRICE_SCALE_CRYPTO,
            "qty_scale": sc.QTY_SCALE_CRYPTO,
            "quote_ccy": int(sc.Currency.USDT),
            "min_order_qty": sc.to_fixed(r.min_sz, sc.QTY_SCALE_CRYPTO),
            "qty_step": sc.to_fixed(r.lot_sz, sc.QTY_SCALE_CRYPTO),
            "list_date": int(r.list_date),
            "delist_date": None,
            "status": int(sc.Status.LISTED if r.listed else sc.Status.DELISTED),
            "attrs": json.dumps(attrs, ensure_ascii=False, sort_keys=True),
        })
    return rows, next_id


def build_bars(src: OKXSource, symbol: str, iid: int) -> pd.DataFrame:
    df = src.daily_bars(symbol)
    if df.empty:
        return df
    out = []
    for r in df.itertuples(index=False):
        ts_open, ts_close = sc.crypto_session_ts(int(r.trading_day))
        out.append({
            "instrument_id": iid,
            "ts_open": ts_open, "ts_close": ts_close,
            "trading_day": int(r.trading_day),
            "open": sc.to_fixed(r.open, sc.PRICE_SCALE_CRYPTO),
            "high": sc.to_fixed(r.high, sc.PRICE_SCALE_CRYPTO),
            "low": sc.to_fixed(r.low, sc.PRICE_SCALE_CRYPTO),
            "close": sc.to_fixed(r.close, sc.PRICE_SCALE_CRYPTO),
            "volume": sc.to_fixed(r.volume, sc.QTY_SCALE_CRYPTO),
            "amount": sc.to_fixed(r.amount, sc.AMOUNT_SCALE),
            "preclose": sc.to_fixed(r.preclose, sc.PRICE_SCALE_CRYPTO),
            # 永续合约没有流通股本，换手率无从谈起 —— 记 0 并在文档里说明，
            # 而不是编一个数出来
            "turn": 0,
            # **零成交日记 tradestatus=0**，与 A 股停牌行同语义（SCHEMA.md 1.3）。
            # 加密不停牌，但 OKX 早期确实有整天没有成交的 bar（BTC 7 天、
            # ETH 8 天，全在 2019-12），其 OHLC 是拿上一根的价格铺平的。
            # 记成 1 的话，引擎会把它当成一个可以按 7,820 元成交的正常交易日 ——
            # 而那一天根本没有对手盘。**「没有流动性」与「正常交易」必须可区分。**
            "tradestatus": 0 if float(r.volume or 0) <= 0 else 1,
            "is_st": 0,
        })
    return pd.DataFrame(out)


INT64_MAX = 2**63 - 1


def check_ranges(bars: pd.DataFrame) -> list[str]:
    """定点值的范围自检。

    单个值离 int64 上限还很远，但**乘积会溢出** —— 引擎算名义额时
    price_fp × qty_fp 在 BTC 上是 7.8e12 × 1e8 = 7.8e20，远超 int64。
    这里把实际的最大值报出来，让「将来写保证金引擎的人」有据可依。
    """
    problems = []
    if bars.empty:
        return ["没有行情数据"]
    for col in ("open", "high", "low", "close", "preclose", "volume", "amount"):
        mx = int(bars[col].abs().max())
        if mx > INT64_MAX:
            problems.append(f"{col} 最大值 {mx} 溢出 int64")
        if mx == 0 and col in ("close", "high"):
            problems.append(f"{col} 全为 0 —— 定点转换多半错了")
    if (bars["close"] <= 0).any():
        problems.append("存在收盘价 <= 0 的行")
    if (bars["high"] < bars["low"]).any():
        problems.append("存在最高价 < 最低价的行")
    return problems


def report_ranges(bars: pd.DataFrame) -> None:
    print("  定点范围自检（离 int64 上限的余量）：")
    for col in ("close", "volume", "amount"):
        mx = int(bars[col].abs().max())
        print(f"    {col:<8} 最大 {mx:>22,}   余量 {INT64_MAX / max(mx, 1):>12,.0f} 倍")
    px = int(bars["close"].abs().max())
    qty = int(bars["volume"].abs().max())
    print(f"    [!] price*qty = {px * qty:.3e}，{'溢出' if px * qty > INT64_MAX else '未溢出'}"
          f" int64 —— 引擎算名义额必须走 mulDiv 拆分")


def build_funding(src: OKXSource, symbol: str, iid: int) -> pd.DataFrame:
    df = src.funding_rates(symbol)
    if df.empty:
        return df
    return pd.DataFrame({
        "instrument_id": iid,
        "funding_time": df["funding_time"].astype("int64"),
        # 结算时刻所属的自然日（UTC+8），供与日线按 trading_day 对齐
        "trading_day": [sc.ms_to_ymd_cst(t) for t in df["funding_time"]],
        "rate": [sc.to_fixed(r, sc.FUNDING_RATE_SCALE) for r in df["rate_raw"]],
        "rate_raw": df["rate_raw"].astype(str),
    })


def check_funding(f: pd.DataFrame) -> list[str]:
    """资金费率的质检。

    结算间隔恒为 8 小时是这张表最强的约束 —— 缺一条就意味着
    某天的持仓成本少算 1/3，而它不会以任何形式报错。
    """
    problems = []
    if f.empty:
        return ["没有资金费率数据"]
    if f.duplicated(["instrument_id", "funding_time"]).any():
        problems.append("存在重复的 (标的, 结算时刻)")
    for iid, g in f.groupby("instrument_id"):
        g = g.sort_values("funding_time")
        gaps = g["funding_time"].diff().dropna()
        bad = gaps[gaps != 8 * 3_600_000]
        if len(bad):
            problems.append(
                f"标的 {iid} 有 {len(bad)} 处结算间隔不是 8 小时"
                f"（{sorted(set((bad // 3_600_000).astype(int)))[:6]} 小时）")
    return problems


def report_funding(f: pd.DataFrame, bars: pd.DataFrame, id2sym: dict) -> None:
    print("  资金费率（正=多头付空头）：")
    for iid, g in f.groupby("instrument_id"):
        r = g["rate"] / sc.FUNDING_RATE_SCALE
        days = len(g) / 3
        print(f"    {id2sym.get(iid, iid):<16} {len(g):>6} 次 / {days:>7,.0f} 天  "
              f"单次中位 {r.median() * 100:+.4f}%  为正 {(r > 0).mean():>5.1%}  "
              f"多头累计 {r.sum() * 100:+7.1f}%  年化 {r.sum() / days * 365 * 100:+5.1f}%")

    # 覆盖率必须和行情放在一起报 —— 单看「拉到 289 条」像是成功了
    print("  对行情的覆盖率：")
    for iid, g in f.groupby("instrument_id"):
        b = bars[bars.instrument_id == iid]
        if b.empty:
            continue
        cov = len(set(g.trading_day) & set(b.trading_day))
        print(f"    {id2sym.get(iid, iid):<16} 行情 {len(b):>5} 天 "
              f"({int(b.trading_day.min())}~{int(b.trading_day.max())})  "
              f"费率覆盖 {cov:>4} 天 = {cov / len(b):>5.1%}"
              f"   未覆盖 {len(b) - cov} 天")


def write_bars(bars: pd.DataFrame) -> list[Path]:
    # 每次都是全量重拉，先清空 —— 否则上一轮的年份分区会留在那里，
    # 引擎读的时候会把两批数据拼起来，而且不会报错
    root = layout.bar_dir("crypto", "1d")
    if root.exists():
        shutil.rmtree(root)
    written = []
    for year, g in bars.groupby(bars["trading_day"] // 10000):
        d = layout.bar_dir("crypto", "1d", int(year))
        d.mkdir(parents=True, exist_ok=True)
        path = d / "part-00000.parquet"
        g = g.sort_values(["instrument_id", "ts_close"]).reset_index(drop=True)
        sc.validate_columns(g, "bar")
        table = pa.Table.from_pandas(g, schema=sc.BAR_SCHEMA, preserve_index=False)
        pq.write_table(table, path, **sc.parquet_write_options("bar"))
        written.append(path)
    return written


def main() -> None:
    ap = argparse.ArgumentParser(description="拉取 OKX 永续合约日线")
    ap.add_argument("--instruments", nargs="*", default=list(DEFAULT_INSTRUMENTS),
                    help="合约 ID，如 BTC-USDT-SWAP")
    args = ap.parse_args()

    print("OKX 永续合约日线（UTC+8 切日，数量按张）")
    print(f"合约：{', '.join(args.instruments)}\n")

    src = OKXSource()
    existing, mapping, next_id = load_existing_instruments()
    print(f"已有标的 {len(existing)} 个，下一个可用 ID {next_id}")

    rows, next_id = build_instrument_rows(src, args.instruments, mapping, next_id)
    for r in rows:
        a = json.loads(r["attrs"])
        print(f"  {r['symbol']:<16} id={r['instrument_id']:<6} "
              f"1 张 = {a['ct_val']} {a['ct_val_ccy']}  "
              f"tick={a['tick_sz']}  lot={a['lot_sz']} 张  上线 {r['list_date']}")

    all_bars = []
    for r in rows:
        t0 = time.time()
        b = build_bars(src, r["symbol"], r["instrument_id"])
        if b.empty:
            print(f"  [!] {r['symbol']} 没有行情，跳过")
            continue
        print(f"  {r['symbol']:<16} {len(b):>5} 根  "
              f"{int(b.trading_day.min())} ~ {int(b.trading_day.max())}  "
              f"{time.time() - t0:.1f}s")
        all_bars.append(b)

    if not all_bars:
        print("\n没有任何行情，中止")
        sys.exit(1)
    bars = pd.concat(all_bars, ignore_index=True)

    problems = check_ranges(bars)
    if problems:
        print("\n质量问题：")
        for p in problems:
            print("  x", p)
        sys.exit(1)
    print()
    report_ranges(bars)

    # 合并写出 instruments：**保留已有行**
    new_df = pd.DataFrame(rows)
    if not existing.empty:
        keep = existing[~(
            (existing["market"] == int(sc.Market.CRYPTO))
            & (existing["symbol"].isin([r["symbol"] for r in rows]))
        )]
        merged = pd.concat([keep, new_df], ignore_index=True)
    else:
        merged = new_df
    merged = merged.sort_values("instrument_id").reset_index(drop=True)
    merged = merged.astype({
        "instrument_id": "int32", "market": "int8", "exchange": "int8",
        "type": "int8", "board": "int8", "tracked_board": "int8",
        "price_scale": "int32", "qty_scale": "int32", "quote_ccy": "int8",
        "min_order_qty": "int32", "qty_step": "int32",
        "list_date": "int32", "delist_date": "Int32", "status": "int8",
    })
    pq_path, csv_path = layout.write_meta(merged, "instruments")
    print(f"\ninstruments {len(existing)} -> {len(merged)} 行 -> {pq_path.name} / {csv_path.name}")

    paths = write_bars(bars)
    total = sum(p.stat().st_size for p in paths)
    print(f"bar {len(bars)} 行 / {len(paths)} 个年份分区 / {total / 1024:.0f} KB")
    print(f"  -> {layout.bar_dir('crypto', '1d')}")

    # ---- 资金费率 ----
    print("\n资金费率（每 8 小时一条，不聚合）")
    funds = []
    for r in rows:
        t0 = time.time()
        f = build_funding(src, r["symbol"], r["instrument_id"])
        if f.empty:
            print(f"  [!] {r['symbol']} 没有资金费率，跳过")
            continue
        print(f"  {r['symbol']:<16} {len(f):>6} 条  "
              f"{int(f.trading_day.min())} ~ {int(f.trading_day.max())}  "
              f"{time.time() - t0:.1f}s")
        funds.append(f)
    if funds:
        fund = pd.concat(funds, ignore_index=True)
        probs = check_funding(fund)
        if probs:
            print("  质量问题：")
            for x in probs:
                print("    x", x)
            sys.exit(1)
        report_funding(fund, bars, {r["instrument_id"]: r["symbol"] for r in rows})
        fund = fund.sort_values(["instrument_id", "funding_time"]).reset_index(drop=True)
        fund = fund.astype({"instrument_id": "int32", "trading_day": "int32"})
        sc.validate_columns(fund, "funding")
        fp, fc = layout.write_meta(fund, "funding")
        print(f"  funding {len(fund)} 行 -> {fp.name} / {fc.name}")

    print("\n[!] 两件事必须知道：")
    print("  1. OKX 的公开资金费率只保留约 3 个月（实测 after=2024-06-01 返回 0 条），")
    print("     所以 6.6 年的行情里只有最近约 96 天有费率。**覆盖率不到 4%。**")
    print("     引擎不能对未覆盖区间静默按 0 计 —— 那比统一不计更糟：")
    print("     它会让「有数据的那段」和「没数据的那段」用不同的成本模型。")
    print("  2. 引擎侧尚未对资金费率计费。表建好了、数据拉了，")
    print("     但 Ledger 还不会在持仓期间扣这笔钱 —— 那要等 CryptoMarket。")


if __name__ == "__main__":
    main()
