import { useEffect, useRef } from 'react'
import {
  CandlestickSeries, ColorType, CrosshairMode, HistogramSeries, LineSeries,
  createChart, createSeriesMarkers,
  type IChartApi, type ISeriesApi, type SeriesMarker, type Time, type UTCTimestamp,
} from 'lightweight-charts'
import type { KBar, Kline } from '../types'

// 三窗格：主图（K 线 + 均线）/ MACD / KDJ。
//
// 全部数值直接来自接口 —— 前端**一行指标都不算**。
// 自己再算一遍就等于新增一条实现，两条对不上时反而分不清谁错了，
// 而这个页面存在的意义正是判断谁对。

const MA_COLORS = ['#e8a33d', '#4f9dfa', '#c464e0', '#3fbf8f', '#ef6c6c', '#8b93a3']

function day(d: number): Time {
  const s = String(d)
  return `${s.slice(0, 4)}-${s.slice(4, 6)}-${s.slice(6, 8)}` as Time
}

type Theme = {
  bg: string; text: string; grid: string; border: string
  up: string; down: string; muted: string
}

function theme(): Theme {
  const dark = matchMedia('(prefers-color-scheme: dark)').matches
  return dark
    ? { bg: '#1b1e24', text: '#e4e7ec', grid: '#262b34', border: '#333944',
        up: '#f06a6a', down: '#3fc79a', muted: '#8b93a3' }
    : { bg: '#ffffff', text: '#1f2329', grid: '#eef0f3', border: '#dcdfe6',
        up: '#d13b3b', down: '#14a06e', muted: '#7a8194' }
}

