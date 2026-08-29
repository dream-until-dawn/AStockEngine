package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"

	"github.com/dream-until-dawn/AStockEngine/engine/internal/mktdata"
	"github.com/dream-until-dawn/AStockEngine/engine/internal/trading"
)

// 输出指纹在**引擎内**滚动计算，而不是事后从 Recorder 的成交流算。
//
// 理由是 recorder.level 不该影响它：`none` 级根本不留成交，
// 若指纹依赖记录，换个记录级别指纹就变了 —— 而记录级别只决定
// 留多少东西给人看，不决定算什么。
//
// 代价是每笔成交多一次哈希写入（微秒级，实测 1,787 笔无可测影响）。

// fillDigest 把一笔成交写进滚动哈希。
//
// 只写**决定结果的字段**：时点、标的、方向、价、量、费、滑点。
// Tag 不写 —— 它是策略自定的归因标签，改个字符串不该让指纹变。
func fillDigest(h hash.Hash, f trading.Fill) {
	fmt.Fprintf(h, "%d|%d|%d|%d|%d|%d|%d\n",
		f.At.TradingDay, int32(f.Instrument), int8(f.Side),
		f.Price, f.Qty, f.Fee.Total, f.SlippageCents)
}

// ResultFingerprint 返回到目前为止的输出指纹。
//
// **不修改滚动状态**，因此可以在任意时刻调用（单步调试每步问一次也行）。
// 末尾混入最终账本：现金、已实现、费用、滑点、以及按 ID 排序的持仓 ——
// 成交流理论上能决定账本，但公司行动、T+1 解冻、取整都在账本这一侧，
// 把账本也纳入才能让指纹覆盖它们。
func (e *Engine) ResultFingerprint() string {
	h := sha256.New()
	h.Write(e.resultHash.Sum(nil)) // Sum 不改变原哈希的状态

	pf := e.pf
	fmt.Fprintf(h, "ledger|%d|%d|%d|%d|%d\n",
		e.steps, pf.Cash, pf.RealizedCents, pf.TotalFeeCents(), pf.SlippageCents)

	// map 遍历顺序随机 —— 不排序会让同一次运行每次算出不同指纹
	ids := make([]mktdata.InstrumentID, 0, len(pf.Positions))
	for id, p := range pf.Positions {
		if p.Total > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		p := pf.Positions[id]
		fmt.Fprintf(h, "pos|%d|%d|%d\n", int32(id), p.Total, p.CostCents)
	}
	return hex.EncodeToString(h.Sum(nil))
}
