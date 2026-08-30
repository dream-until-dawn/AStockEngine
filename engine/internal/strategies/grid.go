package strategies

import (
	"encoding/json"
	"fmt"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Grid 网格策略：维护一组价位，价格穿过就调仓。三种模式。
//
// # 做多网格（单边 5 格）
//
//	       价格          档位   多头持仓
//	+5 格  base×1.25      +5    0/10   ← 空仓，以此价重建整张网
//	 0 线  base            0    5/10   ← 建仓时就在这里，持一半
//	−5 格  base×0.75      −5   10/10   ← 满仓
//	−7 格  base×0.65     止损    0/10   ← 全平，以此价重建
//
// **资金分成 2×levels 份，0 线持一半。** 分一半而不是全仓建仓，
// 是为了在跌到底之前每一格都有份可加、涨上去之前每一格都有份可减 ——
// 否则「网格」就只是一次买入加一次卖出。
//
// 做空网格是它的镜像：越涨越空、跌回来平。
//
// # 双向网格（long + short 同时打开）
//
//	       价格          档位   持仓
//	+7 格                止损    ——    ← 全平，以此价重建
//	+5 格  base×1.25      +5    满仓空
//	 0 线  base            0    **空仓**
//	−5 格  base×0.75      −5    满仓多
//	−7 格                止损    ——    ← 全平，以此价重建
//
// **资金每边分 levels 份，0 线一分钱都不占。** 它不需要「0 线持一半」
// 那个安排：往下有多头可加、往上有空头可开，两个方向本来就都有子弹。
// 两端都是满仓（一端满多、一端满空），所以它**没有空仓端可重建** ——
// 两边各有一条止损线兜底。
//
// 代价是穿越 0 线要平掉一条腿再开另一条，那是两笔手续费。
// 只在允许做空的市场可用（如加密永续）。
//
// # 几何与策略是分开的
//
// `displacement` 只答「站在第几条线上」（跌为负、涨为正，与模式无关），
// `legTargets` 只答「这条线该持多少」。**上一个 bug 正是这两件事
// 混在一个函数里造成的**：几何按模式翻符号，策略按另一套符号算权重，
// 两边各自的单元测试都是绿的，而合起来整张网跑成了追涨杀跌。
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
	// long / short 两条腿各自开不开。三种合法组合：
	//
	//	long        越跌越买、涨回来卖。资金分 2L 份，0 线持一半
	//	short       越涨越空、跌回来平。同上，镜像
	//	long+short  **双向网格**：0 线空仓，跌了做多、涨了做空。
	//	            资金每边分 L 份，任一时刻只有一边有仓
	//
	// 拆成两个 bool 而不是一个 mode 枚举：策略拿到的参数是
	// map[string]float64（字符串到不了这里），而 0/1/2 那种数字模式
	// 在配置里没法读。两个开关在 JSON 里是 "long": 1, "short": 1，
	// 一眼就知道是什么
	long, short bool
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
		{Name: "long", Kind: eng.ParamBool, Default: 1, Min: 0, Max: 1, Step: 1,
			Desc: "做多腿：越跌越买、涨回来卖"},
		{Name: "short", Kind: eng.ParamBool, Default: 0, Min: 0, Max: 1, Step: 1,
			Desc: "做空腿：越涨越空、跌回来平。只在允许做空的市场可用（如加密永续）。" +
				"与 long 同时打开就是**双向网格**：0 线空仓，跌了做多、涨了做空"},
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
	s.long = p.Bool("long", true)
	s.short = p.Bool("short", false)
	if !s.long && !s.short {
		return fmt.Errorf("long 与 short 至少要开一个 —— 两条腿都关掉的网格" +
			"不会发出任何信号，而那看上去和「策略没机会出手」一模一样")
	}
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
			// 第一次见到：以此为 0 线建网。
			// **基准价用不复权价** —— 网格是对着真实价位挂的，
			// 而 ctx.Bar 给的就是原始价（复权价只喂指标）
			s.anchor[id], s.level[id] = bar.Close, 0
			sigs = append(sigs, s.targets(id, 0, "grid_open")...)
			continue
		}

		// 越过止损线：全平，并以当前价重建整张网。
		//
		// **止损线在满仓格之外**（再远 stopLevels 格）：满仓格是仓位而不是
		// 离场位。在满仓那一格就止损的话，网格从来没有机会「跌到底再涨回来」
		// —— 那正是它赚钱的方式
		if s.stopped(base, bar.Close) {
			// **把基准价删掉而不是就地改成现价**：删掉之后下一根 bar 会走
			// 「第一次见到」那条分支，以那根的收盘价重新定 0 线并重建 ——
			// 这才是「全平重新建网」。
			//
			// 就地改成现价的话档位也是 0、目标也是 0，下一根 want == level
			// 直接 continue —— 仓位会一直空着，直到价格自己走出一格为止，
			// 网**再也没被建起来过**
			delete(s.anchor, id)
			delete(s.level, id)
			sigs = append(sigs, s.exits(id, "grid_stop")...)
			continue
		}

		d := s.displacement(base, bar.Close)
		if d == s.level[id] {
			continue
		}
		s.level[id] = d

		// 走到空仓的那一端：**以此为新的 0 线重建**。
		// 不重建的话这张网就永远挂在一个远离现价的位置上，再也不动。
		//
		// 双向网格两端都是满仓（一端满多、一端满空），没有空仓端，
		// 所以它不重建 —— 两端各自由止损线兜底
		if s.rebases(d) {
			s.anchor[id], s.level[id] = bar.Close, 0
			sigs = append(sigs, s.targets(id, 0, "grid_rebase")...)
			continue
		}
		sigs = append(sigs, s.targets(id, d, fmt.Sprintf("grid_L%+d", d))...)
	}
	return sigs, nil
}

