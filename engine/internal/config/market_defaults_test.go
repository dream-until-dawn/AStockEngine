package config

import (
	"encoding/json"
	"testing"
)

// 每个市场都有一套自己的默认值：计价货币不同、账本不同、基准不同。
//
// **这些默认值只在用户没写的时候生效，所以最容易悄悄错。**
// 加密的初始资金若沿用 A 股的 20,000，报告会印「初始 20000.00 USDT」——
// 数字合法、单位合法、结论全错。

// loadFromJSON 从一段 JSON 走完整的 Load 路径（含补默认值与校验）。
func loadFromJSON(t *testing.T, body string) *Config {
	t.Helper()
	var c Config
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	c.applyDefaults()
	return &c
}

// TestMarketDefaultsAShare A 股：20,000 元、现货账本、无基准。
func TestMarketDefaultsAShare(t *testing.T) {
	c := loadFromJSON(t, `{"data":{"market":"ashare"}}`)

	if got := c.Portfolio.InitialCashCents; got != 2_000_000 {
		t.Errorf("初始资金 = %d 分，想要 2000000（20,000 元）", got)
	}
	if got := c.Portfolio.Ledger; got != "spot" {
		t.Errorf("账本 = %q，想要 spot", got)
	}
	if got := c.Market.Impl; got != "ashare" {
		t.Errorf("市场规则 = %q，想要 ashare", got)
	}
	if got := DefaultBenchmark("ashare"); got != "" {
		t.Errorf("A 股默认基准 = %q，想要空（没有指数数据，由配置显式指定 ETF）", got)
	}
}

// TestMarketDefaultsCrypto 加密：1,000 USDT、保证金账本、BTC 永续为基准。
func TestMarketDefaultsCrypto(t *testing.T) {
	c := loadFromJSON(t, `{"data":{"market":"crypto"}}`)

	// 1,000 USDT，单位是 0.01 USDT
	if got := c.Portfolio.InitialCashCents; got != 100_000 {
		t.Errorf("初始资金 = %d，想要 100000（1,000 USDT）", got)
	}
	if got := c.Portfolio.Ledger; got != "margin" {
		t.Errorf("账本 = %q，想要 margin（加密固定逐仓）", got)
	}
	if got := c.Market.Impl; got != "crypto" {
		t.Errorf("市场规则 = %q，想要 crypto", got)
	}
	if got := DefaultBenchmark("crypto"); got != "BTC-USDT-SWAP" {
		t.Errorf("加密默认基准 = %q，想要 BTC-USDT-SWAP", got)
	}
}

// TestMarketDefaultsDoNotOverrideExplicit 写了就以写的为准。
//
// 默认值最坏的失败方式不是「给错了」，而是「盖掉了用户写的」。
func TestMarketDefaultsDoNotOverrideExplicit(t *testing.T) {
	c := loadFromJSON(t, `{
		"data": {"market": "crypto"},
		"portfolio": {"initial_cash_cents": 5000000, "ledger": "spot"},
		"market": {"impl": "ashare"}
	}`)

	if got := c.Portfolio.InitialCashCents; got != 5_000_000 {
		t.Errorf("显式写的初始资金被盖掉了：%d", got)
	}
	if got := c.Portfolio.Ledger; got != "spot" {
		t.Errorf("显式写的账本被盖掉了：%q", got)
	}
	if got := c.Market.Impl; got != "ashare" {
		t.Errorf("显式写的市场规则被盖掉了：%q", got)
	}
}

// TestDefaultMarketIsAShare 没写 market 时按 A 股处理。
//
// **不能默认成加密**：既有的几十份 A 股配置都没写这个字段，
// 默认值一改，它们会安静地按 365 天年化、按 1,000 USDT 起始资金跑。
func TestDefaultMarketIsAShare(t *testing.T) {
	c := loadFromJSON(t, `{}`)
	if got := c.Data.Market; got != "ashare" {
		t.Fatalf("默认市场 = %q，想要 ashare", got)
	}
	if got := c.Portfolio.InitialCashCents; got != 2_000_000 {
		t.Errorf("默认初始资金 = %d，想要 2000000", got)
	}
}
