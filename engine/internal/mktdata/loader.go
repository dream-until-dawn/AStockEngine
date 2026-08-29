package mktdata

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
)

// barRow 对应 SCHEMA.md 1 的 bar 表。
// 未声明 ts_open 与 turn —— parquet-go 会忽略文件中多出的列
// （已由 engine/cmd/subsetread 实测验证）。
type barRow struct {
	InstrumentID int32 `parquet:"instrument_id"`
	TsClose      int64 `parquet:"ts_close"`
	TradingDay   int32 `parquet:"trading_day"`
	Open         int64 `parquet:"open"`
	High         int64 `parquet:"high"`
	Low          int64 `parquet:"low"`
	Close        int64 `parquet:"close"`
	Volume       int64 `parquet:"volume"`
	Amount       int64 `parquet:"amount"`
	PreClose     int64 `parquet:"preclose"`
	TradeStatus  int8  `parquet:"tradestatus"`
	IsST         int8  `parquet:"is_st"`
}

// idRow 只声明标的列。Parquet 是列式的，因此第一遍统计只需读这一列，
// 代价远低于读全表 —— 这正是「两遍扫描」可行的原因。
type idRow struct {
	InstrumentID int32 `parquet:"instrument_id"`
}

// LoadStats 记录一次加载的实测数据，用于设计文档第 7 节的待测项。
type LoadStats struct {
	Files        int
	Rows         int
	Instruments  int
	Steps        int
	CountPass    time.Duration // 第一遍：统计每标的行数
	FillPass     time.Duration // 第二遍：填充列数组
	IndexBuild   time.Duration // 构建时点索引
	Total        time.Duration
	MemoryBytes  int64
}

func (s LoadStats) String() string {
	return fmt.Sprintf(
		"文件 %d  行 %d  标的 %d  时点 %d\n"+
			"  统计 %v  填充 %v  索引 %v  合计 %v\n"+
			"  内存 %.2f MB (%.1f 字节/行)",
		s.Files, s.Rows, s.Instruments, s.Steps,
		s.CountPass.Round(time.Millisecond), s.FillPass.Round(time.Millisecond),
		s.IndexBuild.Round(time.Millisecond), s.Total.Round(time.Millisecond),
		float64(s.MemoryBytes)/1024/1024,
		float64(s.MemoryBytes)/float64(max(s.Rows, 1)))
}

// LoadOptions 控制加载范围。
type LoadOptions struct {
	// Root 是 bar 分区根目录，如 data/bar/market=ashare/freq=1d
	Root string
	// Roots 多个分区根目录，用于把不同市场载入同一份 Columns。
	// 与 Root 同时给出时 Root 排在最前。
	//
	// 多市场共用一条时间轴是**刻意的**：A 股日线的 ts_close 是当日 07:00 UTC，
	// 加密是次日 16:00 UTC，两者自然交错成不同的步 —— 引擎的游标无需分支，
	// 这正是 C9 想要的形态。
	Roots []string
	// Instruments 为空表示全部；否则只加载指定标的
	Instruments []InstrumentID
	// FromDay / ToDay 为 0 表示不限，否则按 trading_day 过滤（YYYYMMDD）
	FromDay, ToDay int32
}

