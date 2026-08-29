import { useState } from 'react'
import { api, fmtDay } from '../api'
import {
  DataTable, DayInput, ErrBox, Loading, Pager, TriSelect, downloadCSV, useAsync, type Col,
} from '../components/ui'

type Row = { market: number; date: number; isTradingDay: boolean }

const WEEK = ['日', '一', '二', '三', '四', '五', '六']

export default function CalendarView() {
  const [from, setFrom] = useState('')
  const [to, setTo] = useState('')
  const [isTradingDay, setIsTradingDay] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(100)

  const params = { market: 1, from, to, isTradingDay, page, pageSize }
  const res = useAsync(() => api.calendar(params), Object.values(params))

  const cols: Col<Row>[] = [
    { key: 'date', title: '日期', render: (r) => fmtDay(r.date) },
    {
      key: 'week', title: '星期',
      render: (r) => {
        const s = String(r.date)
        const d = new Date(+s.slice(0, 4), +s.slice(4, 6) - 1, +s.slice(6, 8))
        // 周末却是交易日，或工作日却休市 —— 两种都值得一眼看出来
        const weekend = d.getDay() === 0 || d.getDay() === 6
        return (
          <span className={weekend ? 'muted' : ''}>
            周{WEEK[d.getDay()]}
            {weekend && r.isTradingDay && <span className="tag warn" style={{ marginLeft: 6 }}>周末开市</span>}
          </span>
        )
      },
    },
    {
      key: 'trading', title: '交易日',
      render: (r) => r.isTradingDay
        ? <span className="tag ok">是</span>
        : <span className="muted">休市</span>,
    },
  ]

  return (
    <>
      <h2>交易日历</h2>
      <div className="sub">
        含休市日 —— 「某标的某日没有 bar」要先能区分「停牌 / 未上市」与「本就休市」
      </div>

      <div className="panel">
        <div className="filters">
          <DayInput label="起始日" value={from} onChange={(v) => { setFrom(v); setPage(1) }} />
          <DayInput label="结束日" value={to} onChange={(v) => { setTo(v); setPage(1) }} />
          <TriSelect label="类型" value={isTradingDay}
            onChange={(v) => { setIsTradingDay(v); setPage(1) }} yes="仅交易日" no="仅休市日" />
          <button onClick={() => { setFrom(''); setTo(''); setIsTradingDay(''); setPage(1) }}>清空</button>
          <button disabled={!res.data?.rows.length} onClick={() => downloadCSV(
            'calendar.csv', ['market', 'date', 'is_trading_day'],
            (res.data?.rows ?? []).map((r) => [r.market, r.date, r.isTradingDay ? 1 : 0]),
          )}>导出本页 CSV</button>
        </div>
      </div>

      {res.err && <ErrBox msg={res.err} />}
      {res.loading && !res.data && <Loading what="日历" />}
      {res.data && (
        <>
          <DataTable cols={cols} rows={res.data.rows} />
          <Pager total={res.data.total} page={page} pageSize={pageSize}
            onPage={setPage} onPageSize={(n) => { setPageSize(n); setPage(1) }} />
        </>
      )}
    </>
  )
}
