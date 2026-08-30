import { useEffect, useState } from 'react'
import { runApi } from '../api'
import { ErrBox, Loading } from '../components/ui'
import type {
  SweepAnalysis, SweepBrief, SweepDetail, SweepMargin, SweepTop,
} from '../types'

// 海选视图。
//
// **顺序就是结论的可信度链条**，界面照着排：
//
//   1. 这次海选有没有意义？（噪声基线 vs 全网格散布）
//   2. 门槛校准对不对？（覆盖不足的比例）
//   3. 结果从哪来？（强平 / 熔断 / 摩擦）
//   4. 哪个轴有用？（逐维边际）
//   5. 有没有稳健区域？（高原）
//
// 第 1 步说「不可分辨」时，后面的排名就是随机数 —— 所以那一屏
// 会直接盖掉排名，而不是让人自己去看一张五颜六色的热力图。
//
// 第 2 步是本项目实测唯一能**悄悄产出错误结论**的地方：门槛比典型
// 轮次还高时，92% 的参数组退出排名，剩下的子集算出来的结论可以完全相反，
// 而没有任何地方报错。所以它在第 3 步之前，且会标红。

const pp = (v: number) => `${v >= 0 ? '+' : ''}${(v * 100).toFixed(2)}`
const ppAbs = (v: number) => (v * 100).toFixed(2)

export default function Sweep() {
  const [list, setList] = useState<SweepBrief[]>([])
  const [picked, setPicked] = useState('')
  const [detail, setDetail] = useState<SweepDetail | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  useEffect(() => {
    runApi.sweeps().then(
      (d) => {
        setList(d.sweeps)
        const first = d.sweeps.find((s) => s.analyzable)
        if (first) setPicked(first.id)
      },
      (e) => setErr(String(e?.message ?? e)),
    )
  }, [])

  useEffect(() => {
    if (!picked) return
    setBusy(true)
    setErr('')
    runApi.sweep(picked).then(
      (d) => { setDetail(d); setBusy(false) },
      (e) => { setErr(String(e?.message ?? e)); setBusy(false); setDetail(null) },
    )
  }, [picked])

  return (
    <>
      <h2>海选</h2>
      <div className="sub">
        跑由 <code>cmd/sweep</code> 负责，这里只读结果 ——
        一次海选要吃满 CPU，塞进 HTTP 请求一刷新就能把服务端顶住。
      </div>

      <div className="panel">
        <div className="filters">
          <div className="field">
            <label>已跑过的海选</label>
            <select value={picked} onChange={(e) => setPicked(e.target.value)}
              style={{ minWidth: 420 }}>
              {list.map((s) => (
                <option key={s.id} value={s.id} disabled={!s.analyzable}>
                  {s.name}（{s.params} 组 · {s.windows} 窗）
                  {s.analyzable ? '' : ' —— 无清单，分析不了'}
                </option>
              ))}
            </select>
          </div>
        </div>
        {list.length === 0 && !err && (
          <p className="note">
            还没有跑过海选。先跑一次：
            <code>go run ./cmd/sweep -config ../configs/sweep/xxx.json -workers 8</code>
          </p>
        )}
      </div>

      {err && <ErrBox msg={err} />}
      {busy && <Loading what="海选结果" />}
      {detail && !busy && <Detail d={detail} />}
    </>
  )
}

function Detail({ d }: { d: SweepDetail }) {
  const a = d.analysis
  return (
    <>
      <Verdict a={a} />
      <GateHealth a={a} gate={d.manifest.gate} />
      <Attribution a={a} />
      <Margins a={a} />
      <Plateaus a={a} />
    </>
  )
}

// ---- 1. 这次海选有没有意义 ----