export default function CandleChart({
  data, priceScale, height = 620, onHover,
}: {
  data: Kline
  priceScale: number
  height?: number
  onHover?: (bar: KBar | null) => void
}) {
  const box = useRef<HTMLDivElement>(null)
  const hoverRef = useRef(onHover)
  hoverRef.current = onHover

  useEffect(() => {
    const el = box.current
    if (!el) return
    const t = theme()

    const chart: IChartApi = createChart(el, {
      autoSize: true,
      layout: {
        background: { type: ColorType.Solid, color: t.bg },
        textColor: t.text,
        fontFamily: 'ui-monospace, Consolas, monospace',
        panes: { separatorColor: t.border, separatorHoverColor: t.muted, enableResize: true },
      },
      grid: { vertLines: { color: t.grid }, horzLines: { color: t.grid } },
      crosshair: { mode: CrosshairMode.Normal },
      rightPriceScale: { borderColor: t.border, scaleMargins: { top: 0.08, bottom: 0.08 } },
      timeScale: { borderColor: t.border, rightOffset: 4, minBarSpacing: 0.5 },
      localization: {
        priceFormatter: (p: number) => p.toFixed(Math.round(Math.log10(priceScale))),
      },
    })

    // ---- 主图 ----
    const candles: ISeriesApi<'Candlestick'> = chart.addSeries(CandlestickSeries, {
      upColor: t.up, downColor: t.down,
      borderUpColor: t.up, borderDownColor: t.down,
      wickUpColor: t.up, wickDownColor: t.down,
      priceFormat: { type: 'price', precision: 3, minMove: 1 / priceScale },
    }, 0)

    const px = (v: number) => v / priceScale
    candles.setData(data.bars.map((b) => ({
      time: day(b.d), open: px(b.o), high: px(b.h), low: px(b.l), close: px(b.c),
    })))

    // 除权日打标：不复权模式下的价格跳变全出在这些日子，
    // 标出来才能一眼确认「跳变是除权造成的」而不是数据错了。
    const marks: SeriesMarker<Time>[] = data.bars
      .filter((b) => b.ex)
      .map((b) => ({
        time: day(b.d), position: 'belowBar' as const,
        color: t.muted, shape: 'arrowUp' as const, text: '除',
      }))
    if (marks.length > 0 && marks.length <= 400) createSeriesMarkers(candles, marks)

    // 均线：接口给什么画什么，周期由后端参数决定
    const maSpecs = data.indicators.filter((s) => s.pane === 'main')
    maSpecs.forEach((sp, i) => {
      const line = chart.addSeries(LineSeries, {
        color: MA_COLORS[i % MA_COLORS.length], lineWidth: 1,
        priceLineVisible: false, lastValueVisible: false, crosshairMarkerVisible: false,
      }, 0)
      line.setData(data.bars
        // 预热期的指标值是垃圾，不画 —— 画出来会让人以为那段也可用
        .filter((b) => b.ready[sp.key] && b.ind[sp.key]?.[0] !== undefined)
        .map((b) => ({ time: day(b.d), value: b.ind[sp.key][0] })))
    })

    // ---- MACD ----
    const macd = data.indicators.find((s) => s.pane === 'macd')
    if (macd) {
      const hist = chart.addSeries(HistogramSeries, {
        priceLineVisible: false, lastValueVisible: false,
        priceFormat: { type: 'price', precision: 4, minMove: 0.0001 },
      }, 1)
      hist.setData(data.bars.filter((b) => b.ready[macd.key]).map((b) => ({
        time: day(b.d), value: b.ind[macd.key][2],
        color: b.ind[macd.key][2] >= 0 ? t.up : t.down,
      })))
      const difL = chart.addSeries(LineSeries, {
        color: '#e8a33d', lineWidth: 1, priceLineVisible: false, lastValueVisible: false,
      }, 1)
      const deaL = chart.addSeries(LineSeries, {
        color: '#4f9dfa', lineWidth: 1, priceLineVisible: false, lastValueVisible: false,
      }, 1)
      const ready = data.bars.filter((b) => b.ready[macd.key])
      difL.setData(ready.map((b) => ({ time: day(b.d), value: b.ind[macd.key][0] })))
      deaL.setData(ready.map((b) => ({ time: day(b.d), value: b.ind[macd.key][1] })))
    }

    // ---- KDJ ----
    const kdj = data.indicators.find((s) => s.pane === 'kdj')
    if (kdj) {
      const paneIdx = macd ? 2 : 1
      const colors = ['#e8a33d', '#4f9dfa', '#c464e0']
      const ready = data.bars.filter((b) => b.ready[kdj.key])
      kdj.names.forEach((_, i) => {
        const s = chart.addSeries(LineSeries, {
          color: colors[i], lineWidth: 1,
          priceLineVisible: false, lastValueVisible: false,
          priceFormat: { type: 'price', precision: 2, minMove: 0.01 },
        }, paneIdx)
        s.setData(ready.map((b) => ({ time: day(b.d), value: b.ind[kdj.key][i] })))
      })
    }

    // 副图矮一些，主图占大头。
    //
    // 只设主图高度，剩下的空间由副图均分 —— 逐个给副图 setHeight 是错的：
    // 每次调用都会挤压其他窗格，先设的会被后设的压扁（实测 MACD 只剩一半）。
    const panes = chart.panes()
    if (panes.length > 1) {
      const sub = Math.max(110, Math.round(height * 0.19))
      panes[0].setHeight(Math.max(200, height - sub * (panes.length - 1)))
    }

    // ---- 十字光标联动 ----
    const byDay = new Map<string, KBar>()
    for (const b of data.bars) byDay.set(String(day(b.d)), b)
    chart.subscribeCrosshairMove((p) => {
      if (!p.time) { hoverRef.current?.(null); return }
      const key = typeof p.time === 'string'
        ? p.time
        : new Date((p.time as UTCTimestamp) * 1000).toISOString().slice(0, 10)
      hoverRef.current?.(byDay.get(key) ?? null)
    })

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
  }, [data, priceScale, height])

  return <div ref={box} className="chart" style={{ height }} />
}
