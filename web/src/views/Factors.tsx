import { useState } from 'react'
import { api, fmtDay } from '../api'
import {
  DataTable, DayInput, ErrBox, Field, Loading, Pager, downloadCSV, useAsync, type Col,
} from '../components/ui'
import type { Meta } from '../types'

type Row = {
  id: number; symbol: string; name: string
  exDate: number; factor: number; ratio: number; hasCorp: boolean
}

export default function Factors({ meta }: { meta: Meta }) {
  const [q, setQ] = useState('')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(100)

  const params = { q, from, to, page, pageSize }
  const res = useAsync(() => api.factors(params), Object.values(params))
  const fscale = meta.scales.factor

  const cols: Col<Row>[] = [
    { key: 'exDate', title: '除权日', render: (r) => fmtDay(r.exDate) },
    { key: 'symbol', title: '代码', render: (r) => (
      <a className="link mono" href={`#/kline/${r.id}`}
        onClick={(e) => e.stopPropagation()}>{r.symbol}</a>
    ) },
    { key: 'name', title: '名称', render: (r) => r.name },
    {
      key: 'factor', title: '后复权因子', num: true,
      render: (r) => (r.factor / fscale).toFixed(6),
    },
    {
      key: 'ratio', title: '跳变比例', num: true,
      // 因子的绝对值看不出「这次除权多大」，与上一个因子的比值才看得出。
      // 1.10 意味着每股在该事件后相当于 1.1 股（10 送 1 或等值分红）。
      render: (r) => r.ratio
        ? <span className={r.ratio > 1.5 ? 'up' : ''}>{r.ratio.toFixed(6)}</span>
        : <span className="muted">首个</span>,
    },
    {
      key: 'hasCorp', title: '分红送配记录',
      render: (r) => r.hasCorp
        ? <span className="tag ok">有</span>
        // 有因子跳变却没有分红送配记录 —— ETL.md 里记着的已知缺口，
        // 引擎靠 ImplySplitFromFactor 按因子比例推算入账。
        : <span className="tag warn">缺</span>,
    },
  ]

  return (
    <>
      <h2>复权因子</h2>
      <div className="sub">
        事件式因子，自除权日<strong>当日</strong>起生效 · 与分红送配表交叉对账，缺一边的标为「缺」
      </div>

      <div className="panel">
        <div className="filters">
          <Field label="代码 / 名称">
            <input value={q} placeholder="600000 或 浦发" style={{ width: 130 }}
              onChange={(e) => { setQ(e.target.value); setPage(1) }} />
          </Field>
          <DayInput label="起始日" value={from} onChange={(v) => { setFrom(v); setPage(1) }} />
          <DayInput label="结束日" value={to} onChange={(v) => { setTo(v); setPage(1) }} />
          <button onClick={() => { setQ(''); setFrom(''); setTo(''); setPage(1) }}>清空</button>
          <button disabled={!res.data?.rows.length} onClick={() => downloadCSV(
            'adj_factor.csv',
            ['instrument_id', 'symbol', 'name', 'ex_date', 'hfq_factor', 'ratio', 'has_corp_action'],
            (res.data?.rows ?? []).map((r) => [r.id, r.symbol, r.name, r.exDate,
              r.factor, r.ratio || '', r.hasCorp ? 1 : 0]),
          )}>导出本页 CSV</button>
        </div>
      </div>

      {res.err && <ErrBox msg={res.err} />}
      {res.loading && !res.data && <Loading what="因子" />}
      {res.data && (
        <>
          <DataTable cols={cols} rows={res.data.rows}
            onRowClick={(r) => { location.hash = `/kline/${r.id}` }} />
          <Pager total={res.data.total} page={page} pageSize={pageSize}
            onPage={setPage} onPageSize={(n) => { setPageSize(n); setPage(1) }} />
        </>
      )}
    </>
  )
}
