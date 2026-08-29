import { api, fmtDay } from '../api'
import { DataTable, ErrBox, Loading, useAsync, type Col } from './ui'
import type { CorpRow, FactorRow, InstrumentDetail, Meta } from '../types'

// 单标的的两张表 + 对账结果。
//
// 对账是这里的重点：因子事件与分红送配本应一一对应，
// 只在一边出现的日子就是缺口，而缺口正是「数据准不准」的具体形态。

export default function InstrumentTables({ id, meta }: { id: string; meta: Meta }) {
  const res = useAsync<InstrumentDetail>(() => api.instrument(id), [id])
  if (res.err) return <ErrBox msg={res.err} />
  if (!res.data) return <Loading what="标的明细" />

  const { factors, corpActions, reconcile } = res.data
  const rs = meta.scales.ratio
  const ps = meta.scales.price
  const lastDay = meta.stats.lastDay
  const per = (v: number) =>
    v ? (v / rs).toFixed(6).replace(/0+$/, '').replace(/\.$/, '') : ''

  const corpDays = new Set(corpActions.map((c) => c.exDate))
  const factorDays = new Set(factors.map((f) => f.exDate))

  const fCols: Col<FactorRow>[] = [
    { key: 'exDate', title: '除权日', render: (r) => fmtDay(r.exDate) },
    { key: 'factor', title: '后复权因子', num: true, render: (r) => (r.factor / 1e12).toFixed(6) },
    {
      key: 'ratio', title: '跳变比例', num: true,
      render: (r) => r.ratio !== undefined
        ? <span className={r.ratio > 1.5 ? 'up' : ''}>{r.ratio.toFixed(6)}</span>
        : <span className="muted">首个</span>,
    },
    {
      key: 'corp', title: '分红送配记录',
      render: (r) => corpDays.has(r.exDate)
        ? <span className="tag ok">有</span>
        : <span className="tag warn">缺</span>,
    },
  ]

  const cCols: Col<CorpRow>[] = [
    { key: 'exDate', title: '除权日', render: (r) => fmtDay(r.exDate) },
    { key: 'cash', title: '现金分红(税前)', num: true, render: (r) => per(r.cashBeforeTax) || <span className="muted">—</span> },
    { key: 'sd', title: '送股', num: true, render: (r) => per(r.stockDividend) || <span className="muted">—</span> },
    { key: 'st', title: '转增', num: true, render: (r) => per(r.stockTransfer) || <span className="muted">—</span> },
    { key: 'rr', title: '配股', num: true, render: (r) => per(r.rightsRatio) || <span className="muted">—</span> },
    { key: 'rp', title: '配股价', num: true, render: (r) => r.rightsPrice ? (r.rightsPrice / ps).toFixed(3) : <span className="muted">—</span> },
    {
      key: 'eff', title: '有影响',
      render: (r) => r.hasEffect ? <span className="tag ok">是</span> : <span className="tag">全零行</span>,
    },
    {
      key: 'f', title: '因子事件',
      render: (r) => factorDays.has(r.exDate)
        ? <span className="tag ok">有</span>
        : r.exDate > lastDay
          ? <span className="tag">未到</span>
          : <span className="tag warn">缺</span>,
    },
  ]

  const fOnly = reconcile.factorOnly ?? []
  const cOnly = (reconcile.corpOnly ?? []).filter((d) => d <= lastDay)
  const cFuture = (reconcile.corpOnly ?? []).filter((d) => d > lastDay)

  return (
    <>
      <div className="panel" style={{ marginTop: 14 }}>
        <h3>两表对账</h3>
        {fOnly.length === 0 && cOnly.length === 0 ? (
          <div className="muted">
            复权因子与分红送配的除权日<strong>完全一致</strong>
            （{factors.length} / {corpActions.length} 条）
            {cFuture.length > 0 && ` · 另有 ${cFuture.length} 条已公告但除权日未到`}
          </div>
        ) : (
          <>
            {fOnly.length > 0 && (
              <div style={{ marginBottom: 6 }}>
                <span className="tag warn">因子有 · 分红送配无</span>{' '}
                <span className="mono">{fOnly.map(fmtDay).join('  ')}</span>
                <div className="note" style={{ marginTop: 4 }}>
                  引擎靠 <code>ImplySplitFromFactor</code> 按因子比例推算这些日子的送转并入账。
                  这是有损近似，Portfolio 会逐条留痕。
                </div>
              </div>
            )}
            {cOnly.length > 0 && (
              <div>
                <span className="tag warn">分红送配有 · 因子无</span>{' '}
                <span className="mono">{cOnly.map(fmtDay).join('  ')}</span>
                <div className="note" style={{ marginTop: 4 }}>
                  价格序列在这些日子没有跳变。若分红金额不为零，后复权价会偏低。
                </div>
              </div>
            )}
            {cFuture.length > 0 && (
              <div className="muted" style={{ marginTop: 6, fontSize: 12 }}>
                另有 {cFuture.length} 条已公告但除权日未到（行情截至 {lastDay}），不算缺口。
              </div>
            )}
          </>
        )}
      </div>

      <div className="panel">
        <h3>复权因子（{factors.length} 条）</h3>
        <DataTable cols={fCols} rows={factors} empty="该标的没有复权因子事件" />
      </div>

      <div className="panel">
        <h3>分红送配（{corpActions.length} 条）</h3>
        <DataTable cols={cCols} rows={corpActions} empty="该标的没有分红送配记录" />
      </div>
    </>
  )
}
