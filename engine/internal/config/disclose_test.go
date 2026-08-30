package config

import (
	"strings"
	"testing"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

func fakeUniverse(markets ...mktdata.Market) (*mktdata.Universe, []mktdata.InstrumentID) {
	insts := make([]*mktdata.Instrument, 0, len(markets))
	ids := make([]mktdata.InstrumentID, 0, len(markets))
	for i, m := range markets {
		id := mktdata.InstrumentID(i + 1)
		insts = append(insts, &mktdata.Instrument{ID: id, Market: m})
		ids = append(ids, id)
	}
	return mktdata.NewUniverse(insts), ids
}

func hasFunding(ds []string) bool {
	for _, d := range ds {
		if strings.Contains(d, "资金费率") {
			return true
		}
	}
	return false
}

// TestDisclosuresFlagFundingForCrypto 标的池里有永续合约时必须提示资金费率未计入。
//
// 这是本项目里最危险的那类缺口：它不报错、不异常，只是让做多为主的结果
// 一致地偏乐观。实测 BTC 年化约 4.4%，比一个年换手 24 倍的 A 股策略的
// 全部摩擦还贵。
func TestDisclosuresFlagFundingForCrypto(t *testing.T) {
	c := &Config{}
	uni, ids := fakeUniverse(mktdata.MarketAShare, mktdata.MarketCrypto)
	if !hasFunding(c.Disclosures(uni, ids)) {
		t.Error("标的池含加密标的时应提示资金费率未计入")
	}
}

// TestDisclosuresNoFundingForAShare 纯 A 股不该提资金费率 —— 那会让真正的
// 提示淹没在噪音里。
func TestDisclosuresNoFundingForAShare(t *testing.T) {
	c := &Config{}
	uni, ids := fakeUniverse(mktdata.MarketAShare, mktdata.MarketAShare)
	if hasFunding(c.Disclosures(uni, ids)) {
		t.Error("纯 A 股标的池不该提资金费率")
	}
}

// TestDisclosuresAlwaysNonEmpty 任何一次回测都有未建模的东西。
//
// 返回空列表意味着「本次回测把一切都算进去了」—— 那句话永远是假的。
func TestDisclosuresAlwaysNonEmpty(t *testing.T) {
	c := &Config{}
	uni, ids := fakeUniverse(mktdata.MarketAShare)
	if len(c.Disclosures(uni, ids)) == 0 {
		t.Error("披露列表不该为空")
	}
	if len(c.Disclosures(nil, nil)) == 0 {
		t.Error("即使没有标的池也应给出通用的未建模项")
	}
}
