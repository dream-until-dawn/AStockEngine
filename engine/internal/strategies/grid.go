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
	// level 当前档位：0 表示空仓，n 表示已穿过第 n 格
	level map[mktdata.InstrumentID]int
	// short 做空方向的网格：**价格每涨一格开一份空**，跌回来平掉。
	// 只在允许做空的市场有意义
	short bool
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
			Desc: "每格幅度（%）：做多是跌幅，做空是涨幅"},
		// 用 bool 而不是 "long"/"short" 字符串：策略经
		// InitContext.Params() 取参，而那是 map[string]float64 ——
		// 字符串到不了这里（规则树能用字符串是因为它走结构化配置那条路）
		{Name: "short", Kind: eng.ParamBool, Default: 0, Min: 0, Max: 1, Step: 1,
			Desc: "做空网格：越涨越空、跌回来平。" +
				"关=越跌越买、涨回来平。做空只在允许做空的市场可用（如加密永续）"},
	}
}

// NeedsShort 做空方向的网格只能配在允许做空的市场上。
//
// 不拦的话是**静默失效**：开空信号会被 Sizer 当成减仓，
// 而手上没有多头可减，订单被丢掉 —— 一笔成交都不会有。
func (s *Grid) NeedsShort() bool { return s.short }

func (s *Grid) Init(ic eng.InitContext) error {
	p := ic.Params()
	s.levels = p.Int("levels", 5)
	if s.levels < 1 {
		s.levels = 1
	}
	s.short = p.Bool("short", false)
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

		if want > have {
			// 又穿了几格 —— 加仓。Strength 表达「这次要加多少份」，
			// 由 strength_weighted 之类的 Sizer 折算成金额
			s.level[id] = want
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalEnter, Side: s.openSide(),
				Strength: float64(want-have) * s.perStep,
				Tag:      fmt.Sprintf("grid_open_L%d", want),
			})
			continue
		}

		// 退回来了 —— **全平，并把档位归零**。
		//
		// 「减仓 N 格」这件事信号模型表达不了：Exit 是全平，
		// 而 `Enter + 反向` 在单向市场里会被 dispatch 当成清仓、
		// 在双向市场里会被当成反向开仓 —— 两个都不是「减一点」。
		//
		// 从前这里发 `Enter + 卖 + Strength` 并把档位记成 want，
		// 结果是**仓位已经全平、档位却还记着 2 格**：
		// 策略以为自己还持有，于是价格再跌一格也不加仓，
		// 要跌到第 3 格才动。仓位与档位对不上，而且不报错。
		//
		// 现在退回任意格都全平、档位归零 —— 行为与实际持仓一致。
		s.level[id] = 0
		sigs = append(sigs, eng.Signal{
			Instrument: id, Kind: eng.SignalExit, Side: s.closeSide(),
			Tag: fmt.Sprintf("grid_exit_L%d", want),
		})
	}
	return sigs, nil
}

// openSide / closeSide 开平方向。做空网格整个反过来。
func (s *Grid) openSide() trading.Side {
	if s.short {
		return trading.SideSell
	}
	return trading.SideBuy
}

func (s *Grid) closeSide() trading.Side {
	if s.short {
		return trading.SideBuy
	}
	return trading.SideSell
}

// targetLevel 由基准价与现价算出应处的档位。
//
// 用**乘法比较**而不是先算涨跌幅再比：定点整数下先除会丢精度，
// 而档位判断错一格就是一次多余的交易。
func (s *Grid) targetLevel(base, price int64) int {
	if base <= 0 {
		return 0
	}
	if s.short {
		// 做空：价格**涨**过第 n 格才开空
		if price <= base {
			return 0
		}
		for n := s.levels; n >= 1; n-- {
			// price >= base × (1 + n×step)
			thr := base * (1_000_000 + int64(n)*s.stepPPM) / 1_000_000
			if price >= thr {
				return n
			}
		}
		return 0
	}
	if price >= base {
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
