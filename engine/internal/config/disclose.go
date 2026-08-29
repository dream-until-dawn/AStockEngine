package config

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// Disclosures 列出**本次回测已知未计入的成本与机制**。
//
// 为什么要有这么个东西：回测报告上出现的每一个数字都在说「发生了什么」，
// 没有任何一处在说「什么没算」。而漏算的成本恰恰是最危险的 ——
// 它不报错、不异常，只是让结果一致地偏乐观，看上去还很合理。
//
// v0.3 的滑点就是这样藏了很久：它以前混在成交价里，报告只看得到佣金
// 与印花税，直到单列出来才发现 `macd_cross` 的滑点是 8.89 万元
// （占初始 8.89%），与全部费用同一量级。
//
// **返回的每一条都必须印在报告上、也必须传给 Web 前端。**
// 一个只存在于源码注释里的缺口，等于不存在。
func (c *Config) Disclosures(
	uni *mktdata.Universe, ids []mktdata.InstrumentID,
) []string {
	var out []string

	// 永续合约的资金费率
	if n := countMarket(uni, ids, mktdata.MarketCrypto); n > 0 {
		out = append(out, fmt.Sprintf(
			"资金费率未计入（%d 只永续合约）。永续每 8 小时结算一次多空互付，"+
				"实测 BTC-USDT-SWAP 最近 96 天有 82.4%% 的结算是多头付钱、"+
				"年化约 4.4%% —— 比一个年换手 24 倍的 A 股策略的全部摩擦还贵。"+
				"做多为主的结果会系统性偏乐观。", n))
	}

	// 下面两条对全部市场都成立，属于 ROADMAP C8「明确不建模的机制」，
	// 写在这里是为了让读报告的人不必去翻文档
	out = append(out,
		"未建模集合竞价：开盘价取 09:30 连续竞价起点，不含 09:25 的竞价撮合",
		"未建模融资融券与个股期权：账本为现货多头，不支持做空与杠杆")

	return out
}

func countMarket(
	uni *mktdata.Universe, ids []mktdata.InstrumentID, m mktdata.Market,
) int {
	if uni == nil {
		return 0
	}
	n := 0
	for _, id := range ids {
		if in := uni.Get(id); in != nil && in.Market == m {
			n++
		}
	}
	return n
}
