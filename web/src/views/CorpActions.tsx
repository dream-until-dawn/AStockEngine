import { useState } from 'react'
import { api, fmtDay } from '../api'
import {
  DataTable, DayInput, ErrBox, Field, Loading, Pager, TriSelect,
  downloadCSV, useAsync, type Col,
} from '../components/ui'
import type { Meta } from '../types'

type Row = {
  id: number; symbol: string; name: string; exDate: number
  cashBeforeTax: number; stockDividend: number; stockTransfer: number
  rightsRatio: number; rightsPrice: number; hasEffect: boolean; hasFactor: boolean
}

export default function CorpActions({ meta }: { meta: Meta }) {
  const [q, setQ] = useState('')
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [hasEffect, setHasEffect] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(100)

  const params = { q, from, to, hasEffect, page, pageSize }
  const res = useAsync(() => api.corpActions(params), Object.values(params))
  const rs = meta.scales.ratio
  const ps = meta.scales.price
  const lastDay = meta.stats.lastDay

  // 每股值一律 scale 1e6。分红只存税前 —— 税率属规则不属数据，
  // 随持股期限分档，由引擎按配置算。
  const per = (v: number) => (v ? (v / rs).toFixed(6).replace(/0+$/, '').replace(/\.$/, '') : '')

  const cols: Col<Row>[] = [
    { key: 'exDate', title: '除权日', render: (r) => fmtDay(r.exDate) },
    { key: 'symbol', title: '代码', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', render: (r) => r.name },
    {
      key: 'cash', title: '每股现金分红(税前)', num: true,
      render: (r) => per(r.cashBeforeTax) || <span className="muted">—</span>,
    },
    {
      key: 'sd', title: '每股送股', num: true,
      render: (r) => per(r.stockDividend) || <span className="muted">—</span>,
    },
    {
      key: 'st', title: '每股转增', num: true,
      render: (r) => per(r.stockTransfer) || <span className="muted">—</span>,
    },
    {
      key: 'rr', title: '每股配股', num: true,
      render: (r) => per(r.rightsRatio) || <span className="muted">—</span>,
    },
    {
      key: 'rp', title: '配股价', num: true,
      render: (r) => r.rightsPrice ? (r.rightsPrice / ps).toFixed(3) : <span className="muted">—</span>,
    },
    {
      key: 'effect', title: '有影响',
      // 数据源里存在全零行（「不分配」被误采）。它们不该被当成缺失 ——
      // 「有一行但全零」与「根本没有这行」是两种不同的数据状况。
      render: (r) => r.hasEffect
        ? <span className="tag ok">是</span>
        : <span className="tag">全零行</span>,
    },
    {
      key: 'factor', title: '因子事件',
      // 除权日晚于行情末日的记录（已公告但还没到），因子当然不存在。
      // 把它和真正的「缺口」混为一谈会让人白追一场 —— 分开标。
      render: (r) => r.hasFactor
        ? <span className="tag ok">有</span>
        : r.exDate > lastDay
          ? <span className="tag">未到</span>
          : <span className="tag warn">缺</span>,
    },
  ]

  return (
    <>
      <h2>分红送配</h2>
      <div className="sub">
        每股值定点 ×{rs.toLocaleString('zh-CN')} · 只存税前分红 · 与复权因子表交叉对账
      </div>

      <div className="panel">
        <div className="filters">
          <Field label="代码 / 名称">
            <input value={q} placeholder="600000 或 浦发" style={{ width: 130 }}
              onChange={(e) => { setQ(e.target.value); setPage(1) }} />
          </Field>
          <DayInput label="起始日" value={from} onChange={(v) => { setFrom(v); setPage(1) }} />
          <DayInput label="结束日" value={to} onChange={(v) => { setTo(v); setPage(1) }} />
          <TriSelect label="内容" value={hasEffect}
            onChange={(v) => { setHasEffect(v); setPage(1) }} yes="仅有影响" no="仅全零行" />
          <button onClick={() => { setQ(''); setFrom(''); setTo(''); setHasEffect(''); setPage(1) }}>清空</button>
          <button disabled={!res.data?.rows.length} onClick={() => downloadCSV(
            'corporate_action.csv',
            ['instrument_id', 'symbol', 'name', 'ex_date', 'cash_before_tax',
             'stock_dividend', 'stock_transfer', 'rights_ratio', 'rights_price',
             'has_effect', 'has_factor'],
            (res.data?.rows ?? []).map((r) => [r.id, r.symbol, r.name, r.exDate,
              r.cashBeforeTax, r.stockDividend, r.stockTransfer, r.rightsRatio,
              r.rightsPrice, r.hasEffect ? 1 : 0, r.hasFactor ? 1 : 0]),
          )}>导出本页 CSV</button>
        </div>
        <p className="note">
          <strong>「因子事件」列标「缺」</strong>表示这天有分红送配记录但复权因子没跳变 ——
          那是 ETL 的已知缺口。标<strong>「未到」</strong>则是除权日还没到
          （行情数据截至 {lastDay}），因子本来就该不存在，不是缺口。
          反过来（有跳变但没记录）在<a className="link" href="#/factors">复权因子</a>页看。
        </p>
      </div>

      {res.err && <ErrBox msg={res.err} />}
      {res.loading && !res.data && <Loading what="分红送配" />}
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
