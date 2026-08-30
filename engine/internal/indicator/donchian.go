package indicator

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// Donchian 唐奇安通道：过去 n 根的最高价与最低价。
//
//	upper = HHV(High, n)   lower = LLV(Low, n)   mid = (upper+lower)/2
//
// 加它是为了给策略池补第三个维度。均线类（MA/MACD）看的是「价格与
// 均值的关系」，RSI 看的是「涨跌动能的失衡」，突破看的是**价格是否
// 走出了近期的区间** —— 三者对「趋势」的定义不同，才谈得上正交。
//
// # 这里有一个必须写死的语义：窗口不含当前这根
//
// 引擎的顺序是「先 Update 全部指标，再调 OnBar」，所以策略看到的指标
// 默认是**含今日**的。对 MACD 那是对的（今日收盘的 MACD），
// 对突破则会**让信号永远无法成立**：
//
//	含今日 → upper = max(..., today.High) ≥ today.High
//	       → 「今日突破 upper」退化成「today.High ≥ today.High」，恒真
//
// 于是 Update 先用**旧窗口**算出通道，再把这根推进去。
// Values() 返回的因此是「截至昨日的通道」，策略可以直接拿今日价格去比。
// 这不是可配置项 —— 含今日的通道没有任何策略用途。
type Donchian struct {
	period int
	scale  float64

	highs *ring
	lows  *ring

	upper, lower float64
	warm         int
}

// NewDonchian 创建唐奇安通道。常用周期 20（短）/ 55（长）。
func NewDonchian(period int, priceScale float64) *Donchian {
	if period < 1 {
		period = 20
	}
	return &Donchian{
		period: period, scale: priceScale,
		highs: newRing(period), lows: newRing(period),
	}
}

func (d *Donchian) Update(b mktdata.Bar) {
	// 先取旧窗口 —— 顺序在这里就是语义，交换两段代码会让通道含今日，
	// 突破信号随之恒真（见类型注释）
	if d.highs.count() > 0 {
		_, hh := d.highs.minMax()
		ll, _ := d.lows.minMax()
		d.upper, d.lower = hh, ll
	}
	d.highs.push(priceOf(b.High, d.scale))
	d.lows.push(priceOf(b.Low, d.scale))
	d.warm++
}

// Ready 需要 period+1 根：前 period 根填满窗口，第 period+1 根才有
// 「不含自己」的完整通道可比。
func (d *Donchian) Ready() bool     { return d.warm > d.period }
func (d *Donchian) Names() []string { return []string{"UPPER", "LOWER", "MID"} }

func (d *Donchian) Values() []float64 {
	if !d.Ready() {
		return []float64{0, 0, 0}
	}
	return []float64{d.upper, d.lower, (d.upper + d.lower) / 2}
}

func (d *Donchian) Snapshot() State {
	f := make([]float64, 0, 2*d.period+2)
	f = append(f, d.upper, d.lower)
	f = append(f, d.highs.buf...)
	f = append(f, d.lows.buf...)
	return State{Kind: "Donchian", Warm: d.warm, Floats: f,
		Ints: []int64{int64(d.highs.n), int64(d.lows.n)}}
}

func (d *Donchian) Restore(st State) error {
	if st.Kind != "Donchian" || len(st.Floats) != 2*d.period+2 || len(st.Ints) != 2 {
		return fmt.Errorf("Donchian 快照不匹配：kind=%s floats=%d", st.Kind, len(st.Floats))
	}
	d.upper, d.lower = st.Floats[0], st.Floats[1]
	copy(d.highs.buf, st.Floats[2:2+d.period])
	copy(d.lows.buf, st.Floats[2+d.period:])
	d.highs.n, d.lows.n = int(st.Ints[0]), int(st.Ints[1])
	d.warm = st.Warm
	return nil
}

var _ Indicator = (*Donchian)(nil)
