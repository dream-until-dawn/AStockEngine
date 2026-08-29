package engine

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/indicator"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// Config 是一次运行的装配配置。
type Config struct {
	Params Params
	// IndicatorAdjMode 指标喂入的复权方式。默认后复权 —— 序列必须连续，
	// 否则除权日会产生假信号（设计 6.1）。
	IndicatorAdjMode mktdata.AdjMode
}

// Engine 是回测的核心状态机。
//
// **它不含任何内部 for 循环**（C4）：每调用一次 Step 前进一个事件时点。
// 单步调试、批量海选、实盘增量三种模式因此可以共用同一个核心，
// 只需换外层的 Driver 与 Recorder。
type Engine struct {
	col      *mktdata.Columns
	cur      *mktdata.Cursor
	adj      *mktdata.Adjuster
	strategy Strategy
	cfg      Config

	// 指标由引擎持有（评审决议 2）：indicators[key][instrument]
	factories map[string]IndicatorFactory
	keys      []string // 稳定顺序，保证快照的确定性
	indicators map[string]map[mktdata.InstrumentID]indicator.Indicator

	steps int
	ctx   *stepCtx // 复用同一实例，避免每步分配
}

// New 装配一个引擎。adj 可为 nil，此时不做复权。
func New(col *mktdata.Columns, adj *mktdata.Adjuster, s Strategy, cfg Config) (*Engine, error) {
	if col == nil || s == nil {
		return nil, fmt.Errorf("列式数据与策略均不可为空")
	}
	e := &Engine{
		col: col, cur: mktdata.NewCursor(col), adj: adj,
		strategy: s, cfg: cfg,
		factories:  make(map[string]IndicatorFactory),
		indicators: make(map[string]map[mktdata.InstrumentID]indicator.Indicator),
	}
	if e.cfg.Params == nil {
		e.cfg.Params = Params{}
	}
	e.ctx = &stepCtx{e: e}
	if err := s.Init(e); err != nil {
		return nil, fmt.Errorf("策略 %s 初始化失败: %w", s.Name(), err)
	}
	return e, nil
}

// ---- InitContext ----

func (e *Engine) Params() Params { return e.cfg.Params }

func (e *Engine) Use(key string, f IndicatorFactory) {
	if _, dup := e.factories[key]; dup {
		return // 重复声明取首次，避免策略里无意覆盖
	}
	e.factories[key] = f
	e.keys = append(e.keys, key)
	e.indicators[key] = make(map[mktdata.InstrumentID]indicator.Indicator, 1024)
}

func (e *Engine) Universe() []mktdata.InstrumentID { return e.col.Instruments() }

// ---- 状态机 ----

// Done 报告是否已走完全部时点。
func (e *Engine) Done() bool { return e.cur.Done() }

// Steps 返回已推进的步数。
func (e *Engine) Steps() int { return e.steps }

// Step 前进一个事件时点：更新指标 → 调用策略。
//
// 顺序不可颠倒：策略在 OnBar 里读到的指标必须**已包含当前 bar**，
// 否则策略看到的是滞后一根的指标值 —— 那是另一种未来函数的镜像错误
// （不是看到未来，而是本该可见的当下没看到），同样会让回测失真。
func (e *Engine) Step() (mktdata.TimePoint, error) {
	tp, updated, ok := e.cur.Advance()
	if !ok {
		return mktdata.TimePoint{}, fmt.Errorf("已到末尾")
	}
	e.steps++

	for _, id := range updated {
		bar, ok := e.cur.Bar(id)
		if !ok {
			continue
		}
		// 指标喂**后复权**价：序列连续才不会在除权日产生假信号
		fed := bar
		if e.adj != nil && e.cfg.IndicatorAdjMode != mktdata.AdjNone {
			fed = e.adj.AdjustBar(id, bar, e.cfg.IndicatorAdjMode)
		}
		for _, key := range e.keys {
			m := e.indicators[key]
			ind, exists := m[id]
			if !exists {
				ind = e.factories[key]()
				m[id] = ind
			}
			ind.Update(fed)
		}
	}

	e.ctx.time = tp
	e.ctx.universe = updated
	if err := e.strategy.OnBar(e.ctx); err != nil {
		return tp, fmt.Errorf("策略在 %d 报错: %w", tp.TradingDay, err)
	}
	return tp, nil
}

