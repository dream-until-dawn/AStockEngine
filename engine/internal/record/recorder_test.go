package record

import (
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

func step(day int32, equity int64, nFills int) Step {
	fills := make([]trading.Fill, nFills)
	for i := range fills {
		fills[i] = trading.Fill{
			Order: trading.Order{Instrument: mktdata.InstrumentID(i + 1), Side: trading.SideBuy},
			At:    mktdata.TimePoint{TradingDay: day}, Price: 10_000, Qty: 100,
		}
	}
	return Step{
		Time: mktdata.TimePoint{TradingDay: day}, EquityCents: equity,
		CashCents: equity / 2, Positions: nFills,
		NumSignals: 3, NumFills: nFills, NumRejects: 1,
		Signals:    []trading.Signal{{Instrument: 1, Kind: trading.SignalEnter}},
		Sized:      []trading.Order{{Instrument: 1, Side: trading.SideBuy, Qty: 100}},
		Fills:      fills,
		Rejections: []trading.Rejection{{Reason: trading.RejectRisk, Rule: "max_positions"}},
	}
}

// TestNoneKeepsOnlyFinalEquity none 级不留逐步记录 —— 海选内层几百组并发跑，
// 逐步留存会把内存吃光。但最终权益必须还在，那是海选唯一关心的数。
func TestNoneKeepsOnlyFinalEquity(t *testing.T) {
	m := NewMemory(None, 0)
	m.OnStep(step(20240102, 100, 1))
	m.OnStep(step(20240103, 200, 2))

	if len(m.Steps()) != 0 || len(m.Fills()) != 0 {
		t.Errorf("none 级不该留记录，得到 %d 步 / %d 成交", len(m.Steps()), len(m.Fills()))
	}
	if m.NumSteps() != 2 {
		t.Errorf("步数：期望 2，得到 %d", m.NumSteps())
	}
	if m.FinalEquityCents() != 200 {
		t.Errorf("最终权益：期望 200，得到 %d", m.FinalEquityCents())
	}
}

// TestSummaryKeepsFills summary 级必须留**全部成交** ——
// 胜率、盈亏比、换手率都从成交流算出来，只留每步汇总数就算不了。
func TestSummaryKeepsFills(t *testing.T) {
	m := NewMemory(Summary, 0)
	m.OnStep(step(20240102, 100, 2))
	m.OnStep(step(20240103, 200, 3))

	if len(m.Steps()) != 2 {
		t.Fatalf("步数：期望 2，得到 %d", len(m.Steps()))
	}
	if len(m.Fills()) != 5 {
		t.Errorf("成交：期望 5 笔，得到 %d", len(m.Fills()))
	}
	// 明细必须被清空，不能留着引用 —— 引擎给的切片下一步就被复用了
	for i, s := range m.Steps() {
		if s.Signals != nil || s.Sized != nil || s.Fills != nil || s.Rejections != nil {
			t.Errorf("第 %d 步：summary 级不该留明细", i)
		}
		if s.NumFills == 0 {
			t.Errorf("第 %d 步：计数不该被一起清掉", i)
		}
	}
	if len(m.Rejections()) != 0 {
		t.Error("summary 级不留拒单明细")
	}
}

// TestFullKeepsDetailAndCopies full 级留全部明细，且必须**拷贝** ——
// 引擎的切片会在下一步被复用，留引用等于留一堆会变的数据。
func TestFullKeepsDetailAndCopies(t *testing.T) {
	m := NewMemory(Full, 0)

	shared := step(20240102, 100, 2)
	m.OnStep(shared)
	// 模拟引擎复用底层数组
	shared.Fills[0].Qty = 999
	shared.Rejections[0].Rule = "被改掉了"

	steps := m.Steps()
	if len(steps) != 1 {
		t.Fatalf("步数：期望 1，得到 %d", len(steps))
	}
	if steps[0].Fills[0].Qty != 100 {
		t.Errorf("成交被外部改动影响了：期望 100，得到 %d", steps[0].Fills[0].Qty)
	}
	if steps[0].Rejections[0].Rule != "max_positions" {
		t.Errorf("拒单被外部改动影响了：得到 %q", steps[0].Rejections[0].Rule)
	}
	if len(m.Rejections()) != 1 {
		t.Errorf("full 级应当汇总拒单，得到 %d", len(m.Rejections()))
	}
}

func TestCurveExtraction(t *testing.T) {
	m := NewMemory(Summary, 0)
	m.OnStep(step(20240102, 100, 0))
	m.OnStep(step(20240103, 150, 0))
	days, eq := m.Curve()
	if len(days) != 2 || days[0] != 20240102 || days[1] != 20240103 {
		t.Errorf("交易日序列不对：%v", days)
	}
	if eq[0] != 100 || eq[1] != 150 {
		t.Errorf("权益序列不对：%v", eq)
	}
}

// TestOverflowWarning 内存记录器没有上限，超过阈值要报警而不是静默吃内存。
func TestOverflowWarning(t *testing.T) {
	m := NewMemory(Full, 10)
	for i := 0; i < 20; i++ {
		m.OnStep(step(20240102, 100, 1))
	}
	if len(m.Warnings) == 0 {
		t.Fatal("超过阈值应当报警")
	}
	if len(m.Warnings) > 1 {
		t.Errorf("只该报一次，得到 %d 条", len(m.Warnings))
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]Level{"none": None, "summary": Summary, "full": Full, "": Summary} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q)：期望 %v，得到 %v / %v", in, want, got, err)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("未知级别应当报错")
	}
}
