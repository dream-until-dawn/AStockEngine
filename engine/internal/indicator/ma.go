package indicator

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// SMA 简单移动平均。
//
// 用滚动累加（加新值、减旧值）而非每步重算窗口 —— 后者实测慢 26.8x。
// 代价是浮点漂移：同一配置从不同起始日跑到同一天，值会有微小差异。
// 这不违反 C5（配置含起始日期，同配置仍逐笔一致），但 Walk-Forward
// 的不同窗口在同一日的指标值会不同，属预期行为。
// 漂移量待实测后再决定是否引入 Kahan 补偿求和（设计 4.3）。
type SMA struct {
	period int
	scale  float64
	r      *ring
	sum    float64
	warm   int
}

func NewSMA(period int, priceScale float64) *SMA {
	if period < 1 {
		period = 1
	}
	return &SMA{period: period, scale: priceScale, r: newRing(period)}
}

func (s *SMA) Update(b mktdata.Bar) {
	v := priceOf(b.Close, s.scale)
	if s.r.full() {
		s.sum -= s.r.oldest()
	}
	s.r.push(v)
	s.sum += v
	s.warm++
}

func (s *SMA) Ready() bool      { return s.warm >= s.period }
func (s *SMA) Names() []string  { return []string{"MA"} }
func (s *SMA) Values() []float64 {
	if !s.Ready() {
		return []float64{0}
	}
	return []float64{s.sum / float64(s.period)}
}

func (s *SMA) Snapshot() State {
	f := make([]float64, 0, s.period+1)
	f = append(f, s.sum)
	f = append(f, s.r.buf...)
	return State{Kind: "SMA", Warm: s.warm, Floats: f, Ints: []int64{int64(s.r.n)}}
}

func (s *SMA) Restore(st State) error {
	if st.Kind != "SMA" || len(st.Floats) != s.period+1 || len(st.Ints) != 1 {
		return fmt.Errorf("SMA 快照不匹配：kind=%s floats=%d", st.Kind, len(st.Floats))
	}
	s.sum = st.Floats[0]
	copy(s.r.buf, st.Floats[1:])
	s.r.n = int(st.Ints[0])
	s.warm = st.Warm
	return nil
}

// EMA 指数移动平均，α = 2/(period+1)。
//
// **初值取第一个价格**（通达信 / 同花顺惯例），而非先用 SMA 预热。
// 不同平台此处约定不同，是 MACD 数值对不上的最常见原因，故在此写明。
type EMA struct {
	period int
	alpha  float64
	scale  float64
	value  float64
	warm   int
}

func NewEMA(period int, priceScale float64) *EMA {
	if period < 1 {
		period = 1
	}
	return &EMA{period: period, alpha: 2.0 / float64(period+1), scale: priceScale}
}

func (e *EMA) Update(b mktdata.Bar) { e.UpdateValue(priceOf(b.Close, e.scale)) }

// UpdateValue 直接喂入一个数值，供 MACD 用 EMA 平滑 DIF 序列。
func (e *EMA) UpdateValue(v float64) {
	if e.warm == 0 {
		e.value = v
	} else {
		e.value += e.alpha * (v - e.value)
	}
	e.warm++
}

func (e *EMA) Ready() bool       { return e.warm >= e.period }
func (e *EMA) Names() []string   { return []string{"EMA"} }
func (e *EMA) Values() []float64 { return []float64{e.value} }

func (e *EMA) Snapshot() State {
	return State{Kind: "EMA", Warm: e.warm, Floats: []float64{e.value}}
}

func (e *EMA) Restore(st State) error {
	if st.Kind != "EMA" || len(st.Floats) != 1 {
		return fmt.Errorf("EMA 快照不匹配：kind=%s", st.Kind)
	}
	e.value = st.Floats[0]
	e.warm = st.Warm
	return nil
}
