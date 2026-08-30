package main

import (
	"net/http"
	"os"
	"path/filepath"

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
	Name string `json:"name"`
	// Desc 一句话中文说明。前端在下拉框里显示成「name（desc）」——
	// 光有 `macd_cross` 这样的英文标识符，没用过引擎的人看不出它是什么
	Desc  string         `json:"desc"`
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

func collect(
	names []string,
	get func(string) ([]spec.ParamSpec, bool),
	desc func(string) string,
) []moduleDTO {
	out := make([]moduleDTO, 0, len(names))
	for _, n := range names {
		specs, _ := get(n)
		m := moduleDTO{Name: n, Desc: desc(n), Specs: make([]paramSpecDTO, 0, len(specs))}
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
		"strategy": collect(eng.Strategies.Names(), eng.Strategies.Specs, eng.Strategies.Desc),
		"sizer":    collect(trading.Sizers.Names(), trading.Sizers.Specs, trading.Sizers.Desc),
		"risk":     collect(trading.Risks.Names(), trading.Risks.Specs, trading.Risks.Desc),
		"slippage": collect(trading.Slippages.Names(), trading.Slippages.Specs, trading.Slippages.Desc),
		"fee":      collect(trading.Fees.Names(), trading.Fees.Specs, trading.Fees.Desc),
		"market":   collect(trading.Markets.Names(), trading.Markets.Specs, trading.Markets.Desc),
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

// ---- 费率 ----

// feeFileDTO 是一份费率配置文件。
type feeFileDTO struct {
	// Path 相对配置目录，直接可以填进 fee.params.path
	Path        string       `json:"path"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Rules       []feeRuleDTO `json:"rules"`
	Err         string       `json:"err,omitempty"`
}

type feeRuleDTO struct {
	Kind            string   `json:"kind"`
	InstrumentTypes []string `json:"instrumentTypes,omitempty"`
	Boards          []string `json:"boards,omitempty"`
	Side            string   `json:"side"`
	From            int32    `json:"from,omitempty"`
	To              int32    `json:"to,omitempty"`
	RatePPM         int64    `json:"ratePpm,omitempty"`
	PerShareCents   int64    `json:"perShareCents,omitempty"`
	FlatCents       int64    `json:"flatCents,omitempty"`
	MinCents        int64    `json:"minCents,omitempty"`
	Note            string   `json:"note,omitempty"`
}

// handleFees 列出 configs/fee/ 下的费率文件与它们**实际生效的规则**。
//
// # 为什么必须能看见
//
// 费率是 A 股策略的主要亏损来源之一 —— 实测 `macd_cross` 的摩擦
// 占初始资金 **20.45%**（费用 11.56% + 滑点 8.89%）。而它此前藏在一个
// 用户从没打开过的 JSON 文件里，Web 上只有一个填路径的文本框。
//
// # 为什么只让改佣金
//
// 佣金是券商定的，各家从万 0.85 到万 3 都有，用户必须能改成自己的。
// 印花税与过户费是监管费率，且**按生效日期分段**（2005 / 2007 / 2008 /
// 2023 各调过一次，2008-09-19 还从双边改成单边）。把它们做成一个
// 可随手填的数字，等于邀请用户拿 2023 年的税率去算 2007 年的回测 ——
// 那不报错，只会让结果安静地偏乐观。要改就去改文件，那里每条都带 note。
func (s *Store) handleFees(w http.ResponseWriter, _ *http.Request) {
	// ConfigDir 指向 configs/backtest，费率在它的**同级**目录 configs/fee，
	// 与配置里写的相对路径 ../fee/xxx.json 对得上
	dir := filepath.Join(mustAbs(s.ConfigDir), "..", "fee")
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取 %s 失败: %v", dir, err)
		return
	}
	out := make([]feeFileDTO, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		// 路径写成配置里能直接用的相对形式（配置在 configs/backtest/ 下）
		d := feeFileDTO{Path: "../fee/" + e.Name()}
		f, err := trading.LoadFee(filepath.Join(dir, e.Name()))
		if err != nil {
			// 坏掉的文件也列出来并带上错误 —— 藏起来只会让人
			// 在下拉框里找不到它然后怀疑是自己看错了
			d.Err = err.Error()
			out = append(out, d)
			continue
		}
		cfg := f.Config()
		d.Name, d.Description = cfg.Name, cfg.Description
		for _, r := range cfg.Rules {
			d.Rules = append(d.Rules, feeRuleDTO{
				Kind: r.Kind, InstrumentTypes: r.InstrumentTypes, Boards: r.Boards,
				Side: r.Side, From: r.From, To: r.To,
				RatePPM: r.RatePPM, PerShareCents: r.PerShareCents,
				FlatCents: r.FlatCents, MinCents: r.MinCents, Note: r.Note,
			})
		}
		out = append(out, d)
	}
	writeJSON(w, map[string]any{"dir": dir, "files": out})
}
