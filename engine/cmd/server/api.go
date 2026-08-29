package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// ---- 通用 ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, a...)})
}

func qInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// qIntSet 解析 `?type=1,2` 形式的多选过滤。返回 nil 表示不过滤。
func qIntSet(r *http.Request, name string) map[int]bool {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return nil
	}
	out := map[int]bool{}
	for _, part := range strings.Split(v, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out[n] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// qTri 解析三态过滤：空 = 不过滤，1 = 必须有，0 = 必须无。
func qTri(r *http.Request, name string) (want bool, active bool) {
	switch strings.TrimSpace(r.URL.Query().Get(name)) {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	}
	return false, false
}

// ---- 枚举字典 ----
//
// 枚举的**取值**是 SCHEMA.md 的一部分（etl/schema.py 与 mktdata 各有一份实现），
// 但**中文标签**只属于展示层，故只在这里出现，不进 mktdata。

type enumItem struct {
	Code  int    `json:"code"`
	Label string `json:"label"`
}

var (
	enumMarket   = []enumItem{{1, "A 股"}, {5, "加密货币"}}
	enumExchange = []enumItem{{1, "上交所"}, {2, "深交所"}, {3, "北交所"}, {10, "OKX"}, {0, "未知"}}
	enumType     = []enumItem{{1, "个股"}, {2, "ETF"}, {10, "永续合约"}, {0, "未知"}}
	enumBoard    = []enumItem{{1, "主板"}, {2, "创业板"}, {3, "科创板"}, {4, "北交所"}, {0, "未知"}}
	enumStatus   = []enumItem{{1, "在市"}, {2, "已退市"}, {0, "未知"}}
	enumCurrency = []enumItem{{1, "CNY"}, {10, "USDT"}, {0, "未知"}}
)

func (s *Store) handleMeta(w http.ResponseWriter, r *http.Request) {
	first, last := s.DataDays()
	writeJSON(w, map[string]any{
		"enums": map[string]any{
			"market": enumMarket, "exchange": enumExchange, "type": enumType,
			"board": enumBoard, "status": enumStatus, "currency": enumCurrency,
			"adj": []map[string]string{
				{"code": "none", "label": "不复权"},
				{"code": "qfq", "label": "前复权"},
				{"code": "hfq", "label": "后复权"},
			},
		},
		// 前端拿到 scale 才能把定点整数还原成人读的数值。
		// 服务端一律传整数：JSON 里传浮点会在这一层就损失精度，
		// 而「数据准不准」正是本工具要回答的问题。
		//
		// ⚠ 这里给的是 **A 股的 scale**。价格/数量的 scale 是**逐标的**的
		// （加密为 1e8），凡是要还原具体标的价格的地方一律用
		// `instrument.priceScale` / K 线响应里的 `engine.priceScale`，
		// 不要用这一组。留着它是因为复权因子、分红比例这类
		// A 股独有的表确实只有一个 scale。
		"scales": map[string]int64{
			"price": mktdata.PriceScale, "amount": mktdata.AmountScale,
			"ratio": mktdata.RatioScale, "factor": mktdata.FactorScale,
		},
		"stats": map[string]any{
			"instruments":    s.Uni.Len(),
			"barRows":        s.BarStats.Rows,
			"barInstruments": s.BarStats.Instruments,
			"steps":          s.Col.NumSteps(),
			"calendarRows":   s.Cal.Len(),
			"tradingDays":    s.Cal.TradingDays(),
			"factorEvents":   s.Adj.TotalFactorEvents(),
			"factorInsts":    s.Adj.NumInstruments(),
			"corpRows":       s.Corp.TotalRows(),
			"corpEffective":  s.Corp.Total(),
			"firstDay":       first,
			"lastDay":        last,
			"memoryMB":       float64(s.Col.MemoryBytes()) / 1024 / 1024,
			"diskMB":         s.FileSizeMB(),
			"loadMS":         s.LoadMS,
			"loadedAt":       s.LoadedAt.Format("2006-01-02 15:04:05"),
			"feeName":        s.Fee.Name(),
			"dataRoot":       s.DataRoot,
		},
		// 明示当前范围 —— ROADMAP「Web 端向用户明示」的落地点
		"scope": map[string]any{
			"market": s.MarketScope(), "freq": "仅日线",
			"types": "个股（含已退市）+ ETF + 永续合约", "factors": "纯技术面",
		},
	})
}

// ---- 标的列表 ----

type instrumentDTO struct {
	ID           int32  `json:"id"`
	Symbol       string `json:"symbol"`
	Name         string `json:"name"`
	Market       int8   `json:"market"`
	Exchange     int8   `json:"exchange"`
	Type         int8   `json:"type"`
	Board        int8   `json:"board"`
	TrackedBoard int8   `json:"trackedBoard"`
	PriceScale   int32  `json:"priceScale"`
	QtyScale     int32  `json:"qtyScale"`
	QuoteCcy     int8   `json:"quoteCcy"`
	MinOrderQty  int32  `json:"minOrderQty"`
	QtyStep      int32  `json:"qtyStep"`
	ListDate     int32  `json:"listDate"`
	DelistDate   int32  `json:"delistDate"`
	Status       int8   `json:"status"`
	InstStat
}

func (s *Store) dto(in *mktdata.Instrument) instrumentDTO {
	return instrumentDTO{
		ID: int32(in.ID), Symbol: in.Symbol, Name: in.Name,
		Market: int8(in.Market), Exchange: int8(in.Exchange),
		Type: int8(in.Type), Board: int8(in.Board), TrackedBoard: int8(in.TrackedBoard),
		PriceScale: in.PriceScale, QtyScale: in.QtyScale, QuoteCcy: int8(in.QuoteCcy),
		MinOrderQty: in.MinOrderQty, QtyStep: in.QtyStep,
		ListDate: in.ListDate, DelistDate: in.DelistDate, Status: int8(in.Status),
		InstStat: s.Stat(in.ID),
	}
}

func (s *Store) handleInstruments(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	fMarket := qIntSet(r, "market")
	fExch := qIntSet(r, "exchange")
	fType := qIntSet(r, "type")
	fBoard := qIntSet(r, "board")
	fTracked := qIntSet(r, "trackedBoard")
	fStatus := qIntSet(r, "status")
	wantBars, hasBarsFilter := qTri(r, "hasBars")
	wantFactor, hasFactorFilter := qTri(r, "hasFactor")
	wantCorp, hasCorpFilter := qTri(r, "hasCorp")
	// listedOn 是 point-in-time 过滤：给定日期时只留当日在市的标的。
	// 这是核对 C3（幸存者偏差）最直接的工具。
	listedOn := int32(qInt(r, "listedOn", 0))

	rows := make([]instrumentDTO, 0, 256)
	for _, in := range s.Uni.All() {
		if fMarket != nil && !fMarket[int(in.Market)] {
			continue
		}
		if fExch != nil && !fExch[int(in.Exchange)] {
			continue
		}
		if fType != nil && !fType[int(in.Type)] {
			continue
		}
		if fBoard != nil && !fBoard[int(in.Board)] {
			continue
		}
		if fTracked != nil && !fTracked[int(in.TrackedBoard)] {
			continue
		}
		if fStatus != nil && !fStatus[int(in.Status)] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(in.Symbol), q) &&
			!strings.Contains(strings.ToLower(in.Name), q) {
			continue
		}
		st := s.Stat(in.ID)
		if hasBarsFilter && (st.Bars > 0) != wantBars {
			continue
		}
		if hasFactorFilter && (st.FactorEvents > 0) != wantFactor {
			continue
		}
		if hasCorpFilter && (st.CorpActions > 0) != wantCorp {
			continue
		}
		if listedOn != 0 {
			if in.ListDate > listedOn {
				continue
			}
			if in.DelistDate != 0 && in.DelistDate <= listedOn {
				continue
			}
		}
		rows = append(rows, s.dto(in))
	}

	sortInstruments(rows, r.URL.Query().Get("sort"), r.URL.Query().Get("order"))

	total := len(rows)
	page := qInt(r, "page", 1)
	size := qInt(r, "pageSize", 50)
	if size < 1 || size > 5000 {
		size = 50
	}
	if page < 1 {
		page = 1
	}
	lo := (page - 1) * size
	if lo > total {
		lo = total
	}
	hi := lo + size
	if hi > total {
		hi = total
	}
	writeJSON(w, map[string]any{
		"total": total, "page": page, "pageSize": size, "rows": rows[lo:hi],
	})
}

