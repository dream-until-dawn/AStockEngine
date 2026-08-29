"""新浪适配器（经 AkShare）—— ETF 日线与个股复权因子。

v0.0 实测结论：
  - `stock_zh_a_daily(adjust="hfq-factor")` 提供 16 位精度的**事件式**复权因子，
    还原后复权价的最大相对误差 4.19e-07（485 个交易日）
  - `fund_etf_hist_sina` 覆盖 ETF 全历史（510300 自 2012-05-28 起 3466 行）
  - 完全没有退市股数据，故个股行情不走本源

已知缺陷（SCHEMA.md 1.4）：ETF 日线**字段在不同标的间不一致**，
只有部分标的带 `prevclose`；且不提供 turn / tradestatus / is_st。
"""

from __future__ import annotations

import time

import akshare as ak
import pandas as pd

from .base import Capability, DataSource, SourceError

_ETF_COLUMNS = [
    "trading_day", "open", "high", "low", "close", "preclose",
    "volume", "amount", "turn", "tradestatus", "is_st",
]


def _ymd(v) -> int:
    s = str(v)[:10].replace("-", "")
    return int(s) if len(s) == 8 and s.isdigit() else 0


class SinaSource(DataSource):
    name = "sina"
    capabilities = Capability.ETF_BARS | Capability.ADJ_FACTORS

    def __init__(self, pause: float = 0.4, retries: int = 3) -> None:
        # 新浪未观察到硬限流，但保留节流与重试以免长时间批量拉取被判定为异常流量
        self.pause = pause
        self.retries = retries

    def _call(self, fn, *args, **kwargs):
        last = None
        for i in range(self.retries):
            try:
                return fn(*args, **kwargs)
            except Exception as exc:  # noqa: BLE001
                last = exc
                if i < self.retries - 1:
                    time.sleep(self.pause * (i + 1) * 2)
        raise SourceError(f"{self.name} 调用失败：{type(last).__name__}: {last}")

    def instruments(self):
        raise NotImplementedError("新浪不作为标的清单来源，见 BaoStockSource")

    def calendar(self, start: str, end: str):
        raise NotImplementedError("新浪不作为交易日历来源，见 BaoStockSource")

    # --- ETF 日线 ---

    def daily_bars(self, symbol: str, exchange: str, start: str, end: str) -> pd.DataFrame:
        self._require(Capability.ETF_BARS)
        raw = self._call(ak.fund_etf_hist_sina, symbol=f"{exchange}{symbol}")
        time.sleep(self.pause)
        if raw is None or len(raw) == 0:
            return pd.DataFrame(columns=_ETF_COLUMNS)

        out = pd.DataFrame()
        out["trading_day"] = raw["date"].map(_ymd)
        for c in ("open", "high", "low", "close"):
            out[c] = raw[c]
        # 字段不一致：仅部分标的带 prevclose。缺失时留空，
        # 由构建器按「前一交易日 close」补齐（SCHEMA.md 1.4）
        out["preclose"] = raw["prevclose"] if "prevclose" in raw.columns else None
        out["volume"] = pd.to_numeric(raw["volume"], errors="coerce").fillna(0).astype("int64")
        out["amount"] = raw["amount"] if "amount" in raw.columns else 0
        # 新浪不提供以下三项，按 SCHEMA.md 1.4 的降级约定取值
        out["turn"] = 0            # 无流通份额，无法计算
        out["tradestatus"] = 1     # 无停牌标记，缺行即视为停牌/未上市
        out["is_st"] = 0           # ETF 无 ST 概念

        s, e = _ymd(start), _ymd(end)
        out = out[(out["trading_day"] >= s) & (out["trading_day"] <= e)]
        return out.reset_index(drop=True)

    # --- 复权因子 ---

    def adj_factors(self, symbol: str, exchange: str) -> pd.DataFrame:
        self._require(Capability.ADJ_FACTORS)
        raw = self._call(ak.stock_zh_a_daily, symbol=f"{exchange}{symbol}", adjust="hfq-factor")
        time.sleep(self.pause)
        if raw is None or len(raw) == 0:
            return pd.DataFrame(columns=["ex_date", "factor_raw"])

        out = pd.DataFrame()
        out["ex_date"] = raw["date"].map(_ymd)
        # 因子是 16 位精度字符串，**必须原样保留**，转 float 会丢低位
        out["factor_raw"] = raw["hfq_factor"].astype(str)
        # 1900-01-01 是数据源的哨兵行，非真实除权事件
        out = out[out["ex_date"] > 19000102]
        return out.sort_values("ex_date").reset_index(drop=True)
