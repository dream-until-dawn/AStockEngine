import { useEffect, useMemo, useState } from 'react'
import { fmtCompact, fmtDay, fmtNum, runApi } from '../api'
import EquityChart from '../components/EquityChart'
import ConfigBuilder from '../components/ConfigBuilder'
import { DataTable, ErrBox, Loading, downloadCSV, type Col } from '../components/ui'
import { getRun, setRun } from '../runStore'
import type {
  ConfigItem, Meta, RoundTrip, RunFill, RunReject, RunResult,
} from '../types'

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
  const [pickUni, setPickUni] = useState(true)
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

  // 配置的原始 JSON 是**唯一真相**，选择器只是它的一个视图：
  // 读出 data.universe 给选择器，选完写回去。两边各存一份状态必然会不同步。
  let parsed: Record<string, any> | null = null
  try {
    parsed = JSON.parse(text)
  } catch {
    parsed = null
  }
  // 装配器与 JSON 编辑器是同一份数据的两个视图：
  // 装配器改完，整份写回 text；text 是唯一真相
  const setCfg = (next: Record<string, any>) => setText(JSON.stringify(next, null, 2))

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
          <button onClick={() => setPickUni(!pickUni)}>
            {pickUni ? '收起装配' : '装配 / 改参数'}
          </button>
          <button onClick={() => setEditing(!editing)}>
            {editing ? '收起 JSON' : '看 JSON'}
          </button>
        </div>

        {pickUni && (
          <div style={{ marginTop: 12, borderTop: '1px solid var(--border)', paddingTop: 12 }}>
            {parsed ? (
              <ConfigBuilder cfg={parsed} onChange={setCfg} meta={meta} />
            ) : (
              <div className="muted">配置 JSON 当前不合法，改好之后装配器才能用</div>
            )}
            <p className="note">
              标的池决定结论。同一个 <code>macd_cross</code>，
              在「按 ID 前 300 只」上跑输基准 15 个点，
              换成「在市主板全部个股」就跑赢 5.6 个点 ——
              <strong>换池子换的是结论，不是精度</strong>。
            </p>
          </div>
        )}
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
  // 计价单位由服务端从 Market 取。加密账户的余额不是「元」，
  // 而单位错的数字看上去完全正常 —— 前端不自己按市场名查表
  const cur = run.market?.money ?? '元'
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
    // 离场原因：正常卖出、止损、止盈、熔断清仓、被强平 ——
    // 几种结局对「策略行不行」的意义完全不同，只看胜率是看不出来的。
    // 强平尤其要显眼：那不是策略的决定，是市场施加的
    ...(closeTop(m.trades.close_by) ? [{
      k: '离场原因',
      v: closeTop(m.trades.close_by),
      n: closeBreakdown(m.trades.close_by),
      cls: (m.trades.close_by?.liquidation ?? 0) > 0 ? 'down' : undefined,
    }] : []),
    { k: '摩擦成本', v: `${(friction / m.initial_cents * 100).toFixed(2)}%`,
      n: `费用 ${fmtCompact(yuan(m.fee_cents))} + 滑点 ${fmtCompact(yuan(m.slippage_cents))} ${cur}` },
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

      {!!run.disclosures?.length && (
        <div className="panel">
          {/* 报告里的每个数字都在说「发生了什么」，这一块说的是「什么没算」。
              漏算的成本不报错、不异常，只是让结果一致地偏乐观 —— 所以它
              必须和绩效放在同一屏，而不是藏在文档里 */}
          <h3>本次回测未计入</h3>
          <ul className="note" style={{ margin: 0, paddingLeft: 18 }}>
            {run.disclosures.map((d, i) => <li key={i}>{d}</li>)}
          </ul>
        </div>
      )}

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
        {tab === 'trips' && <TripTable rows={run.roundTrips} meta={meta} cur={cur} />}
        {tab === 'fills' && <FillTable rows={run.fills} cur={cur} />}
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

