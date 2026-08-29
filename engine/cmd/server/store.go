package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// Store 是服务端的只读内存数据仓。
//
// **全量常驻，启动时一次载入。** 这不是偷懒：加载器无论过不过滤都要把
// 22 个年份分区整个读一遍（实测取单只标的同样要 ~30 秒），所以「按需加载」
// 在这里比全量还慢。全量 1.25 GB / 30 秒，之后每个请求都是内存读。
//
// 载入后不再变更，故所有读取无需加锁。数据更新的方式是重启服务 ——
// 这与 ETL 的批式产出节奏一致。
type Store struct {
	Uni  *mktdata.Universe
	Cal  *mktdata.Calendar
	Adj  *mktdata.Adjuster
	Corp *mktdata.CorpActions
	Col  *mktdata.Columns
	Fee  trading.Fee
	Mkt  *trading.AShareMarket

	// InstStats 是逐标的的派生统计，启动时算好。
	// 「这只标的到底有没有行情、覆盖到哪天」是核对时第一个要问的问题。
	InstStats map[mktdata.InstrumentID]InstStat

	DataRoot string
	// ConfigDir 回测配置所在目录。也是从浏览器传来的配置里相对路径的解析基准
	ConfigDir string
	LoadedAt  time.Time

	// 最近一次回测用的标的子集。调参时会反复跑同一个池子，
	// 缓存一份省掉重复拷贝（大池子一次 550 MB）
	uniMu    sync.Mutex
	uniKey   string
	uniCols  *mktdata.Columns
	LoadMS   int64
	BarStats mktdata.LoadStats
}

// InstStat 是单个标的的数据覆盖情况。
type InstStat struct {
	Bars         int   `json:"bars"`
	FirstDay     int32 `json:"firstDay"`
	LastDay      int32 `json:"lastDay"`
	FactorEvents int   `json:"factorEvents"`
	CorpActions  int   `json:"corpActions"`
}

// LoadStore 载入全部数据。progress 用于把进度打到控制台 ——
// 30 秒的静默启动会让人以为卡死了。
func LoadStore(dataRoot, feePath string, progress func(string, ...any)) (*Store, error) {
	s := &Store{DataRoot: dataRoot}
	meta := filepath.Join(dataRoot, "meta")

	must := func(name string, fn func() error) error {
		t0 := time.Now()
		progress("  载入 %-18s ", name)
		if err := fn(); err != nil {
			progress("失败\n")
			return err
		}
		progress("%v\n", time.Since(t0).Round(time.Millisecond))
		return nil
	}

	var err error
	if err = must("instruments", func() error {
		s.Uni, err = mktdata.LoadUniverse(filepath.Join(meta, "instruments.parquet"))
		return err
	}); err != nil {
		return nil, err
	}
	if err = must("calendar", func() error {
		s.Cal, err = mktdata.LoadCalendar(filepath.Join(meta, "calendar.parquet"))
		return err
	}); err != nil {
		return nil, err
	}
	if err = must("adj_factor", func() error {
		s.Adj, err = mktdata.LoadAdjuster(filepath.Join(meta, "adj_factor.parquet"))
		return err
	}); err != nil {
		return nil, err
	}
	if err = must("corporate_action", func() error {
		s.Corp, err = mktdata.LoadCorpActions(filepath.Join(meta, "corporate_action.parquet"))
		return err
	}); err != nil {
		return nil, err
	}
	if err = must("fee", func() error {
		f, e := trading.LoadFee(feePath)
		s.Fee = f
		return e
	}); err != nil {
		return nil, err
	}

	progress("  载入 %-18s ", "bar（全量）")
	t0 := time.Now()
	col, st, err := mktdata.Load(mktdata.LoadOptions{
		Root: filepath.Join(dataRoot, "bar", "market=ashare", "freq=1d"),
	})
	if err != nil {
		progress("失败\n")
		return nil, err
	}
	s.Col, s.BarStats = col, st
	s.LoadMS = time.Since(t0).Milliseconds()
	progress("%v  %d 行 / %d 只 / %.0f MB\n",
		time.Since(t0).Round(time.Millisecond), st.Rows, st.Instruments,
		float64(col.MemoryBytes())/1024/1024)

	s.Mkt = trading.NewAShareMarket()
	s.LoadedAt = time.Now()

	// 逐标的统计
	s.InstStats = make(map[mktdata.InstrumentID]InstStat, s.Uni.Len())
	for _, in := range s.Uni.All() {
		var stat InstStat
		if first, last, rows, ok := col.DayRange(in.ID); ok {
			stat.Bars, stat.FirstDay, stat.LastDay = rows, first, last
		}
		stat.FactorEvents = len(s.Adj.Factors(in.ID))
		stat.CorpActions = len(s.Corp.ByInstrument(in.ID))
		s.InstStats[in.ID] = stat
	}
	return s, nil
}

// Stat 取标的统计，不存在时返回零值。
func (s *Store) Stat(id mktdata.InstrumentID) InstStat { return s.InstStats[id] }

// DataDays 返回全量行情覆盖的首末交易日。
func (s *Store) DataDays() (first, last int32) {
	if s.Col.NumSteps() == 0 {
		return 0, 0
	}
	return s.Col.StepAt(0).TradingDay, s.Col.StepAt(s.Col.NumSteps() - 1).TradingDay
}

// FileSizeMB 返回 bar 分区的磁盘占用，用于与内存占用对照。
func (s *Store) FileSizeMB() float64 {
	n, err := mktdata.FileSizeBytes(
		filepath.Join(s.DataRoot, "bar", "market=ashare", "freq=1d"))
	if err != nil {
		return 0
	}
	return float64(n) / 1024 / 1024
}

// checkDataRoot 在启动前确认数据目录存在，给出比 parquet 报错更可读的提示。
func checkDataRoot(dataRoot string) error {
	need := []string{
		filepath.Join(dataRoot, "meta", "instruments.parquet"),
		filepath.Join(dataRoot, "meta", "calendar.parquet"),
		filepath.Join(dataRoot, "meta", "adj_factor.parquet"),
		filepath.Join(dataRoot, "meta", "corporate_action.parquet"),
	}
	var missing []string
	for _, p := range need {
		if _, err := os.Stat(p); err != nil {
			missing = append(missing, p)
		}
	}
	bars, _ := filepath.Glob(
		filepath.Join(dataRoot, "bar", "market=ashare", "freq=1d", "year=*", "*.parquet"))
	if len(bars) == 0 {
		missing = append(missing, filepath.Join(dataRoot, "bar/market=ashare/freq=1d/year=*"))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("数据缺失，请先跑 ETL：\n    %s",
			joinLines(missing, "\n    "))
	}
	return nil
}

func joinLines(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