func sortInstruments(rows []instrumentDTO, key, order string) {
	desc := order == "desc"
	less := func(i, j int) bool { return rows[i].Symbol < rows[j].Symbol }
	switch key {
	case "id":
		less = func(i, j int) bool { return rows[i].ID < rows[j].ID }
	case "name":
		less = func(i, j int) bool { return rows[i].Name < rows[j].Name }
	case "listDate":
		less = func(i, j int) bool { return rows[i].ListDate < rows[j].ListDate }
	case "delistDate":
		less = func(i, j int) bool { return rows[i].DelistDate < rows[j].DelistDate }
	case "bars":
		less = func(i, j int) bool { return rows[i].Bars < rows[j].Bars }
	case "firstDay":
		less = func(i, j int) bool { return rows[i].FirstDay < rows[j].FirstDay }
	case "lastDay":
		less = func(i, j int) bool { return rows[i].LastDay < rows[j].LastDay }
	case "factorEvents":
		less = func(i, j int) bool { return rows[i].FactorEvents < rows[j].FactorEvents }
	case "corpActions":
		less = func(i, j int) bool { return rows[i].CorpActions < rows[j].CorpActions }
	}
	// 稳定排序 + 代码兜底，保证同值行的顺序可复现
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			if less(j, i) {
				return true
			}
			if less(i, j) {
				return false
			}
			return rows[i].Symbol < rows[j].Symbol
		}
		if less(i, j) {
			return true
		}
		if less(j, i) {
			return false
		}
		return rows[i].Symbol < rows[j].Symbol
	})
}

