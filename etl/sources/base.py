"""数据源适配器接口。

对应约束 C9：数据源不是「将来可能要换」的预留，而是核心设计 ——
v0.0 已经导致主力源从 AkShare 换成 BaoStock，且 ETF 与个股走不同的源。

**适配器的职责是归一化**：把各数据源千奇百怪的字段名、单位、类型
转成本模块定义的规范形态。归一化之后的下游代码不得再感知数据源差异。

规范形态一律用 Python 原生类型与「人类单位」（价格为元、比率为百分数），
定点整数转换统一由 `etl/schema.py` 在写出前完成 —— 适配器不碰定点。
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from enum import Flag, auto


class Capability(Flag):
    """数据源能力位。没有一个免费源覆盖全部能力，因此必须显式声明。"""

    NONE = 0
    INSTRUMENTS = auto()        # 标的清单（含上市/退市日期）
    CALENDAR = auto()           # 交易日历
    STOCK_BARS = auto()         # 个股日线
    ETF_BARS = auto()           # ETF 日线
    ADJ_FACTORS = auto()        # 复权因子（事件式）
    CORPORATE_ACTIONS = auto()  # 分红送配


class DataSource(ABC):
    """数据源适配器基类。

    未声明的能力对应的方法会抛 `UnsupportedCapability`，
    调用方应先查 `capabilities` 而非依赖异常控制流程。
    """

    name: str = "unnamed"
    capabilities: Capability = Capability.NONE

    def supports(self, cap: Capability) -> bool:
        return bool(self.capabilities & cap)

    def _require(self, cap: Capability) -> None:
        if not self.supports(cap):
            raise UnsupportedCapability(f"{self.name} 不支持 {cap.name}")

    # --- 生命周期（部分源需要登录/长连接） ---

    def open(self) -> None:
        return None

    def close(self) -> None:
        return None

    def __enter__(self):
        self.open()
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    # --- 数据接口 ---
    #
    # 返回值均为 pandas.DataFrame，列名与语义如各方法 docstring 所定义。
    # 空结果返回空 DataFrame（列齐全），而非 None。

    @abstractmethod
    def instruments(self):
        """标的清单。

        列：symbol(str, 不含交易所前缀) / name(str) /
            type('stock'|'etf') / list_date(YYYYMMDD int) /
            delist_date(YYYYMMDD int, 0 表示在市) / listed(bool)
        """

    @abstractmethod
    def calendar(self, start: str, end: str):
        """交易日历。

        列：date(YYYYMMDD int) / is_trading_day(bool)
        """

    @abstractmethod
    def daily_bars(self, symbol: str, exchange: str, start: str, end: str):
        """日线。exchange 取 'sh' / 'sz' / 'bj'。

        列：trading_day(YYYYMMDD int) / open / high / low / close / preclose
            （价格为元，可为 str 或 float —— 下游用 Decimal 解析）/
            volume(股或份) / amount(元) / turn(百分数) /
            tradestatus(1 正常 0 停牌) / is_st(1|0)

        数据源未提供的列以 None 填充，由构建器按 SCHEMA.md 1.4 的约定补齐。
        """

    def adj_factors(self, symbol: str, exchange: str):
        """复权因子，事件式，仅除权日一行。

        列：ex_date(YYYYMMDD int) / factor_raw(str, 保留数据源原始精度)
        """
        self._require(Capability.ADJ_FACTORS)
        raise NotImplementedError

    def corporate_actions(self, symbol: str, exchange: str, year: int):
        """分红送配。

        列：ex_date / record_date / pay_date (YYYYMMDD int, 缺失为 0) /
            cash_before_tax(每股税前现金红利, 元) /
            stock_dividend(每股送股) / stock_transfer(每股转增)
        """
        self._require(Capability.CORPORATE_ACTIONS)
        raise NotImplementedError


class UnsupportedCapability(NotImplementedError):
    pass


class SourceError(RuntimeError):
    """数据源返回了错误码或不可解析的内容。"""
