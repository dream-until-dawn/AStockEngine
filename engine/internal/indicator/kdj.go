package indicator

import (
	"fmt"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
)

// KDJ 随机指标。默认参数 (9, 3, 3)。
//
//	RSV = (C - LLV(L,n)) / (HHV(H,n) - LLV(L,n)) × 100
//	K   = (2·K_prev + RSV) / 3        K 初值 50
//	D   = (2·D_prev + K)   / 3        D 初值 50
//	J   = 3K - 2D
//
// K/D 用的是国内平台的 SMA(x, m, 1) 平滑，等价于 α = 1/m 的 EMA；
// 初值取 50 是通达信惯例。这两点与部分国外实现不同，会导致数值对不上。
//
// RSV 是比值，与价格 scale 无关，故本指标不需要 priceScale。
type KDJ struct {
	period  int
	kSmooth int
	dSmooth int

	highs *ring
	lows  *ring

	k, d, j float64
	rsv     float64
	warm    int
}

// NewKDJ 创建 KDJ。常用参数为 (9, 3, 3)。
func NewKDJ(period, kSmooth, dSmooth int) *KDJ {
	if period < 1 {
		period = 9
	}
	if kSmooth < 1 {
		kSmooth = 3
	}
	if dSmooth < 1 {
		dSmooth = 3
	}
	return &KDJ{
		period: period, kSmooth: kSmooth, dSmooth: dSmooth,
		highs: newRing(period), lows: newRing(period),
		k: 50, d: 50, j: 50,
	}
}

func (kd *KDJ) Update(b mktdata.Bar) {
	// scale 传 1：RSV 是比值，分子分母同量纲相消，与 scale 无关
	kd.highs.push(float64(b.High))
	kd.lows.push(float64(b.Low))

	_, hh := kd.highs.minMax()
	ll, _ := kd.lows.minMax()
	c := float64(b.Close)

	rng := hh - ll
	if rng <= 0 {
		// 窗口内最高与最低相等 —— 在本项目的数据里这**必然会发生**：
		// 停牌行的 OHLC 全等于停牌前收盘价（SCHEMA.md 1.3），
		// 连续停牌满 period 根时窗口就完全打平。
		// 取中性值 50，避免除零并防止 J 值飞出。
		kd.rsv = 50
	} else {
		kd.rsv = (c - ll) / rng * 100
	}

	kd.k += (kd.rsv - kd.k) / float64(kd.kSmooth)
	kd.d += (kd.k - kd.d) / float64(kd.dSmooth)
	kd.j = 3*kd.k - 2*kd.d
	kd.warm++
}

// Ready 在喂满 period 根后为真 —— 此前 LLV/HHV 的窗口不完整。
func (kd *KDJ) Ready() bool { return kd.warm >= kd.period }

func (kd *KDJ) Names() []string { return []string{"K", "D", "J"} }

func (kd *KDJ) Values() []float64 { return []float64{kd.k, kd.d, kd.j} }

func (kd *KDJ) K() float64   { return kd.k }
func (kd *KDJ) D() float64   { return kd.d }
func (kd *KDJ) J() float64   { return kd.j }
func (kd *KDJ) RSV() float64 { return kd.rsv }

func (kd *KDJ) Snapshot() State {
	f := make([]float64, 0, 4+2*kd.period)
	f = append(f, kd.k, kd.d, kd.j, kd.rsv)
	f = append(f, kd.highs.buf...)
	f = append(f, kd.lows.buf...)
	return State{
		Kind: "KDJ", Warm: kd.warm, Floats: f,
		Ints: []int64{int64(kd.highs.n), int64(kd.lows.n)},
	}
}

func (kd *KDJ) Restore(st State) error {
	want := 4 + 2*kd.period
	if st.Kind != "KDJ" || len(st.Floats) != want || len(st.Ints) != 2 {
		return fmt.Errorf("KDJ 快照不匹配：kind=%s floats=%d（期望 %d）",
			st.Kind, len(st.Floats), want)
	}
	kd.k, kd.d, kd.j, kd.rsv = st.Floats[0], st.Floats[1], st.Floats[2], st.Floats[3]
	copy(kd.highs.buf, st.Floats[4:4+kd.period])
	copy(kd.lows.buf, st.Floats[4+kd.period:])
	kd.highs.n, kd.lows.n = int(st.Ints[0]), int(st.Ints[1])
	kd.warm = st.Warm
	return nil
}
