import { useEffect, useRef, useState } from 'react'
import { dbgApi, fmtCompact, fmtDay, fmtNum, runApi } from '../api'
import { DataTable, type Col } from '../components/ui'
import type {
  ConfigItem, DbgPending, DbgPosition, Inspect, Meta,
  SessionState, StepEvent, StepResult,
} from '../types'

// 单步调试（v0.4）。
//
// 回测视图回答「跑完是什么样」，这里回答**「这一天为什么是这个决定」**。
// 两者要看的东西完全不同：跑完只需要成交记录，单步需要看到那条漏斗
//
//   信号 → 定量(Sizer) → 风控 → 入队 → 撮合 → 成交 / 拒单
//
// 每一环都可能把单子吃掉，而只看成交记录时**全部表现为「什么也没发生」**。
// 所以这里把四段并排摆出来，一眼能看出单子死在哪一环。

const yuan = (c: number) => c / 100
const money = (c: number) => fmtNum(yuan(c), 2)

export default function Debug({ meta: _meta }: { meta: Meta }) {
  const [configs, setConfigs] = useState<ConfigItem[]>([])
  const [picked, setPicked] = useState('')
  const [text, setText] = useState('')
  const [editing, setEditing] = useState(false)

  const [state, setState] = useState<SessionState | null>(null)
  const [events, setEvents] = useState<StepEvent[]>([])
  const [last, setLast] = useState<StepResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  const [gotoDay, setGotoDay] = useState('')
  const [symbol, setSymbol] = useState('')
  const [insp, setInsp] = useState<Inspect | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    runApi.configs().then(
      (d) => {
        setConfigs(d.configs)
        if (d.configs.length) {
          setPicked(d.configs[0].name)
          setText(JSON.stringify(d.configs[0].config, null, 2))
        }
      },
      (e) => setErr(String(e?.message ?? e)),
    )
  }, [])

  const guard = async (fn: () => Promise<void>) => {
    setBusy(true)
    setErr('')
    try {
      await fn()
    } catch (e) {
      setErr(String((e as Error)?.message ?? e))
    } finally {
      setBusy(false)
    }
  }

  const create = () => guard(async () => {
    const r = await dbgApi.create(text)
    setState(r.state)
    setEvents([])
    setLast(null)
    setInsp(null)
  })

  // 步进后**把新事件拼在旧的前面**：调试时要看的是「刚才发生了什么」，
  // 而不是从头翻。旧的留着是因为「上一次成交是哪天」经常要回头对。
  const step = (req: { n?: number; day?: number; until?: 'signal' | 'fill' | 'reject' }) =>
    guard(async () => {
      if (!state) return
      const r = await dbgApi.step(state.id, req)
      setState(r.state)
      setLast(r)
      setEvents((prev) => [...r.steps].reverse().concat(prev).slice(0, 400))
      if (insp) setInsp(await dbgApi.inspect(r.state.id, insp.symbol))
    })

  const reset = () => guard(async () => {
    if (!state) return
    const r = await dbgApi.reset(state.id)
    setState(r.state)
    setEvents([])
    setLast(null)
    setInsp(null)
  })

  const inspect = () => guard(async () => {
    if (!state || !symbol.trim()) return
    setInsp(await dbgApi.inspect(state.id, symbol.trim()))
  })

  const restore = (f: File) => guard(async () => {
    if (!state) return
    const r = await dbgApi.restore(state.id, await f.text())
    setState(r.state)
    setEvents([])
    setLast(null)
  })

  const cur = configs.find((c) => c.name === picked)

  return (
    <>
      <div className="panel">
        <h3>单步调试</h3>
        <p className="note">
          回测视图回答「跑完是什么样」，这里回答
          <strong>「这一天为什么是这个决定」</strong>。
          单子可能死在漏斗的任何一环 —— 信号 → 定量 → 风控 → 入队 → 撮合 ——
          而只看成交记录时它们<strong>全都表现为「什么也没发生」</strong>。
        </p>

        {!state && (
          <>
            <div className="filters">
              <select
                value={picked}
                onChange={(e) => {
                  setPicked(e.target.value)
                  const c = configs.find((x) => x.name === e.target.value)
                  if (c) setText(JSON.stringify(c.config, null, 2))
                }}
              >
                {configs.map((c) => <option key={c.name} value={c.name}>{c.name}</option>)}
              </select>
              <button onClick={() => setEditing((v) => !v)}>
                {editing ? '收起配置' : '改配置'}
              </button>
              <button className="primary" disabled={busy || !text} onClick={create}>
                {busy ? '装配中…' : '开始调试'}
              </button>
            </div>
            {cur?.error && <div className="error" style={{ marginTop: 8 }}>{cur.error}</div>}
            {editing && (
              <textarea
                value={text}
                onChange={(e) => setText(e.target.value)}
                spellCheck={false}
                style={{
                  width: '100%', height: 280, marginTop: 10, fontFamily: 'var(--mono)',
                  fontSize: 12, background: 'var(--panel-2)', color: 'var(--text)',
                  border: '1px solid var(--border)', borderRadius: 4, padding: 8,
                }}
              />
            )}
            <p className="note">
              步进本身很快（实测 285 只标的走 36 步耗时 6 毫秒），
              但会话会<strong>驻留一台引擎</strong> —— 引擎状态无法从「第 N 步」
              这个数字重建，只能从头跑。所以同时最多 8 个会话。
            </p>
          </>
        )}

        {state && (
          <Controls
            state={state}
            busy={busy}
            last={last}
            gotoDay={gotoDay}
            setGotoDay={setGotoDay}
            step={step}
            reset={reset}
            onDrop={() => guard(async () => {
              await dbgApi.drop(state.id)
              setState(null)
              setEvents([])
              setLast(null)
              setInsp(null)
            })}
            onRestore={() => fileRef.current?.click()}
          />
        )}
        <input
          ref={fileRef}
          type="file"
          accept=".json"
          style={{ display: 'none' }}
          onChange={(e) => {
            const f = e.target.files?.[0]
            if (f) restore(f)
            e.target.value = ''
          }}
        />
        {err && <div className="error" style={{ marginTop: 8 }}>{err}</div>}
      </div>

      {state && <Account state={state} />}
      {state && <Funnel events={events} last={last} />}
      {state && (
        <Inspector
          symbol={symbol}
          setSymbol={setSymbol}
          insp={insp}
          onGo={inspect}
          busy={busy}
        />
      )}
      {state && <Holdings state={state} />}
    </>
  )
}

