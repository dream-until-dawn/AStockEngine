package mktdata

import (
	"fmt"
	"sort"
)

// Subset 从已载入的 Columns 中抽出若干标的（并可再按交易日收窄），
// 构造一个新的 Columns。
//
// 存在的理由是加载器的代价结构：`Load` 无论过滤与否都要把 22 个年份分区
// **整个读一遍**，实测取单个标的与取全市场同样是 ~30 秒。服务端要为
// 每次 K 线请求跑一遍引擎，走 Load 显然不可行；先全量载入一次、
// 之后从内存切片则是毫秒级。
//
// 返回的是另一个 *Columns 而不是底层切片，因此 C1 的结构性保证
// （包外拿不到数组、绕不过 History 的时点边界）依旧成立。
//
// fromDay / toDay 为 0 表示该侧不限。
func (c *Columns) Subset(ids []InstrumentID, fromDay, toDay int32) (*Columns, error) {
	dayOK := func(d int32) bool {
		return (fromDay == 0 || d >= fromDay) && (toDay == 0 || d <= toDay)
	}

	// 去重并按 ID 排序 —— 与 Load 保持一致的确定性顺序
	uniq := make(map[InstrumentID]bool, len(ids))
	picked := make([]InstrumentID, 0, len(ids))
	for _, id := range ids {
		if uniq[id] {
			continue
		}
		if _, ok := c.spans[id]; !ok {
			continue // 该标的不在已载入范围内，静默跳过
		}
		uniq[id] = true
		picked = append(picked, id)
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i] < picked[j] })

	total := 0
	counts := make(map[InstrumentID]int32, len(picked))
	for _, id := range picked {
		sp := c.spans[id]
		var n int32
		for r := sp.start; r < sp.start+sp.n; r++ {
			if dayOK(c.tradingDay[r]) {
				n++
			}
		}
		counts[id] = n
		total += int(n)
	}
	if total == 0 {
		return nil, fmt.Errorf("所选标的在该区间内无数据")
	}

	out := &Columns{
		tradingDay:  make([]int32, total),
		tsClose:     make([]int64, total),
		open:        make([]int64, total),
		high:        make([]int64, total),
		low:         make([]int64, total),
		close:       make([]int64, total),
		volume:      make([]int64, total),
		amount:      make([]int64, total),
		preClose:    make([]int64, total),
		tradeStatus: make([]int8, total),
		isST:        make([]int8, total),
		spans:       make(map[InstrumentID]span, len(picked)),
		ids:         make([]InstrumentID, 0, len(picked)),
	}

	var w int32
	for _, id := range picked {
		if counts[id] == 0 {
			continue // 该标的整段被日期过滤掉，不进 ids
		}
		out.spans[id] = span{start: w, n: counts[id]}
		out.ids = append(out.ids, id)
		sp := c.spans[id]
		for r := sp.start; r < sp.start+sp.n; r++ {
			if !dayOK(c.tradingDay[r]) {
				continue
			}
			out.tradingDay[w] = c.tradingDay[r]
			out.tsClose[w] = c.tsClose[r]
			out.open[w] = c.open[r]
			out.high[w] = c.high[r]
			out.low[w] = c.low[r]
			out.close[w] = c.close[r]
			out.volume[w] = c.volume[r]
			out.amount[w] = c.amount[r]
			out.preClose[w] = c.preClose[r]
			out.tradeStatus[w] = c.tradeStatus[r]
			out.isST[w] = c.isST[r]
			w++
		}
	}
	if err := out.buildStepIndex(); err != nil {
		return nil, err
	}
	return out, nil
}

// DayRange 返回某标的已载入数据的首末交易日。ok 为 false 表示无该标的。
func (c *Columns) DayRange(id InstrumentID) (first, last int32, rows int, ok bool) {
	sp, ok := c.spans[id]
	if !ok || sp.n == 0 {
		return 0, 0, 0, false
	}
	// 行按 ts_close 升序，首末即区间端点
	return c.tradingDay[sp.start], c.tradingDay[sp.start+sp.n-1], int(sp.n), true
}
