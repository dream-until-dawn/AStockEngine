"""数据源适配器包。

新增数据源只需在此实现 `DataSource` 子类并声明 `capabilities`，
DataGateway 以上的代码不应改动（约束 C9）。
"""

from .base import Capability, DataSource, SourceError, UnsupportedCapability
from .baostock_src import BaoStockSource
from .external import ExternalSource
from .sina_src import SinaSource

__all__ = [
    "Capability",
    "DataSource",
    "SourceError",
    "UnsupportedCapability",
    "BaoStockSource",
    "SinaSource",
    "ExternalSource",
]

# 非 Python 数据源放在仓库根的 ingest/ 下，经 ExternalSource 接入，
# 调用方无从得知其实现语言。见 ingest/README.md。
# 当前为空 —— A 股需求由上面两个 Python 适配器完全覆盖。
