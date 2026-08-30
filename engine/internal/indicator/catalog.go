package indicator

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// 指标目录：按名字 + 参数构造指标。
//
// # 为什么现在才需要它
//
// 此前指标都是策略在代码里直接 `NewSMA(20, ...)` 出来的 ——
// 策略是编译期定死的，指标自然也是。规则树策略把这件事反过来了：
// **用户在界面上决定要算哪些指标**，然后用它们的输出拼条件。
// 那就必须有一个「名字 → 构造器 + 参数规格 + 输出字段」的表。
//
// 复用 registry.Registry 而不是另造一套：它已经管好了重名 panic、
// 参数解码与规格暴露，而这三件事正是这里需要的。
//
// # Fields 为什么要单列
//
// `Indicator.Names()` 要有实例才能问，而界面在**用户还没添加指标之前**
// 就得知道「选了 KDJ 会多出 K / D / J 三个可用列」。
// 所以字段名写在目录里，与实例的 Names() 是同一份东西的两处表达 ——
// `TestCatalogFieldsMatchNames` 保证它们不会分叉。

// Catalog 是指标注册表。
var Catalog = registry.New[Indicator]("indicator")

// fields 记录每个指标的输出字段名。registry 只管参数规格，
// 输出字段是指标特有的，单独存一份。
var fields = map[string][]string{}

// Register 注册一个指标。
//
// outFields 必须与实例的 `Names()` 完全一致 —— 界面按前者给出可选列，
// 求值时按后者取值，两者分叉会让「选得出但取不到」。
func Register(
	name, desc string, outFields []string,
	specs []spec.ParamSpec, f registry.Factory[Indicator],
) {
	Catalog.Register(name, desc, specs, f)
	fields[name] = outFields
}

// Fields 返回某指标的输出字段名。
func Fields(name string) []string { return fields[name] }

// Kinds 返回全部已注册的指标名，升序。
func Kinds() []string {
	ks := make([]string, 0, len(fields))
	for k := range fields {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func init() {
	Register("sma", "简单移动平均", []string{"MA"},
		[]spec.ParamSpec{
			{Name: "period", Kind: spec.ParamInt, Default: 20, Min: 1, Max: 500, Step: 1,
				Desc: "周期"},
		},
		func(raw json.RawMessage) (Indicator, error) {
			p, err := registry.DecodeParams(smaSpecs(), raw)
			if err != nil {
				return nil, err
			}
			return NewSMA(p.Int("period", 20), DefaultPriceScale), nil
		})

	Register("ema", "指数移动平均", []string{"EMA"},
		[]spec.ParamSpec{
			{Name: "period", Kind: spec.ParamInt, Default: 12, Min: 1, Max: 500, Step: 1,
				Desc: "周期"},
		},
		func(raw json.RawMessage) (Indicator, error) {
			p, err := registry.DecodeParams(emaSpecs(), raw)
			if err != nil {
				return nil, err
			}
			return NewEMA(p.Int("period", 12), DefaultPriceScale), nil
		})

	Register("macd", "MACD 指数平滑异同移动平均", []string{"DIF", "DEA", "MACD"},
		macdSpecs(),
		func(raw json.RawMessage) (Indicator, error) {
			p, err := registry.DecodeParams(macdSpecs(), raw)
			if err != nil {
				return nil, err
			}
			short, long := p.Int("short", 12), p.Int("long", 26)
			if short >= long {
				return nil, fmt.Errorf("快线周期 %d 必须小于慢线周期 %d", short, long)
			}
			return NewMACD(short, long, p.Int("signal", 9), DefaultPriceScale), nil
		})

	Register("kdj", "KDJ 随机指标", []string{"K", "D", "J"},
		kdjSpecs(),
		func(raw json.RawMessage) (Indicator, error) {
			p, err := registry.DecodeParams(kdjSpecs(), raw)
			if err != nil {
				return nil, err
			}
			return NewKDJ(p.Int("period", 9), p.Int("k_smooth", 3),
				p.Int("d_smooth", 3)), nil
		})

	Register("rsi", "RSI 相对强弱（均值回归）", []string{"RSI"},
		[]spec.ParamSpec{
			{Name: "period", Kind: spec.ParamInt, Default: 14, Min: 2, Max: 200, Step: 1,
				Desc: "周期"},
		},
		func(raw json.RawMessage) (Indicator, error) {
			p, err := registry.DecodeParams(rsiSpecs(), raw)
			if err != nil {
				return nil, err
			}
			return NewRSI(p.Int("period", 14)), nil
		})

	Register("donchian", "唐奇安通道（上下轨不含当前这根）",
		[]string{"UPPER", "LOWER", "MID"},
		[]spec.ParamSpec{
			{Name: "period", Kind: spec.ParamInt, Default: 20, Min: 2, Max: 500, Step: 1,
				Desc: "周期"},
		},
		func(raw json.RawMessage) (Indicator, error) {
			p, err := registry.DecodeParams(donchianSpecs(), raw)
			if err != nil {
				return nil, err
			}
			return NewDonchian(p.Int("period", 20), DefaultPriceScale), nil
		})
}

// 规格各写一份取值函数，避免 init 里的求值顺序问题。

func smaSpecs() []spec.ParamSpec {
	return []spec.ParamSpec{{Name: "period", Kind: spec.ParamInt,
		Default: 20, Min: 1, Max: 500, Step: 1, Desc: "周期"}}
}

func emaSpecs() []spec.ParamSpec {
	return []spec.ParamSpec{{Name: "period", Kind: spec.ParamInt,
		Default: 12, Min: 1, Max: 500, Step: 1, Desc: "周期"}}
}

func rsiSpecs() []spec.ParamSpec {
	return []spec.ParamSpec{{Name: "period", Kind: spec.ParamInt,
		Default: 14, Min: 2, Max: 200, Step: 1, Desc: "周期"}}
}

func donchianSpecs() []spec.ParamSpec {
	return []spec.ParamSpec{{Name: "period", Kind: spec.ParamInt,
		Default: 20, Min: 2, Max: 500, Step: 1, Desc: "周期"}}
}

func macdSpecs() []spec.ParamSpec {
	return []spec.ParamSpec{
		{Name: "short", Kind: spec.ParamInt, Default: 12, Min: 2, Max: 200, Step: 1,
			Desc: "快线周期"},
		{Name: "long", Kind: spec.ParamInt, Default: 26, Min: 3, Max: 400, Step: 1,
			Desc: "慢线周期"},
		{Name: "signal", Kind: spec.ParamInt, Default: 9, Min: 2, Max: 200, Step: 1,
			Desc: "信号周期"},
	}
}

func kdjSpecs() []spec.ParamSpec {
	return []spec.ParamSpec{
		{Name: "period", Kind: spec.ParamInt, Default: 9, Min: 2, Max: 200, Step: 1,
			Desc: "RSV 周期"},
		{Name: "k_smooth", Kind: spec.ParamInt, Default: 3, Min: 1, Max: 50, Step: 1,
			Desc: "K 值平滑系数"},
		{Name: "d_smooth", Kind: spec.ParamInt, Default: 3, Min: 1, Max: 50, Step: 1,
			Desc: "D 值平滑系数"},
	}
}
