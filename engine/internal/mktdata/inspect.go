package mktdata

import "sort"

// 本文件的访问器只服务于**数据核对**（engine/cmd/server），不在引擎热路径上。
//
// 之所以单独成文件：它们按标的横向取数，与引擎按时点纵向推进的访问模式相反。
// 放在一起容易让人误以为策略也可以这样取数 —— 那正是 C1 要禁止的。

// FactorPoint 是一个复权因子事件，供核对视图展示。
type FactorPoint struct {
	ExDate int32
	Factor int64 // 定点，scale = FactorScale
}

// Factors 返回某标的的全部复权因子事件，按除权日升序。
func (a *Adjuster) Factors(id InstrumentID) []FactorPoint {
	pts := a.byInstrument[id]
	out := make([]FactorPoint, len(pts))
	for i, p := range pts {
		out[i] = FactorPoint{ExDate: p.exDate, Factor: p.factor}
	}
	return out
}

// TotalFactorEvents 返回全部标的的因子事件总数。
func (a *Adjuster) TotalFactorEvents() int {
	n := 0
	for _, pts := range a.byInstrument {
		n += len(pts)
	}
	return n
}

// ByInstrument 返回某标的的全部公司行动记录，按除权日升序。
//
// 与 OnDay 不同，这里**包含全零行** —— 核对时需要区分
// 「表里有一行但内容为零」与「表里根本没有这一行」。
func (c *CorpActions) ByInstrument(id InstrumentID) []CorpAction {
	if c.byInst == nil {
		c.byInst = make(map[InstrumentID][]CorpAction, 8192)
		for _, a := range c.all {
			c.byInst[a.Instrument] = append(c.byInst[a.Instrument], a)
		}
		for _, v := range c.byInst {
			sort.Slice(v, func(i, j int) bool { return v[i].ExDate < v[j].ExDate })
		}
	}
	return c.byInst[id]
}

// TotalRows 返回 corporate_action 表的原始行数（含全零行）。
func (c *CorpActions) TotalRows() int { return len(c.all) }
