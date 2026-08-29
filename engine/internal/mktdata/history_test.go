package mktdata

import "testing"

// 手工构造一小份列式数据，避免测试依赖真实 Parquet。
func tinyColumns() *Columns {
	// 两只标的：A 有 5 根（时点 1..5），B 有 3 根（时点 3..5）
	c := &Columns{
		tradingDay:  []int32{20200101, 20200102, 20200103, 20200106, 20200107, 20200103, 20200106, 20200107},
		tsClose:     []int64{1, 2, 3, 4, 5, 3, 4, 5},
		open:        []int64{10, 20, 30, 40, 50, 300, 400, 500},
		high:        []int64{10, 20, 30, 40, 50, 300, 400, 500},
		low:         []int64{10, 20, 30, 40, 50, 300, 400, 500},
		close:       []int64{11, 21, 31, 41, 51, 301, 401, 501},
		volume:      []int64{1, 1, 1, 1, 1, 1, 1, 1},
		amount:      []int64{1, 1, 1, 1, 1, 1, 1, 1},
		preClose:    []int64{10, 11, 21, 31, 41, 300, 301, 401},
		tradeStatus: []int8{1, 1, 1, 1, 1, 1, 1, 1},
		isST:        []int8{0, 0, 0, 0, 0, 0, 0, 0},
		spans:       map[InstrumentID]span{1: {start: 0, n: 5}, 2: {start: 5, n: 3}},
		ids:         []InstrumentID{1, 2},
	}
	if err := c.buildStepIndex(); err != nil {
		panic(err)
	}
	return c
}

// C1 的核心保证：任何时点都取不到未来数据。
//
// 这不是「运行时拒绝」而是「区间里没有」—— History 只携带 [start, cur]，
// 未来的行号落在区间外。本测试把这个边界固定住。
func TestHistoryCannotReachFuture(t *testing.T) {
	c := tinyColumns()
	cur := NewCursor(c)

	// 推进到第 3 个时点（ts=3），此时标的 1 应可见 3 根
	for i := 0; i < 3; i++ {
		if _, _, ok := cur.Advance(); !ok {
			t.Fatal("提前结束")
		}
	}
	h := cur.History(1)
	if got := h.Len(); got != 3 {
		t.Fatalf("可见 bar 数 = %d，期望 3", got)
	}
	if v, _ := h.Close(0); v != 31 {
		t.Fatalf("At(0) 收盘 = %d，期望 31（当前 bar）", v)
	}
	if v, _ := h.Close(2); v != 11 {
		t.Fatalf("At(2) 收盘 = %d，期望 11（最早一根）", v)
	}
	// 越过起点
	if _, ok := h.Close(3); ok {
		t.Error("取到了起点之前的数据")
	}
	// 负偏移即「未来」，必须取不到
	for _, back := range []int{-1, -2, -100} {
		if _, ok := h.Close(back); ok {
			t.Errorf("负偏移 %d 取到了数据 —— 未来泄漏", back)
		}
	}
}

// 上市晚于回测起点的标的，在其上市前必须是 invalid，而不是返回零值 bar。
// 这是 C3（幸存者偏差）在引擎侧的体现：标的不是「一直存在」的。
func TestInstrumentNotYetListed(t *testing.T) {
	c := tinyColumns()
	cur := NewCursor(c)
	cur.Advance() // ts=1，标的 2 尚未上市

	h := cur.History(2)
	if h.Valid() {
		t.Error("标的 2 在上市前不应 Valid")
	}
	if h.Len() != 0 {
		t.Errorf("上市前可见 bar 数 = %d，期望 0", h.Len())
	}
	if _, ok := h.Close(0); ok {
		t.Error("上市前取到了数据")
	}
	if u := cur.Universe(); len(u) != 1 || u[0] != 1 {
		t.Errorf("首个时点的标的池 = %v，期望仅含标的 1", u)
	}
}

// 时点是 ts_close 的并集去重排序 —— 多市场交错与休市都靠它自然成立。
func TestStepIndexIsUnionOfTsClose(t *testing.T) {
	c := tinyColumns()
	if got := c.NumSteps(); got != 5 {
		t.Fatalf("时点数 = %d，期望 5（ts 1..5 去重）", got)
	}
	cur := NewCursor(c)
	want := []int{1, 1, 2, 2, 2} // 各时点的标的数
	for i, n := range want {
		_, upd, ok := cur.Advance()
		if !ok {
			t.Fatalf("第 %d 步提前结束", i)
		}
		if len(upd) != n {
			t.Errorf("第 %d 个时点更新标的数 = %d，期望 %d", i+1, len(upd), n)
		}
	}
	if !cur.Done() {
		t.Error("走完全部时点后 Done 应为 true")
	}
}

// 游标快照往返 —— C6 实盘增量的前提。
func TestCursorSnapshotRoundTrip(t *testing.T) {
	c := tinyColumns()
	a := NewCursor(c)
	for i := 0; i < 3; i++ {
		a.Advance()
	}
	snap := a.Snapshot()

	b := NewCursor(c)
	if err := b.Restore(snap); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	for {
		_, _, okA := a.Advance()
		_, _, okB := b.Advance()
		if okA != okB {
			t.Fatal("恢复后步进长度不一致")
		}
		if !okA {
			break
		}
		for _, id := range []InstrumentID{1, 2} {
			ba, _ := a.Bar(id)
			bb, _ := b.Bar(id)
			if ba != bb {
				t.Fatalf("标的 %d 的 bar 不一致: %+v vs %+v", id, ba, bb)
			}
		}
	}
}
