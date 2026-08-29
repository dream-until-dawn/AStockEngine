// Package registry 是「按名字取实现」的泛型容器。
//
// 它**不认识任何领域类型** —— 这是设计的关键。若 registry 的 map 值类型写成
// trading.Fee，那么 trading 注册自己的实现时就要 import registry，
// 而 registry 又要 import trading，构成循环。泛型让 registry 保持无知，
// 各领域包在自己内部持有 registry 变量并自注册，谁也不用绕道。
//
// 这不是「热插拔」：Go 的 plugin 在 Windows 上不可用（ROADMAP 已定），
// 新增实现仍需重新编译。registry 买到的是**运行时按字符串选实现**，
// 让一份 JSON 就能决定引擎的装配。
package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// Factory 从配置片段构造一个实现。
//
// 参数是该模块自己那段 JSON（配置里的 `params` 字段），由实现自行解析 ——
// registry 不该知道 bps 是什么。params 可以为 nil，表示「全用默认值」。
type Factory[T any] func(params json.RawMessage) (T, error)

type entry[T any] struct {
	specs []spec.ParamSpec
	make  Factory[T]
}

// Registry 是某一类模块的全部可用实现。
type Registry[T any] struct {
	kind string
	mu   sync.RWMutex
	m    map[string]entry[T]
}

// New 创建一个注册表。kind 只用于错误信息，如 "fee" / "sizer"。
func New[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, m: make(map[string]entry[T], 8)}
}

// Register 注册一个实现。
//
// **重名直接 panic。** 注册发生在 init() 里，重名是编译期就该发现的编程错误；
// 静默覆盖会让「配置里写的那个」和「实际跑的那个」不是同一个东西。
func (r *Registry[T]) Register(name string, specs []spec.ParamSpec, f Factory[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[name]; dup {
		panic(fmt.Sprintf("registry[%s]: 实现 %q 重复注册", r.kind, name))
	}
	r.m[name] = entry[T]{specs: specs, make: f}
}

// Build 按名字构造实现。
func (r *Registry[T]) Build(name string, params json.RawMessage) (T, error) {
	var zero T
	r.mu.RLock()
	e, ok := r.m[name]
	r.mu.RUnlock()
	if !ok {
		return zero, fmt.Errorf("未知的 %s 实现 %q，可选：%s",
			r.kind, name, strings.Join(r.Names(), " / "))
	}
	v, err := e.make(params)
	if err != nil {
		return zero, fmt.Errorf("构造 %s.%s 失败: %w", r.kind, name, err)
	}
	return v, nil
}

// Has 报告是否存在该实现。
func (r *Registry[T]) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.m[name]
	return ok
}

// Names 返回全部实现名，**已排序** —— 错误信息与 Web 下拉框都要稳定顺序。
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Specs 返回某实现的参数规格。
func (r *Registry[T]) Specs(name string) ([]spec.ParamSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.m[name]
	if !ok {
		return nil, false
	}
	return e.specs, true
}

// Kind 返回这一类模块的名字。
func (r *Registry[T]) Kind() string { return r.kind }

// ---- 参数解析辅助 ----
//
// 各模块的 Factory 都要做同一件事：把 raw JSON 解出来、填默认值、校验范围。
// 抽在这里，省得每个实现各写一遍，也保证「未知参数名报错」这条到处一致。

// DecodeParams 把配置片段解析为数值参数，缺省项用规格里的默认值补齐。
func DecodeParams(specs []spec.ParamSpec, raw json.RawMessage) (spec.Params, error) {
	p := spec.Defaults(specs)
	if len(raw) == 0 {
		return p, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("参数不是合法的 JSON 对象: %w", err)
	}
	byName := make(map[string]spec.ParamSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	for k, v := range m {
		s, ok := byName[k]
		if !ok {
			return nil, fmt.Errorf("未知参数 %q", k)
		}
		if s.Kind == spec.ParamString {
			continue // 字符串参数由模块自行从 raw 取
		}
		f, err := toFloat(v)
		if err != nil {
			return nil, fmt.Errorf("参数 %q: %w", k, err)
		}
		if err := s.Validate(f); err != nil {
			return nil, err
		}
		p[k] = f
	}
	return p, nil
}

// DecodeString 取一个字符串参数，缺省时用规格里的默认值，并校验取值域。
func DecodeString(specs []spec.ParamSpec, raw json.RawMessage, name string) (string, error) {
	var s spec.ParamSpec
	found := false
	for _, x := range specs {
		if x.Name == name {
			s, found = x, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("规格中没有参数 %q", name)
	}
	val := s.DefaultStr
	if len(raw) > 0 {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("参数不是合法的 JSON 对象: %w", err)
		}
		if v, ok := m[name]; ok {
			if err := json.Unmarshal(v, &val); err != nil {
				return "", fmt.Errorf("参数 %q 不是字符串: %w", name, err)
			}
		}
	}
	if err := s.ValidateStr(val); err != nil {
		return "", err
	}
	return val, nil
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return 0, fmt.Errorf("期望数值，得到字符串 %q", x)
	case nil:
		return 0, fmt.Errorf("期望数值，得到 null")
	}
	return 0, fmt.Errorf("期望数值，得到 %T", v)
}
