package mktdata

import "fmt"

// Cursor 维护「当前时点」以及每个标的的当前行号，并据此签发 History 视图。
//
// 它是 C4（状态机而非 for 循环）在数据层的对应物：Cursor 不自行推进，
// 只在被调用 Advance 时前进一步，因此单步调试、批量海选、实盘增量
// 三种模式可以共用同一份数据层。
type Cursor struct {
	col *Columns

	step int32   // 当前时点下标；-1 表示尚未开始
	rows []int32 // 每个标的的当前行号；start-1 表示尚未有 bar

	// idIndex 把 InstrumentID 映射到 rows 的下标。
	// 用切片下标而非 map 查询，是因为它落在每步的热路径上。
	idIndex map[InstrumentID]int32
	starts  []int32 // 与 rows 对齐的各标的起始行
	ends    []int32 // 与 rows 对齐的各标的末行（含）
	ids     []InstrumentID

	// updated 复用同一块内存，避免每步分配
	updated []InstrumentID
}

// NewCursor 基于已加载的列式数据创建游标，初始处于「尚未开始」状态。
func NewCursor(c *Columns) *Cursor {
	n := len(c.ids)
	cur := &Cursor{
		col:     c,
		step:    -1,
		rows:    make([]int32, n),
		starts:  make([]int32, n),
		ends:    make([]int32, n),
		ids:     c.ids,
		idIndex: make(map[InstrumentID]int32, n),
		updated: make([]InstrumentID, 0, 512),
	}
	for i, id := range c.ids {
		sp := c.spans[id]
		cur.idIndex[id] = int32(i)
		cur.starts[i] = sp.start
		cur.ends[i] = sp.start + sp.n - 1
		cur.rows[i] = sp.start - 1 // 尚未有 bar
	}
	return cur
}

// Done 报告是否已走完全部时点。
func (cu *Cursor) Done() bool { return int(cu.step) >= len(cu.col.steps)-1 }

// Step 返回当前时点下标；-1 表示尚未开始。
func (cu *Cursor) Step() int32 { return cu.step }

// Now 返回当前时点。尚未开始时返回零值与 false。
func (cu *Cursor) Now() (TimePoint, bool) {
	if cu.step < 0 || int(cu.step) >= len(cu.col.steps) {
		return TimePoint{}, false
	}
	return cu.col.steps[cu.step], true
}

// Advance 前进一个事件时点，返回该时点有新 bar 的标的。
//
// 返回的切片由 Cursor 复用，调用方**不得持有到下一次 Advance 之后**。
// 这是为了让每步零分配 —— 海选场景下每步分配一次会显著抬高 GC 压力。
func (cu *Cursor) Advance() (TimePoint, []InstrumentID, bool) {
	if cu.Done() {
		return TimePoint{}, nil, false
	}
	cu.step++
	tp := cu.col.steps[cu.step]

	cu.updated = cu.updated[:0]
	for _, row := range cu.col.stepRows[cu.step] {
		// 行号已按标的分块，用二分找到所属标的的下标
		i := cu.instrumentIndexOfRow(row)
		if i < 0 {
			continue
		}
		cu.rows[i] = row
		cu.updated = append(cu.updated, cu.ids[i])
	}
	return tp, cu.updated, true
}

// instrumentIndexOfRow 由全局行号反查标的下标。
//
// starts 是升序的，故可二分。相比为每行额外存一个 int32 的标的下标，
// 二分省下 70 MB 内存，代价是每行一次 O(log n)（n≈7200，约 13 次比较）。
func (cu *Cursor) instrumentIndexOfRow(row int32) int32 {
	lo, hi := 0, len(cu.starts)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if row < cu.starts[mid] {
			hi = mid - 1
		} else if row > cu.ends[mid] {
			lo = mid + 1
		} else {
			return int32(mid)
		}
	}
	return -1
}

// History 返回指定标的在当前时点的视图。
//
// 未上市或已退市（当前时点无 bar）的标的返回的视图 Valid() 为 false，
// 而非报错 —— 遍历全市场时这是常态。
func (cu *Cursor) History(id InstrumentID) History {
	i, ok := cu.idIndex[id]
	if !ok {
		return History{}
	}
	return History{col: cu.col, start: cu.starts[i], cur: cu.rows[i]}
}

// Bar 返回指定标的在当前时点的最新 bar。
func (cu *Cursor) Bar(id InstrumentID) (Bar, bool) {
	return cu.History(id).At(0)
}

// Universe 返回当前时点**有 bar 的**标的。
//
// 这直接落实 C3：某日的标的池由 bar 表本身决定 —— 上市前无行、退市后无行、
// 停牌有行且 tradestatus=0，故无需单独的 point-in-time 宇宙表。
func (cu *Cursor) Universe() []InstrumentID {
	if cu.step < 0 {
		return nil
	}
	rows := cu.col.stepRows[cu.step]
	out := make([]InstrumentID, 0, len(rows))
	for _, row := range rows {
		if i := cu.instrumentIndexOfRow(row); i >= 0 {
			out = append(out, cu.ids[i])
		}
	}
	return out
}

// CursorState 是 Cursor 的可序列化状态，服务于 C6 的实盘增量恢复。
type CursorState struct {
	Step int32   `json:"step"`
	Rows []int32 `json:"rows"`
}

// Snapshot 导出游标状态。
func (cu *Cursor) Snapshot() CursorState {
	rows := make([]int32, len(cu.rows))
	copy(rows, cu.rows)
	return CursorState{Step: cu.step, Rows: rows}
}

// Restore 从快照恢复游标状态。
//
// 快照必须来自**同一份数据集**：行号是全局下标，数据变了行号就失去意义。
// 因此调用方需自行校验 data_version（SCHEMA.md 6 的 _manifest.json）。
func (cu *Cursor) Restore(s CursorState) error {
	if len(s.Rows) != len(cu.rows) {
		return fmt.Errorf("快照标的数 %d 与当前 %d 不一致（数据集不同？）",
			len(s.Rows), len(cu.rows))
	}
	if int(s.Step) >= len(cu.col.steps) {
		return fmt.Errorf("快照时点下标 %d 超出范围 %d", s.Step, len(cu.col.steps))
	}
	cu.step = s.Step
	copy(cu.rows, s.Rows)
	return nil
}
