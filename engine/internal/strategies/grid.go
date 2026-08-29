package strategies

import (
	"encoding/json"
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Grid 网格策略：价格每跌一格加一份，每涨回一格减一份。
//
// 写它是为了验证一件事：**引擎的信号模型能不能表达「非信号驱动」的策略。**
// 网格不是「金叉了就买」，它是「维护一组价位，价格穿过就动」——
// 状态在策略这边，而不是在某个指标里。
//
// 结论：能，但要靠 `StatefulStrategy` 记住两样东西 ——
// 基准价与当前档位。这两样都无法从 StepContext 导出：
// ctx 只给当前时点，而基准价是**第一次见到这只标的时**定下的。
//
// 一处刻意的取舍：网格在日线上是**失真**的。
// 真实网格靠盘中挂单，一天内可能来回穿好几格；日线只有一根 OHLC，
// 最多知道「今天跌到过第 3 格」。这里按**收盘价**定档，
// 于是日内的来回全被抹掉 —— 它会系统性地低估网格的交易次数。
// 用最高最低价去猜穿了几格更不老实：那等于假设你在最优点位成交。
type Grid struct {
	levels  int     // 单边格数
	stepPPM int64   // 每格跌幅（百万分之一）
	perStep float64 // 每格投入占总资金的比例，交给 Sizer 时体现为 Strength

	// anchor 各标的的基准价（首次见到时的收盘价，定点）。
	// **一旦定下就不再变** —— 让它跟着价格漂移就成了追涨杀跌，不是网格
	anchor map[mktdata.InstrumentID]int64
	// level 当前档位：0 表示在基准价之上（空仓），n 表示已跌破第 n 格
	level map[mktdata.InstrumentID]int
}

func NewGrid() *Grid {
	return &Grid{
		anchor: make(map[mktdata.InstrumentID]int64, 4096),
		level:  make(map[mktdata.InstrumentID]int, 4096),
	}
}

func (s *Grid) Name() string { return "grid" }

func (s *Grid) Specs() []eng.ParamSpec {
	return []eng.ParamSpec{
		{Name: "levels", Kind: eng.ParamInt, Default: 5, Min: 1, Max: 50, Step: 1,
			Desc: "单边格数：最多跌几格就满仓"},
		{Name: "step_pct", Kind: eng.ParamFloat, Default: 5, Min: 0.1, Max: 50, Step: 0.1,
			Desc: "每格跌幅（%）"},
	}
}

func (s *Grid) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.levels = p.Int("levels", 5)
	if s.levels < 1 {
		s.levels = 1
	}
	stepPct := p.Float("step_pct", 5)
	if stepPct <= 0 {
		return fmt.Errorf("step_pct 必须为正")
	}
	s.stepPPM = int64(stepPct * 10_000)
	s.perStep = 1.0 / float64(s.levels)
	s.anchor = make(map[mktdata.InstrumentID]int64, 4096)
	s.level = make(map[mktdata.InstrumentID]int, 4096)
	// 网格不用任何指标 —— 它的全部状态都在自己手里。
	// 这本身也是个验证点：引擎不该要求策略必须声明指标
	return nil
}

func (s *Grid) OnBar(ctx eng.StepContext) ([]eng.Signal, error) {
	_, inFlight := holdingSet(ctx)
	var sigs []eng.Signal

	for _, id := range ctx.Universe() {
		if inFlight[id] {
			continue // 上一单还没成交，先不叠加
		}
		bar, ok := ctx.Bar(id)
		if !ok || bar.Suspended() || bar.Close <= 0 {
			continue
		}
		base, seen := s.anchor[id]
		if !seen {
			// 第一次见到：定基准，本步不动作。
			// **基准价必须用不复权价** —— 网格是对着真实价位挂的，
			// 而 ctx.Bar 给的就是原始价（复权价只喂指标）
			s.anchor[id] = bar.Close
			continue
		}

		want := s.targetLevel(base, bar.Close)
		have := s.level[id]
		if want == have {
			continue
		}
		s.level[id] = want

		switch {
		case want > have:
			// 又跌了几格 —— 加仓。Strength 表达「这次要加多少份」，
			// 由 strength_weighted 之类的 Sizer 折算成金额
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: trading.SideBuy,
				Strength: float64(want-have) * s.perStep,
				Tag:      fmt.Sprintf("grid_buy_L%d", want),
			})
		case want == 0:
			// 涨回基准之上 —— 清仓
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalExit, Side: trading.SideSell,
				Tag: "grid_exit",
			})
		default:
			// 涨回若干格 —— 减仓。**减仓用 Exit 的定量语义表达不了**
			// （Exit 是全平），所以这里退回按比例给 Strength，
			// 由 Sizer 决定卖多少
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: trading.SideSell,
				Strength: float64(have-want) * s.perStep,
				Tag:      fmt.Sprintf("grid_sell_L%d", want),
			})
		}
	}
	return sigs, nil
}

// targetLevel 由基准价与现价算出应处的档位。
//
// 用**乘法比较**而不是先算跌幅再比：定点整数下先除会丢精度，
// 而档位判断错一格就是一次多余的交易。
func (s *Grid) targetLevel(base, price int64) int {
	if base <= 0 || price >= base {
		return 0
	}
	for n := s.levels; n >= 1; n-- {
		// price <= base × (1 - n×step)
		thr := base * (1_000_000 - int64(n)*s.stepPPM) / 1_000_000
		if price <= thr {
			return n
		}
	}
	return 0
}

// ---- 跨步状态 ----

type gridState struct {
	Anchor map[string]int64 `json:"anchor"`
	Level  map[string]int   `json:"level"`
}

func (s *Grid) SnapshotState() ([]byte, error) {
	st := gridState{
		Anchor: make(map[string]int64, len(s.anchor)),
		Level:  make(map[string]int, len(s.level)),
	}
	for id, v := range s.anchor {
		st.Anchor[fmt.Sprint(int32(id))] = v
	}
	for id, v := range s.level {
		if v != 0 { // 0 是零值，省一半体积
			st.Level[fmt.Sprint(int32(id))] = v
		}
	}
	return json.Marshal(st)
}

func (s *Grid) RestoreState(b []byte) error {
	var st gridState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	s.anchor = make(map[mktdata.InstrumentID]int64, len(st.Anchor))
	s.level = make(map[mktdata.InstrumentID]int, len(st.Level))
	parse := func(k string) (mktdata.InstrumentID, error) {
		var id int32
		if _, err := fmt.Sscan(k, &id); err != nil {
			return 0, fmt.Errorf("快照中的标的 ID %q 无法解析: %w", k, err)
		}
		return mktdata.InstrumentID(id), nil
	}
	for k, v := range st.Anchor {
		id, err := parse(k)
		if err != nil {
			return err
		}
		s.anchor[id] = v
	}
	for k, v := range st.Level {
		id, err := parse(k)
		if err != nil {
			return err
		}
		s.level[id] = v
	}
	return nil
}

var (
	_ eng.Strategy         = (*Grid)(nil)
	_ eng.StatefulStrategy = (*Grid)(nil)
)
