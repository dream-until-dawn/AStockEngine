import { useState } from 'react'
import { api, fmtDay, fmtNum, labelOf } from '../api'
import {
  DataTable, DayInput, EnumSelect, ErrBox, Field, Loading, Pager, TriSelect,
  downloadCSV, useAsync, type Col,
} from '../components/ui'
import type { Instrument, Meta, Paged } from '../types'

export default function Instruments({ meta }: { meta: Meta }) {
  const [q, setQ] = useState('')
  const [market, setMarket] = useState('')
  const [exchange, setExchange] = useState('')
  const [type, setType] = useState('')
  const [board, setBoard] = useState('')
  const [trackedBoard, setTrackedBoard] = useState('')
  const [status, setStatus] = useState('')
  const [hasBars, setHasBars] = useState('')
  const [hasFactor, setHasFactor] = useState('')
  const [hasCorp, setHasCorp] = useState('')
  const [listedOn, setListedOn] = useState('')
  const [sort, setSort] = useState('symbol')
  const [order, setOrder] = useState('asc')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)

  const params = {
    q, market, exchange, type, board, trackedBoard, status,
    hasBars, hasFactor, hasCorp, listedOn, sort, order, page, pageSize,
  }
  const res = useAsync<Paged<Instrument>>(
    () => api.instruments(params),
    Object.values(params),
  )

  const clickSort = (key: string) => {
    if (sort === key) setOrder(order === 'asc' ? 'desc' : 'asc')
    else { setSort(key); setOrder('asc') }
    setPage(1)
  }
  const reset = (fn: (v: string) => void) => (v: string) => { fn(v); setPage(1) }

  const cols: Col<Instrument>[] = [
    { key: 'symbol', title: '代码', sort: 'symbol', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', sort: 'name', render: (r) => r.name },
    { key: 'type', title: '类型', render: (r) => labelOf(meta.enums.type, r.type) },
    { key: 'exchange', title: '交易所', render: (r) => labelOf(meta.enums.exchange, r.exchange) },
    { key: 'board', title: '板块', render: (r) => labelOf(meta.enums.board, r.board) },
    {
      key: 'tracked', title: '涨跌停依据',
      render: (r) =>
        // ETF 自身不属于任何板块，涨跌停由跟踪的指数决定。
        // 这一列存在的理由就是让这条规则可见、可核对。
        r.trackedBoard === r.board
          ? <span className="muted">{labelOf(meta.enums.board, r.trackedBoard)}</span>
          : <span className="tag">{labelOf(meta.enums.board, r.trackedBoard)}</span>,
    },
    {
      key: 'status', title: '状态',
      render: (r) => r.status === 2
        ? <span className="tag warn">已退市</span>
        : <span className="muted">在市</span>,
    },
    { key: 'listDate', title: '上市日', num: true, sort: 'listDate', render: (r) => fmtDay(r.listDate) },
    {
      key: 'delistDate', title: '退市日', num: true, sort: 'delistDate',
      render: (r) => r.delistDate ? fmtDay(r.delistDate) : <span className="muted">—</span>,
    },
    {
      key: 'bars', title: 'bar 行数', num: true, sort: 'bars',
      render: (r) => r.bars > 0
        ? fmtNum(r.bars)
        : <span className="tag warn">无行情</span>,
    },
    { key: 'firstDay', title: '首个交易日', num: true, sort: 'firstDay', render: (r) => fmtDay(r.firstDay) },
    { key: 'lastDay', title: '末个交易日', num: true, sort: 'lastDay', render: (r) => fmtDay(r.lastDay) },
    {
      key: 'factorEvents', title: '因子事件', num: true, sort: 'factorEvents',
      render: (r) => r.factorEvents || <span className="muted">0</span>,
    },
    {
      key: 'corpActions', title: '分红送配', num: true, sort: 'corpActions',
      render: (r) => r.corpActions || <span className="muted">0</span>,
    },
    { key: 'minQty', title: '最小单位', num: true, render: (r) => `${r.minOrderQty}/${r.qtyStep}` },
  ]

  const exportCSV = () => {
    const rows = res.data?.rows ?? []
    downloadCSV(
      `instruments_p${page}.csv`,
      ['instrument_id', 'symbol', 'name', 'type', 'exchange', 'board', 'tracked_board',
       'status', 'list_date', 'delist_date', 'bars', 'first_day', 'last_day',
       'factor_events', 'corp_actions'],
      rows.map((r) => [r.id, r.symbol, r.name, labelOf(meta.enums.type, r.type),
        labelOf(meta.enums.exchange, r.exchange), labelOf(meta.enums.board, r.board),
        labelOf(meta.enums.board, r.trackedBoard), labelOf(meta.enums.status, r.status),
        r.listDate, r.delistDate, r.bars, r.firstDay, r.lastDay,
        r.factorEvents, r.corpActions]),
    )
  }

  return (
    <>
      <h2>标的列表</h2>
      <div className="sub">点任意一行进 K 线视图</div>

      <div className="panel">
        <div className="filters">
          <Field label="代码 / 名称">
            <input value={q} placeholder="600000 或 浦发"
              onChange={(e) => { setQ(e.target.value); setPage(1) }} style={{ width: 130 }} />
          </Field>
          <EnumSelect label="市场" items={meta.enums.market} value={market} onChange={reset(setMarket)} />
          <EnumSelect label="类型" items={meta.enums.type} value={type} onChange={reset(setType)} />
          <EnumSelect label="交易所" items={meta.enums.exchange} value={exchange} onChange={reset(setExchange)} />
          <EnumSelect label="板块" items={meta.enums.board} value={board} onChange={reset(setBoard)} />
          <EnumSelect label="涨跌停依据" items={meta.enums.board} value={trackedBoard} onChange={reset(setTrackedBoard)} />
          <EnumSelect label="状态" items={meta.enums.status} value={status} onChange={reset(setStatus)} />
          <TriSelect label="行情" value={hasBars} onChange={reset(setHasBars)} yes="有 bar" no="无 bar" />
          <TriSelect label="复权因子" value={hasFactor} onChange={reset(setHasFactor)} />
          <TriSelect label="分红送配" value={hasCorp} onChange={reset(setHasCorp)} />
          <DayInput label="某日在市" value={listedOn} onChange={reset(setListedOn)} />
          <button onClick={() => {
            setQ(''); setMarket(''); setExchange(''); setType(''); setBoard('')
            setTrackedBoard(''); setStatus(''); setHasBars(''); setHasFactor('')
            setHasCorp(''); setListedOn(''); setPage(1)
          }}>清空</button>
          <button onClick={exportCSV} disabled={!res.data?.rows.length}>导出本页 CSV</button>
        </div>
        <p className="note">
          <strong>某日在市</strong> 是 point-in-time 过滤：填 20190101 就只留当天在市的标的。
          回测 2019 年时用的应当是这份名单，而不是今天的 —— 这一列是核对 C3（幸存者偏差）用的。
        </p>
      </div>

      {res.err && <ErrBox msg={res.err} />}
      {res.loading && !res.data && <Loading what="标的" />}
      {res.data && (
        <>
          <DataTable
            cols={cols}
            rows={res.data.rows}
            sort={sort}
            order={order}
            onSort={clickSort}
            onRowClick={(r) => { location.hash = `/kline/${r.id}` }}
          />
          <Pager
            total={res.data.total} page={page} pageSize={pageSize}
            onPage={setPage} onPageSize={(n) => { setPageSize(n); setPage(1) }}
          />
        </>
      )}
    </>
  )
}
