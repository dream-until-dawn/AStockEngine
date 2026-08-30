package indicator

import (
	"encoding/json"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// TestCatalogFieldsMatchNames 目录里写的输出字段必须与实例的 Names() 一致。
//
// **这是这个文件存在的主要理由。** 界面按目录给出可选列
// （用户还没添加指标时就得知道「选 KDJ 会多出 K/D/J」），
// 求值时按实例的 Names() 取值。两者分叉会让用户「选得出但取不到」——
// 而且是在跑起来之后才发现。
func TestCatalogFieldsMatchNames(t *testing.T) {
	for _, kind := range Kinds() {
		ind, err := Catalog.Build(kind, nil)
		if err != nil {
			t.Errorf("%s 用默认参数构造失败: %v", kind, err)
			continue
		}
		got, want := ind.Names(), Fields(kind)
		if len(got) != len(want) {
			t.Errorf("%s: 实例有 %v，目录写的是 %v", kind, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: 实例有 %v，目录写的是 %v", kind, got, want)
				break
			}
		}
		// Values 的长度也要对得上 —— 否则按下标取字段会越界
		if len(ind.Values()) != len(want) {
			t.Errorf("%s: Values 长度 %d ≠ 字段数 %d",
				kind, len(ind.Values()), len(want))
		}
	}
}

func TestCatalogHasAllKinds(t *testing.T) {
	want := []string{"donchian", "ema", "kdj", "macd", "rsi", "sma"}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("期望 %v，得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("期望 %v，得到 %v", want, got)
		}
	}
}

// TestCatalogRejectsBadParams 参数不合法要在构造时报错，不能等到跑起来。
func TestCatalogRejectsBadParams(t *testing.T) {
	// MACD 快线不小于慢线
	if _, err := Catalog.Build("macd",
		json.RawMessage(`{"short":30,"long":20,"signal":9}`)); err == nil {
		t.Error("快线 ≥ 慢线时应报错")
	}
	// 超出规格范围
	if _, err := Catalog.Build("sma", json.RawMessage(`{"period":9999}`)); err == nil {
		t.Error("周期超出上限时应报错")
	}
	if _, err := Catalog.Build("sma", json.RawMessage(`{"perid":20}`)); err == nil {
		t.Error("参数名拼错时应报错")
	}
}

// TestCatalogBuildsUsable 构造出来的指标喂满 bar 之后要就绪且有值。
func TestCatalogBuildsUsable(t *testing.T) {
	for _, kind := range Kinds() {
		ind, err := Catalog.Build(kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := int64(10_000)
		for i := 0; i < 300; i++ {
			p += int64((i*37)%201) - 100
			if p < 1000 {
				p = 1000
			}
			ind.Update(mktdata.Bar{
				High: p + 50, Low: p - 50, Close: p, Open: p, PreClose: p,
				Volume: 1000, Amount: 1000 * p, TradeStatus: 1,
			})
		}
		if !ind.Ready() {
			t.Errorf("%s 喂了 300 根还没就绪", kind)
		}
	}
}