// ---- 单标的详情：元数据 + 因子事件 + 公司行动 ----

func (s *Store) handleInstrumentDetail(w http.ResponseWriter, r *http.Request) {
	in, err := s.lookup(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}

	factors := s.Adj.Factors(in.ID)
	fRows := make([]map[string]any, 0, len(factors))
	var prev int64
	for i, f := range factors {
		row := map[string]any{"exDate": f.ExDate, "factor": f.Factor}
		// ratio = 本次因子 / 上一次因子，即每股在该事件后相当于多少股。
		// 直接看因子值看不出「这次除权到底多大」，比值才看得出。
		if i > 0 && prev > 0 {
			row["ratio"] = float64(f.Factor) / float64(prev)
		}
		prev = f.Factor
		fRows = append(fRows, row)
	}

	corps := s.Corp.ByInstrument(in.ID)
	cRows := make([]map[string]any, 0, len(corps))
	for _, c := range corps {
		cRows = append(cRows, map[string]any{
			"exDate": c.ExDate, "cashBeforeTax": c.CashBeforeTax,
			"stockDividend": c.StockDividend, "stockTransfer": c.StockTransfer,
			"rightsRatio": c.RightsRatio, "rightsPrice": c.RightsPrice,
			"hasEffect": c.HasEffect(),
		})
	}

	// 因子事件与公司行动的对账：两边都该有，缺一边就是已知缺口
	factorDays := map[int32]bool{}
	for _, f := range factors {
		factorDays[f.ExDate] = true
	}
	corpDays := map[int32]bool{}
	for _, c := range corps {
		corpDays[c.ExDate] = true
	}
	var factorOnly, corpOnly []int32
	for d := range factorDays {
		if !corpDays[d] {
			factorOnly = append(factorOnly, d)
		}
	}
	for d := range corpDays {
		if !factorDays[d] {
			corpOnly = append(corpOnly, d)
		}
	}
	sort.Slice(factorOnly, func(i, j int) bool { return factorOnly[i] < factorOnly[j] })
	sort.Slice(corpOnly, func(i, j int) bool { return corpOnly[i] < corpOnly[j] })

	writeJSON(w, map[string]any{
		"instrument":  s.dto(in),
		"factors":     fRows,
		"corpActions": cRows,
		"reconcile": map[string]any{
			"factorOnly": factorOnly, // 有因子跳变但表里没有分红送配记录
			"corpOnly":   corpOnly,   // 有分红送配记录但因子没跳变
		},
	})
}

func (s *Store) lookup(key string) (*mktdata.Instrument, error) {
	if key == "" {
		return nil, fmt.Errorf("缺少标的标识")
	}
	// 先按 ID，再按代码 —— 前端用 ID，手工 curl 用代码更顺手
	if n, err := strconv.Atoi(key); err == nil {
		if in := s.Uni.Get(mktdata.InstrumentID(n)); in != nil {
			return in, nil
		}
	}
	if in := s.Uni.BySymbol(key); in != nil {
		return in, nil
	}
	return nil, fmt.Errorf("未找到标的 %q", key)
}

// ---- 交易日历 ----

func (s *Store) handleCalendar(w http.ResponseWriter, r *http.Request) {
	m := mktdata.Market(qInt(r, "market", 1))
	from := int32(qInt(r, "from", 0))
	to := int32(qInt(r, "to", 0))
	days := s.Cal.Days(m, from, to)

	onlyTrading, active := qTri(r, "isTradingDay")
	if active {
		filtered := days[:0:0]
		for _, d := range days {
			if d.IsTradingDay == onlyTrading {
				filtered = append(filtered, d)
			}
		}
		days = filtered
	}

	total := len(days)
	page := qInt(r, "page", 1)
	size := qInt(r, "pageSize", 100)
	if size < 1 || size > 20000 {
		size = 100
	}
	lo := (page - 1) * size
	if lo < 0 || lo > total {
		lo = total
	}
	hi := lo + size
	if hi > total {
		hi = total
	}
	rows := make([]map[string]any, 0, hi-lo)
	for _, d := range days[lo:hi] {
		rows = append(rows, map[string]any{
			"market": int8(d.Market), "date": d.Date, "isTradingDay": d.IsTradingDay,
		})
	}
	writeJSON(w, map[string]any{"total": total, "page": page, "pageSize": size, "rows": rows})
}

