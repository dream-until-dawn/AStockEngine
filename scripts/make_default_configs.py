"""生成六份默认回测配置。

每一份都对应一个**明确的组合**（市场 × 标的数 × 策略 × 风控形态），
而不是「随便调一组参数看看」。放在脚本里而不是手写六份 JSON，
是因为它们共享大量结构（数据段、费率段、账户段），
手写六遍必然会有一份和别的不一样，而那种不一样查起来很费劲。

单标的配置的**基准就是它自己** —— 单只标的跟大盘比没有意义，
它的超额收益说的是「选时做得怎么样」，那才是这类配置要回答的问题。
"""

import io
import json
import os

OUT = os.path.join(os.path.dirname(__file__), "..", "configs", "backtest")

# 单标的用哪一只：A 股取招商银行（主板、全程在市、流动性足），
# 加密取 BTC / ETH 两只永续。
ASHARE_ONE = "600036"
ASHARE_GRID_ONE = "600519"
BTC = "BTC-USDT-SWAP"
ETH = "ETH-USDT-SWAP"


def ind(name, field):
    return {"kind": "ind", "ind": name, "field": field}


def bar(field):
    return {"kind": "bar", "field": field}


def val(v):
    return {"kind": "value", "value": v}


def cond(left, cmp_, right):
    return {"left": left, "cmp": cmp_, "right": right}


MACD = {"name": "macd", "kind": "macd",
        "params": {"short": 12, "long": 26, "signal": 9}}
RSI = {"name": "rsi", "kind": "rsi", "params": {"period": 14}}
MA60 = {"name": "ma60", "kind": "sma", "params": {"period": 60}}

DIF, DEA = ind("macd", "DIF"), ind("macd", "DEA")


def base(market, universe, benchmark, cash, name):
    """数据 / 市场 / 费率 / 账户 / 绩效这几段的共同骨架。"""
    crypto = market == "crypto"
    return {
        "name": name,
        "data": {
            "root": "../../data",
            "market": market,
            "freq": "1d",
            "from": 20200101 if crypto else 20150101,
            "to": 0,
            "universe": universe,
        },
        "market": {"impl": "crypto" if crypto else "ashare"},
        "fee": {
            "impl": "config",
            "params": {
                "path": "../fee/crypto_okx.json" if crypto
                else "../fee/ashare_default.json"
            },
        },
        "slippage": {"impl": "fixed_bps", "params": {"bps": 5}},
        "portfolio": {
            "initial_cash_cents": cash,
            "ledger": "margin" if crypto else "spot",
            **({"leverage": 2, "maint_margin_ppm": 5000} if crypto else {}),
        },
        "broker": {"volume_cap_ppm": 100000, "allow_partial_fill": True},
        "metrics": {"benchmark": benchmark, "risk_free_ppm": 0},
        "recorder": {"level": "summary"},
    }


def tree(indicators, buy, sell, valid=None, direction="long"):
    p = {"indicators": indicators, "direction": direction,
         "buy": buy, "sell": sell}
    if valid is not None:
        p["valid"] = valid
    return {"impl": "rule_tree", "params": p}


CONFIGS = {}

# ---- 1. A 股 · 单标的 · 规则树 · 默认有效 · 带止盈无止损 ----
#
# 「默认有效」= 不配 valid 树。买入树说买就是真买，不产生虚拟持仓。
c = base("ashare", {"symbols": [ASHARE_ONE]}, ASHARE_ONE, 2_000_000,
         "A 股单标的（%s）· 规则树 · 默认有效 · 带止盈无止损。"
         "单标的的基准就是它自己 —— 超额收益说的是「选时比一直持有强多少」。"
         % ASHARE_ONE)
c["strategy"] = tree([MACD], cond(DIF, "cross_above", DEA),
                     cond(DIF, "cross_below", DEA))
c["sizer"] = {"impl": "pct_equity",
              "params": {"pct": 95, "base": "cost", "max_positions": 1}}
c["risk"] = []
# 只止盈不止损：亏了交给卖出信号处理，赚够了就先落袋
c["exit"] = [{"impl": "take_profit", "params": {"pct": 20}}]
CONFIGS["ashare_single_ruletree"] = c

