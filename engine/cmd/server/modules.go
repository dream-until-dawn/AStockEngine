package main

import (
	"net/http"

	eng "github.com/dream-until-dawn/AStockEngine/engine/internal/engine"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 模块目录：把 registry 里的自描述原样交给前端。
//
// `ParamSpec` 从一开始就是为三处共用而设计的（spec.go 的注释写着
// 「Web 自动生成表单、海选自动展开参数网格、配置校验」）。
// 前两处此前都没接上 —— 海选在 v0.5 接了，Web 到这一版才接。
//
// **前端不得自己维护一份模块清单或参数默认值。**
// 那会立刻分叉：引擎里加一个风控规则，前端不知道；引擎把某个参数的
// 上限从 100 改成 500，前端还在按 100 拦。表单填得出、引擎不认，
// 是这类重复定义最典型的症状。

type paramSpecDTO struct {
	Name string `json:"name"`
	// Kind 用字符串而不是数字：前端拿到 2 得去查表，拿到 "bool" 不用
	Kind    string   `json:"kind"`
	Desc    string   `json:"desc"`
	Default float64  `json:"default"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Step    float64  `json:"step"`
	DefStr  string   `json:"defaultStr,omitempty"`
	Options []string `json:"options,omitempty"`
	// Unbounded 为 true 表示没有上下限（Min==Max 是引擎里「不限」的写法）。
	// 显式给出来，免得前端把 [0,0] 当成真的区间去拦
	Unbounded bool `json:"unbounded"`
}

type moduleDTO struct {
	Name  string         `json:"name"`
	Specs []paramSpecDTO `json:"specs"`
}

func toSpecDTO(s spec.ParamSpec) paramSpecDTO {
	return paramSpecDTO{
		Name: s.Name, Kind: s.Kind.String(), Desc: s.Desc,
		Default: s.Default, Min: s.Min, Max: s.Max, Step: s.Step,
		DefStr: s.DefaultStr, Options: s.Options,
		Unbounded: s.Min == s.Max,
	}
}

func collect(names []string, get func(string) ([]spec.ParamSpec, bool)) []moduleDTO {
	out := make([]moduleDTO, 0, len(names))
	for _, n := range names {
		specs, _ := get(n)
		m := moduleDTO{Name: n, Specs: make([]paramSpecDTO, 0, len(specs))}
		for _, s := range specs {
			m.Specs = append(m.Specs, toSpecDTO(s))
		}
		out = append(out, m)
	}
	return out
}

// handleModules 列出全部可用模块与它们的参数规格。
func (s *Store) handleModules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"strategy": collect(eng.Strategies.Names(), eng.Strategies.Specs),
		"sizer":    collect(trading.Sizers.Names(), trading.Sizers.Specs),
		"risk":     collect(trading.Risks.Names(), trading.Risks.Specs),
		"slippage": collect(trading.Slippages.Names(), trading.Slippages.Specs),
		"fee":      collect(trading.Fees.Names(), trading.Fees.Specs),
		"market":   collect(trading.Markets.Names(), trading.Markets.Specs),
		// 下面几个不是 registry 里的模块，但表单要用到它们的取值域。
		// 放在同一个响应里，前端就只需要一次请求
		"enums": map[string]any{
			"indicator_adj": []map[string]string{
				{"code": "hfq", "label": "后复权（回测基准）"},
				{"code": "none", "label": "不复权"},
			},
			"recorder_level": []map[string]string{
				{"code": "summary", "label": "summary（够算全部指标）"},
				{"code": "full", "label": "full（留每步信号，很占内存）"},
				{"code": "none", "label": "none（只跑不记）"},
			},
			"sizer_base": []map[string]string{
				{"code": "initial", "label": "initial（定额下注）"},
				{"code": "equity", "label": "equity（复利）"},
			},
		},
		// 说明性文本随接口一起给，前端不必把它们抄一遍
		"notes": map[string]string{
			// 纯文本，不写 Markdown —— 前端是直接渲染的，
			// 星号会原样显示成星号
			"indicator_adj": "拒绝前复权：它锚定末日，用于决策即构成未来函数（C1）" +
				"且不可复现（C5）。前复权只允许出现在展示路径上。",
			"risk": "风控是一条链：多条规则顺序执行，任一条拒绝即拒绝，" +
				"通过的订单可被前一条缩量后传给下一条。顺序有意义。",
		},
	})
}