function Verdict({ a }: { a: SweepAnalysis }) {
  const v = a.verdict
  const ok = v.meaningful
  return (
    <div className="panel" style={{ marginTop: 14 }}>
      <h3>① 这次海选有没有意义</h3>
      <div className="cards">
        <div className="card">
          <div className="k">噪声基线（极差）</div>
          <div className="v">{ppAbs(a.noise.range)}%</div>
          <div className="n">
            什么都没改时结果本身的抖动 · {a.noise.samples} 组样本 ×{a.noise.repeats} 次
          </div>
        </div>
        <div className="card">
          <div className="k">全网格散布</div>
          <div className="v">{ppAbs(v.spread)}%</div>
          <div className="n">逐参数{a.metricLabel}中位数的标准差 · {v.params} 组参与</div>
        </div>
        <div className="card">
          <div className="k">比值</div>
          <div className={`v ${ok ? 'up' : 'down'}`}>{v.ratio.toFixed(2)}×</div>
          <div className="n">判定阈值 1.5</div>
        </div>
      </div>
      {ok ? (
        <p className="note">
          ✅ <strong>参数确有影响，可以往下看。</strong>
          散布明显大于噪声，说明参数之间的差异不是抖出来的。
        </p>
      ) : (
        <div className="error" style={{ marginTop: 10 }}>
          <strong>⛔ 这些参数在此区间内没有可辨别的影响。</strong>
          <div style={{ marginTop: 6 }}>
            整张网格的散布与噪声同量级 —— 任何排名都是随机的，所以下面不出排名。
            这不是失败：便宜地知道「这条路走不通」，比拿着一个不可信的第一名去实盘便宜得多。
          </div>
          <div style={{ marginTop: 6 }}>
            可以试的方向：换取值范围、换标的池、换策略族，或先把噪声压下去 ——
            实测 A 股 slots 10→100，极差从 8.99 降到 0.55 个百分点。
          </div>
        </div>
      )}
    </div>
  )
}

// ---- 2. 门槛校准 ----
//
// 这是实测里**唯一一个能悄悄产出错误结论**的地方，所以它单独一屏。

function GateHealth({
  a, gate,
}: {
  a: SweepAnalysis
  gate: { min_round_trips: number } & Record<string, unknown>
}) {
  const pct = a.params > 0 ? (a.thinParams / a.params) * 100 : 0
  return (
    <div className="panel" style={{ marginTop: 14 }}>
      <h3>② 门槛校准得对不对</h3>
      <p className="note">
        硬门槛「每个{a.repeatLabel}的完整轮次 ≥ <strong>{gate.min_round_trips}</strong>」。
        定得比典型轮次还高的话，绝大多数参数组会因覆盖不足退出排名，
        而下面每个数字都只算在活下来的那个子集上。
      </p>
      <div className="cards">
        <div className="card">
          <div className="k">门槛拦下</div>
          <div className="v">{a.gated}</div>
          <div className="n">
            {Object.entries(a.gateBy ?? {}).map(([k, n]) => `${k} ${n}`).join(' · ') || '—'}
          </div>
        </div>
        <div className="card">
          <div className="k">退出排名的参数组</div>
          <div className={`v ${a.thinWarn ? 'down' : ''}`}>
            {a.thinParams} / {a.params}
          </div>
          <div className="n">{pct.toFixed(0)}% · {a.repeatLabel}覆盖不足 60%</div>
        </div>
        <div className="card">
          <div className="k">{a.repeatLabel}数</div>
          <div className="v">{a.windows}</div>
          <div className="n">少于 6 个时中位数与四分位距没有意义</div>
        </div>
      </div>
      {a.thinWarn && (
        <div className="error" style={{ marginTop: 10 }}>
          <strong>⚠ 这个比例偏高，门槛多半相对单次回测长度定得太严。</strong>
          <div style={{ marginTop: 6 }}>
            下面的排名与逐维边际都只算在活下来的那些组上，
            <strong>是一个偏窄且未必有代表性的子集</strong>。
            实测踩过一次：门槛设成每年 30 轮而配置全样本每年只有 24.8 轮，
            92% 的参数组退出排名，逐维边际算出来的结论与正确结论
            <strong>完全相反</strong> —— 而没有任何地方报错。
          </div>
          <div style={{ marginTop: 6 }}>
            先跑一次全样本回测看「完整轮次 ÷ 年数」，再把门槛定在那个数以下。
          </div>
        </div>
      )}
    </div>
  )
}

// ---- 3. 结果从哪来 ----

