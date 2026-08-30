package config

import (
	"encoding/json"
	"testing"
)

// feeParams 把相对路径解成绝对路径时，**不能把同段里的其他参数丢掉**。
//
// 早先它是直接重建一个 `{"path": ...}` 交出去的，于是 commission_ppm
// （佣金覆盖）整个消失 —— 而且不报错：引擎照常跑，只是覆盖没生效，
// 费用还是文件里的默认值。这类「悄悄不生效」比崩溃难查得多。
func TestFeeParamsKeepsOtherParams(t *testing.T) {
	c := &Config{dir: "/cfg"}
	c.Fee.Impl = "config"
	c.Fee.Params = json.RawMessage(
		`{"path":"../fee/x.json","commission_ppm":85,"commission_min_yuan":0}`)

	var got map[string]any
	if err := json.Unmarshal(c.feeParams(), &got); err != nil {
		t.Fatal(err)
	}
	if got["commission_ppm"] != 85.0 {
		t.Errorf("commission_ppm 丢了：%v", got)
	}
	if got["commission_min_yuan"] != 0.0 {
		t.Errorf("commission_min_yuan 丢了：%v", got)
	}
	p, _ := got["path"].(string)
	if p == "" || p == "../fee/x.json" {
		t.Errorf("path 应被解成绝对路径，得到 %q", p)
	}
}

func TestFeeParamsPassThroughForOtherImpls(t *testing.T) {
	c := &Config{dir: "/cfg"}
	c.Fee.Impl = "zero"
	c.Fee.Params = json.RawMessage(`{"whatever":1}`)
	if string(c.feeParams()) != `{"whatever":1}` {
		t.Errorf("非 config 实现应原样透传，得到 %s", c.feeParams())
	}
}
