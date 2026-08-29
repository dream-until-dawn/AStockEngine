package indicator

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// MACD 指数平滑异同移动平均。
//
// 采用**国内平台（通达信 / 同花顺 / 东财）的口径**，与国外常见实现有两点差异，
// 都会导致数值对不上，故在此写明：
//
//	1. EMA 初值取第一个价格，而非先用 SMA 预热 period 根
//	2. 柱状值 **乘 2**：MACD = (DIF - DEA) × 2
//	   国外实现通常直接取 DIF - DEA（不乘 2）
//
// 计算链：
//
//	DIF  = EMA(short) - EMA(long)          默认 12 / 26
//	DEA  = EMA(signal) of DIF              默认 9，初值取第一个 DIF
//	MACD = (DIF - DEA) × 2
type MACD struct {
	short, long, signal int
	fast, slow          *EMA
	dea                 *EMA
	dif, deaV, hist     float64
	warm                int
}

// NewMACD 创建 MACD。常用参数为 (12, 26, 9)。
func NewMACD(short, long, signal int, priceScale float64) *MACD {
	if short < 1 {
		short = 12
	}
	if long < 1 {
		long = 26
	}
	if signal < 1 {
		signal = 9
	}
	return &MACD{
		short: short, long: long, signal: signal,
		fast: NewEMA(short, priceScale),
		slow: NewEMA(long, priceScale),
		// DEA 平滑的是 DIF 序列（已是「元」量纲），故 scale 传 1
		dea: NewEMA(signal, 1),
	}
}

func (m *MACD) Update(b mktdata.Bar) {
	m.fast.Update(b)
	m.slow.Update(b)
	m.dif = m.fast.value - m.slow.value
	m.dea.UpdateValue(m.dif)
	m.deaV = m.dea.value
	m.hist = (m.dif - m.deaV) * 2
	m.warm++
}

// Ready 在喂满 long + signal 根后为真。
//
// 由于 EMA 初值取第一个价格，形式上第一根就有值，但那是无意义的 ——
// 快慢线尚未分化。取 long+signal 是与国内平台一致的保守口径。
func (m *MACD) Ready() bool { return m.warm >= m.long+m.signal }

func (m *MACD) Names() []string { return []string{"DIF", "DEA", "MACD"} }

func (m *MACD) Values() []float64 { return []float64{m.dif, m.deaV, m.hist} }

// DIF / DEA / Hist 提供具名访问，避免策略里出现 Values()[0] 这类魔法下标。
func (m *MACD) DIF() float64  { return m.dif }
func (m *MACD) DEA() float64  { return m.deaV }
func (m *MACD) Hist() float64 { return m.hist }

func (m *MACD) Snapshot() State {
	return State{
		Kind: "MACD",
		Warm: m.warm,
		Floats: []float64{
			m.fast.value, m.slow.value, m.dea.value,
			m.dif, m.deaV, m.hist,
		},
		Ints: []int64{int64(m.fast.warm), int64(m.slow.warm), int64(m.dea.warm)},
	}
}

func (m *MACD) Restore(st State) error {
	if st.Kind != "MACD" || len(st.Floats) != 6 || len(st.Ints) != 3 {
		return fmt.Errorf("MACD 快照不匹配：kind=%s floats=%d ints=%d",
			st.Kind, len(st.Floats), len(st.Ints))
	}
	m.fast.value, m.slow.value, m.dea.value = st.Floats[0], st.Floats[1], st.Floats[2]
	m.dif, m.deaV, m.hist = st.Floats[3], st.Floats[4], st.Floats[5]
	m.fast.warm, m.slow.warm, m.dea.warm = int(st.Ints[0]), int(st.Ints[1]), int(st.Ints[2])
	m.warm = st.Warm
	return nil
}