// RunAll 是便利方法：一直步进到结束。
//
// **注意它只是 Step 的外壳**，核心仍是状态机 —— 单步调试与海选都不走这里。
func (e *Engine) RunAll() error {
	for !e.Done() {
		if _, err := e.Step(); err != nil {
			return err
		}
	}
	return nil
}

// ---- 快照（C6：实盘每天从快照恢复，而非重算多年）----

type snapshot struct {
	Steps      int                                     `json:"steps"`
	Cursor     mktdata.CursorState                     `json:"cursor"`
	Indicators map[string]map[string]indicator.State   `json:"indicators"`
}

// Snapshot 导出引擎状态。
func (e *Engine) Snapshot() ([]byte, error) {
	s := snapshot{
		Steps:      e.steps,
		Cursor:     e.cur.Snapshot(),
		Indicators: make(map[string]map[string]indicator.State, len(e.keys)),
	}
	for _, key := range e.keys {
		m := make(map[string]indicator.State, len(e.indicators[key]))
		for id, ind := range e.indicators[key] {
			m[fmt.Sprint(int32(id))] = ind.Snapshot()
		}
		s.Indicators[key] = m
	}
	return json.Marshal(s)
}

// Restore 从快照恢复。
//
// 快照必须来自**同一份数据集**：Cursor 存的是全局行号，数据变了行号即失效。
// 调用方需先校验 data_version（SCHEMA.md 6 的 _manifest.json）。
func (e *Engine) Restore(data []byte) error {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("解析快照失败: %w", err)
	}
	if err := e.cur.Restore(s.Cursor); err != nil {
		return err
	}
	for _, key := range e.keys {
		saved, ok := s.Indicators[key]
		if !ok {
			return fmt.Errorf("快照缺少指标 %q —— 策略声明的指标与快照不一致", key)
		}
		m := make(map[mktdata.InstrumentID]indicator.Indicator, len(saved))
		for idStr, st := range saved {
			var raw int32
			if _, err := fmt.Sscan(idStr, &raw); err != nil {
				return fmt.Errorf("快照标的 ID %q 无法解析: %w", idStr, err)
			}
			ind := e.factories[key]()
			if err := ind.Restore(st); err != nil {
				return fmt.Errorf("恢复指标 %s/%s 失败: %w", key, idStr, err)
			}
			m[mktdata.InstrumentID(raw)] = ind
		}
		e.indicators[key] = m
	}
	e.steps = s.Steps
	return nil
}

// ---- StepContext 实现 ----

type stepCtx struct {
	e        *Engine
	time     mktdata.TimePoint
	universe []mktdata.InstrumentID
}

func (c *stepCtx) Time() mktdata.TimePoint                 { return c.time }
func (c *stepCtx) Universe() []mktdata.InstrumentID        { return c.universe }
func (c *stepCtx) History(id mktdata.InstrumentID) mktdata.History { return c.e.cur.History(id) }
func (c *stepCtx) Bar(id mktdata.InstrumentID) (mktdata.Bar, bool) { return c.e.cur.Bar(id) }

func (c *stepCtx) Indicator(id mktdata.InstrumentID, key string) (indicator.Indicator, bool) {
	m, ok := c.e.indicators[key]
	if !ok {
		return nil, false
	}
	ind, ok := m[id]
	return ind, ok
}

// AdjClose 返回复权后的收盘价。
//
// **拒绝 AdjQFQ**：前复权锚定末日，用于决策即构成未来函数（C1）
// 且不可复现（C5）。展示层若需要前复权，应走独立接口，不经由 StepContext。
func (c *stepCtx) AdjClose(id mktdata.InstrumentID, back int, mode mktdata.AdjMode) (int64, bool) {
	if mode == mktdata.AdjQFQ {
		return 0, false
	}
	h := c.e.cur.History(id)
	raw, ok := h.Close(back)
	if !ok {
		return 0, false
	}
	if mode == mktdata.AdjNone || c.e.adj == nil {
		return raw, true
	}
	day, _ := h.TradingDay(back)
	return c.e.adj.Adjust(id, day, raw, mode), true
}
