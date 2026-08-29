package config

import (
	"encoding/json"
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/fingerprint"
)

// Canonical 返回配置的规范化 JSON —— **只含影响结果的字段**。
//
// 哪些字段进指纹是设计决定，不是实现细节：
//
//	进：strategy / sizer / risk / fee / slippage / market 的实现名与参数
//	    data.from / to / market / freq / universe
//	    engine.* / portfolio.* / broker.*
//	不进：name（人给的标签）
//	      data.root（换个目录放不该改变结果；数据本身由数据指纹覆盖）
//	      metrics.*（绩效是对结果的**事后度量**，不参与产生结果）
//	      recorder.level（只决定留多少东西给人看）
//
// 规范化的手段是过一遍 map[string]any 再 Marshal —— Go 的 json 对 map
// 按键排序，于是配置里写字段的先后顺序不再影响指纹。
func (c *Config) Canonical() ([]byte, error) {
	mod := func(m Module) (any, error) {
		out := map[string]any{"impl": m.Impl}
		p, err := canonicalRaw(m.Params)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out["params"] = p
		}
		return out, nil
	}

	risks := make([]any, 0, len(c.Risk))
	for i, r := range c.Risk {
		v, err := mod(r)
		if err != nil {
			return nil, fmt.Errorf("risk[%d]: %w", i, err)
		}
		risks = append(risks, v)
	}

	var err error
	root := map[string]any{}
	for key, m := range map[string]Module{
		"market": c.Market, "fee": c.Fee, "slippage": c.Slippage,
		"sizer": c.Sizer, "strategy": c.Strategy,
	} {
		if root[key], err = mod(m); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	root["risk"] = risks

	// fee.params.path 只留文件名不留目录：同一份费率配置换个位置放
	// 不该改变指纹，而费率**内容**由下面的 feeDigest 覆盖
	if c.Fee.Impl == "config" {
		d, err := c.feeDigest()
		if err != nil {
			return nil, err
		}
		root["fee"] = map[string]any{"impl": "config", "content": d}
	}

	u := c.Data.Univers
	root["data"] = map[string]any{
		"market": c.Data.Market, "freq": c.Data.Freq,
		"from": c.Data.From, "to": c.Data.To,
		"universe": map[string]any{
			"symbols": u.Symbols, "type": u.Type, "board": u.Board,
			"exchange": u.Exchange, "status": u.Status,
			"require_factor": u.RequireFactor, "limit": u.Limit,
		},
	}

	allowPartial := true
	if c.Broker.AllowPartialFill != nil {
		allowPartial = *c.Broker.AllowPartialFill
	}
	implySplit := true
	if c.Engine.ImplySplitFromFactor != nil {
		implySplit = *c.Engine.ImplySplitFromFactor
	}
	root["broker"] = map[string]any{
		"volume_cap_ppm": c.Broker.VolumeCapPPM, "allow_partial_fill": allowPartial,
	}
	root["portfolio"] = map[string]any{
		"initial_cash_cents": c.Portfolio.InitialCashCents,
		"dividend_tax_ppm":   c.Portfolio.DividendTaxPPM,
	}
	root["engine"] = map[string]any{
		"indicator_adj": c.Engine.IndicatorAdj, "imply_split_from_factor": implySplit,
	}
	return json.Marshal(root)
}

// feeDigest 取费率配置文件的内容摘要。
//
// 费率**内容**必须进指纹（换一套费率结果就变了），但**路径**不该进 ——
// 同一份配置换个目录放，结果是一样的。
func (c *Config) feeDigest() (string, error) {
	path, err := decodeFeePath(c.Fee.Params)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("fee.params.path 为空")
	}
	b, err := readFile(c.resolvePath(path))
	if err != nil {
		return "", fmt.Errorf("读取费率配置失败: %w", err)
	}
	// 先规范化再摘要：费率 JSON 里字段顺序或缩进变了不该改变指纹
	v, err := canonicalRaw(b)
	if err != nil {
		return "", fmt.Errorf("费率配置不是合法 JSON: %w", err)
	}
	cb, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return fingerprint.Hex(cb), nil
}

// canonicalRaw 把一段 JSON 过一遍 any 再吐出来，从而让对象的键有序。
func canonicalRaw(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// InputFingerprint 计算输入指纹：规范化配置 ‖ 数据指纹 ‖ 引擎版本。
func (c *Config) InputFingerprint(dataFP string) (string, error) {
	canon, err := c.Canonical()
	if err != nil {
		return "", err
	}
	return fingerprint.Hex(canon, []byte(dataFP), []byte(fingerprint.EngineVersion())), nil
}

// DataRoot 返回解析后的数据根目录绝对路径。
//
// 配置里的相对路径是**相对配置文件所在目录**解析的，不是相对进程 CWD ——
// 后者会让同一份配置在不同目录下跑出不同结果。
func (c *Config) DataRoot() string { return c.resolvePath(c.Data.Root) }
