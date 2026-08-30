package config

import "testing"

// 预热段必须从绩效里砍掉。
//
// Walk-Forward 的每个窗口都多裁一段预热前缀，那一段权益恒等于初始资金。
// 留着它的后果不是「多几个点」：4 年窗口里 3 年零收益，
// 年化收益被摊薄 4 倍、年化波动被压低、**夏普被系统性抬高** ——
// 而这些数字看上去完全正常，不会有任何报错。

func TestTrimToTradeFrom(t *testing.T) {
	days := []int32{20200101, 20200102, 20200103, 20200106, 20200107}
	eq := []int64{100, 100, 100, 110, 120}

	d, e := trimToTradeFrom(days, eq, 20200103)
	// 保留 TradeFrom 前一个点作基准起点 —— 收益是相对起点算的
	if len(d) != 4 || d[0] != 20200102 {
		t.Fatalf("应从 20200102 起保留 4 个点，得到 %v", d)
	}
	if e[0] != 100 || e[len(e)-1] != 120 {
		t.Errorf("净值切错了: %v", e)
	}
}

func TestTrimToTradeFromNoop(t *testing.T) {
	days := []int32{20200101, 20200102}
	eq := []int64{100, 110}

	if d, _ := trimToTradeFrom(days, eq, 0); len(d) != 2 {
		t.Error("TradeFrom 为 0 时不该动")
	}
	if d, _ := trimToTradeFrom(days, eq, 20200101); len(d) != 2 {
		t.Error("TradeFrom 等于第一天时不该动")
	}
	// 全部早于 TradeFrom：整段都是预热，此时不能砍成空的 ——
	// 让调用方看到「一个交易日都没有」比看到空曲线更容易发现问题
	if d, _ := trimToTradeFrom(days, eq, 20300101); len(d) != 2 {
		t.Error("全在预热区间内时应原样返回，由调用方发现异常")
	}
}

func TestTrimToTradeFromEmpty(t *testing.T) {
	if d, e := trimToTradeFrom(nil, nil, 20200101); d != nil || e != nil {
		t.Error("空曲线应原样返回")
	}
}
