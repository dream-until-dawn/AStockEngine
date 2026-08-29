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
}

var zeroFeeSpecs = []spec.ParamSpec{}

// ---- Slippage ----

var bpsSlippageSpecs = []spec.ParamSpec{
	{Name: "bps", Kind: spec.ParamInt, Default: 5, Min: 0, Max: 1000, Step: 1,
		Desc: "滑点基点：买入价上浮、卖出价下压万分之 bps/10"},
}

var noSlippageSpecs = []spec.ParamSpec{}

func init() {
	Markets.Register("ashare", ashareMarketSpecs,
		func(json.RawMessage) (Market, error) { return NewAShareMarket(), nil })

	Fees.Register("config", configFeeSpecs, func(raw json.RawMessage) (Fee, error) {
		path, err := registry.DecodeString(configFeeSpecs, raw, "path")
		if err != nil {
			return nil, err
		}
		if path == "" {
			return nil, fmt.Errorf("fee.params.path 不能为空")
		}
		return LoadFee(path)
	})

	// zero 存在的意义是**隔离**：想知道某个结果里有多少是被费用吃掉的，
	// 换成它跑一遍就知道了。它不该被当成「默认」使用。
	Fees.Register("zero", zeroFeeSpecs,
		func(json.RawMessage) (Fee, error) { return ZeroFee{}, nil })

	Slippages.Register("fixed_bps", bpsSlippageSpecs,
		func(raw json.RawMessage) (Slippage, error) {
			p, err := registry.DecodeParams(bpsSlippageSpecs, raw)
			if err != nil {
				return nil, err
			}
			return BpsSlippage{Bps: int64(p.Int("bps", 5))}, nil
		})

	Slippages.Register("none", noSlippageSpecs,
		func(json.RawMessage) (Slippage, error) { return NoSlippage{}, nil })
}