// exits 平掉本模式用到的每一条腿。
func (s *Grid) exits(id mktdata.InstrumentID, tag string) []eng.Signal {
	var out []eng.Signal
	if s.long {
		out = append(out, eng.Signal{
			Instrument: id, Kind: eng.SignalExit, Side: trading.SideSell, Tag: tag})
	}
	if s.short {
		out = append(out, eng.Signal{
			Instrument: id, Kind: eng.SignalExit, Side: trading.SideBuy, Tag: tag})
	}
	return out
}

// rebases 这个档位是不是「空仓端」，要以现价重建整张网。
func (s *Grid) rebases(d int) bool {
	if s.long && s.short {
		return false // 双向两端都是满仓，没有空仓端
	}
	if s.short {
		return d <= -s.levels // 做空网格在下方空仓
	}
	return d >= s.levels // 做多网格在上方空仓
}

// legTargets 由档位算出多空两条腿各自的目标仓位比例。
//
// # 单向（只开一条腿）：资金分 2L 份，0 线持一半
//
//	d = −L  →  满仓（2L/2L）      d = 0  →  半仓（L/2L）
//	d = +L  →  空仓（0/2L）
//
// 分一半而不是全仓建仓，是为了**在跌到底之前每一格都有份可加、
// 涨上去之前每一格都有份可减** —— 否则「网格」就只是一次买入。
// 做空腿整个镜像。
//
// # 双向：资金**每边分 L 份**，0 线空仓
//
//	d = −L  →  满仓多      d = 0  →  不持仓      d = +L  →  满仓空
//
// 双向不需要「0 线持一半」那个安排：往下有多头可加、往上有空头可开，
// 两个方向本来就都有子弹。它换来的是**0 线附近不占用任何资金**，
// 代价是穿越 0 线时要平掉一条腿再开另一条 —— 那是两笔手续费。
func (s *Grid) legTargets(d int) (longW, shortW float64) {
	L := float64(s.levels)
	if s.long && s.short {
		if d < 0 {
			longW = float64(-d) / L
		} else if d > 0 {
			shortW = float64(d) / L
		}
		return clamp01(longW), clamp01(shortW)
	}
	if s.short {
		return 0, clamp01(float64(s.levels+d) / (2 * L))
	}
	return clamp01(float64(s.levels-d) / (2 * L)), 0
}

// targets 把档位变成信号：本模式用到的每条腿各一条 SignalTarget。
//
// **两条腿要分别发。** Sizer 的 targetOrder 一次只看一条腿
// （按 side 取 Exposure.Long 或 .Short），发一条信号只能把一边调到位。
// 双向网格穿越 0 线时要「平掉多头 + 开出空头」，那正是两条信号的事。
func (s *Grid) targets(id mktdata.InstrumentID, d int, tag string) []eng.Signal {
	lw, sw := s.legTargets(d)
	var out []eng.Signal
	if s.long {
		out = append(out, eng.Signal{
			Instrument: id, Kind: eng.SignalTarget, Side: trading.SideBuy,
			Weight: lw, Tag: tag})
	}
	if s.short {
		out = append(out, eng.Signal{
			Instrument: id, Kind: eng.SignalTarget, Side: trading.SideSell,
			Weight: sw, Tag: tag})
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// stopped 判断是否越过了止损线。
//
// 止损线在**满仓格之外**再 stopLevels 格。stopLevels ≤ 0 表示不设止损。
// 单向只有一条（做多在下方、做空在上方）；**双向两边各一条** ——
// 双向的两端都是满仓，哪边穿出去都是同一件事。
func (s *Grid) stopped(base, price int64) bool {
	if s.stopLevels <= 0 || base <= 0 {
		return false
	}
	n := int64(s.levels + s.stopLevels)
	down := price <= base*(1_000_000-n*s.stepPPM)/1_000_000
	up := price >= base*(1_000_000+n*s.stepPPM)/1_000_000
	switch {
	case s.long && s.short:
		return down || up
	case s.short:
		return up
	default:
		return down
	}
}

// displacement 现价相对基准价跨过了几格。**跌为负、涨为正，与模式无关。**
//
// # 为什么它不认识模式
//
// 它只回答几何问题：现在站在第几条线上。「这条线该持多少仓」是
// legTargets 的事。**上一个 bug 正是这两件事混在一个函数里造成的** ——
// 那时它按模式翻符号，做多返回 +3 表示跌了 3 格，而算权重的那半边
// 认为 +3 是「涨上去该减仓」，整张网跑成了越跌卖得越多的追涨杀跌。
// 两个函数各自都有单元测试、各自都是绿的，因为它们用的是**相反**的
// 符号约定，而没有任何一个测试同时看见两边。
//
// 拆开之后这类错误无处可藏：几何只有一种符号，策略只读它。
//
// 用**乘法比较**而不是先算涨跌幅再比：定点整数下先除会丢精度，
// 而档位判断错一格就是一次多余的交易。
func (s *Grid) displacement(base, price int64) int {
	if base <= 0 {
		return 0
	}
	if price > base {
		for n := s.levels; n >= 1; n-- {
			// price >= base × (1 + n×step)
			if price >= base*(1_000_000+int64(n)*s.stepPPM)/1_000_000 {
				return n
			}
		}
		return 0
	}
	if price < base {
		for n := s.levels; n >= 1; n-- {
			// price <= base × (1 − n×step)
			if price <= base*(1_000_000-int64(n)*s.stepPPM)/1_000_000 {
				return -n
			}
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