// ---- 全表视图：因子与公司行动的跨标的浏览 ----
//
// 逐标的看能验证单点，但「哪一天全市场有 300 个除权」这类问题
// 只有横着看才答得出。

func (s *Store) handleFactorsAll(w http.ResponseWriter, r *http.Request) {
	from := int32(qInt(r, "from", 0))
	to := int32(qInt(r, "to", 0))
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	type row struct {
		ID      int32   `json:"id"`
		Symbol  string  `json:"symbol"`
		Name    string  `json:"name"`
		ExDate  int32   `json:"exDate"`
		Factor  int64   `json:"factor"`
		Ratio   float64 `json:"ratio"`
		HasCorp bool    `json:"hasCorp"`
	}
	rows := make([]row, 0, 1024)
	for _, in := range s.Uni.All() {
		if q != "" && !strings.Contains(strings.ToLower(in.Symbol), q) &&
			!strings.Contains(strings.ToLower(in.Name), q) {
			continue
		}
		fs := s.Adj.Factors(in.ID)
		for i, f := range fs {
			if (from != 0 && f.ExDate < from) || (to != 0 && f.ExDate > to) {
				continue
			}
			ratio := 0.0
			if i > 0 && fs[i-1].Factor > 0 {
				ratio = float64(f.Factor) / float64(fs[i-1].Factor)
			}
			rows = append(rows, row{
				ID: int32(in.ID), Symbol: in.Symbol, Name: in.Name,
				ExDate: f.ExDate, Factor: f.Factor, Ratio: ratio,
				HasCorp: s.Corp.Has(in.ID, f.ExDate),
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ExDate != rows[j].ExDate {
			return rows[i].ExDate > rows[j].ExDate // 新的在前
		}
		return rows[i].Symbol < rows[j].Symbol
	})
	writePaged(w, r, len(rows), func(lo, hi int) any { return rows[lo:hi] })
}

func (s *Store) handleCorpAll(w http.ResponseWriter, r *http.Request) {
	from := int32(qInt(r, "from", 0))
	to := int32(qInt(r, "to", 0))
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	onlyEffect, effectFilter := qTri(r, "hasEffect")

	type row struct {
		ID            int32  `json:"id"`
		Symbol        string `json:"symbol"`
		Name          string `json:"name"`
		ExDate        int32  `json:"exDate"`
		CashBeforeTax int64  `json:"cashBeforeTax"`
		StockDividend int64  `json:"stockDividend"`
		StockTransfer int64  `json:"stockTransfer"`
		RightsRatio   int64  `json:"rightsRatio"`
		RightsPrice   int64  `json:"rightsPrice"`
		HasEffect     bool   `json:"hasEffect"`
		HasFactor     bool   `json:"hasFactor"`
	}
	rows := make([]row, 0, 1024)
	for _, in := range s.Uni.All() {
		if q != "" && !strings.Contains(strings.ToLower(in.Symbol), q) &&
			!strings.Contains(strings.ToLower(in.Name), q) {
			continue
		}
		factorDays := map[int32]bool{}
		for _, f := range s.Adj.Factors(in.ID) {
			factorDays[f.ExDate] = true
		}
		for _, c := range s.Corp.ByInstrument(in.ID) {
			if (from != 0 && c.ExDate < from) || (to != 0 && c.ExDate > to) {
				continue
			}
			if effectFilter && c.HasEffect() != onlyEffect {
				continue
			}
			rows = append(rows, row{
				ID: int32(in.ID), Symbol: in.Symbol, Name: in.Name, ExDate: c.ExDate,
				CashBeforeTax: c.CashBeforeTax, StockDividend: c.StockDividend,
				StockTransfer: c.StockTransfer, RightsRatio: c.RightsRatio,
				RightsPrice: c.RightsPrice, HasEffect: c.HasEffect(),
				HasFactor: factorDays[c.ExDate],
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ExDate != rows[j].ExDate {
			return rows[i].ExDate > rows[j].ExDate
		}
		return rows[i].Symbol < rows[j].Symbol
	})
	writePaged(w, r, len(rows), func(lo, hi int) any { return rows[lo:hi] })
}

func writePaged(w http.ResponseWriter, r *http.Request, total int, slice func(lo, hi int) any) {
	page := qInt(r, "page", 1)
	size := qInt(r, "pageSize", 100)
	if size < 1 || size > 20000 {
		size = 100
	}
	if page < 1 {
		page = 1
	}
	lo := (page - 1) * size
	if lo > total {
		lo = total
	}
	hi := lo + size
	if hi > total {
		hi = total
	}
	writeJSON(w, map[string]any{
		"total": total, "page": page, "pageSize": size, "rows": slice(lo, hi),
	})
}
