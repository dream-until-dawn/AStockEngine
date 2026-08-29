package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/dream-until-dawn/AStockEngine/engine/internal/strategies"
)

// writeCfg 把一份配置写到临时目录，顺带造一个最小费率文件。
func writeCfg(t *testing.T, dir string, mutate func(m map[string]any)) string {
	t.Helper()
	fee := filepath.Join(dir, "fee.json")
	if _, err := os.Stat(fee); err != nil {
		must(t, os.WriteFile(fee, []byte(`{
  "name":"t","rules":[{"kind":"commission","side":"both","rate_ppm":250,"min_cents":500}]
}`), 0o644))
	}
	m := map[string]any{
		"name": "t",
		"data": map[string]any{
			"root": "data", "market": "ashare", "freq": "1d",
			"from": 20200101, "to": 0,
			"universe": map[string]any{"type": "stock", "limit": 10},
		},
		"market":    map[string]any{"impl": "ashare"},
		"fee":       map[string]any{"impl": "config", "params": map[string]any{"path": "fee.json"}},
		"slippage":  map[string]any{"impl": "fixed_bps", "params": map[string]any{"bps": 5}},
		"sizer":     map[string]any{"impl": "equal_weight", "params": map[string]any{"slots": 10}},
		"broker":    map[string]any{"volume_cap_ppm": 100000},
		"portfolio": map[string]any{"initial_cash_cents": 100000000},
		"engine":    map[string]any{"indicator_adj": "hfq"},
		"strategy":  map[string]any{"impl": "macd_cross", "params": map[string]any{"short": 12}},
		"recorder":  map[string]any{"level": "summary"},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	must(t, err)
	p := filepath.Join(dir, "cfg.json")
	must(t, os.WriteFile(p, b, 0o644))
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// loadNoValidate 绕开 Validate（它会 dryBuild，需要真实费率文件格式）。
func loadNoValidate(t *testing.T, path string) *Config {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	must(t, dec.Decode(&c))
	c.dir = filepath.Dir(path)
	c.applyDefaults()
	return &c
}

func canon(t *testing.T, c *Config) string {
	t.Helper()
	b, err := c.Canonical()
	must(t, err)
	return string(b)
}

// ---- 验收 1：同配置 → 同指纹 ----

func TestSameConfigSameFingerprint(t *testing.T) {
	dir := t.TempDir()
	c := loadNoValidate(t, writeCfg(t, dir, nil))
	a, err := c.InputFingerprint("DATA")
	must(t, err)
	b, err := c.InputFingerprint("DATA")
	must(t, err)
	if a != b {
		t.Fatalf("同配置两次算出不同指纹：\n  %s\n  %s", a, b)
	}
}

// TestFieldOrderDoesNotMatter 配置里字段的书写顺序不该影响指纹。
func TestFieldOrderDoesNotMatter(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	c1 := loadNoValidate(t, writeCfg(t, d1, nil))
	// 换一份参数书写顺序不同、内容相同的配置
	c2 := loadNoValidate(t, writeCfg(t, d2, func(m map[string]any) {
		m["sizer"] = map[string]any{
			"params": map[string]any{"slots": 10}, "impl": "equal_weight",
		}
	}))
	if canon(t, c1) != canon(t, c2) {
		t.Errorf("字段顺序改变了规范化结果：\n  %s\n  %s", canon(t, c1), canon(t, c2))
	}
}

// ---- 验收 2：改一个影响结果的数 → 指纹改变 ----

func TestSlippageChangesFingerprint(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	c1 := loadNoValidate(t, writeCfg(t, d1, nil))
	c2 := loadNoValidate(t, writeCfg(t, d2, func(m map[string]any) {
		m["slippage"] = map[string]any{"impl": "fixed_bps", "params": map[string]any{"bps": 6}}
	}))
	a, err := c1.InputFingerprint("DATA")
	must(t, err)
	b, err := c2.InputFingerprint("DATA")
	must(t, err)
	if a == b {
		t.Fatal("改了 slippage.bps 指纹却没变 —— 它没进指纹")
	}
}

// TestEveryResultAffectingFieldEntersFingerprint 逐个字段确认它们真的进了指纹。
//
// 漏掉任何一个，指纹就会给出「同配置」的假象，而结果其实不同 ——
// 那比没有指纹更糟。
func TestEveryResultAffectingFieldEntersFingerprint(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"strategy.impl": func(m map[string]any) { m["strategy"] = map[string]any{"impl": "ma_cross"} },
		"strategy.params": func(m map[string]any) {
			m["strategy"] = map[string]any{"impl": "macd_cross", "params": map[string]any{"short": 11}}
		},
		"sizer.impl": func(m map[string]any) { m["sizer"] = map[string]any{"impl": "fixed_cash"} },
		"sizer.params": func(m map[string]any) {
			m["sizer"] = map[string]any{"impl": "equal_weight", "params": map[string]any{"slots": 9}}
		},
		"risk":      func(m map[string]any) { m["risk"] = []any{map[string]any{"impl": "max_positions"}} },
		"data.from": func(m map[string]any) { m["data"].(map[string]any)["from"] = 20210101 },
		"data.universe": func(m map[string]any) {
			m["data"].(map[string]any)["universe"] = map[string]any{"type": "etf", "limit": 10}
		},
		"broker":    func(m map[string]any) { m["broker"] = map[string]any{"volume_cap_ppm": 50000} },
		"portfolio": func(m map[string]any) { m["portfolio"] = map[string]any{"initial_cash_cents": 200000000} },
		"engine":    func(m map[string]any) { m["engine"] = map[string]any{"indicator_adj": "none"} },
	}
	base := loadNoValidate(t, writeCfg(t, t.TempDir(), nil))
	baseFP, err := base.InputFingerprint("DATA")
	must(t, err)

	for name, mut := range cases {
		c := loadNoValidate(t, writeCfg(t, t.TempDir(), mut))
		fp, err := c.InputFingerprint("DATA")
		must(t, err)
		if fp == baseFP {
			t.Errorf("改了 %s 指纹却没变 —— 它没进指纹", name)
		}
	}
}

// ---- 验收 3：只改不影响结果的字段 → 指纹不变 ----

func TestNonResultFieldsExcluded(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"name":           func(m map[string]any) { m["name"] = "换个名字" },
		"recorder.level": func(m map[string]any) { m["recorder"] = map[string]any{"level": "full"} },
		"metrics":        func(m map[string]any) { m["metrics"] = map[string]any{"benchmark": "510300", "risk_free_ppm": 30000} },
	}
	base := loadNoValidate(t, writeCfg(t, t.TempDir(), nil))
	baseFP, err := base.InputFingerprint("DATA")
	must(t, err)

	for name, mut := range cases {
		c := loadNoValidate(t, writeCfg(t, t.TempDir(), mut))
		fp, err := c.InputFingerprint("DATA")
		must(t, err)
		if fp != baseFP {
			t.Errorf("改了 %s 指纹却变了 —— 它不影响结果，不该进指纹", name)
		}
	}
}

