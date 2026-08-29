import { useMemo, useState } from 'react'
import { api, fmtCompact, fmtDay, fmtNum, labelOf } from '../api'
import CandleChart from '../components/CandleChart'
import InstrumentTables from '../components/InstrumentTables'
import {
  DataTable, DayInput, ErrBox, Field, Loading, downloadCSV, useAsync, type Col,
} from '../components/ui'
import type { KBar, Kline as KlineData, Meta } from '../types'

const ADJ_LABEL: Record<string, string> = { none: '不复权', qfq: '前复权', hfq: '后复权' }

export default function Kline({ id, meta }: { id: string; meta: Meta }) {
  const [adj, setAdj] = useState('none')
  // 默认只看最近一年。20 年一次画完，KDJ 会糊成一条带、MACD 被极值压扁，
  // 什么也看不出来 —— 而「全区间」只有一次点击的距离。
  // 注意这只影响**显示**：引擎照样跑全程，指标不受区间影响（见下方说明）。
  const [from, setFrom] = useState(String(meta.stats.lastDay - 10000))
  const [to, setTo] = useState('')
  const [ma, setMa] = useState('5,10,20,60')
  const [macd, setMacd] = useState('12,26,9')
  const [kdj, setKdj] = useState('9,3,3')
  const [hover, setHover] = useState<KBar | null>(null)
  const [showTable, setShowTable] = useState(false)
  const [showMeta, setShowMeta] = useState(false)

  const params = { adj, from, to, ma, macd, kdj }
  const res = useAsync<KlineData>(() => api.kline(id, params), [id, ...Object.values(params)])

  const bars = res.data?.bars ?? []
  // 没悬停时显示最后一根 —— 空着的读数面板浪费了最值钱的那块位置
  const cur = hover ?? bars[bars.length - 1] ?? null
  const scale = res.data?.engine.priceScale || meta.scales.price
  const inst = res.data?.instrument

  const exportCSV = () => {
    if (!res.data) return
    const specs = res.data.indicators
    const bt = !res.data.sameAsBT
    const header = ['trading_day', 'open', 'high', 'low', 'close', 'raw_close',
      'preclose', 'volume', 'amount', 'limit_up', 'limit_dn', 'factor',
      'suspended', 'is_st', 'ex_date']
    for (const s of specs) for (const n of s.names) header.push(`${s.key}_${n}`)
    if (bt) for (const s of specs) for (const n of s.names) header.push(`${s.key}_${n}_hfq`)
    downloadCSV(
      `${res.data.instrument.symbol}_${adj}_${from || 'all'}_${to || 'all'}.csv`,
      header,
      bars.map((b) => {
        const row: (string | number)[] = [b.d, b.o, b.h, b.l, b.c, b.rawC, b.pre,
          b.v, b.amt, b.limitUp, b.limitDn, b.factor,
          b.susp ? 1 : 0, b.st ? 1 : 0, b.ex ? 1 : 0]
        for (const s of specs)
          for (let i = 0; i < s.names.length; i++)
            row.push(b.ready[s.key] ? (b.ind[s.key]?.[i] ?? '') : '')
        if (bt)
          for (const s of specs)
            for (let i = 0; i < s.names.length; i++)
              row.push(b.readyBt?.[s.key] ? (b.indBt?.[s.key]?.[i] ?? '') : '')
        return row
      }),
    )
  }

  return (
    <>
      <h2>
        {inst ? `${inst.symbol} ${inst.name}` : `标的 ${id}`}
        {inst && (
          <span className="muted" style={{ fontSize: 13, fontWeight: 400, marginLeft: 10 }}>
            {labelOf(meta.enums.type, inst.type)} · {labelOf(meta.enums.board, inst.board)}
            {inst.trackedBoard !== inst.board &&
              ` · 涨跌停按${labelOf(meta.enums.board, inst.trackedBoard)}`}
            {inst.status === 2 && ' · 已退市'}
          </span>
        )}
      </h2>
      <div className="sub">
        <a className="link" href="#/instruments">← 回标的列表</a>
        {inst && ` · ${fmtDay(inst.listDate)} 上市 · ${fmtNum(inst.bars)} 根 bar · ${inst.factorEvents} 个因子事件`}
      </div>

      <div className="panel">
        <div className="filters">
          <Field label="复权模式">
            <select value={adj} onChange={(e) => setAdj(e.target.value)}>
              {meta.enums.adj.map((a) => (
                <option key={a.code} value={a.code}>{a.label}</option>
              ))}
            </select>
          </Field>
          <DayInput label="起始日" value={from} onChange={setFrom} />
          <DayInput label="结束日" value={to} onChange={setTo} />
          <Field label="均线">
            <input value={ma} onChange={(e) => setMa(e.target.value)} style={{ width: 100 }} />
          </Field>
          <Field label="MACD">
            <input value={macd} onChange={(e) => setMacd(e.target.value)} style={{ width: 80 }} />
          </Field>
          <Field label="KDJ">
            <input value={kdj} onChange={(e) => setKdj(e.target.value)} style={{ width: 70 }} />
          </Field>
          <button onClick={() => { setFrom(''); setTo('') }} disabled={!from && !to}>全区间</button>
          <button onClick={() => { setFrom(String(meta.stats.lastDay - 10000)); setTo('') }}>近一年</button>
          <button onClick={exportCSV} disabled={!bars.length}>导出 CSV</button>
          <button onClick={() => setShowTable(!showTable)}>
            {showTable ? '隐藏逐日数值' : '逐日数值'}
          </button>
          <button onClick={() => setShowMeta(!showMeta)}>
            {showMeta ? '隐藏因子/分红' : '因子 · 分红送配'}
          </button>
        </div>
        <p className="note">
          复权模式<strong>同时作用于 K 线与其上的指标</strong> —— 两者必须同基准，
          否则均线会和 K 线差出几十倍而完全不可读。
          {res.data && !res.data.sameAsBT && (
            <> 回测固定用<strong>后复权</strong>，因此读数面板会把两组值并排给出。</>
          )}
        </p>
      </div>

      {res.err && <ErrBox msg={res.err} />}
      {res.loading && !res.data && <Loading what="K 线" />}

      {res.data && bars.length > 0 && (
        <>
          <div className="panel" style={{ padding: 10 }}>
            <Readout bar={cur} data={res.data} scale={scale} pinned={hover === null} />
          </div>

          <div className="chartbox">
            <CandleChart data={res.data} priceScale={scale} onHover={setHover} />
          </div>

          <div className="panel" style={{ marginTop: 14 }}>
            <div className="legend">
              <span><span className="sw" style={{ background: '#e8a33d' }} />MA / DIF / K</span>
              <span><span className="sw" style={{ background: '#4f9dfa' }} />MA / DEA / D</span>
              <span><span className="sw" style={{ background: '#c464e0' }} />MA / J</span>
              <span className="muted">「除」标记 = 复权因子事件日</span>
            </div>
            <p className="note">
              <strong>指标由后端引擎算出</strong>，走的是与回测逐字节相同的代码路径：
              同样的 AdjustBar、同样的 Update 顺序。本次跑了 {res.data.engine.runs} 遍引擎
              {res.data.engine.runs > 1 && '（展示基准一遍、回测基准一遍）'}。
            </p>
            <p className="note">
              引擎实际跑了 <strong>{fmtNum(res.data.engine.steps)}</strong> 步，
              其中 <strong>{fmtNum(res.data.engine.warmupBars)}</strong> 步在所选区间之前。
              指标是增量的，从区间首日冷启动算出的值与真实回测不同，
              所以引擎跑全程、只裁剪输出 —— 区间首日的指标因此<strong>是可信的</strong>。
            </p>
            {adj === 'qfq' && (
              <p className="note">
                <strong>前复权只能用于看。</strong>它锚定末日，每来一次新除权就改写全部历史，
                用于决策即构成未来函数（C1）且不可复现（C5）。
                这里能算出它，是因为这条是展示路径 —— 引擎的策略上下文根本不提供它。
              </p>
            )}
            {adj === 'none' && (
              <p className="note">
                不复权价是<strong>撮合、涨跌停判定与账户市值</strong>用的价格。
                除权日的价格跳变是真实的，图上有「除」标记；
                指标在这个基准下会在除权日产生假信号，这正是回测改用后复权的原因。
              </p>
            )}
          </div>

          {showMeta && <InstrumentTables id={id} meta={meta} />}
          {showTable && <BarTable bars={bars} data={res.data} scale={scale} />}
        </>
      )}

      {res.data && bars.length === 0 && (
        <div className="panel"><div className="empty">该区间内没有 bar</div></div>
      )}
    </>
  )
}