// ---- 步进控制 ----

function Controls(p: {
  state: SessionState
  busy: boolean
  last: StepResult | null
  gotoDay: string
  setGotoDay: (s: string) => void
  step: (r: { n?: number; day?: number; until?: 'signal' | 'fill' | 'reject' }) => void
  reset: () => void
  onDrop: () => void
  onRestore: () => void
}) {
  const { state: s, busy } = p
  const pct = s.totalSteps ? (s.step / s.totalSteps) * 100 : 0
  return (
    <>
      <div className="filters" style={{ marginTop: 4 }}>
        <span className="mono">
          第 {fmtNum(s.step)} / {fmtNum(s.totalSteps)} 步
          {s.day ? ` · ${fmtDay(s.day)}` : ' · 尚未开始'}
        </span>
        <button disabled={busy || s.done} onClick={() => p.step({ n: 1 })}>下一步</button>
        <button disabled={busy || s.done} onClick={() => p.step({ n: 5 })}>+5</button>
        <button disabled={busy || s.done} onClick={() => p.step({ n: 20 })}>+20</button>
        {/* until 是这套控件里最有用的一个：调试时真正想问的是
            「下一次成交发生在哪天、为什么」，而不是「再走 37 步」 */}
        <button disabled={busy || s.done} onClick={() => p.step({ until: 'signal' })}>
          跑到下个信号
        </button>
        <button disabled={busy || s.done} onClick={() => p.step({ until: 'fill' })}>
          跑到下笔成交
        </button>
        <button disabled={busy || s.done} onClick={() => p.step({ until: 'reject' })}>
          跑到下次拒单
        </button>
      </div>
      <div className="filters" style={{ marginTop: 6 }}>
        <input
          value={p.gotoDay}
          onChange={(e) => p.setGotoDay(e.target.value)}
          placeholder="跑到某日 YYYYMMDD"
          style={{ width: 170 }}
        />
        <button
          disabled={busy || s.done || !p.gotoDay}
          onClick={() => p.step({ day: Number(p.gotoDay.replaceAll('-', '')) })}
        >
          跑到该日
        </button>
        <button disabled={busy || s.done} onClick={() => p.step({ n: 0 })}>跑到末尾</button>
        <span style={{ flex: 1 }} />
        <a href={dbgApi.snapshotURL(s.id)} download>
          <button type="button" disabled={busy || !s.started}>存档</button>
        </a>
        <button disabled={busy} onClick={p.onRestore}>读档</button>
        <button disabled={busy} onClick={p.reset}>重置</button>
        <button disabled={busy} onClick={p.onDrop}>关闭会话</button>
      </div>
      <div style={{ height: 4, background: 'var(--panel-2)', borderRadius: 2, marginTop: 8 }}>
        <div
          style={{
            height: '100%', width: `${pct}%`,
            background: 'var(--accent)', borderRadius: 2,
          }}
        />
      </div>
      {p.last && (
        <p className="note" style={{ marginTop: 6 }}>
          上次前进 <strong>{fmtNum(p.last.advanced)}</strong> 步
          （其中 {fmtNum(p.last.quiet)} 步无事发生），
          停因：<strong>{p.last.stoppedBy}</strong>，耗时 {p.last.elapsedMs} 毫秒
          {p.last.truncated && ' · 明细过多已截断'}
        </p>
      )}
      {s.done && <p className="note">已到末尾。要重看请「重置」，或「读档」回到某个存档点。</p>}
    </>
  )
}

