import { Fragment, useEffect, useState } from 'react'
import { fmtDay, fmtNum, runApi } from '../api'
import type { Meta, UniverseSpec, UniversePreview } from '../types'

// 标的池选择器。
//
// **它不自己算命中了哪些标的** —— 那由服务端的 /api/universe 解析，
// 走的是与真跑**完全相同**的 ResolveUniverse。预览说 3,194 只、
// 真跑却是别的数，那种不一致比没有预览更糟。
//
// 「指定标的」填了就忽略其余条件：「就在这几只上跑」是最直接的意图，
// 不该再被板块类型二次过滤掉。

const BOARDS: [string, string][] = [
  ['main', '主板'], ['chinext', '创业板'], ['star', '科创板'], ['bse', '北交所'],
]
const EXCHANGES: [string, string][] = [
  ['sse', '上交所'], ['szse', '深交所'], ['bse', '北交所'],
]

function toggle(list: string[] | undefined, v: string): string[] | undefined {
  const cur = list ?? []
  const next = cur.includes(v) ? cur.filter((x) => x !== v) : [...cur, v]
  return next.length === 0 ? undefined : next
}

export default function UniversePicker({
  value, onChange, meta,
}: {
  value: UniverseSpec
  onChange: (u: UniverseSpec) => void
  meta: Meta
}) {
  const [preview, setPreview] = useState<UniversePreview | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [symbolText, setSymbolText] = useState((value.symbols ?? []).join(' '))

  // 换配置时外部会整个换掉 value —— 本地这份文本得跟着走，
  // 否则选了新配置、输入框里还留着上一份的代码
  const extSymbols = (value.symbols ?? []).join(' ')
  useEffect(() => {
    setSymbolText((cur) => (cur.trim() === extSymbols.trim() ? cur : extSymbols))
  }, [extSymbols])

  const key = JSON.stringify(value)
  useEffect(() => {
    let alive = true
    setBusy(true)
    setErr('')
    runApi.universe(value).then(
      (p) => alive && (setPreview(p), setBusy(false)),
      (e) => alive && (setErr(String(e?.message ?? e)), setPreview(null), setBusy(false)),
    )
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])

  const set = (patch: Partial<UniverseSpec>) => onChange({ ...value, ...patch })
  const bySymbols = (value.symbols?.length ?? 0) > 0

  const commitSymbols = (raw: string) => {
    setSymbolText(raw)
    const list = raw.split(/[\s,，、]+/).map((s) => s.trim()).filter(Boolean)
    set({ symbols: list.length > 0 ? list : undefined })
  }

  return (
    <div>
      <div className="filters">
        <div className="field">
          <label>市场</label>
          <div className="chips">
            {meta.enums.market.map((m) => {
              const code = m.code === 1 ? 'ashare' : String(m.code)
              const on = (value.market ?? []).includes(code)
              return (
                <span key={code} className={`chip ${on ? 'on' : ''}`}
                  onClick={() => set({ market: toggle(value.market, code) })}>
                  {m.label}
                </span>
              )
            })}
          </div>
        </div>

        <div className="field">
          <label>类型</label>
          <select value={value.type ?? 'all'} onChange={(e) => set({ type: e.target.value })}>
            <option value="all">全部</option>
            <option value="stock">个股</option>
            <option value="etf">ETF</option>
          </select>
        </div>

        <div className="field">
          <label>板块</label>
          <div className="chips">
            {BOARDS.map(([k, label]) => (
              <span key={k} className={`chip ${(value.board ?? []).includes(k) ? 'on' : ''}`}
                onClick={() => set({ board: toggle(value.board, k) })}>
                {label}
              </span>
            ))}
          </div>
        </div>

        <div className="field">
          <label>交易所</label>
          <div className="chips">
            {EXCHANGES.map(([k, label]) => (
              <span key={k} className={`chip ${(value.exchange ?? []).includes(k) ? 'on' : ''}`}
                onClick={() => set({ exchange: toggle(value.exchange, k) })}>
                {label}
              </span>
            ))}
          </div>
        </div>

        <div className="field">
          <label>状态</label>
          <select value={value.status ?? 'all'} onChange={(e) => set({ status: e.target.value })}>
            <option value="all">全部（含退市）</option>
            <option value="listed">仅在市</option>
            <option value="delisted">仅已退市</option>
          </select>
        </div>

        <div className="field">
          <label>上限（0 = 不限）</label>
          <input value={value.limit ?? 0} style={{ width: 80 }}
            onChange={(e) => set({ limit: Number(e.target.value.replace(/\D/g, '')) || undefined })} />
        </div>

        <div className="field">
          <label>只要有复权因子的</label>
          <select value={value.require_factor ? '1' : '0'}
            onChange={(e) => set({ require_factor: e.target.value === '1' || undefined })}>
            <option value="0">不限</option>
            <option value="1">是</option>
          </select>
        </div>
      </div>

      <div className="field" style={{ marginTop: 10 }}>
        <label>指定标的（代码，空格或逗号分隔；填了就忽略上面全部条件）</label>
        <input value={symbolText} placeholder="600519 000858 510300"
          style={{ width: '100%', fontFamily: 'var(--mono)' }}
          onChange={(e) => commitSymbols(e.target.value)} />
      </div>

      <div style={{ marginTop: 10 }}>
        {err && <div className="error">{err}</div>}
        {busy && !preview && <span className="muted">解析中…</span>}
        {preview && <Preview p={preview} bySymbols={bySymbols} />}
      </div>
    </div>
  )
}

