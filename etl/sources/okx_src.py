"""OKX 数据源：加密货币永续合约。

**不需要 API key。** 行情端点（`/api/v5/market/*`、`/api/v5/public/*`）是公开的，
只有下单与查账户才要鉴权。这个模块因此完全不碰凭证 —— 少一个能泄漏的东西。

与 A 股源的三处关键差异，都在这里归一化掉，下游不再感知：

1. **按 UTC+8 切日**。OKX 的 `bar=1D` 就是 UTC+8 口径（`1Dutc` 才是 UTC 0 点），
   返回的 ts 是**开盘**时刻。实测：ts=1787846400000 → UTC+8 2026-08-28 00:00。
2. **数量单位是「张」**，不是币。1 张 BTC-USDT-SWAP = 0.01 BTC（`ctVal`）。
   `vol` 是张数、`volCcy` 是币数、`volCcyQuote` 是计价额（USDT）。
3. **未收盘的 bar 会被返回**，靠 `confirm` 字段区分（1=已收盘）。
   不过滤的话最后一根是半截的 —— 那会让「今天」的回测结果每小时都在变。
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

import pandas as pd

from .base import Capability, DataSource, SourceError

# 两个域名互为备用。okx.com 在部分网络下不可达，aws.okx.com 是官方镜像。
_BASES = ("https://www.okx.com", "https://aws.okx.com")

# 单次最多 100 根（OKX 上限）。history-candles 支持 after 游标向更早翻页。
_PAGE = 100


class OKXSource(DataSource):
    """OKX 永续合约行情。"""

    name = "okx"
    capabilities = Capability.INSTRUMENTS | Capability.STOCK_BARS

    def __init__(self, timeout: int = 25, retries: int = 5, pause: float = 0.12):
        # OKX 公开端点限流约 20 次/2 秒，pause 0.12 秒远在安全线内。
        # v0.0 被东财封过 IP（≥45 分钟未恢复），此后一律宁慢勿快。
        self.timeout = timeout
        self.retries = retries
        self.pause = pause
        self._inst_cache: dict[str, dict] = {}

    # --- HTTP ---

    def _get(self, path: str) -> dict:
        last = None
        for attempt in range(self.retries):
            for base in _BASES:
                try:
                    req = urllib.request.Request(
                        base + path, headers={"User-Agent": "AStockEngine/etl"})
                    with urllib.request.urlopen(req, timeout=self.timeout) as r:
                        body = json.loads(r.read().decode("utf-8"))
                    if body.get("code") != "0":
                        # OKX 用 HTTP 200 + code 报业务错误，不看 code 会把
                        # 错误当成空结果，然后静默写出一张空表
                        raise SourceError(
                            f"OKX 返回 code={body.get('code')} msg={body.get('msg')} "
                            f"（{path}）")
                    return body
                except SourceError:
                    raise
                except Exception as e:  # noqa: BLE001
                    last = f"{base}: {type(e).__name__} {e}"
            # 指数退避：限流与网络抖动都靠它，别把重试打成新的压力源
            time.sleep(min(2 ** attempt * 0.5, 8.0))
        raise SourceError(f"OKX 请求失败（重试 {self.retries} 次）：{path} —— {last}")

    # --- 合约规格 ---

    def contract(self, inst_id: str) -> dict:
        """取合约规格。结果缓存 —— 拉行情时每根 bar 都要用到 ctVal。

        返回 OKX 原样字段，调用方自行解读。关键几个：
          ctVal      每张的标的数量（BTC-USDT-SWAP 为 0.01 BTC）
          ctValCcy   ctVal 的单位币种
          tickSz     价格最小变动
          lotSz      数量最小变动（**张**，BTC/ETH 均为 0.01，不是整数张）
          settleCcy  结算币种（linear 合约为 USDT）
          listTime   上线时间（毫秒）
        """
        if inst_id in self._inst_cache:
            return self._inst_cache[inst_id]
        body = self._get(f"/api/v5/public/instruments?instType=SWAP&instId={inst_id}")
        data = body.get("data") or []
        if not data:
            raise SourceError(f"OKX 没有合约 {inst_id}")
        self._inst_cache[inst_id] = data[0]
        return data[0]

    def instruments(self, inst_ids: list[str] | None = None):
        """标的清单。

        与 A 股源不同，这里**必须显式给出要拉哪些合约** ——
        OKX 有数百个永续合约，全拉是几十万行且大部分是我们不关心的小币种。
        当前只拉 BTC / ETH（用户指定）。
        """
        ids = inst_ids or []
        rows = []
        for iid in ids:
            c = self.contract(iid)
            rows.append({
                "symbol": iid,
                "name": iid,  # OKX 不提供中文名，用 instId 本身
                "type": "swap",
                "list_date": _ms_to_ymd_cst(c.get("listTime")),
                "delist_date": 0,
                "listed": c.get("state") == "live",
                # 原样带上合约规格，由构建器转成 attrs
                "ct_val": c.get("ctVal"),
                "ct_val_ccy": c.get("ctValCcy"),
                "ct_mult": c.get("ctMult"),
                "ct_type": c.get("ctType"),
                "tick_sz": c.get("tickSz"),
                "lot_sz": c.get("lotSz"),
                "min_sz": c.get("minSz"),
                "settle_ccy": c.get("settleCcy"),
            })
        return pd.DataFrame(rows)

    def calendar(self, start: str, end: str):
        """加密货币 24×7，没有交易日历这回事。

        返回空表而不是抛异常：调用方查 `supports(CALENDAR)` 即可，
        但真调用了也不该炸 —— 「每天都是交易日」是个有意义的答案。
        """
        return pd.DataFrame(columns=["date", "is_trading_day"])

    # --- 行情 ---

    def daily_bars(self, symbol: str, exchange: str = "okx",
                   start: str = "", end: str = ""):
        """日线，UTC+8 切日，**只返回已收盘的 bar**。

        列与基类约定一致：trading_day / open / high / low / close / preclose /
        volume / amount / turn / tradestatus / is_st。

        其中：
          volume       张数（不是币数）—— 用户指定按张计
          amount       计价额，USDT（OKX 的 volCcyQuote）
          preclose     前一根的收盘价。加密没有除权，前收就是前收
          turn         None —— 永续合约没有流通股本，换手率无从谈起
          tradestatus  恒为 1。加密不停牌（交易所维护属异常，会表现为缺 bar）
          is_st        恒为 0
        """
        rows = self._fetch_all(symbol)
        if not rows:
            return pd.DataFrame(columns=[
                "trading_day", "open", "high", "low", "close", "preclose",
                "volume", "amount", "turn", "tradestatus", "is_st"])

        # OKX 返回倒序（新→旧），翻正才能算前收
        rows.sort(key=lambda r: int(r[0]))
        out = []
        prev_close = None
        for r in rows:
            ts, o, h, low, c, vol, _vol_ccy, vol_quote, _confirm = r
            day = _ms_to_ymd_cst(ts)
            if start and day < int(start.replace("-", "")):
                prev_close = c
                continue
            if end and day > int(end.replace("-", "")):
                break
            out.append({
                "trading_day": day,
                "open": o, "high": h, "low": low, "close": c,
                # 首根没有前收。**用开盘价而不是 0** —— 0 会让下游算出
                # -100% 的涨跌幅，而首日「前收=开盘」是通行处理
                "preclose": prev_close if prev_close is not None else o,
                "volume": vol, "amount": vol_quote,
                "turn": None, "tradestatus": 1, "is_st": 0,
            })
            prev_close = c
        return pd.DataFrame(out)

    # --- 资金费率 ---

    def funding_rates(self, inst_id: str) -> pd.DataFrame:
        """全历史资金费率，每 8 小时一条。

        列：funding_time（UTC 毫秒）/ rate_raw（原始字符串）。
        **不做聚合**：一天 3 条原样返回，由构建器落成 8h 粒度的表。

        为什么它不能省：永续合约没有交割日，靠这笔多空互付把合约价
        钉在现货附近。实测最近 96 天 BTC 有 82.4% 的结算是多头付钱，
        年化约 4.4% —— 比一个年换手 24 倍的 A 股策略的全部摩擦还贵。
        少算这一项，任何长期做多的加密回测都会系统性偏乐观。
        """
        rows: list[dict] = []
        seen: set[str] = set()
        cursor = ""
        while True:
            path = (f"/api/v5/public/funding-rate-history?instId={inst_id}"
                    f"&limit={_PAGE}")
            if cursor:
                path += f"&after={cursor}"
            data = self._get(path).get("data") or []
            if not data:
                break
            for r in data:
                t = r.get("fundingTime")
                if t in seen:
                    continue
                seen.add(t)
                rows.append({"funding_time": int(t),
                             "rate_raw": str(r.get("realizedRate")
                                             or r.get("fundingRate") or "0")})
            # 与 K 线同一套翻页规则：游标取**这一页最旧的一条**
            cursor = data[-1]["fundingTime"]
            if len(data) < _PAGE:
                break
            time.sleep(self.pause)
        rows.sort(key=lambda x: x["funding_time"])
        return pd.DataFrame(rows)

    def _fetch_all(self, inst_id: str) -> list[list]:
        """向更早翻页，直到没有更多数据。

        用 history-candles 而非 candles：后者只给最近约 300 根。
        `after` 是「返回早于该时刻的数据」的游标。
        """
        rows: list[list] = []
        seen: set[str] = set()
        cursor = ""
        while True:
            path = (f"/api/v5/market/history-candles?instId={inst_id}"
                    f"&bar=1D&limit={_PAGE}")
            if cursor:
                path += f"&after={cursor}"
            data = self._get(path).get("data") or []
            if not data:
                break
            fresh = 0
            for r in data:
                # confirm=0 是尚未收盘的当前 bar。留着它会让「今天」的
                # 回测结果每小时都在变，而且那根 bar 的收盘价根本还没定
                if r[8] != "1":
                    continue
                if r[0] in seen:
                    continue
                seen.add(r[0])
                rows.append(r)
                fresh += 1
            # 游标必须用**这一页最旧的一根**，与是否 confirm 无关 ——
            # 用 rows 的最旧值会在整页都未收盘时卡住不动
            cursor = data[-1][0]
            if len(data) < _PAGE or fresh == 0 and len(data) < _PAGE:
                break
            if len(data) < _PAGE:
                break
            time.sleep(self.pause)
        return rows


def _ms_to_ymd_cst(ms) -> int:
    """毫秒时间戳 → UTC+8 下的 YYYYMMDD。

    **必须用 UTC+8**：OKX 的 1D bar 以 UTC+8 00:00 为界，
    用 UTC 折算会让每根 bar 的日期都早一天。
    """
    if ms is None or ms == "":
        return 0
    import datetime as _dt
    cst = _dt.timezone(_dt.timedelta(hours=8))
    return int(_dt.datetime.fromtimestamp(int(ms) / 1000, cst).strftime("%Y%m%d"))