// ---- 账户 ----

function Account({ state: s }: { state: SessionState }) {
  const ret = s.initial ? (s.equity - s.initial) / s.initial : 0
  const dd = s.peak ? (s.peak - s.equity) / s.peak : 0
  return (
    <div className="panel">
      <h3>账户</h3>
      <div className="cards">
        <Stat k="权益" v={money(s.equity)} sub={`${ret >= 0 ? '+' : ''}${(ret * 100).toFixed(2)}%`} />
        <Stat k="现金" v={money(s.cash)} />
        <Stat k="持仓" v={`${s.holdings.length} 只`} sub={`在途 ${s.pending.length} 单`} />
        <Stat k="已实现" v={money(s.realized)} />
        <Stat k="费用" v={money(s.fee)} sub={`滑点 ${money(s.slippage)}`} />
        <Stat k="峰值回撤" v={`${(dd * 100).toFixed(2)}%`} sub={`峰值 ${money(s.peak)}`} />
      </div>
      {!!s.warnings?.length && s.warnings.slice(0, 3).map((w, i) => (
        <p className="note" key={i}>⚠ {w}</p>
      ))}
      {!!s.disclosures?.length && (
        <p className="note">未计入：{s.disclosures.join('；')}</p>
      )}
    </div>
  )
}

// 复用概览页的 .card 样式，不另造一套 —— 同一个项目里两套统计卡片
// 只会在改主题时变成两处要改的地方
function Stat({ k, v, sub }: { k: string; v: string; sub?: string }) {
  return (
    <div className="card">
      <div className="k">{k}</div>
      <div className="v">{v}</div>
      {sub && <div className="n">{sub}</div>}
    </div>
  )
}

// ---- 漏斗 ----

