// Package indicator 提供**增量式**技术指标。
//
// 增量不只是为了快（实测每步重算 MA20 比顺序扫描慢 26.8x），
// 更是约束 C1 的结构性保证：指标每步只 Update 一根 bar、内部维护状态，
// **它从来没有拿到过完整序列，因此连「看到未来」的能力都没有**。
//
// 数值约定（见 docs/DESIGN-v0.2-dataflow.md 4.4）：
// 内部一律用 float64。价格用定点是为了 C5（并发累加顺序不同导致结果不同），
// 而指标是单标的顺序更新，不存在并发累加，float64 的确定性足够；
// 且均线、RSI 这类量本身就是分数，用定点反而要处理除法精度。
package indicator

import "github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"

// Indicator 是全部技术指标的统一接口。
type Indicator interface {
	// Update 喂入一根 bar。每步恰好调用一次，O(1)。
	Update(b mktdata.Bar)
	// Ready 报告预热是否完成。**策略必须检查**——预热期内的值是垃圾，
	// 据此下单会让回测的前 N 步产生虚假交易。
	Ready() bool
	// Values 返回当前值。多值指标（MACD/KDJ）按固定顺序返回。
	Values() []float64
	// Names 返回与 Values 对应的名称，供展示与调试。
	Names() []string
	// Snapshot / Restore 服务于 C6：实盘每天从快照恢复，而非重算多年。
	Snapshot() State
	Restore(State) error
}

// State 是指标的可序列化状态。用 float64 切片而非各指标自定结构，
// 是为了让快照格式与指标实现解耦 —— 新增指标不改快照层。
type State struct {
	Kind   string    `json:"kind"`
	Warm   int       `json:"warm"`   // 已喂入的 bar 数
	Floats []float64 `json:"floats"` // 指标自定的状态向量
	Ints   []int64   `json:"ints"`
}

// priceOf 把定点价格换算为「元」。
//
// 指标之所以需要知道 scale，是为了让 MACD 这类**有量纲**的指标
// 输出以元计价的值；KDJ 这类比值型指标其实与 scale 无关。
func priceOf(v int64, scale float64) float64 { return float64(v) / scale }

// DefaultPriceScale 与 SCHEMA.md 0.2 一致。
// 实际应由 instruments.price_scale 逐标的给出。
const DefaultPriceScale = float64(mktdata.PriceScale)

// ring 是定长环形缓冲，供需要固定窗口的指标复用。
//
// 取值时线性扫描而非单调队列：窗口小时（KDJ 用 9）扫描更简单，
// 且状态可直接序列化。**窗口超过约 100 时应改用单调队列**，
// 届时快照格式需要相应调整。
type ring struct {
	buf  []float64
	n    int // 已写入总数
	size int
}

func newRing(size int) *ring {
	return &ring{buf: make([]float64, size), size: size}
}

func (r *ring) push(v float64) {
	r.buf[r.n%r.size] = v
	r.n++
}

func (r *ring) full() bool { return r.n >= r.size }

func (r *ring) count() int {
	if r.n < r.size {
		return r.n
	}
	return r.size
}

func (r *ring) minMax() (mn, mx float64) {
	c := r.count()
	if c == 0 {
		return 0, 0
	}
	mn, mx = r.buf[0], r.buf[0]
	for i := 1; i < c; i++ {
		v := r.buf[i]
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	return mn, mx
}

// oldest 返回即将被挤出的值，供滚动求和使用。
func (r *ring) oldest() float64 { return r.buf[r.n%r.size] }