function Attribution({ a }: { a: SweepAnalysis }) {
  const at = a.attribution
  const alarm = at.liquidations > 0 || at.haltExits > 0
  return (
    <div className="panel" style={{ marginTop: 14 }}>
      <h3>③ 结果从哪来</h3>
      <p className="note">
        光看收益分不出四种情况：策略有边际、强平替你止损、熔断替你择时、
        以及压根就是低摩擦低换手显得稳。
      </p>
      <div className="cards">
        <div className="card">
          <div className="k">强平</div>
          <div className={`v ${at.liquidations > 0 ? 'down' : ''}`}>{at.liquidations}</div>
          <div className="n">轮 · 高杠杆下它相当于一道很紧的止损</div>
        </div>
        <div className="card">
          <div className="k">熔断清仓</div>
          <div className={`v ${at.haltExits > 0 ? 'down' : ''}`}>{at.haltExits}</div>
          <div className="n">轮 · 那部分收益来自风控不是信号</div>
        </div>
        <div className="card">
          <div className="k">止损</div>
          <div className="v">{at.stopExits}</div>
          <div className="n">轮</div>
        </div>
        <div className="card">
          <div className="k">平均摩擦</div>
          <div className={`v ${at.avgFrictionRatio > 0.2 ? 'down' : ''}`}>
            {ppAbs(at.avgFrictionRatio)}%
          </div>
          <div className="n">占初始资金 · 高的话比的是费率不是策略</div>
        </div>
        <div className="card">
          <div className="k">未平仓开仓金额</div>
          <div className="v">{ppAbs(at.avgOpenCostRatio)}%</div>
          <div className="n">占初始资金 · 高的话收益挂在没结算的浮盈上</div>
        </div>
        {at.hasVirtual && (
          <div className="card">
            <div className="k">有效性树边际</div>
            <div className={`v ${at.avgVirtualEdge >= 0 ? 'up' : 'down'}`}>
              {pp(at.avgVirtualEdge)}%
            </div>
            <div className="n">
              过滤掉 {at.virtualTrips} 轮 · 实仓逐轮 − 虚拟逐轮，为正才该留
            </div>
          </div>
        )}
      </div>
      {alarm && (
        <p className="note">
          ⚠ 有强平或熔断清仓 —— 这部分收益不是信号挣的。
          实测同一份配置杠杆从 1 调到 20，收益从 +115% 涨到 +202%，
          而强平从 0 次涨到 139 次：<strong>那是强平在替你砍亏损腿</strong>，
          换个行情就反过来。
        </p>
      )}
      {at.hasVirtual && at.avgVirtualEdge < 0 && (
        <p className="note">
          ⚠ 有效性树的边际为负 —— 它挡掉的比放行的更好，在帮倒忙。
        </p>
      )}
    </div>
  )
}

// ---- 4. 逐维边际 ----

function Margins({ a }: { a: SweepAnalysis }) {
  if (a.margins.length === 0) return null
  return (
    <div className="panel" style={{ marginTop: 14 }}>
      <h3>④ 哪个轴有用</h3>
      <p className="note">
        把参数按某一维的取值分组，各看各的{a.metricLabel}中位数。它答不了「哪片区域稳健」，
        但答得了「这个轴有没有用、往哪边用」——
        <strong>子树开关这类非数值轴只有这一个视图</strong>，高原分析对它们无能为力。
        跨度小于噪声基线（{ppAbs(a.noise.range)}%）的标成惰性。
      </p>
      {a.margins.map((m) => <MarginRow key={m.axis} m={m} metricLabel={a.metricLabel} />)}
    </div>
  )
}