// ---- 读数面板 ----
//
// 每个价格都同时给「元」与「定点整数」：核对时前者用来跟行情软件比，
// 后者用来跟 Parquet 里的原值比。少了任何一个都要多绕一步。

function Readout({ bar, data, scale, pinned }: {
  bar: KBar | null; data: KlineData; scale: number; pinned: boolean
}) {
  if (!bar) return <div className="muted">把鼠标移到图上查看逐日数值</div>
  const p = (v: number) => (v / scale).toFixed(3)
  const chg = bar.pre > 0 ? ((bar.rawC - bar.pre) / bar.pre) * 100 : 0
  const adjLabel = ADJ_LABEL[data.adj] ?? data.adj

  const price: { k: string; v: string; raw?: string; cls?: string }[] = [
    { k: '交易日', v: fmtDay(bar.d) },
    { k: '开', v: p(bar.o), raw: String(bar.o) },
    { k: '高', v: p(bar.h), raw: String(bar.h) },
    { k: '低', v: p(bar.l), raw: String(bar.l) },
    { k: `收（${adjLabel}）`, v: p(bar.c), raw: String(bar.c) },
    { k: '收（不复权）', v: p(bar.rawC), raw: String(bar.rawC) },
    { k: '前收', v: p(bar.pre), raw: String(bar.pre) },
    { k: '涨跌幅', v: chg.toFixed(2) + '%', cls: chg > 0 ? 'up' : chg < 0 ? 'down' : '' },
    { k: '成交量', v: fmtCompact(bar.v), raw: String(bar.v) },
    { k: '成交额', v: fmtCompact(bar.amt / 100) + ' 元', raw: String(bar.amt) },
    { k: '涨停价', v: bar.limitUp ? p(bar.limitUp) : '无限制', raw: bar.limitUp ? String(bar.limitUp) : undefined },
    { k: '跌停价', v: bar.limitDn ? p(bar.limitDn) : '无限制', raw: bar.limitDn ? String(bar.limitDn) : undefined },
    { k: '复权因子', v: (bar.factor / 1e12).toFixed(6), raw: String(bar.factor) },
  ]

  // 指标：所选基准的值在上，回测基准（后复权）的值在下。
  // 并排摆着，「我看到的」与「引擎回测时看到的」差多少一眼可见。
  const ind: { k: string; v: string; bt?: string; cls?: string }[] = []
  for (const sp of data.indicators) {
    const short = sp.pane === 'main' ? sp.label : `${sp.key.toUpperCase()}·`
    sp.names.forEach((n, i) => {
      ind.push({
        k: sp.pane === 'main' ? short : short + n,
        v: bar.ready[sp.key] ? (bar.ind[sp.key]?.[i]?.toFixed(4) ?? '—') : '预热中',
        bt: data.sameAsBT
          ? undefined
          : bar.readyBt?.[sp.key]
            ? bar.indBt?.[sp.key]?.[i]?.toFixed(4)
            : '预热中',
        cls: bar.ready[sp.key] ? '' : 'muted',
      })
    })
  }

  return (
    <>
      <div style={{ marginBottom: 8, fontSize: 12 }} className="muted">
        {pinned ? '最后一根 bar' : '悬停中'}
        {bar.susp && <span className="tag warn" style={{ marginLeft: 8 }}>停牌</span>}
        {bar.st && <span className="tag warn" style={{ marginLeft: 6 }}>ST</span>}
        {bar.ex && <span className="tag" style={{ marginLeft: 6 }}>除权日</span>}
      </div>
      <div className="readout">
        {price.map((it) => (
          <div key={it.k}>
            <div className="k">{it.k}</div>
            <div className={`v ${it.cls ?? ''}`}>{it.v}</div>
            {it.raw && <div className="raw">{it.raw}</div>}
          </div>
        ))}
      </div>
      <div style={{ marginTop: 8, borderTop: '1px solid var(--border)', paddingTop: 8 }}>
        <div className="muted" style={{ fontSize: 11, marginBottom: 5 }}>
          指标（{adjLabel}）
          {!data.sameAsBT && <> · 灰色行为回测基准（{ADJ_LABEL[data.btAdj]}）下的同一指标</>}
        </div>
        <div className="readout">
          {ind.map((it) => (
            <div key={it.k}>
              <div className="k">{it.k}</div>
              <div className={`v ${it.cls ?? ''}`}>{it.v}</div>
              {it.bt !== undefined && <div className="raw">{it.bt}</div>}
            </div>
          ))}
        </div>
      </div>
    </>
  )
}