# ---- 2. A 股 · 多标的 · 规则树 · 带有效判断 · 带止损无止盈 ----
#
# valid 树把「已经涨过头」的买点过滤成虚拟持仓 —— 它们不占资金，
# 但会出现在逐轮交易里，用来回答「这个过滤到底该不该留」。
c = base("ashare",
         {"market": ["ashare"], "type": "stock", "board": ["main"],
          "status": "listed"},
         "510300", 20_000_000,
         "A 股多标的（主板在市个股）· 规则树 · 带有效判断 · 带止损无止盈 · "
         "本金 20 万（10 份 × 2 万，刚好压住 5 元最低佣金）。"
         "有效性树把 RSI 已经过热的买点滤成虚拟持仓，逐轮交易里能与实仓并排比。")
# 买入树加一条**趋势过滤**：只在 60 日均线之上做多。
#
# 不是为了把收益调好看，是为了让这份配置的数字由策略决定而不是由
# 手续费决定：光靠 MACD 金叉在三千只股票上轮动，年换手 26 倍，
# 而 A 股佣金有 5 元最低一笔 —— 两万本金切几份之后每笔都踩在最低收费上，
# 摩擦吃掉四成本金，看到的就不再是「这个信号好不好」了。
c["strategy"] = tree(
    [MACD, RSI, MA60],
    {"op": "and", "children": [
        cond(DIF, "cross_above", DEA),
        cond(bar("close"), "gt", ind("ma60", "MA")),
    ]},
    # 卖出用**跌破 60 日线**而不是 MACD 死叉。
    #
    # 死叉在震荡里反复出现，实测年换手 30 倍、平均持有 8 天 ——
    # 在一个买入条件是「金叉且在均线上」的趋势型配置里，
    # 用一个震荡型的条件出场，本身就是自相矛盾的。
    # 进场看趋势，出场也看趋势。
    cond(bar("close"), "lt", ind("ma60", "MA")),
    valid=cond(ind("rsi", "RSI"), "lt", val(70)))
# **本金 20 万而不是 2 万**，这是唯一诚实的选择。
#
# A 股佣金 0.025% 但有 5 元最低一笔 —— 那条下限一直咬到
# 5.00 / 0.00025 = **20,000 元**为止。也就是说，任何低于两万的成交
# 都在按高于名义费率的价钱付钱。
#
# 两万本金切 10 份，每份两千，5 元就是单边 0.25%、一个来回 0.5%，
# 比名义费率高一个数量级。实测：摩擦吃掉 47% 的初始资金，
# 佣金 8,425 元 ÷ 1,685 笔 = 每笔正好 5.00 元 —— **笔笔踩在下限上**。
# 而且越亏份额越小、下限占比越高，是个死亡螺旋
# （切成 5 份反而更差：−94.5% → −97.6%）。
#
# 20 万 ÷ 10 份 = 每份两万，正好是下限不再咬人的那个点。
# 装配器里 A 股默认仍是 2 万（那是单标的的合理量级），
# 但一份要同时持有 10 只的配置，本金必须配得上它的份数。
c["sizer"] = {"impl": "equal_weight",
              "params": {"slots": 10, "base": "cost", "order_by": "amount"}}
c["risk"] = [{"impl": "min_turnover", "params": {"amount_wan": 500}}]
# 只止损不止盈：让利润跑，亏损砍掉。
# 15% 而不是 8%：A 股个股一周走 8% 是常事，止损设在噪声里只会来回被打
c["exit"] = [{"impl": "stop_loss", "params": {"pct": 15}}]
CONFIGS["ashare_multi_ruletree"] = c

# ---- 3. A 股 · 单标的 · 网格 ----
c = base("ashare", {"symbols": [ASHARE_GRID_ONE]}, ASHARE_GRID_ONE, 2_000_000,
         "A 股单标的（%s）· 网格 · 越跌越买、涨回基准就全平。"
         "网格在日线上是失真的：真实网格靠盘中挂单，一天可能来回穿好几格，"
         "而这里按收盘价定档，会系统性地低估交易次数。"
         % ASHARE_GRID_ONE)