function MarginRow({ m, metricLabel }: { m: SweepMargin; metricLabel: string }) {
  const meds = m.values.map((v) => v.median)
  const lo = Math.min(...meds)
  const hi = Math.max(...meds)
  const span = hi - lo || 1
  return (
    <div style={{ marginTop: 12 }}>
      <div className="k">
        <span className="mono">{m.axis}</span>
        <span className="muted"> —— {metricLabel}中位数，跨度 {ppAbs(m.spread)}%</span>
        {m.inert && <span className="tag" style={{ marginLeft: 6 }}>惰性</span>}
      </div>
      <table style={{ width: '100%', marginTop: 4 }}>
        <tbody>
          {m.values.map((v) => (
            <tr key={v.label}>
              <td style={{ width: 180 }}>{v.label}</td>
              <td className="num mono" style={{ width: 90 }}>{pp(v.median)}%</td>
              <td className="muted" style={{ width: 70 }}>{v.count} 组</td>
              <td>
                {/* 条形只表达相对位置，不表达绝对值 —— 中位数多为负，
                    画成「离最差有多远」比画成长度更好读 */}
                <div style={{
                  height: 8, borderRadius: 4, background: 'var(--panel-2)',
                  overflow: 'hidden',
                }}>
                  <div style={{
                    width: `${((v.median - lo) / span) * 100}%`,
                    height: '100%',
                    background: v.median === hi ? 'var(--up)' : 'var(--border)',
                  }} />
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// ---- 5. 稳健区域 ----

function Plateaus({ a }: { a: SweepAnalysis }) {
  return (
    <div className="panel" style={{ marginTop: 14 }}>
      <h3>⑤ 有没有稳健区域</h3>
      {a.plateaus.length > 0 ? (
        <>
          <p className="note">
            <strong>这里给的是区域不是排名。</strong>每片区域的中心参数只是坐标，
            真正的结论是「这一带整体表现如何」—— 单点估计在噪声下没有意义。
            <code>IQR ÷ 噪声</code> ≈ 1 是好事：邻居之间的差异不超过噪声，这片是平的。
          </p>
          {a.plateaus.slice(0, 8).map((p, i) => (
            <div key={p.centerId} style={{
              borderTop: '1px solid var(--border)', paddingTop: 8, marginTop: 8,
            }}>
              <div className="k">
                #{i + 1} 邻居 {p.neighbors} 个 · 样本 {p.samples}
                <span className={p.median >= 0 ? 'up' : 'down'} style={{ marginLeft: 10 }}>
                  {a.metricLabel}中位数 {pp(p.median)}%
                </span>
                <span className="muted" style={{ marginLeft: 10 }}>
                  四分位 [{pp(p.q1)}, {pp(p.q3)}]% · 正的比例 {(p.posRatio * 100).toFixed(0)}%
                  · IQR {p.flatVsNoise.toFixed(2)}× 噪声
                </span>
              </div>
              <div className="mono" style={{ fontSize: 12, marginTop: 4 }}>{p.params}</div>
            </div>
          ))}
        </>
      ) : (
        <>
          <p className="note">
            没有区域同时满足{a.plateauCriteria ?? '全部判据'}。
            <strong>这是一个结论，不是一次失败</strong> ——
            说明网格里没有稳健的参数区域，排名第一那组多半是尖峰。
          </p>
          {/* plateauNote 只在它说的是**别的原因**时才印 ——
              「没有区域满足判据」上面那段已经讲过了，重复一遍是噪音。
              几何反推失败（非数值轴）才是需要单独说明的那种 */}
          {a.plateauNote && !a.plateauNote.startsWith('没有区域') && (
            <p className="note">⚠ {a.plateauNote}</p>
          )}
          <TopTable rows={a.top} a={a} />
        </>
      )}
    </div>
  )
}

function TopTable({ rows, a }: { rows: SweepTop[]; a: SweepAnalysis }) {
  if (rows.length === 0) return null
  return (
    <>
      <div className="k" style={{ marginTop: 12 }}>
        按分数排前 {Math.min(rows.length, 10)} ——
        <span className="muted"> 仅供参考，上面已说明单点排名不可依赖</span>
      </div>
      <table style={{ width: '100%', marginTop: 6 }}>
        <thead>
          <tr>
            <th style={{ width: 90 }}>{a.metricLabel}中位数</th>
            <th style={{ width: 120 }}>四分位</th>
            <th style={{ width: 70 }}>正的比例</th>
            <th style={{ width: 60 }}>{a.repeatLabel}</th>
            <th style={{ width: 130 }}>逐{a.repeatLabel}</th>
            <th>参数</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((t) => (
            <tr key={t.paramId}>
              <td className={`num mono ${t.median >= 0 ? 'up' : 'down'}`}>{pp(t.median)}%</td>
              <td className="num mono muted">[{pp(t.q1)}, {pp(t.q3)}]</td>
              <td className="num">{(t.posRatio * 100).toFixed(0)}%</td>
              <td className="num">{t.windows}</td>
              <td><Spark oos={t.oos} /></td>
              <td className="mono" style={{ fontSize: 11 }}>{t.params}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}

// Spark 把各窗口的样本外收益画成一排小格：正绿负红。
//
// **不画折线**：窗口之间没有连续性（每个窗口是独立的一段样本外），
// 连起来会让人读出一条并不存在的趋势。
function Spark({ oos }: { oos: number[] }) {
  if (!oos.length) return <span className="muted">—</span>
  const max = Math.max(...oos.map(Math.abs)) || 1
  return (
    <div style={{ display: 'flex', gap: 2, alignItems: 'center', height: 18 }}>
      {oos.map((v, i) => (
        <div key={i} title={`${pp(v)}%`} style={{
          width: 6, height: `${Math.max(3, (Math.abs(v) / max) * 16)}px`,
          background: v >= 0 ? 'var(--up)' : 'var(--down)', borderRadius: 1,
        }} />
      ))}
    </div>
  )
}
