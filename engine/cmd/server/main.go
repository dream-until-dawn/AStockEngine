// Command server 是数据核对服务：把 ETL 产出的表与引擎算出的指标
// 一并暴露成 HTTP API，供前端直观比对。
//
// 它**不是**回测服务。存在的唯一目的是回答「数据准不准」——
// 因此凡是能由引擎算的，一律走引擎，不在这里另写一份实现。
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/fingerprint"

	// 策略经 init() 注册进 engine.Strategies，必须导入才会生效
	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
)

// gitCommit 由构建时注入，进结果指纹。
var gitCommit string

func main() {
	addr := flag.String("addr", "127.0.0.1:8123", "监听地址")
	dataRoot := flag.String("data", "../data", "数据根目录")
	feePath := flag.String("fee", "../configs/fee/ashare_default.json", "费率配置")
	webDir := flag.String("web", "../web/dist", "前端构建产物目录，不存在则只提供 API")
	cfgDir := flag.String("configs", "../configs/backtest", "回测配置目录")
	flag.Parse()

	fingerprint.SetEngineVersion(gitCommit)

	if err := checkDataRoot(*dataRoot); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	fmt.Println("AStockEngine 数据核对服务")
	fmt.Printf("数据目录 %s\n", mustAbs(*dataRoot))
	t0 := time.Now()
	store, err := LoadStore(*dataRoot, *feePath, func(f string, a ...any) {
		fmt.Printf(f, a...)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n错误:", err)
		os.Exit(1)
	}
	store.ConfigDir = *cfgDir
	first, last := store.DataDays()
	fmt.Printf("\n就绪：%d 只标的 / %d 行 / %d ~ %d / 合计 %v\n",
		store.Uni.Len(), store.BarStats.Rows, first, last,
		time.Since(t0).Round(time.Millisecond))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/meta", store.handleMeta)
	mux.HandleFunc("GET /api/instruments", store.handleInstruments)
	mux.HandleFunc("GET /api/instruments/{id}", store.handleInstrumentDetail)
	mux.HandleFunc("GET /api/calendar", store.handleCalendar)
	mux.HandleFunc("GET /api/factors", store.handleFactorsAll)
	mux.HandleFunc("GET /api/corp-actions", store.handleCorpAll)
	mux.HandleFunc("GET /api/kline/{id}", store.handleKline)
	mux.HandleFunc("GET /api/configs", store.handleConfigs)
	mux.HandleFunc("POST /api/backtest", store.handleBacktest)
	mux.HandleFunc("POST /api/universe", store.handleUniverse)
	// 模块目录：前端据此自动生成表单。**前端不得自己维护一份清单**
	mux.HandleFunc("GET /api/modules", store.handleModules)

	// 单步调试会话（v0.4）。纯 HTTP，不用 WebSocket ——
	// 步进是用户驱动的请求/响应，没有服务端主动产生的事件。理由见 session.go
	mux.HandleFunc("GET /api/session", store.handleSessionList)
	mux.HandleFunc("POST /api/session", store.handleSessionCreate)
	mux.HandleFunc("GET /api/session/{id}", store.handleSessionGet)
	mux.HandleFunc("DELETE /api/session/{id}", store.handleSessionDelete)
	mux.HandleFunc("POST /api/session/{id}/step", store.handleSessionStep)
	mux.HandleFunc("POST /api/session/{id}/reset", store.handleSessionReset)
	mux.HandleFunc("GET /api/session/{id}/inspect", store.handleSessionInspect)
	mux.HandleFunc("GET /api/session/{id}/snapshot", store.handleSessionSnapshot)
	mux.HandleFunc("POST /api/session/{id}/restore", store.handleSessionRestore)

	// 前端产物存在就一并伺服，这样 `go run` 一条命令即可用；
	// 开发时走 Vite（它把 /api 代理到这里），不依赖本分支。
	if st, err := os.Stat(*webDir); err == nil && st.IsDir() {
		mux.Handle("/", spaHandler(*webDir))
		fmt.Printf("前端目录 %s\n", mustAbs(*webDir))
	} else {
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, apiIndex)
		})
		fmt.Printf("前端未构建（%s 不存在），当前只提供 API\n", *webDir)
	}

	fmt.Printf("监听 http://%s\n\n", *addr)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           logging(cors(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// spaHandler 伺服前端产物：静态文件命中就返回，否则回落到 index.html
// 交给前端路由处理。
func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean(r.URL.Path))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

// cors 放行任意来源。服务默认只绑 127.0.0.1，本机开发工具，
// 放行是为了让 Vite dev server 与手工 curl 都能直连。
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		t0 := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		q := r.URL.RawQuery
		if q != "" {
			q = "?" + q
		}
		fmt.Printf("%s %d  %-60s %v\n", time.Now().Format("15:04:05"),
			rec.code, r.URL.Path+q, time.Since(t0).Round(time.Millisecond))
	})
}

func mustAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

const apiIndex = `AStockEngine 数据核对服务

前端尚未构建。构建方式：
    cd web && npm install && npm run build
开发模式（带热更新，把 /api 代理到本服务）：
    cd web && npm run dev

可用接口：
  GET /api/meta                    枚举字典、定点 scale、数据统计
  GET /api/instruments             标的列表（筛选 + 排序 + 分页）
      ?q=&market=&exchange=&type=&board=&trackedBoard=&status=
      &hasBars=&hasFactor=&hasCorp=&listedOn=YYYYMMDD
      &sort=symbol|bars|listDate|...&order=asc|desc&page=&pageSize=
  GET /api/instruments/{id}        单标的详情 + 因子事件 + 公司行动 + 两表对账
  GET /api/calendar                交易日历 ?market=1&from=&to=&isTradingDay=
  GET /api/factors                 全市场复权因子事件 ?from=&to=&q=
  GET /api/corp-actions            全市场分红送配 ?from=&to=&q=&hasEffect=
  GET /api/configs                 列出回测配置（含解析结果，供前端改参数）
  POST /api/backtest               跑一次回测，返回绩效 / 净值 / 成交 / 拒单 / 逐轮
  POST /api/universe               预览标的池：命中多少只、都是些什么
  GET /api/kline/{id}              K 线 + 引擎算出的指标
      ?adj=none|qfq|hfq            复权模式，K 线与指标同基准（默认 none）
      &from=&to=&macd=12,26,9&kdj=9,3,3&ma=5,10,20,60
      指标同时给两组：ind 为所选基准，indBt 为回测基准（后复权）。
      adj=hfq 时两者相同，indBt 省略。

{id} 可以是 instrument_id，也可以是代码（如 600000）。

价格一律以定点整数返回，除以 /api/meta 给出的 scale 才是元。
`
