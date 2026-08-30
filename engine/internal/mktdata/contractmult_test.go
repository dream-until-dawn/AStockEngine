package mktdata

import "testing"

// 0.01 × 1e8 走 float64 会得到 999999.9999999999，取整成 999999。
// **合约乘数差一个单位，全部名义额跟着差** —— 而这类错不会报警。
func TestDecimalToFixedNoFloat(t *testing.T) {
	cases := []struct {
		in    string
		scale int64
		want  int64
	}{
		{"0.01", 100_000_000, 1_000_000},
		{"0.1", 100_000_000, 10_000_000},
		{"1", 100_000_000, 100_000_000},
		{"0.001", 100_000_000, 100_000},
		{"12.345", 1000, 12_345},
		{"-0.5", 1000, -500},
		{"0", 100_000_000, 0},
		// 超出 scale 的位直接截断
		{"0.123456789", 1000, 123},
	}
	for _, c := range cases {
		if got := decimalToFixed(c.in, c.scale); got != c.want {
			t.Errorf("decimalToFixed(%q, %d) = %d，期望 %d", c.in, c.scale, got, c.want)
		}
	}
}

func TestParseContractMult(t *testing.T) {
	// OKX 的 ct_val 是字符串（SCHEMA 2.3），不是数字
	if got := parseContractMult(`{"ct_val":"0.01","ct_val_ccy":"BTC"}`); got != 1_000_000 {
		t.Errorf("BTC 1 张 = 0.01 币 应为 1e6，得到 %d", got)
	}
	if got := parseContractMult(`{"ct_val":"0.1"}`); got != 10_000_000 {
		t.Errorf("ETH 1 张 = 0.1 币 应为 1e7，得到 %d", got)
	}
	// A 股的 attrs 是 null / 没有 ct_val —— 返回 0，由调用方用默认值
	if got := parseContractMult(`{}`); got != 0 {
		t.Errorf("没有 ct_val 时应返回 0，得到 %d", got)
	}
	if got := parseContractMult(`不是 json`); got != 0 {
		t.Errorf("解析失败时应返回 0，得到 %d", got)
	}
}