function Preview({ p, bySymbols }: { p: UniversePreview; bySymbols: boolean }) {
  return (
    <>
      <div style={{ marginBottom: 6 }}>
        命中 <strong>{fmtNum(p.count)}</strong> 只
        {p.withBars !== p.count && (
          <>
            {' '}· 其中 <strong>{fmtNum(p.withBars)}</strong> 只在数据里有行情
            <span className="muted">（{fmtNum(p.count - p.withBars)} 只没有，会被自动跳过）</span>
          </>
        )}
        {bySymbols && <span className="tag" style={{ marginLeft: 8 }}>按指定列表</span>}
      </div>
      {p.overLimit && (
        <div className="error">
          超过服务端回测上限 {fmtNum(p.limit)} 只。装配时要把全量列式数据裁成子集，
          那是一次内存拷贝，太大会把服务端撑爆。
          请收窄条件，或用命令行跑（命令行只载入所需标的，没有这个问题）：
          <br />
          <code>cd engine &amp;&amp; go run ./cmd/backtest -config 你的配置.json</code>
        </div>
      )}
      <div className="tablewrap" style={{ maxHeight: 220, overflowY: 'auto' }}>
        <table>
          <thead>
            <tr><th>代码</th><th>名称</th><th className="num">bar</th><th>区间</th></tr>
          </thead>
          <tbody>
            {p.sample.map((s, i) => (
              <Fragment key={s.id}>
                {p.truncated > 0 && i === p.sample.length / 2 && (
                  <tr>
                    <td colSpan={4} className="muted" style={{ textAlign: 'center' }}>
                      … 中间省略 {fmtNum(p.truncated)} 只 …
                    </td>
                  </tr>
                )}
                <tr>
                  <td className="mono">{s.symbol}</td>
                  <td>{s.name}</td>
                  <td className="num">{s.bars > 0 ? fmtNum(s.bars) : <span className="tag warn">无行情</span>}</td>
                  <td className="muted">
                    {s.bars > 0 ? `${fmtDay(s.firstDay)} ~ ${fmtDay(s.lastDay)}` : '—'}
                  </td>
                </tr>
              </Fragment>
            ))}
          </tbody>
        </table>
      </div>
    </>
  )
}