function Funnel({ events, last }: { events: StepEvent[]; last: StepResult | null }) {
  if (!events.length) {
    return (
      <div className="panel">
        <h3>事件流</h3>
        <p className="note">
          还没走过任何一步。<strong>无事发生的步不会出现在这里</strong> ——
          走 500 步时要看的是「哪几天有事」，不是 500 份空表。
        </p>
      </div>
    )
  }
  return (
    <div className="panel">
      <h3>事件流 · 信号 → 定量 → 成交 / 拒单</h3>
      <p className="note">
        新的在上。四列并排是为了看出单子<strong>死在哪一环</strong>：
        有信号没订单 = Sizer 没给额度（仓位满了或钱不够）；
        有订单没成交也没拒单 = <strong>单进了队列等下一个可成交时点</strong>（T+1）。
        {last?.truncated && ' 明细超过上限已截断。'}
      </p>
      <div style={{ maxHeight: 520, overflowY: 'auto' }}>
        {events.map((ev) => (
          <div key={`${ev.step}-${ev.day}`} style={{ borderTop: '1px solid var(--border)', padding: '8px 0' }}>
            <div className="mono" style={{ fontSize: 12, marginBottom: 4 }}>
              <strong>步 {fmtNum(ev.step)}</strong> · {fmtDay(ev.day)} · 权益{' '}
              {money(ev.equity)} · 现金 {money(ev.cash)} · 持仓 {ev.positions}
            </div>
            <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', fontSize: 12 }}>
              <Bucket title="信号" n={ev.signals?.length ?? 0} tone="">
                {ev.signals?.slice(0, 6).map((x, i) => (
                  <div key={i} className="mono">
                    {x.symbol} {x.name} · {x.kind}/{x.side}
                    {x.tag ? ` · ${x.tag}` : ''}
                  </div>
                ))}
              </Bucket>
              <Bucket title="定量" n={ev.orders?.length ?? 0} tone="">
                {ev.orders?.slice(0, 6).map((x, i) => (
                  <div key={i} className="mono">{x.symbol} {x.side} {fmtNum(x.qty)}</div>
                ))}
              </Bucket>
              <Bucket title="成交" n={ev.fills?.length ?? 0} tone="ok">
                {ev.fills?.slice(0, 6).map((x, i) => (
                  <div key={i} className="mono">
                    {x.symbol} {x.side} {fmtNum(x.qty)} @ {(x.price / 1000).toFixed(3)} · 费{' '}
                    {money(x.fee)} 滑 {money(x.slippage)}
                  </div>
                ))}
              </Bucket>
              <Bucket title="拒单" n={ev.rejects?.length ?? 0} tone="warn">
                {ev.rejects?.slice(0, 6).map((x, i) => (
                  <div key={i} className="mono">
                    {x.symbol} {x.side} {fmtNum(x.qty)} ·{' '}
                    <span className="tag warn">{x.rule || x.reason}</span> {x.detail}
                  </div>
                ))}
              </Bucket>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function Bucket(p: { title: string; n: number; tone: string; children?: React.ReactNode }) {
  return (
    <div style={{ flex: '1 1 240px', minWidth: 220 }}>
      <div className="k">
        {p.title} <span className={`tag ${p.tone}`}>{p.n}</span>
      </div>
      {p.n > 0 ? p.children : <div className="muted">—</div>}
      {p.n > 6 && <div className="muted">…还有 {p.n - 6} 条</div>}
    </div>
  )
}

// ---- 标的检视 ----

function Inspector(p: {
  symbol: string
  setSymbol: (s: string) => void
  insp: Inspect | null
  onGo: () => void
  busy: boolean
}) {
  const d = p.insp
  const ps = d?.priceScale || 1000
  const px = (v: number) => (v / ps).toFixed(ps >= 1e8 ? 2 : 3)
  return (
    <div className="panel">
      <h3>标的检视 · 这一天它到底什么情况</h3>
      <div className="filters">
        <input
          value={p.symbol}
          onChange={(e) => p.setSymbol(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && p.onGo()}
          placeholder="代码，如 600000"
          style={{ width: 160 }}
        />
        <button disabled={p.busy || !p.symbol.trim()} onClick={p.onGo}>看</button>
        {d && <a href={`#/kline/${d.symbol}`}>在 K 线里看 →</a>}
      </div>
      {!d && (
        <p className="note">
          「今天为什么没买它」的答案不在成交记录里 ——
          可能是<strong>指标没就绪</strong>、可能是<strong>一字涨停</strong>、
          可能是<strong>单还在队列里</strong>。这几件事要放在一起才看得出来。
        </p>
      )}
      {d && (
        <>
          <div className="cards">
            <Stat
              k="收盘"
              v={d.hasBar ? px(d.close) : '无 bar'}
              sub={d.hasBar ? `前收 ${px(d.preclose)}` : '当日无行情'}
            />
            <Stat k="最高/最低" v={d.hasBar ? `${px(d.high)} / ${px(d.low)}` : '—'} />
            <Stat
              k="涨停/跌停"
              v={d.hasLimit ? `${px(d.limitUp)} / ${px(d.limitDn)}` : '无限制'}
              sub={d.hasBar && d.hasLimit && d.close >= d.limitUp ? '⚠ 已达涨停' : undefined}
            />
            <Stat k="后复权收盘" v={d.hasBar ? px(d.adjClose) : '—'} sub="指标吃的是这个" />
            <Stat k="成交额" v={d.hasBar ? fmtCompact(d.amount / 100) + ' 元' : '—'} />
            <Stat
              k="持仓 / 可卖"
              v={`${fmtNum(d.held)} / ${fmtNum(d.available)}`}
              sub={d.held ? `成本 ${money(d.cost)}` : undefined}
            />
          </div>
          {(d.suspended || d.isST) && (
            <p className="note">
              {d.suspended && '⚠ 该 bar 不可成交（停牌 / 零成交）。'}
              {d.isST && ' ⚠ ST 标的，涨跌停幅度不同。'}
            </p>
          )}
          <table style={{ width: 'auto', marginTop: 8 }}>
            <tbody>
              {d.indicators.map((i) => (
                <tr key={i.key}>
                  <td className="muted">{i.key}</td>
                  <td>
                    {/* 未就绪时**绝不显示数值** —— 画成 0 会让人以为
                        「指标是 0 所以没信号」，而真相是「还没算出来」 */}
                    {i.ready ? (
                      <span className="mono">
                        {(i.names ?? [])
                          .map((n, k) => `${n}=${(i.values?.[k] ?? 0).toFixed(4)}`)
                          .join('   ')}
                      </span>
                    ) : (
                      <span className="tag warn">
                        {i.names?.length ? '预热未完成，值不可用' : '该标的还没有这个指标实例'}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!!d.pending?.length && (
            <p className="note">
              在途 {d.pending.length} 单：
              {d.pending.map((x, i) => (
                <span key={i} className="mono">
                  {' '}
                  {x.side} {fmtNum(x.qty)}（{fmtDay(x.signalDay)} 发出，按 {x.priceRef} 价，已试{' '}
                  {x.tried}/{x.maxSteps}）
                </span>
              ))}
            </p>
          )}
        </>
      )}
    </div>
  )
}

// ---- 持仓与在途 ----

function Holdings({ state: s }: { state: SessionState }) {
  const hCols: Col<DbgPosition>[] = [
    { key: 'symbol', title: '代码', render: (r) => <a href={`#/kline/${r.symbol}`}>{r.symbol}</a> },
    { key: 'name', title: '名称', render: (r) => r.name },
    { key: 'qty', title: '持仓', num: true, render: (r) => fmtNum(r.qty) },
    {
      key: 'available',
      title: '可卖',
      num: true,
      // 可卖小于持仓 = T+1 锁着。这是「为什么卖不掉」最常见的答案
      render: (r) =>
        r.available < r.qty
          ? <span className="warn">{fmtNum(r.available)}</span>
          : fmtNum(r.available),
    },
    { key: 'last', title: '现价', num: true, render: (r) => (r.last / 1000).toFixed(3) },
    { key: 'value', title: '市值', num: true, render: (r) => money(r.value) },
    {
      key: 'pnl',
      title: '浮盈',
      num: true,
      render: (r) => <span className={r.pnl >= 0 ? 'up' : 'down'}>{money(r.pnl)}</span>,
    },
    {
      key: 'suspended',
      title: '状态',
      render: (r) => (r.suspended ? <span className="tag warn">不可成交</span> : ''),
    },
  ]
  const pCols: Col<DbgPending>[] = [
    { key: 'symbol', title: '代码', render: (r) => r.symbol },
    { key: 'name', title: '名称', render: (r) => r.name },
    { key: 'side', title: '方向', render: (r) => (r.side === 'buy' ? '买' : '卖') },
    { key: 'qty', title: '数量', num: true, render: (r) => fmtNum(r.qty) },
    { key: 'signalDay', title: '信号日', render: (r) => fmtDay(r.signalDay) },
    { key: 'priceRef', title: '价基准', render: (r) => r.priceRef },
    { key: 'tried', title: '已试', num: true, render: (r) => `${r.tried} / ${r.maxSteps}` },
  ]
  return (
    <>
      <div className="panel">
        <h3>持仓 · {s.holdings.length} 只</h3>
        {s.holdings.length ? (
          <DataTable rows={s.holdings} cols={hCols} />
        ) : (
          <p className="note">空仓。</p>
        )}
      </div>
      <div className="panel">
        <h3>在途订单 · {s.pending.length} 单</h3>
        <p className="note">
          <strong>「为什么今天没买」最常见的答案就在这张表里</strong> ——
          单已经发出，但要等到最早可成交时点（主板 T+1 次日开盘）才撮合。
          看不到这张表，就只会得出「策略没发信号」这个错误结论。
        </p>
        {s.pending.length ? (
          <DataTable rows={s.pending} cols={pCols} />
        ) : (
          <p className="note">没有在途订单。</p>
        )}
      </div>
    </>
  )
}
