package trading

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/registry"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/spec"
)

// 本文件把 v0.2 就已经是接口的三个模块接进 registry：Market / Fee / Slippage。
//
// 它们的实现没有改动，只是从「在 Go 代码里 new 出来」变成「配置里写个名字」。
// Sizer 与 Risk 的注册在各自文件里。

var (
	// Markets 市场规则。远期接美股 / 加密货币时新增实现即可，
	// 引擎与策略不需要知道多了一个（C9）。
	Markets = registry.New[Market]("market")
	// Fees 费用模型。**费率是用户配置项**：不同券商佣金不同，
	// 加密货币的费率结构（maker/taker、提现费）与 A 股完全不是一回事。
	Fees = registry.New[Fee]("fee")
	// Slippages 滑点模型。
	Slippages = registry.New[Slippage]("slippage")
)

// ---- Market ----

var ashareMarketSpecs = []spec.ParamSpec{}

// ---- Fee ----

var configFeeSpecs = []spec.ParamSpec{
	{Name: "path", Kind: spec.ParamString, DefaultStr: "configs/fee/ashare_default.json",
		Desc: "费率配置文件路径"},
	// 只开放佣金的覆盖，不开放印花税与过户费。
	//
	// **因为这三者的性质完全不同**：佣金是券商定的，各家从万 0.85 到万 3
	// 都有，用户必须能改成自己的；印花税与过户费是监管费率，且**按生效日期
	// 分段**（2005/2007/2008/2023 各调过一次，2008-09-19 还从双边改成单边）。
	// 把后两者做成一个可随手填的数字，等于邀请用户把 2007 年的回测
	// 按 2023 年的税率算 —— 那不会报错，只会让结果安静地偏乐观。
	{Name: "commission_ppm", Kind: spec.ParamFloat, Default: 0, Min: 0, Max: 10000, Step: 0.5,
		Desc: "佣金费率覆盖（百万分之一；万 2.5 填 250）。0 表示用文件里的值"},
	{Name: "commission_min_yuan", Kind: spec.ParamFloat, Default: -1, Min: -1, Max: 1000, Step: 1,
		Desc: "佣金每笔最低（元）覆盖。填 0 表示无最低；-1 表示用文件里的值"},
}

var zeroFeeSpecs = []spec.ParamSpec{}

// ---- Slippage ----

var bpsSlippageSpecs = []spec.ParamSpec{
	{Name: "bps", Kind: spec.ParamInt, Default: 5, Min: 0, Max: 1000, Step: 1,
		Desc: "滑点基点：买入价上浮、卖出价下压万分之 bps/10"},
}

var noSlippageSpecs = []spec.ParamSpec{}

func init() {
	Markets.Register("ashare", "A 股规则：T+1、涨跌停、最小申报单位、盘后固定价格交易", ashareMarketSpecs,
		func(json.RawMessage) (Market, error) { return NewAShareMarket(), nil })

	Fees.Register("config", "按费率文件计费：佣金 / 印花税 / 过户费，支持按日期分段与板块区分", configFeeSpecs, func(raw json.RawMessage) (Fee, error) {
		path, err := registry.DecodeString(configFeeSpecs, raw, "path")
		if err != nil {
			return nil, err
		}
		if path == "" {
			return nil, fmt.Errorf("fee.params.path 不能为空")
		}
		f, err := LoadFee(path)
		if err != nil {
			return nil, err
		}
		p, err := registry.DecodeParams(configFeeSpecs, raw)
		if err != nil {
			return nil, err
		}
		f.OverrideCommission(
			int64(p.Float("commission_ppm", 0)),
			p.Float("commission_min_yuan", -1))
		return f, nil
	})

	// zero 存在的意义是**隔离**：想知道某个结果里有多少是被费用吃掉的，
	// 换成它跑一遍就知道了。它不该被当成「默认」使用。
	Fees.Register("zero", "零费用。只用于隔离测试 —— 摩擦是 A 股策略的主要亏损来源，别拿它下结论", zeroFeeSpecs,
		func(json.RawMessage) (Fee, error) { return ZeroFee{}, nil })

	Slippages.Register("fixed_bps", "固定基点滑点：按成交额的万分之几计成本", bpsSlippageSpecs,
		func(raw json.RawMessage) (Slippage, error) {
			p, err := registry.DecodeParams(bpsSlippageSpecs, raw)
			if err != nil {
				return nil, err
			}
			return BpsSlippage{Bps: int64(p.Int("bps", 5))}, nil
		})

	Slippages.Register("none", "无滑点。同样只用于隔离测试", noSlippageSpecs,
		func(json.RawMessage) (Slippage, error) { return NoSlippage{}, nil })
}