// TestDataRootExcluded 数据目录换个位置放，指纹不该变。
//
// 数据**内容**由数据指纹覆盖，路径只是它放在哪。把路径写进指纹会让
// 同一份数据在两台机器上算出不同指纹，而那正是可复现性要否定的。
func TestDataRootExcluded(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	c1 := loadNoValidate(t, writeCfg(t, d1, nil))
	c2 := loadNoValidate(t, writeCfg(t, d2, func(m map[string]any) {
		m["data"].(map[string]any)["root"] = "/somewhere/else/data"
	}))
	a, err := c1.InputFingerprint("DATA")
	must(t, err)
	b, err := c2.InputFingerprint("DATA")
	must(t, err)
	if a != b {
		t.Error("data.root 进了指纹 —— 换个目录放不该改变结果")
	}
}

// TestFeeContentEntersFingerprintNotPath 费率**内容**进指纹，**路径**不进。
func TestFeeContentEntersFingerprintNotPath(t *testing.T) {
	// 同内容不同路径 → 指纹相同
	d1, d2 := t.TempDir(), t.TempDir()
	c1 := loadNoValidate(t, writeCfg(t, d1, nil))
	must(t, os.WriteFile(filepath.Join(d2, "rates.json"),
		[]byte(`{"name":"t","rules":[{"kind":"commission","side":"both","rate_ppm":250,"min_cents":500}]}`), 0o644))
	c2 := loadNoValidate(t, writeCfg(t, d2, func(m map[string]any) {
		m["fee"] = map[string]any{"impl": "config", "params": map[string]any{"path": "rates.json"}}
	}))
	a, err := c1.InputFingerprint("DATA")
	must(t, err)
	b, err := c2.InputFingerprint("DATA")
	must(t, err)
	if a != b {
		t.Error("同一份费率换个文件名，指纹变了 —— 路径进了指纹")
	}

	// 改费率内容 → 指纹必须变
	d3 := t.TempDir()
	must(t, os.WriteFile(filepath.Join(d3, "fee.json"),
		[]byte(`{"name":"t","rules":[{"kind":"commission","side":"both","rate_ppm":300,"min_cents":500}]}`), 0o644))
	c3 := loadNoValidate(t, writeCfg(t, d3, nil))
	c, err := c3.InputFingerprint("DATA")
	must(t, err)
	if c == a {
		t.Error("改了费率内容指纹却没变 —— 换一套费率结果就不同了")
	}
}

// ---- 引擎版本 ----

func TestEngineVersionEntersFingerprint(t *testing.T) {
	c := loadNoValidate(t, writeCfg(t, t.TempDir(), nil))
	a, err := c.InputFingerprint("DATA")
	must(t, err)
	// 数据指纹变了，输入指纹也必须变
	b, err := c.InputFingerprint("OTHER")
	must(t, err)
	if a == b {
		t.Error("数据指纹没进输入指纹")
	}
}
