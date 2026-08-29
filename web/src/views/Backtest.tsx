import { useEffect, useMemo, useState } from 'react'
import { fmtCompact, fmtDay, fmtNum, runApi } from '../api'
import EquityChart from '../components/EquityChart'
import { DataTable, ErrBox, Loading, downloadCSV, type Col } from '../components/ui'
import { getRun, setRun } from '../runStore'
import type { ConfigItem, Meta, RoundTrip, RunFill, RunReject, RunResult } from '../types'

// 回测结果视图。**只看跑完的结果，不做单步** —— 单步驱动、会话、
// WebSocket 是 v0.4 的事。这里回答的是「跑完是什么样」。

const yuan = (c: number) => c / 100
const pct = (v: number, sign = true) =>
  Number.isFinite(v) ? `${sign && v > 0 ? '+' : ''}${(v * 100).toFixed(2)}%` : '—'
const num = (v: number, d = 3) => (Number.isFinite(v) ? v.toFixed(d) : '—')

export default function Backtest({ meta }: { meta: Meta }) {
  const [configs, setConfigs] = useState<ConfigItem[]>([])
  const [picked, setPicked] = useState('')
  const [text, setText] = useState('')
  const [editing, setEditing] = useState(false)
  const [run, setRunState] = useState<RunResult | null>(getRun())
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [tab, setTab] = useState<'trips' | 'fills' | 'rejects'>('trips')

  useEffect(() => {
    runApi.configs().then(
      (d) => {
        setConfigs(d.configs)
        if (d.configs.length > 0 && !picked) {
          setPicked(d.configs[0].name)
          setText(JSON.stringify(d.configs[0].config, null, 2))
        }
      },
      (e) => setErr(String(e?.message ?? e)),
    )
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const pick = (name: string) => {
    setPicked(name)
    const c = configs.find((x) => x.name === name)
    if (c) setText(JSON.stringify(c.config, null, 2))
  }

  const go = async () => {
    setBusy(true)
    setErr('')
    try {
      const r = await runApi.backtest(text)
      setRunState(r)
      setRun(r) // 存进全局，K 线页据此叠成交标注
    } catch (e) {
      setErr(String((e as Error)?.message ?? e))
    } finally {
      setBusy(false)
    }
  }

  const current = configs.find((c) => c.name === picked)

  return (
    <>
      <h2>回测</h2>
      <div className="sub">
        服务端用已载入的数据跑，约 200 毫秒出结果 ——
        不必每次等 30 秒重读盘。<strong>不做单步</strong>，那是 v0.4。
      </div>

      <div className="panel">
        <div className="filters">
          <div className="field">
            <label>配置</label>
            <select value={picked} onChange={(e) => pick(e.target.value)} style={{ minWidth: 320 }}>
              {configs.map((c) => (
                <option key={c.name} value={c.name}>
                  {c.title}（{c.name}）{c.error ? ' ⚠ 有错' : ''}
                </option>
              ))}
            </select>
          </div>
          <button className="primary" onClick={go} disabled={busy || !text}>
            {busy ? '跑着…' : '跑'}
          </button>
          <button onClick={() => setEditing(!editing)}>
            {editing ? '收起配置' : '改参数'}
          </button>
        </div>
        {current?.error && <div className="error" style={{ marginTop: 8 }}>{current.error}</div>}
        {editing && (
          <>
            <textarea
              value={text}
              onChange={(e) => setText(e.target.value)}
              spellCheck={false}
              style={{
                width: '100%', height: 300, marginTop: 10, fontFamily: 'var(--mono)',
                fontSize: 12, background: 'var(--panel-2)', color: 'var(--text)',
                border: '1px solid var(--border)', borderRadius: 4, padding: 8,
              }}
            />
            <p className="note">
              改完直接跑，不落盘。参数名写错会在跑之前就报出来 ——
              配置校验会把每个模块真构造一遍（<code>dryBuild</code>），
              不必等数据载入才发现拼错了。
            </p>
          </>
        )}
      </div>

      {err && <ErrBox msg={err} />}
      {busy && !run && <Loading what="回测" />}
      {run && <Result run={run} meta={meta} tab={tab} setTab={setTab} />}
    </>
  )
}

function Result({
  run, meta, tab, setTab,
}: {
  run: RunResult
  meta: Meta
  tab: 'trips' | 'fills' | 'rejects'
  setTab: (t: 'trips' | 'fills' | 'rejects') => void
}) {
  const m = run.metrics
  const friction = m.fee_cents + m.slippage_cents
  const cards = [
    { k: '总收益', v: pct(m.total_return), cls: m.total_return >= 0 ? 'up' : 'down' },
    { k: '年化收益', v: pct(m.annual_return), n: m.annual_reliable ? undefined : '样本不足一年，外推',
      cls: m.annual_return >= 0 ? 'up' : 'down' },
    { k: '年化波动', v: pct(m.annual_vol, false) },
    { k: '夏普', v: num(m.sharpe), n: `无风险 ${(m.risk_free_ppm / 10000).toFixed(2)}%` },
    { k: '最大回撤', v: pct(m.max_drawdown.ratio, false), cls: 'down',
      n: `${fmtDay(m.max_drawdown.peak_day)} → ${fmtDay(m.max_drawdown.trough_day)}` },
    { k: '卡玛', v: num(m.calmar) },
    { k: '胜率', v: pct(m.trades.win_rate, false),
      n: `${m.trades.wins} 赢 / ${m.trades.losses} 输` },
    { k: '盈亏比', v: num(m.trades.profit_factor) },
    { k: '年化换手', v: `${m.turnover.toFixed(1)} 倍` },
    { k: '摩擦成本', v: `${(friction / m.initial_cents * 100).toFixed(2)}%`,
      n: `费用 ${fmtCompact(yuan(m.fee_cents))} + 滑点 ${fmtCompact(yuan(m.slippage_cents))} 元` },
  ]

  return (
    <>
      <div className="cards">
        {cards.map((c) => (
          <div className="card" key={c.k}>
            <div className="k">{c.k}</div>
            <div className={`v ${c.cls ?? ''}`}>{c.v}</div>
            {c.n && <div className="n">{c.n}</div>}
          </div>
        ))}
      </div>

      <div className="panel" style={{ marginTop: 14 }}>
        <h3>
          净值曲线
          {m.benchmark && `（对标 ${m.benchmark.name}）`}
        </h3>
        <EquityChart
          curve={run.curve} initialCents={m.initial_cents}
          benchName={m.benchmark?.name}
        />
        {m.benchmark && <BenchNote run={run} />}
        <p className="note">
          区间 {fmtDay(m.from_day)} ~ {fmtDay(m.to_day)}，{fmtNum(m.steps)} 个交易日 ≈{' '}
          {m.years.toFixed(2)} 年 · 年化系数 <strong>{m.trading_days_per_year.toFixed(2)}</strong>{' '}
          交易日/年（由日历数出，不是 252） · 标的 {run.stats.instruments} 只 ·
          引擎耗时 {run.stats.durationMs} 毫秒
        </p>
        {run.warnings?.map((w, i) => (
          <p className="note" key={i}>⚠ {w}</p>
        ))}
      </div>

      <div className="panel">
        <h3>指纹（C5）</h3>
        <table style={{ width: 'auto' }}>
          <tbody>
            <tr><td className="muted">输入</td><td className="mono">{run.fingerprint.input}</td></tr>
            <tr><td className="muted">输出</td><td className="mono">{run.fingerprint.output}</td></tr>
            <tr><td className="muted">数据</td><td className="mono">{run.fingerprint.data}</td></tr>
            <tr><td className="muted">引擎</td><td className="mono">{run.fingerprint.engine}</td></tr>
          </tbody>
        </table>
        <p className="note">
          同输入指纹必须给出同输出指纹。
          {!run.fingerprint.reproducible &&
            ' ⚠ dev 构建，指纹不保证跨构建可复现 —— 两次 dev 构建之间源码可能已经变了。'}
        </p>
      </div>

      <div className="panel">
        <div className="filters" style={{ marginBottom: 10 }}>
          {([
            ['trips', `逐轮交易 ${fmtNum(run.roundTrips.length)}`],
            ['fills', `成交 ${fmtNum(run.fills.length)}`],
            ['rejects', `拒单 ${fmtNum(run.rejectTotal)}`],
          ] as const).map(([k, label]) => (
            <span key={k} className={`chip ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>
              {label}
            </span>
          ))}
        </div>
        {tab === 'trips' && <TripTable rows={run.roundTrips} meta={meta} />}
        {tab === 'fills' && <FillTable rows={run.fills} meta={meta} />}
        {tab === 'rejects' && <RejectTable run={run} />}
      </div>
    </>
  )
}

function BenchNote({ run }: { run: RunResult }) {
  const b = run.metrics.benchmark!
  const full = b.covered === b.total
  return (
    <p className="note">
      策略 <strong>{pct(b.strategy_return)}</strong> · 基准 <strong>{pct(b.return)}</strong> ·
      超额 <strong className={b.excess >= 0 ? 'up' : 'down'}>{pct(b.excess)}</strong>
      {' · '}Beta {num(b.beta)} · Alpha {pct(b.alpha)}（年化）· 信息比率 {num(b.information_ratio)}
      {!full && (
        <>
          <br />⚠ 基准只覆盖 {b.covered} / {b.total} 个时点
          （{((b.covered / b.total) * 100).toFixed(1)}%），
          <strong>未覆盖区间不计超额</strong> —— 数据里没有指数，
          宽基 ETF 最早到 2012，按 0 收益补齐会凭空造出一段超额收益。
        </>
      )}
    </p>
  )
}

// ---- 表格 ----

function useLocalPage<T>(rows: T[], size: number) {
  const [page, setPage] = useState(1)
  useEffect(() => setPage(1), [rows])
  const pages = Math.max(1, Math.ceil(rows.length / size))
  const slice = useMemo(() => rows.slice((page - 1) * size, page * size), [rows, page, size])
  return { page, pages, slice, setPage }
}

function Pager({ page, pages, setPage }: { page: number; pages: number; setPage: (n: number) => void }) {
  return (
    <div className="pager">
      <span className="muted">第 {page} / {pages} 页</span>
      <span className="grow" />
      <button disabled={page <= 1} onClick={() => setPage(page - 1)}>上一页</button>
      <button disabled={page >= pages} onClick={() => setPage(page + 1)}>下一页</button>
    </div>
  )
}

function TripTable({ rows, meta }: { rows: RoundTrip[]; meta: Meta }) {
  const [sortPnl, setSortPnl] = useState(false)
  const sorted = useMemo(
    () => (sortPnl ? [...rows].sort((a, b) => a.pnl - b.pnl) : rows),
    [rows, sortPnl],
  )
  const { page, pages, slice, setPage } = useLocalPage(sorted, 40)
  const cols: Col<RoundTrip>[] = [
    { key: 'sym', title: '代码', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', render: (r) => r.name },
    { key: 'open', title: '开仓', render: (r) => fmtDay(r.openDay) },
    { key: 'close', title: '平仓', render: (r) => fmtDay(r.closeDay) },
    { key: 'hold', title: '持有', num: true, render: (r) => `${r.holdDays} 天` },
    { key: 'qty', title: '数量', num: true, render: (r) => fmtNum(r.qty) },
    { key: 'cost', title: '成本', num: true, render: (r) => yuan(r.cost).toFixed(2) },
    { key: 'proceed', title: '收入', num: true, render: (r) => yuan(r.proceed).toFixed(2) },
    {
      key: 'pnl', title: '盈亏', num: true, sort: 'pnl',
      render: (r) => (
        <span className={r.pnl > 0 ? 'up' : r.pnl < 0 ? 'down' : 'muted'}>
          {r.pnl > 0 ? '+' : ''}{yuan(r.pnl).toFixed(2)}
        </span>
      ),
    },
    {
      key: 'flag', title: '', render: (r) =>
        r.fromBonus ? <span className="tag">送转份额</span> : null,
    },
  ]
  return (
    <>
      <div className="filters" style={{ marginBottom: 8 }}>
        <button onClick={() => setSortPnl(!sortPnl)}>
          {sortPnl ? '按平仓时间排' : '按盈亏排（找最惨的）'}
        </button>
        <button onClick={() => downloadCSV('round_trips.csv',
          ['symbol', 'name', 'open_day', 'close_day', 'hold_days', 'qty',
           'cost_cents', 'proceed_cents', 'pnl_cents', 'from_bonus'],
          rows.map((r) => [r.symbol, r.name, r.openDay, r.closeDay, r.holdDays,
            r.qty, r.cost, r.proceed, r.pnl, r.fromBonus ? 1 : 0]))}>
          导出全部 CSV
        </button>
        <span className="muted" style={{ alignSelf: 'center', fontSize: 12 }}>
          点任意一行进该标的 K 线，成交点会叠在图上
        </span>
      </div>
      <DataTable cols={cols} rows={slice} onRowClick={(r) => { location.hash = `/kline/${r.id}` }}
        empty="本次回测没有完整的一轮交易" />
      <Pager page={page} pages={pages} setPage={setPage} />
      <p className="note">
        <strong>「送转份额」</strong>表示这一轮的建仓份额来自送股 / 转增，
        按零成本入队 —— 成本已经付在原有份额上，再计一次会让盈亏比虚高。
        回测结束时仍持有的仓位<strong>不计入胜率</strong>
        （{meta.stats.instruments > 0 ? '' : ''}未平仓既不是赢也不是输）。
      </p>
    </>
  )
}

function FillTable({ rows, meta }: { rows: RunFill[]; meta: Meta }) {
  const { page, pages, slice, setPage } = useLocalPage(rows, 40)
  const ps = meta.scales.price
  const cols: Col<RunFill>[] = [
    { key: 'd', title: '交易日', render: (r) => fmtDay(r.d) },
    { key: 'sym', title: '代码', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', render: (r) => r.name },
    {
      key: 'side', title: '方向', render: (r) => (
        <span className={r.side === 'buy' ? 'up' : 'down'}>{r.side === 'buy' ? '买' : '卖'}</span>
      ),
    },
    { key: 'price', title: '成交价', num: true, render: (r) => (r.price / ps).toFixed(3) },
    { key: 'qty', title: '数量', num: true, render: (r) => fmtNum(r.qty) },
    { key: 'amt', title: '成交额', num: true, render: (r) => fmtCompact(yuan(r.amount)) },
    { key: 'fee', title: '费用', num: true, render: (r) => yuan(r.fee).toFixed(2) },
    { key: 'slip', title: '滑点', num: true, render: (r) => yuan(r.slippage).toFixed(2) },
    { key: 'tag', title: '来源', render: (r) => <span className="tag">{r.tag}</span> },
  ]
  return (
    <>
      <DataTable cols={cols} rows={slice} onRowClick={(r) => { location.hash = `/kline/${r.id}` }}
        empty="本次回测没有成交" />
      <Pager page={page} pages={pages} setPage={setPage} />
      <p className="note">
        <strong>成交价不含滑点</strong> —— 滑点按成交额单列，因为它是执行质量的损耗，
        不是付给第三方的费用。这样成交价仍是市场上真实存在的价格，可与行情核对。
      </p>
    </>
  )
}

function RejectTable({ run }: { run: RunResult }) {
  const [reason, setReason] = useState('')
  const filtered = useMemo(
    () => (reason ? run.rejections.filter((r) => keyOf(r) === reason) : run.rejections),
    [run.rejections, reason],
  )
  const { page, pages, slice, setPage } = useLocalPage(filtered, 40)
  const cols: Col<RunReject>[] = [
    { key: 'd', title: '交易日', render: (r) => fmtDay(r.d) },
    { key: 'sym', title: '代码', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', render: (r) => r.name },
    {
      key: 'side', title: '方向', render: (r) => (
        <span className={r.side === 'buy' ? 'up' : 'down'}>{r.side === 'buy' ? '买' : '卖'}</span>
      ),
    },
    { key: 'qty', title: '申报', num: true, render: (r) => fmtNum(r.qty) },
    { key: 'reason', title: '原因', render: (r) => <span className="tag warn">{keyOf(r)}</span> },
    { key: 'detail', title: '数值依据', render: (r) => <span className="muted">{r.detail}</span> },
  ]
  const entries = Object.entries(run.rejectBy).sort((a, b) => b[1] - a[1])
  return (
    <>
      <div className="chips" style={{ marginBottom: 10 }}>
        <span className={`chip ${reason === '' ? 'on' : ''}`} onClick={() => setReason('')}>
          全部 {fmtNum(run.rejectTotal)}
        </span>
        {entries.map(([k, n]) => (
          <span key={k} className={`chip ${reason === k ? 'on' : ''}`} onClick={() => setReason(k)}>
            {k} {fmtNum(n)}
          </span>
        ))}
      </div>
      <DataTable cols={cols} rows={slice} onRowClick={(r) => { location.hash = `/kline/${r.id}` }}
        empty="没有这一类拒单" />
      <Pager page={page} pages={pages} setPage={setPage} />
      <p className="note">
        <strong>拒单原因是结构化的</strong>，还带数值依据 ——
        「涨停买不进」会告诉你参考价多少、涨停价多少。
        这正是单步调试的核心价值：回答「为什么没成交」，而不只是「没成交」。
        {run.rejections.length < run.rejectTotal && (
          <> 服务端最多返回 {fmtNum(run.rejections.length)} 条明细，
            上面的分布是全部 {fmtNum(run.rejectTotal)} 条的统计。</>
        )}
      </p>
    </>
  )
}

function keyOf(r: RunReject): string {
  return r.rule ? `风控:${r.rule}` : r.reason
}
