package strategies

import (
	"encoding/json"
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Grid 网格策略：0 线之上每涨一格减一份，之下每跌一格加一份。
//
// # 一张网长什么样（单边 5 格）
//
//	       价格          档位   持仓
//	+5 格  base×1.25      +5    0/10   ← 空仓，以此价重建整张网
//	+1 格  base×1.05      +1    4/10
//	 0 线  base            0    5/10   ← 建仓时就在这里，持一半
//	−1 格  base×0.95      −1    6/10
//	−5 格  base×0.75      −5   10/10   ← 满仓
//	−7 格  base×0.65     止损    0/10   ← 全平，以此价重建
//
// **资金分成 2×levels 份，0 线持一半。** 分一半而不是全仓建仓，
// 是为了在跌到底之前每一格都有份可加、涨上去之前每一格都有份可减 ——
// 否则「网格」就只是一次买入加一次卖出。
//
// **止损线在满仓格之下再几格**，不在满仓那一格：−levels 是满仓位而不是
// 离场位，在那里止损的话，网格从来没有机会「跌到底再涨回来」，
// 而那正是它赚钱的方式。
//
// # 靠 SignalTarget 表达「加一格 / 减一格」
//
// 信号模型原本只有「买一份」（Enter）与「全平」（Exit）。
// 「卖掉十分之一」两者都不是。从前这里用 `Enter + 卖 + Strength` 凑，
// 而它在单向市场里被当成清仓（全平）、在双向市场里被当成反向开仓 ——
// 两个都不是「减一点」，于是网格实际上从来没有真正分层减过仓。
//
// 现在每一格都发一条 `SignalTarget{Weight: k/2L}`，由 Sizer 看着
// 当前持仓补差额。加、减、不动三种情况用同一条信号表达。
//
// # 为什么有它
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
	levels  int   // 单边格数
	stepPPM int64 // 每格跌幅（百万分之一）

	// anchor 各标的的基准价（首次见到时的收盘价，定点）。
	// **一旦定下就不再变** —— 让它跟着价格漂移就成了追涨杀跌，不是网格
	anchor map[mktdata.InstrumentID]int64
	// level 当前档位：0 表示空仓，n 表示已穿过第 n 格
	level map[mktdata.InstrumentID]int
	// short 做空方向的网格：**价格每涨一格开一份空**，跌回来平掉。
	// 只在允许做空的市场有意义
	short bool
	// stopLevels 止损线在 −(levels + stopLevels) 格。0 表示不设止损。
	//
	// **必须在 −levels 之下**：−levels 是满仓位，不是离场位 ——
	// 在满仓那一格就止损的话，网格从来没有机会「跌到底再涨回来」，
	// 而那正是它赚钱的方式
	stopLevels int
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
		{Name: "stop_levels", Kind: eng.ParamInt, Default: 2, Min: 0, Max: 50, Step: 1,
			Desc: "止损线在满仓格之下再几格（0=不止损）。" +
				"触发后全平并以当时价格重建整张网"},
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
	s.stopLevels = p.Int("stop_levels", 2)
	stepPct := p.Float("step_pct", 5)
	if stepPct <= 0 {
		return fmt.Errorf("step_pct 必须为正")
	}
	s.stepPPM = int64(stepPct * 10_000)
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
			// 第一次见到：以此为 0 线，并**立刻建半仓**。
			// **基准价用不复权价** —— 网格是对着真实价位挂的，
			// 而 ctx.Bar 给的就是原始价（复权价只喂指标）
			s.anchor[id], s.level[id] = bar.Close, 0
			sigs = append(sigs, s.target(id, 0, "grid_open"))
			continue
		}

		// 跌破止损线：全平，并以当前价重建整张网。
		//
		// **止损线在 −levels 之下**（再往下 stopLevels 格）：
		// −levels 是满仓位，不是离场位。在满仓那一格就止损的话，
		// 网格从来没有机会「跌到底再涨回来」—— 那正是它赚钱的方式
		if s.stopped(base, bar.Close) {
			s.anchor[id], s.level[id] = bar.Close, 0
			sigs = append(sigs, eng.Signal{
				Instrument: id, Kind: eng.SignalExit, Side: s.closeSide(),
				Tag: "grid_stop",
			})
			continue
		}

		want := s.targetLevel(base, bar.Close)
		if want == s.level[id] {
			continue
		}
		s.level[id] = want

		// 涨到 +levels：仓位已经归零，**以此为新的 0 线重建**。
		// 不重建的话这张网就永远挂在一个远低于现价的位置上，再也不动
		if want >= s.levels {
			s.anchor[id], s.level[id] = bar.Close, 0
			sigs = append(sigs, s.target(id, 0, "grid_rebase"))
			continue
		}
		sigs = append(sigs, s.target(id, want, fmt.Sprintf("grid_L%+d", want)))
	}
	return sigs, nil
}

// target 生成一条「持到第 n 格对应比例」的调仓信号。
//
// # 份数怎么算
//
// 单边 levels 格 → 上下共 2×levels 格，资金分成 2×levels 份。
// 0 线持一半，每跌一格加一份，每涨一格减一份：
//
//	n = −levels  →  满仓（2L/2L）
//	n = 0        →  半仓（L/2L）
//	n = +levels  →  空仓（0/2L）
//
// 分一半而不是全仓建仓，是为了**在跌到底之前每一格都有份可加、
// 涨上去之前每一格都有份可减** —— 否则「网格」就只是一次买入。
func (s *Grid) target(id mktdata.InstrumentID, n int, tag string) eng.Signal {
	w := float64(s.levels-n) / float64(2*s.levels)
	if w < 0 {
		w = 0
	}
	if w > 1 {
		w = 1
	}
	return eng.Signal{
		Instrument: id, Kind: eng.SignalTarget, Side: s.openSide(),
		Weight: w, Tag: tag,
	}
}

// stopped 判断是否跌破（做空则是涨破）止损线。
//
// 止损线在 **−(levels + stopLevels)** 格。stopLevels ≤ 0 表示不设止损。
func (s *Grid) stopped(base, price int64) bool {
	if s.stopLevels <= 0 || base <= 0 {
		return false
	}
	n := int64(s.levels + s.stopLevels)
	if s.short {
		return price >= base*(1_000_000+n*s.stepPPM)/1_000_000
	}
	return price <= base*(1_000_000-n*s.stepPPM)/1_000_000
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
