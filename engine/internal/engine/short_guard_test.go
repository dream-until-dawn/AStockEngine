package engine

import (
	"strings"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 「做空策略不能配在不许做空的市场上」这条守卫曾经**在 Init 之前**执行，
// 于是对整整一类策略完全失效。
//
// 网格的 short 开关是 `Init` 里从参数读出来的；在那之前问 NeedsShort()，
// 拿到的是字段的零值 false。实测后果：做空网格配在 A 股上，
// 信号 606 条、成交 0 笔、拒单 0 笔、总收益 +0.00%，全程零报错 ——
// 开空信号被 Sizer 当成减仓，而手上没有多头可减，订单就那么没了。
//
// 规则树当时没暴露这个问题，因为它走 ConfigurableStrategy.Configure，
// 那个在装配阶段就调了。**一个守卫对一半的策略有效，比没有守卫更危险。**

// lateShort 模拟那一类策略：要不要做空，Init 之后才知道。
type lateShort struct{ short bool }

func (s *lateShort) Name() string       { return "late_short" }
func (s *lateShort) Specs() []ParamSpec { return nil }
func (s *lateShort) NeedsShort() bool   { return s.short }
func (s *lateShort) Init(InitContext) error {
	s.short = true // 参数是这时候才写进来的
	return nil
}
func (s *lateShort) OnBar(StepContext) ([]Signal, error) { return nil, nil }

func TestShortGuardNeedsPostInitState(t *testing.T) {
	s := &lateShort{}
	ashare := trading.NewAShareMarket()

	// Init 之前问，答案是零值 —— 守卫放行。**这正是当年那个 bug**，
	// 所以把它作为一条断言钉在这里：不是「应该这样」，
	// 而是「在这个时点问，本来就问不出真话」
	if err := checkShortSupport(s, ashare); err != nil {
		t.Fatalf("Init 之前 NeedsShort 还是零值，此时不该报错，却报了：%v", err)
	}

	if err := s.Init(nil); err != nil {
		t.Fatal(err)
	}
	err := checkShortSupport(s, ashare)
	if err == nil {
		t.Fatal("做空策略配在 A 股上必须报错 —— 放过去是静默失效：" +
			"信号照发、订单全丢、收益恒等于 0，报告上看不出任何异常")
	}
	if !strings.Contains(err.Error(), "ashare") {
		t.Errorf("报错要说清是哪个市场不支持，得到：%v", err)
	}
}

// TestShortGuardAllowsShortMarket 允许做空的市场上照常放行。
func TestShortGuardAllowsShortMarket(t *testing.T) {
	s := &lateShort{}
	if err := s.Init(nil); err != nil {
		t.Fatal(err)
	}
	if !trading.NewCryptoMarket().AllowsShort() {
		t.Fatal("加密永续应当允许做空 —— 这条前提不成立的话下面测的就不是守卫")
	}
	if err := checkShortSupport(s, trading.NewCryptoMarket()); err != nil {
		t.Errorf("加密永续允许做空，不该拦：%v", err)
	}
}

// TestShortGuardIgnoresLongOnly 不做空的策略在任何市场上都放行。
func TestShortGuardIgnoresLongOnly(t *testing.T) {
	long := &lateShort{}
	long.short = false
	// 不调 Init，保持 short = false
	if err := checkShortSupport(long, trading.NewAShareMarket()); err != nil {
		t.Errorf("只做多的策略不该被拦：%v", err)
	}
}