// CLOSE_LABEL 把成交 tag 译成人读的离场原因。
//
// **认不出来的 tag 原样显示**，不归到「其他」—— 策略可以自定 tag，
// 把它们混成一堆就等于把归因扔了（A 股的 macd_death / ma_death 就是这样）。
const CLOSE_LABEL: Record<string, string> = {
  liquidation: '强平',
  stop_loss: '止损',
  take_profit: '止盈',
  trailing_stop: '移动止损',
  drawdown_halt: '熔断清仓',
  tree_sell: '卖出信号',
  tree_buy_invalid: '虚拟买入',
}
/** 需要显眼标出来的离场原因 —— 它们不是策略自己的决定。 */
const CLOSE_ALERT = new Set(['liquidation', 'drawdown_halt', 'stop_loss'])

function closeLabel(tag?: string): string {
  if (!tag) return '未标注'
  return CLOSE_LABEL[tag] ?? tag
}

type TripFilter = 'all' | 'real' | 'virtual'

function TripTable({ rows, meta, cur }: { rows: RoundTrip[]; meta: Meta; cur: string }) {
  const [sortPnl, setSortPnl] = useState(false)
  const [kind, setKind] = useState<TripFilter>('all')
  const nVirtual = useMemo(() => rows.filter((r) => r.virtual).length, [rows])
  const sorted = useMemo(() => {
    const kept = kind === 'all' ? rows : rows.filter((r) => (kind === 'virtual') === !!r.virtual)
    // 虚拟轮次没有金额，按盈亏排时用收益率
    return sortPnl
      ? [...kept].sort((a, b) => (a.virtual ? a.ratio : a.pnl / 1e9) - (b.virtual ? b.ratio : b.pnl / 1e9))
      : kept
  }, [rows, sortPnl, kind])
  const { page, pages, slice, setPage } = useLocalPage(sorted, 40)
  const cols: Col<RoundTrip>[] = [
    {
      key: 'kind', title: '仓位',
      render: (r) => r.virtual
        ? <span className="tag" title="策略说该买，但被「有效性」树挡下来了：没有真实成交、不占资金、不计入胜率">虚拟</span>
        : <span className="muted">实仓</span>,
    },
    { key: 'sym', title: '代码', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', render: (r) => r.name },
    { key: 'open', title: '开仓', render: (r) => fmtDay(r.openDay) },
    { key: 'close', title: '平仓', render: (r) => fmtDay(r.closeDay) },
    { key: 'hold', title: '持有', num: true, render: (r) => `${r.holdDays} 天` },
    {
      key: 'dir', title: '方向', render: (r) =>
        r.short ? <span className="down">空</span> : <span className="up">多</span>,
    },
    {
      key: 'why', title: '离场原因',
      render: (r) => (
        <span className={CLOSE_ALERT.has(r.closeTag ?? '') ? 'down' : ''}>
          {closeLabel(r.closeTag)}
        </span>
      ),
    },
    {
      key: 'qty', title: '数量', num: true,
      render: (r) => (r.virtual ? <span className="muted">—</span> : fmtNum(r.qty / (r.qtyScale || 1))),
    },
    {
      key: 'cost', title: `成本(${cur})`, num: true,
      render: (r) => (r.virtual ? <span className="muted">—</span> : yuan(r.cost).toFixed(2)),
    },
    {
      key: 'proceed', title: `收入(${cur})`, num: true,
      render: (r) => (r.virtual ? <span className="muted">—</span> : yuan(r.proceed).toFixed(2)),
    },
    {
      key: 'pnl', title: `盈亏(${cur})`, num: true, sort: 'pnl',
      render: (r) => {
        // 虚拟持仓从未占用资金，编一个「本该赚多少钱」出来是假的 ——
        // 收益率才是这笔决策唯一真实可算的东西
        if (r.virtual) {
          return (
            <span className={r.ratio > 0 ? 'up' : r.ratio < 0 ? 'down' : 'muted'}>
              {r.ratio > 0 ? '+' : ''}{(r.ratio * 100).toFixed(2)}%
            </span>
          )
        }
        return (
          <span className={r.pnl > 0 ? 'up' : r.pnl < 0 ? 'down' : 'muted'}>
            {r.pnl > 0 ? '+' : ''}{yuan(r.pnl).toFixed(2)}
          </span>
        )
      },
    },
    {
      key: 'flag', title: '', render: (r) =>
        r.fromBonus ? <span className="tag">送转份额</span> : null,
    },
  ]
  return (
    <>
      <div className="filters" style={{ marginBottom: 8 }}>
        {nVirtual > 0 && ([
          ['all', `全部 ${rows.length}`],
          ['real', `实仓 ${rows.length - nVirtual}`],
          ['virtual', `虚拟 ${nVirtual}`],
        ] as const).map(([k, label]) => (
          <span key={k} className={`chip ${kind === k ? 'on' : ''}`}
            onClick={() => { setKind(k); setPage(1) }}>{label}</span>
        ))}
        <button onClick={() => setSortPnl(!sortPnl)}>
          {sortPnl ? '按平仓时间排' : '按盈亏排（找最惨的）'}
        </button>
        <button onClick={() => downloadCSV('round_trips.csv',
          ['symbol', 'name', 'virtual', 'short', 'open_day', 'close_day', 'hold_days',
           'close_tag', 'qty', 'cost_cents', 'proceed_cents', 'pnl_cents', 'ratio',
           'from_bonus'],
          rows.map((r) => [r.symbol, r.name, r.virtual ? 1 : 0, r.short ? 1 : 0,
            r.openDay, r.closeDay, r.holdDays, r.closeTag ?? '',
            r.qty, r.cost, r.proceed, r.pnl, r.ratio, r.fromBonus ? 1 : 0]))}>
          导出全部 CSV
        </button>
        <span className="muted" style={{ alignSelf: 'center', fontSize: 12 }}>
          点任意一行进该标的 K 线，成交点会叠在图上
        </span>
      </div>
      {nVirtual > 0 && (
        <p className="note">
          <strong>虚拟</strong>=「买入树说买、有效性树说不算数」的那些机会：
          没有真实成交、不占用资金、不计入胜率与盈亏。
          它们只有<strong>收益率</strong>没有金额 —— 从未经过仓位定量，
          编一个「本该赚多少钱」出来是假的。
          把虚拟与实仓并排看，才判断得出那棵有效性树到底该不该过滤它们。
        </p>
      )}
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

function FillTable({ rows, cur }: { rows: RunFill[]; cur: string }) {
  const { page, pages, slice, setPage } = useLocalPage(rows, 40)
  // 每行带着自己的 scale —— 全局 scale 是 A 股口径，
  // 拿它去除 BTC 的成交价会差好几个数量级
  const cols: Col<RunFill>[] = [
    { key: 'd', title: '交易日', render: (r) => fmtDay(r.d) },
    { key: 'sym', title: '代码', render: (r) => <span className="mono">{r.symbol}</span> },
    { key: 'name', title: '名称', render: (r) => r.name },
    {
      key: 'side', title: '方向', render: (r) => (
        <span className={r.side === 'buy' ? 'up' : 'down'}>
          {r.leg || (r.side === 'buy' ? '买' : '卖')}
        </span>
      ),
    },
    {
      key: 'price', title: '成交价', num: true,
      render: (r) => (r.price / (r.priceScale || 1000)).toFixed(3),
    },
    {
      key: 'qty', title: '数量', num: true,
      render: (r) => fmtNum(r.qty / (r.qtyScale || 1)),
    },
    { key: 'amt', title: `成交额(${cur})`, num: true, render: (r) => fmtCompact(yuan(r.amount)) },
    { key: 'fee', title: `费用(${cur})`, num: true, render: (r) => yuan(r.fee).toFixed(2) },
    { key: 'slip', title: `滑点(${cur})`, num: true, render: (r) => yuan(r.slippage).toFixed(2) },
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

/** closeTop 占比最大的那个离场原因，用作卡片主数字。 */
function closeTop(by?: Record<string, number>): string {
  const entries = Object.entries(by ?? {})
  if (!entries.length) return ''
  // 强平优先显示 —— 哪怕只有一笔。它不是策略的决定，
  // 被最常见的那个原因盖过去就等于没报
  const liq = by?.liquidation ?? 0
  if (liq > 0) return `强平 ${liq}`
  const [tag, n] = entries.sort((a, b) => b[1] - a[1])[0]
  return `${closeLabel(tag)} ${n}`
}

/** closeBreakdown 全部离场原因，按轮次数降序。 */
function closeBreakdown(by?: Record<string, number>): string | undefined {
  const entries = Object.entries(by ?? {}).sort((a, b) => b[1] - a[1])
  if (!entries.length) return undefined
  return entries.map(([tag, n]) => `${closeLabel(tag)} ${n}`).join(' · ')
}
