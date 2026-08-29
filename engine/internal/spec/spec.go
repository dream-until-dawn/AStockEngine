// Package spec 是模块参数的自描述。
//
// 它**不依赖任何其他内部包** —— 这是刻意的：registry 与各领域包
// （trading / engine / strategies）都要用它，放在任何一个里面都会形成循环。
//
// v0.2 时 ParamSpec 住在 engine 包，只有策略用得上。v0.3 起 Fee / Slippage /
// Sizer / Risk 全都要自描述（Web 表单、海选参数网格、配置校验共用一份元数据），
// 于是它必须下沉到所有人都能引用的位置。
package spec

import "fmt"

// ParamKind 参数类型。
type ParamKind int8

const (
	ParamInt ParamKind = iota
	ParamFloat
	ParamBool
	// ParamString 用于枚举型取值（如 equal_weight 的 base）。
	// 取值范围由 Options 给出 —— 自由文本参数不该出现在这里，
	// 它无法生成表单也无法展开成参数网格。
	ParamString
)

func (k ParamKind) String() string {
	switch k {
	case ParamInt:
		return "int"
	case ParamFloat:
		return "float"
	case ParamBool:
		return "bool"
	case ParamString:
		return "string"
	}
	return "unknown"
}

// ParamSpec 是一个参数的自描述。
//
// 同时喂三处：Web 自动生成表单（v1.0）、海选自动展开参数网格（v0.5）、
// 配置校验（本刀）。三者共用一份定义，才不会出现「表单能填但引擎不认」。
type ParamSpec struct {
	Name string    `json:"name"`
	Kind ParamKind `json:"kind"`
	Desc string    `json:"desc"`

	// 数值型（Int / Float / Bool）用这三个。Bool 的取值域是 [0,1]。
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step"`

	// 字符串型用这两个。分开放而不是塞进 Default，
	// 是因为 JSON 里 `"default": 5` 与 `"default": "equity"` 类型不同，
	// 用 any 会让前端每次都要判类型。
	DefaultStr string   `json:"defaultStr,omitempty"`
	Options    []string `json:"options,omitempty"`
}

// Validate 校验一个取值是否落在规格内。
func (s ParamSpec) Validate(v float64) error {
	if s.Kind == ParamString {
		return fmt.Errorf("参数 %s 是字符串型，不能按数值校验", s.Name)
	}
	if s.Min != s.Max && (v < s.Min || v > s.Max) {
		return fmt.Errorf("参数 %s = %g 超出范围 [%g, %g]", s.Name, v, s.Min, s.Max)
	}
	return nil
}

// ValidateStr 校验字符串取值是否在 Options 中。
func (s ParamSpec) ValidateStr(v string) error {
	if s.Kind != ParamString {
		return fmt.Errorf("参数 %s 不是字符串型", s.Name)
	}
	if len(s.Options) == 0 {
		return nil
	}
	for _, o := range s.Options {
		if o == v {
			return nil
		}
	}
	return fmt.Errorf("参数 %s = %q 不在允许取值 %v 中", s.Name, v, s.Options)
}

// Params 是一次运行的实际数值参数取值。
//
// 只装数值：字符串型参数由各模块从自己那段原始 JSON 里取
// （registry 的 Factory 拿到的就是那段 JSON）。策略是唯一的例外 ——
// 它经 InitContext.Params() 取参，因此策略参数目前限于数值。
type Params map[string]float64

func (p Params) Int(name string, def int) int {
	if v, ok := p[name]; ok {
		return int(v)
	}
	return def
}

func (p Params) Float(name string, def float64) float64 {
	if v, ok := p[name]; ok {
		return v
	}
	return def
}

func (p Params) Bool(name string, def bool) bool {
	if v, ok := p[name]; ok {
		return v != 0
	}
	return def
}

// Defaults 由一组规格生成默认取值。
func Defaults(specs []ParamSpec) Params {
	p := make(Params, len(specs))
	for _, s := range specs {
		if s.Kind == ParamString {
			continue
		}
		p[s.Name] = s.Default
	}
	return p
}

// ValidateAll 校验一组取值，并拒绝规格里没有的参数名。
//
// **拒绝未知参数名是有意的**：配置里把 `slots` 写成 `slot` 时，
// 若静默忽略，引擎会用默认值跑完并给出一个看似正常的结果 ——
// 那比报错难查得多。
func ValidateAll(specs []ParamSpec, p Params) error {
	known := make(map[string]ParamSpec, len(specs))
	for _, s := range specs {
		known[s.Name] = s
	}
	for name, v := range p {
		s, ok := known[name]
		if !ok {
			return fmt.Errorf("未知参数 %q（可用：%s）", name, names(specs))
		}
		if err := s.Validate(v); err != nil {
			return err
		}
	}
	return nil
}

func names(specs []ParamSpec) string {
	out := ""
	for i, s := range specs {
		if i > 0 {
			out += ", "
		}
		out += s.Name
	}
	if out == "" {
		return "无"
	}
	return out
}
