package main

import (
	"net/http"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/sweep"
)

// 海选视图的服务端。
//
// # 为什么只读不跑
//
// 一次海选几秒到几分钟，而且要吃满 CPU。塞进 HTTP 请求的话，
// 一个刷新就能把服务端顶住，而且没法给进度。
// 跑由 `cmd/sweep` 负责，这里只把跑完的结果读出来。
//
// # 数字在哪算
//
// 全在 `sweep.Analyze` 里 —— 命令行报告与这里用的是同一个函数。
// 两处各算一遍的话迟早有一处口径不同，这个项目已经吃过一次那种亏
// （开平方向那张表写了四遍，见 trading.LegOf）。

type sweepBrief struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Base 基准配置路径，供「拿这一行去回测」时指回去
	Base       string  `json:"base"`
	CreatedAt  string  `json:"createdAt"`
	Params     int     `json:"params"`
	Windows    int     `json:"windows"`
	AnnualDays float64 `json:"annualDays"`
	// Analyzable 为 false 表示这个目录没有清单（v0.5.1 之前跑的），
	// 只能列出来、分析不了
	Analyzable bool `json:"analyzable"`
}

// handleSweepList 列出全部跑过的海选。
func (s *Store) handleSweepList(w http.ResponseWriter, _ *http.Request) {
	ms, err := sweep.ListSweeps(mustAbs(s.DataRoot))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := make([]sweepBrief, 0, len(ms))
	for _, m := range ms {
		b := sweepBrief{
			ID: m.SweepID, Name: m.Name, Base: m.Base,
			CreatedAt: m.CreatedAt, Params: len(m.Params),
			Windows: len(m.Windows), AnnualDays: m.AnnualDays,
			Analyzable: m.Config != nil,
		}
		out = append(out, b)
	}
	writeJSON(w, map[string]any{"sweeps": out})
}

// handleSweepDetail 读一次海选的全部结论。
func (s *Store) handleSweepDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dir := sweep.ResultDir(mustAbs(s.DataRoot), id)

	m, err := sweep.ReadManifest(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取海选清单失败: %v", err)
		return
	}
	if m == nil || m.Config == nil {
		// **说清楚为什么**，别只回一个 404 —— 目录是在的，
		// 只是它没有清单，那是 v0.5.1 之前跑出来的
		writeErr(w, http.StatusBadRequest,
			"这次海选没有清单（manifest.json），分析不了 —— "+
				"它是 v0.5.1 之前跑的。重跑一次即可")
		return
	}
	rows, err := sweep.ReadAll(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取海选结果失败: %v", err)
		return
	}
	a := sweep.Analyze(rows, m.ParamSets(), m.Config, id, m.Name)
	writeJSON(w, map[string]any{
		"analysis": a,
		"manifest": map[string]any{
			"base": m.Base, "createdAt": m.CreatedAt,
			"annualDays": m.AnnualDays, "windows": m.Windows,
			"gate": m.Config.Gate, "rank": m.Config.Rank,
			"walkForward": m.Config.WalkForward,
			"noiseProbe":  m.Config.NoiseProbe,
		},
	})
}