// ---- 数据表 ----

function BarTable({ bars, data, scale }: { bars: KBar[]; data: KlineData; scale: number }) {
  const [page, setPage] = useState(1)
  const size = 60
  // 倒序：核对通常从最近的日子开始
  const rows = useMemo(() => [...bars].reverse(), [bars])
  const slice = rows.slice((page - 1) * size, page * size)
  const p = (v: number) => (v / scale).toFixed(3)

  const cols: Col<KBar>[] = [
    { key: 'd', title: '交易日', render: (b) => fmtDay(b.d) },
    { key: 'o', title: '开', num: true, render: (b) => p(b.o) },
    { key: 'h', title: '高', num: true, render: (b) => p(b.h) },
    { key: 'l', title: '低', num: true, render: (b) => p(b.l) },
    { key: 'c', title: `收(${ADJ_LABEL[data.adj] ?? data.adj})`, num: true, render: (b) => p(b.c) },
    { key: 'rawC', title: '收(不复权)', num: true, render: (b) => <span className="muted">{p(b.rawC)}</span> },
    { key: 'pre', title: '前收', num: true, render: (b) => p(b.pre) },
    {
      key: 'lim', title: '涨停/跌停', num: true,
      render: (b) => b.limitUp ? `${p(b.limitUp)} / ${p(b.limitDn)}` : <span className="muted">无限制</span>,
    },
    { key: 'v', title: '成交量', num: true, render: (b) => fmtCompact(b.v) },
    { key: 'f', title: '因子', num: true, render: (b) => (b.factor / 1e12).toFixed(6) },
    {
      key: 'flag', title: '标记', render: (b) => (
        <>
          {b.susp && <span className="tag warn">停牌</span>}
          {b.st && <span className="tag warn">ST</span>}
          {b.ex && <span className="tag">除权</span>}
        </>
      ),
    },
    ...data.indicators.flatMap((sp) =>
      sp.names.map((n, i) => ({
        key: `${sp.key}_${n}`,
        title: sp.pane === 'main' ? sp.label : `${sp.key.toUpperCase()}·${n}`,
        num: true,
        render: (b: KBar) => b.ready[sp.key]
          ? (b.ind[sp.key]?.[i]?.toFixed(4) ?? '—')
          : <span className="muted">预热</span>,
      })),
    ),
  ]

  const pages = Math.ceil(rows.length / size)
  return (
    <div className="panel" style={{ marginTop: 14 }}>
      <h3>逐日数值（{fmtNum(rows.length)} 行，新→旧）</h3>
      <DataTable cols={cols} rows={slice} />
      <div className="pager">
        <span className="muted">第 {page} / {pages} 页</span>
        <span className="grow" />
        <button disabled={page <= 1} onClick={() => setPage(page - 1)}>上一页</button>
        <button disabled={page >= pages} onClick={() => setPage(page + 1)}>下一页</button>
      </div>
    </div>
  )
}
