import { useEffect, useRef } from 'react'
import {
  AreaSeries, ColorType, CrosshairMode, LineSeries, createChart,
  type IChartApi, type Time,
} from 'lightweight-charts'
import type { CurvePoint } from '../types'

// 净值曲线：策略 vs 基准（上），回撤（下）。
//
// 两条线都**归一化到初始资金**，画在同一根坐标轴上 ——
// 单看策略 +11% 觉得还行，跟基准摆一起才知道跑输了 15 个点。
// 回撤单开一格：它和净值的量纲不同，挤在一起谁也看不清。

function day(d: number): Time {
  const s = String(d)
  return `${s.slice(0, 4)}-${s.slice(4, 6)}-${s.slice(6, 8)}` as Time
}

function theme() {
  const dark = matchMedia('(prefers-color-scheme: dark)').matches
  return dark
    ? { bg: '#1b1e24', text: '#e4e7ec', grid: '#262b34', border: '#333944' }
    : { bg: '#ffffff', text: '#1f2329', grid: '#eef0f3', border: '#dcdfe6' }
}

export default function EquityChart({
  curve, initialCents, benchName, height = 380,
}: {
  curve: CurvePoint[]
  initialCents: number
  benchName?: string
  height?: number
}) {
  const box = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = box.current
    if (!el || curve.length === 0) return
    const t = theme()

    const chart: IChartApi = createChart(el, {
      autoSize: true,
      layout: {
        background: { type: ColorType.Solid, color: t.bg },
        textColor: t.text,
        fontFamily: 'ui-monospace, Consolas, monospace',
        panes: { separatorColor: t.border, separatorHoverColor: t.text, enableResize: true },
      },
      grid: { vertLines: { color: t.grid }, horzLines: { color: t.grid } },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: t.border, scaleMargins: { top: 0.1, bottom: 0.1 } },
      timeScale: { borderColor: t.border, rightOffset: 2 },
      localization: { priceFormatter: (v: number) => v.toFixed(2) },
    })

    const yuan = (c: number) => c / 100

    const strat = chart.addSeries(LineSeries, {
      color: '#2563eb', lineWidth: 2, priceLineVisible: false,
      title: '策略',
    }, 0)
    strat.setData(curve.map((p) => ({ time: day(p.d), value: yuan(p.equity) })))

    // 基准只画有数据的日子。**覆盖不到的不连线** ——
    // 拉一条直线过去会让人以为那段基准没涨没跌，而事实是那段没有数据
    const withBench = curve.filter((p) => (p.bench ?? 0) > 0)
    if (withBench.length > 1) {
      const bench = chart.addSeries(LineSeries, {
        color: '#94a3b8', lineWidth: 1, priceLineVisible: false, lastValueVisible: true,
        title: benchName ?? '基准',
      }, 0)
      bench.setData(withBench.map((p) => ({ time: day(p.d), value: yuan(p.bench!) })))
    }

    // 初始资金水平线：盈亏分界，比看纵轴刻度直观
    strat.createPriceLine({
      price: yuan(initialCents), color: t.border, lineWidth: 1,
      lineStyle: 2, axisLabelVisible: true, title: '初始',
    })

    // ---- 回撤 ----
    let peak = curve[0].equity
    const dd = curve.map((p) => {
      if (p.equity > peak) peak = p.equity
      const ratio = peak > 0 ? (p.equity - peak) / peak : 0
      return { time: day(p.d), value: ratio * 100 }
    })
    const ddSeries = chart.addSeries(AreaSeries, {
      lineColor: '#d13b3b', topColor: 'rgba(209,59,59,0.05)',
      bottomColor: 'rgba(209,59,59,0.35)', lineWidth: 1,
      priceLineVisible: false, lastValueVisible: false,
      priceFormat: { type: 'price', precision: 2, minMove: 0.01 },
    }, 1)
    ddSeries.setData(dd)

    const panes = chart.panes()
    if (panes.length > 1) {
      // 只设主图高度，剩下的留给回撤 —— 逐个给副图 setHeight 会互相挤压
      panes[0].setHeight(Math.max(180, Math.round(height * 0.68)))
    }
    chart.timeScale().fitContent()

    const mq = matchMedia('(prefers-color-scheme: dark)')
    const onScheme = () => {
      const n = theme()
      chart.applyOptions({
        layout: { background: { type: ColorType.Solid, color: n.bg }, textColor: n.text },
        grid: { vertLines: { color: n.grid }, horzLines: { color: n.grid } },
      })
    }
    mq.addEventListener('change', onScheme)
    return () => {
      mq.removeEventListener('change', onScheme)
      chart.remove()
    }
  }, [curve, initialCents, benchName, height])

  if (curve.length === 0) return <div className="empty">没有净值数据</div>
  return <div ref={box} className="chart" style={{ height }} />
}