// Load 把 Parquet 分区读入列式内存表示。
//
// 采用两遍扫描：第一遍只读 instrument_id 列统计每标的行数以确定偏移，
// 第二遍按偏移直接写入最终位置。这样全程只有一份全局数组，
// 无需「先追加再整体排序」——后者在 1700 万行上代价高昂且需要双倍内存。
//
// 之所以能这么做，是因为 SCHEMA.md 0.5 保证每个文件内部已按
// (instrument_id, ts_close) 升序：同一标的在各年份文件中的片段按年份顺序
// 拼接后，整体天然有序。
func Load(opt LoadOptions) (*Columns, LoadStats, error) {
	started := time.Now()
	var st LoadStats

	roots := opt.Roots
	if opt.Root != "" {
		roots = append([]string{opt.Root}, roots...)
	}
	if len(roots) == 0 {
		return nil, st, fmt.Errorf("未指定分区根目录")
	}
	var files []string
	for _, r := range roots {
		fs, err := filepath.Glob(filepath.Join(r, "year=*", "*.parquet"))
		if err != nil {
			return nil, st, fmt.Errorf("扫描分区失败: %w", err)
		}
		// 按年份目录名排序 —— 保证同一标的的片段按时间顺序拼接。
		// **逐根排序后拼接，而不是全局排序**：全局排序把根目录名也算进去，
		// 结论碰巧一样（标的不跨市场），但那是巧合而非保证
		sort.Strings(fs)
		files = append(files, fs...)
	}
	if len(files) == 0 {
		return nil, st, fmt.Errorf("在 %s 下未找到分区文件", strings.Join(roots, "、"))
	}
	st.Files = len(files)

	var want map[InstrumentID]bool
	if len(opt.Instruments) > 0 {
		want = make(map[InstrumentID]bool, len(opt.Instruments))
		for _, id := range opt.Instruments {
			want[id] = true
		}
	}
	dayOK := func(d int32) bool {
		return (opt.FromDay == 0 || d >= opt.FromDay) && (opt.ToDay == 0 || d <= opt.ToDay)
	}

	// ---- 第一遍：统计每标的行数 ----
	//
	// 有日期过滤时无法只读 id 列，必须连 trading_day 一起读。
	t0 := time.Now()
	counts := make(map[InstrumentID]int32, 8192)
	total := 0
	needDay := opt.FromDay != 0 || opt.ToDay != 0
	for _, f := range files {
		if needDay {
			rows, err := parquet.ReadFile[struct {
				InstrumentID int32 `parquet:"instrument_id"`
				TradingDay   int32 `parquet:"trading_day"`
			}](f)
			if err != nil {
				return nil, st, fmt.Errorf("统计 %s 失败: %w", f, err)
			}
			for i := range rows {
				id := InstrumentID(rows[i].InstrumentID)
				if want != nil && !want[id] {
					continue
				}
				if !dayOK(rows[i].TradingDay) {
					continue
				}
				counts[id]++
				total++
			}
		} else {
			rows, err := parquet.ReadFile[idRow](f)
			if err != nil {
				return nil, st, fmt.Errorf("统计 %s 失败: %w", f, err)
			}
			for i := range rows {
				id := InstrumentID(rows[i].InstrumentID)
				if want != nil && !want[id] {
					continue
				}
				counts[id]++
				total++
			}
		}
	}
	st.CountPass = time.Since(t0)
	if total == 0 {
		return nil, st, fmt.Errorf("过滤后无数据")
	}

	// ---- 分配并计算偏移 ----
	ids := make([]InstrumentID, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	c := &Columns{
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
		spans:       make(map[InstrumentID]span, len(ids)),
		ids:         ids,
	}
	cursor := make(map[InstrumentID]int32, len(ids))
	var off int32
	for _, id := range ids {
		c.spans[id] = span{start: off, n: counts[id]}
		cursor[id] = off
		off += counts[id]
	}

	// ---- 第二遍：按偏移写入最终位置 ----
	t0 = time.Now()
	for _, f := range files {
		rows, err := parquet.ReadFile[barRow](f)
		if err != nil {
			return nil, st, fmt.Errorf("读取 %s 失败: %w", f, err)
		}
		for i := range rows {
			r := &rows[i]
			id := InstrumentID(r.InstrumentID)
			if want != nil && !want[id] {
				continue
			}
			if needDay && !dayOK(r.TradingDay) {
				continue
			}
			p := cursor[id]
			c.tradingDay[p] = r.TradingDay
			c.tsClose[p] = r.TsClose
			c.open[p] = r.Open
			c.high[p] = r.High
			c.low[p] = r.Low
			c.close[p] = r.Close
			c.volume[p] = r.Volume
			c.amount[p] = r.Amount
			c.preClose[p] = r.PreClose
			c.tradeStatus[p] = r.TradeStatus
			c.isST[p] = r.IsST
			cursor[id] = p + 1
		}
		rows = nil // 及早释放该年份的临时切片，压低峰值内存
	}
	st.FillPass = time.Since(t0)

	// ---- 构建时点索引 ----
	t0 = time.Now()
	if err := c.buildStepIndex(); err != nil {
		return nil, st, err
	}
	st.IndexBuild = time.Since(t0)

	st.Rows = total
	st.Instruments = len(ids)
	st.Steps = len(c.steps)
	st.MemoryBytes = c.MemoryBytes()
	st.Total = time.Since(started)
	return c, st, nil
}

// buildStepIndex 由全部 ts_close 的并集构成事件时点序列，并记录每个时点的行号。
//
// 时点是**去重排序后的 ts_close 并集**，而非交易日历 —— 这使多市场交错、
// 24×7 与休市日在同一套逻辑下自然成立（设计 1.1）。
func (c *Columns) buildStepIndex() error {
	n := len(c.tsClose)
	if n == 0 {
		return fmt.Errorf("无数据可建索引")
	}
	// 收集去重时点。用 map 后排序，简单且一次性。
	seen := make(map[int64]int32, 8192)
	for i := 0; i < n; i++ {
		if _, ok := seen[c.tsClose[i]]; !ok {
			seen[c.tsClose[i]] = c.tradingDay[i]
		}
	}
	tsList := make([]int64, 0, len(seen))
	for ts := range seen {
		tsList = append(tsList, ts)
	}
	sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })

	idx := make(map[int64]int32, len(tsList))
	c.steps = make([]TimePoint, len(tsList))
	for i, ts := range tsList {
		idx[ts] = int32(i)
		c.steps[i] = TimePoint{TsClose: ts, TradingDay: seen[ts]}
	}

	// 两遍：先算每个时点的行数，再一次性分配，避免 append 反复扩容
	cnt := make([]int32, len(tsList))
	for i := 0; i < n; i++ {
		cnt[idx[c.tsClose[i]]]++
	}
	c.stepRows = make([][]int32, len(tsList))
	for i := range c.stepRows {
		c.stepRows[i] = make([]int32, 0, cnt[i])
	}
	for i := 0; i < n; i++ {
		s := idx[c.tsClose[i]]
		c.stepRows[s] = append(c.stepRows[s], int32(i))
	}
	return nil
}

// ReadInstrumentIDs 只读取分区中出现过的标的 ID，用于在完整加载前做规划。
func ReadInstrumentIDs(root string) ([]InstrumentID, error) {
	files, err := filepath.Glob(filepath.Join(root, "year=*", "*.parquet"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("在 %s 下未找到分区文件", root)
	}
	seen := make(map[InstrumentID]bool, 8192)
	for _, f := range files {
		rows, err := parquet.ReadFile[idRow](f)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			seen[InstrumentID(rows[i].InstrumentID)] = true
		}
	}
	out := make([]InstrumentID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// FileSizeBytes 汇总分区文件的磁盘占用，便于与内存占用对比。
func FileSizeBytes(roots ...string) (int64, error) {
	var sum int64
	for _, root := range roots {
		files, err := filepath.Glob(filepath.Join(root, "year=*", "*.parquet"))
		if err != nil {
			return 0, err
		}
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil {
				return 0, err
			}
			sum += fi.Size()
		}
	}
	return sum, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