c["strategy"] = {"impl": "grid",
                 "params": {"levels": 5, "step_pct": 5, "short": 0}}
c["sizer"] = {"impl": "strength_weighted",
              "params": {"total_pct": 95, "base": "cost"}}
c["risk"] = []
c["exit"] = []
CONFIGS["ashare_single_grid"] = c

# ---- 4. 加密 · 单标的 · 规则树 · 仅开多 · 默认有效 · 带止盈止损 ----
c = base("crypto", {"symbols": [BTC]}, BTC, 100_000,
         "加密单标的（%s）· 规则树 · 仅开多 · 默认有效 · 带止盈止损。"
         "基准就是它自己 —— 在一个涨了十倍的标的上，"
         "跑赢「一直拿着」才说明选时有价值。" % BTC)
c["strategy"] = tree([MACD], cond(DIF, "cross_above", DEA),
                     cond(DIF, "cross_below", DEA), direction="long")
c["sizer"] = {"impl": "pct_equity",
              "params": {"pct": 90, "base": "cost", "max_positions": 1}}
c["risk"] = []
c["exit"] = [
    {"impl": "stop_loss", "params": {"pct": 10}},
    {"impl": "take_profit", "params": {"pct": 25}},
]
CONFIGS["crypto_single_ruletree"] = c

# ---- 5. 加密 · 多标的 · 规则树 · 双向 · 带有效判断 · 峰点回落清仓 + 冷静期 ----
#
# 双向 = composite/union 组两棵树，一多一空，各写各的条件。
# 引擎里没有「双向模式」这个开关。
long_tree = tree([MACD, RSI], cond(DIF, "cross_above", DEA),
                 cond(DIF, "cross_below", DEA),
                 valid=cond(ind("rsi", "RSI"), "lt", val(75)), direction="long")
short_tree = tree([MACD, RSI], cond(DIF, "cross_below", DEA),
                  cond(DIF, "cross_above", DEA),
                  valid=cond(ind("rsi", "RSI"), "gt", val(25)), direction="short")
c = base("crypto", {"market": ["crypto"]}, BTC, 100_000,
         "加密多标的 · 规则树双向（一多一空两棵树 union）· 带有效判断 · "
         "峰值回撤 15% 熔断并清仓、冷静期 20 根。"
         "冷静期期满会从当下权益重新起算回撤 —— 不重算的话清仓后权益不再变动，"
         "熔断一到期就再次触发，从此再也不交易。")
c["strategy"] = {"impl": "composite", "mode": "union",
                 "sources": [long_tree, short_tree]}
c["sizer"] = {"impl": "equal_weight",
              "params": {"slots": 4, "base": "cost", "order_by": "amount"}}
c["risk"] = [{"impl": "drawdown_halt",
              "params": {"pct": 15, "cooldown_bars": 20, "flatten": True}}]
c["exit"] = []
CONFIGS["crypto_multi_ruletree_hedge"] = c

# ---- 6. 加密 · 单标的 · 网格 · 仅开空 ----
c = base("crypto", {"symbols": [ETH]}, ETH, 100_000,
         "加密单标的（%s）· 网格 · 仅开空：越涨越空、跌回基准就全平。"
         "做空网格只在允许做空的市场可用；配到 A 股上会在装配时直接报错，"
         "而不是安静地跑成零成交。" % ETH)
c["strategy"] = {"impl": "grid",
                 "params": {"levels": 5, "step_pct": 5, "short": 1}}
c["sizer"] = {"impl": "strength_weighted",
              "params": {"total_pct": 80, "base": "cost"}}
c["risk"] = []
c["exit"] = []
CONFIGS["crypto_single_grid_short"] = c


def main():
    for name, cfg in CONFIGS.items():
        path = os.path.join(OUT, name + ".json")
        with io.open(path, "w", encoding="utf-8", newline="\n") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
            f.write("\n")
        print("写出", name + ".json")


if __name__ == "__main__":
    main()
