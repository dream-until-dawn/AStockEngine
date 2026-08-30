import { useEffect, useState } from 'react'
import { runApi } from '../api'
import UniversePicker from './UniversePicker'
import type { Meta, ModuleCatalog, ModuleSpec, ModuleValue, ParamSpec, UniverseSpec } from '../types'

// 可视化装配配置。
//
// # 一条不能破的规矩：JSON 是唯一真相
//
// 这个组件**不持有任何自己的配置状态** —— 它读一份 config 对象，
// 改完把新的整份交回去。JSON 编辑器与它是同一份数据的两个视图。
// 两边各存一份状态必然会不同步，然后「我明明改了却没生效」。
// 标的池选择器（UniversePicker）从 v0.3.1 起就是这个形态，这里照搬。
//
// # 模块清单与参数默认值全部来自 /api/modules
//
// **前端不抄一份。** 引擎 registry 里的 ParamSpec 从设计之初就写着
// 「同时喂三处：Web 自动生成表单、海选展开参数网格、配置校验」。
// 抄一份到前端会立刻分叉：引擎加了风控规则前端不知道；
// 引擎把某参数上限从 100 改到 500，前端还在按 100 拦 ——
// 表单填得出、引擎不认，是重复定义最典型的症状。

type Cfg = Record<string, any>

export default function ConfigBuilder({
  cfg, onChange, meta,
}: {
  cfg: Cfg
  onChange: (next: Cfg) => void
  meta: Meta
}) {
  const [cat, setCat] = useState<ModuleCatalog | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    runApi.modules().then(setCat, (e) => setErr(String(e?.message ?? e)))
  }, [])

  if (err) return <div className="error">模块目录加载失败：{err}</div>
  if (!cat) return <p className="note">正在读模块目录…</p>

  // set 走点号路径，与海选配置的 grid 用同一套寻址方式 ——
  // 两处都在「往一份 JSON 里定点写值」，没有理由是两套写法
  const set = (path: string, v: unknown) => onChange(setPath(cfg, path, v))

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <Section title="数据与标的池">
        <div className="filters">
          <Field label="起始交易日">
            <input
              value={cfg.data?.from || ''}
              onChange={(e) => set('data.from', num(e.target.value))}
              placeholder="20200101" style={{ width: 120 }}
            />
          </Field>
          <Field label="结束交易日（0=不限）">
            <input
              value={cfg.data?.to ?? 0}
              onChange={(e) => set('data.to', num(e.target.value))}
              placeholder="0" style={{ width: 120 }}
            />
          </Field>
          <Field label="行情分区">
            <select value={cfg.data?.market ?? 'ashare'}
              onChange={(e) => set('data.market', e.target.value)}>
              <option value="ashare">ashare</option>
              <option value="crypto">crypto</option>
            </select>
          </Field>
        </div>
        <p className="note">
          区间是<strong>可配的</strong>：全量数据 2005-01-04 起共 5,260 个交易日。
          <code>data.market</code> 选的是行情分区路径，与标的池里的「市场」
          不是一回事 —— 后者筛的是标的属性。
        </p>
        <div style={{ marginTop: 8 }}>
          <UniversePicker
            value={(cfg.data?.universe ?? {}) as UniverseSpec}
            onChange={(u) => set('data.universe', u)}
            meta={meta}
          />
        </div>
      </Section>

      <Section title="策略">
        <StrategySlot
          cat={cat}
          value={(cfg.strategy ?? { impl: '' }) as ModuleValue}
          onChange={(v) => set('strategy', v)}
        />
      </Section>

      <Section title="仓位（Sizer）· 信号 → 数量">
        <ModuleSlot
          list={cat.sizer}
          value={(cfg.sizer ?? { impl: '' }) as ModuleValue}
          onChange={(v) => set('sizer', v)}
        />
        <p className="note">
          实测 <code>slots</code> 对结果的影响比多数策略参数都大：
          同一策略在 <code>slots=10</code> 时的<strong>噪声基线是 18.95 个百分点</strong>，
          <code>slots=100</code> 时只有 4.48 —— 太集中的组合测的是随机数不是策略。
        </p>
      </Section>

      <Section title={`风控链 · ${(cfg.risk ?? []).length} 条`}>
        <RiskChain
          list={cat.risk}
          value={(cfg.risk ?? []) as ModuleValue[]}
          onChange={(v) => set('risk', v)}
          note={cat.notes.risk}
        />
      </Section>

      <Section title="撮合与成本">
        <div style={{ display: 'grid', gap: 10 }}>
          <ModuleSlot label="市场规则" list={cat.market}
            value={(cfg.market ?? { impl: 'ashare' }) as ModuleValue}
            onChange={(v) => set('market', v)} />
          <ModuleSlot label="费率" list={cat.fee}
            value={(cfg.fee ?? { impl: 'zero' }) as ModuleValue}
            onChange={(v) => set('fee', v)} />
          <ModuleSlot label="滑点" list={cat.slippage}
            value={(cfg.slippage ?? { impl: 'none' }) as ModuleValue}
            onChange={(v) => set('slippage', v)} />
        </div>
        <div className="filters" style={{ marginTop: 10 }}>
          <Field label="单笔占当日成交量上限（ppm）">
            <input value={cfg.broker?.volume_cap_ppm ?? 100000}
              onChange={(e) => set('broker.volume_cap_ppm', num(e.target.value))}
              style={{ width: 120 }} />
          </Field>
          <Field label="允许部分成交">
            <input type="checkbox"
              checked={cfg.broker?.allow_partial_fill !== false}
              onChange={(e) => set('broker.allow_partial_fill', e.target.checked)} />
          </Field>
        </div>
      </Section>

      <Section title="账户与引擎">
        <div className="filters">
          <Field label="初始资金（元）">
            <input
              value={(cfg.portfolio?.initial_cash_cents ?? 0) / 100}
              onChange={(e) => set('portfolio.initial_cash_cents',
                Math.round(Number(e.target.value || 0) * 100))}
              style={{ width: 130 }} />
          </Field>
          <Field label="红利税（ppm）">
            <input value={cfg.portfolio?.dividend_tax_ppm ?? 0}
              onChange={(e) => set('portfolio.dividend_tax_ppm', num(e.target.value))}
              style={{ width: 100 }} />
          </Field>
          <Field label="指标复权基准">
            <select value={cfg.engine?.indicator_adj ?? 'hfq'}
              onChange={(e) => set('engine.indicator_adj', e.target.value)}>
              {cat.enums.indicator_adj.map((o) =>
                <option key={o.code} value={o.code}>{o.label}</option>)}
            </select>
          </Field>
          <Field label="由因子推算送转">
            <input type="checkbox"
              checked={cfg.engine?.imply_split_from_factor !== false}
              onChange={(e) => set('engine.imply_split_from_factor', e.target.checked)} />
          </Field>
        </div>
        <p className="note">{cat.notes.indicator_adj}</p>
      </Section>

      <Section title="绩效与记录">
        <div className="filters">
          <Field label="基准标的">
            <input value={cfg.metrics?.benchmark ?? ''}
              onChange={(e) => set('metrics.benchmark', e.target.value)}
              placeholder="510300，留空则不算超额" style={{ width: 180 }} />
          </Field>
          <Field label="无风险利率（ppm/年）">
            <input value={cfg.metrics?.risk_free_ppm ?? 0}
              onChange={(e) => set('metrics.risk_free_ppm', num(e.target.value))}
              style={{ width: 120 }} />
          </Field>
          <Field label="记录级别">
            <select value={cfg.recorder?.level ?? 'summary'}
              onChange={(e) => set('recorder.level', e.target.value)}>
              {cat.enums.recorder_level.map((o) =>
                <option key={o.code} value={o.code}>{o.label}</option>)}
            </select>
          </Field>
        </div>
        <p className="note">
          基准数据里<strong>没有指数</strong>（C10 纯技术面，ETL 没拉指数行情），
          只能用宽基 ETF 代理，而 510300 最早到 2012-05-28 ——
          再早的区间报告会显式标注覆盖比例，不会把「没有基准」变成「超额为零」。
        </p>
      </Section>
    </div>
  )
}

// ---- 策略槽位（含组合）----

function StrategySlot({
  cat, value, onChange,
}: {
  cat: ModuleCatalog
  value: ModuleValue
  onChange: (v: ModuleValue) => void
}) {
  const isComposite = value.impl === 'composite'
  const sources = value.sources ?? []
  return (
    <>
      <div className="filters">
        <Field label="实现">
          <select value={value.impl}
            onChange={(e) => onChange(switchImpl(cat.strategy, value, e.target.value))}>
            {cat.strategy.map((m) => <option key={m.name} value={m.name}>{m.name}</option>)}
            {/* composite 不在 registry 里 —— 它是装配层的东西，
                由 sources 与 mode 描述，没有自己的 ParamSpec */}
            <option value="composite">composite（多决策源组合）</option>
          </select>
        </Field>
        {isComposite && (
          <Field label="合并方式">
            <select value={value.mode ?? 'union'}
              onChange={(e) => onChange({ ...value, mode: e.target.value })}>
              <option value="union">union（任一发信号即采纳）</option>
              <option value="confirm">confirm（全部同意才采纳）</option>
              <option value="veto">veto（后续源发反向信号即否决）</option>
            </select>
          </Field>
        )}
      </div>

      {!isComposite && (
        <ParamForm
          specs={specsOf(cat.strategy, value.impl)}
          value={value.params ?? {}}
          onChange={(p) => onChange({ ...value, params: p })}
        />
      )}

      {isComposite && (
        <div style={{ marginTop: 8 }}>
          <p className="note">
            <strong>顺序有意义</strong>：union 同标的同方向保留靠前的源；
            veto 只有<strong>第一个源</strong>产生订单，其余源只做否决。
          </p>
          {sources.map((s, i) => (
            <div key={i} style={{
              border: '1px solid var(--border)', borderRadius: 4,
              padding: 8, marginTop: 6,
            }}>
              <div className="filters">
                <span className="k">源 {i}{i === 0 ? '（主）' : ''}</span>
                <select value={s.impl}
                  onChange={(e) => onChange({
                    ...value,
                    sources: sources.map((x, j) =>
                      j === i ? switchImpl(cat.strategy, x, e.target.value) : x),
                  })}>
                  {cat.strategy.map((m) =>
                    <option key={m.name} value={m.name}>{m.name}</option>)}
                </select>
                <button disabled={i === 0} onClick={() => onChange({
                  ...value, sources: swap(sources, i, i - 1),
                })}>↑</button>
                <button disabled={i === sources.length - 1} onClick={() => onChange({
                  ...value, sources: swap(sources, i, i + 1),
                })}>↓</button>
                <button onClick={() => onChange({
                  ...value, sources: sources.filter((_, j) => j !== i),
                })}>移除</button>
              </div>
              <ParamForm
                specs={specsOf(cat.strategy, s.impl)}
                value={s.params ?? {}}
                onChange={(p) => onChange({
                  ...value,
                  sources: sources.map((x, j) => (j === i ? { ...x, params: p } : x)),
                })}
              />
            </div>
          ))}
          <button style={{ marginTop: 8 }} onClick={() => {
            const first = cat.strategy[0]
            onChange({
              ...value,
              sources: [...sources, { impl: first.name, params: defaults(first.specs) }],
            })
          }}>+ 加一个决策源</button>
        </div>
      )}
    </>
  )
}

// ---- 通用模块槽位 ----

function ModuleSlot({
  label, list, value, onChange,
}: {
  label?: string
  list: ModuleSpec[]
  value: ModuleValue
  onChange: (v: ModuleValue) => void
}) {
  return (
    <div>
      <div className="filters">
        {label && <span className="k" style={{ minWidth: 68 }}>{label}</span>}
        <select value={value.impl}
          onChange={(e) => onChange(switchImpl(list, value, e.target.value))}>
          {list.map((m) => <option key={m.name} value={m.name}>{m.name}</option>)}
        </select>
      </div>
      <ParamForm
        specs={specsOf(list, value.impl)}
        value={value.params ?? {}}
        onChange={(p) => onChange({ ...value, params: p })}
      />
    </div>
  )
}

// ---- 风控链 ----

function RiskChain({
  list, value, onChange, note,
}: {
  list: ModuleSpec[]
  value: ModuleValue[]
  onChange: (v: ModuleValue[]) => void
  note?: string
}) {
  return (
    <>
      {note && <p className="note">{note}</p>}
      {value.length === 0 && <p className="note">没有风控规则 —— 订单不受拦截。</p>}
      {value.map((r, i) => (
        <div key={i} style={{
          border: '1px solid var(--border)', borderRadius: 4,
          padding: 8, marginTop: 6,
        }}>
          <div className="filters">
            <span className="k" style={{ minWidth: 34 }}>#{i + 1}</span>
            <select value={r.impl}
              onChange={(e) => onChange(value.map((x, j) =>
                j === i ? switchImpl(list, x, e.target.value) : x))}>
              {list.map((m) => <option key={m.name} value={m.name}>{m.name}</option>)}
            </select>
            <button disabled={i === 0}
              onClick={() => onChange(swap(value, i, i - 1))}>↑</button>
            <button disabled={i === value.length - 1}
              onClick={() => onChange(swap(value, i, i + 1))}>↓</button>
            <button onClick={() => onChange(value.filter((_, j) => j !== i))}>移除</button>
          </div>
          <ParamForm
            specs={specsOf(list, r.impl)}
            value={r.params ?? {}}
            onChange={(p) => onChange(value.map((x, j) =>
              (j === i ? { ...x, params: p } : x)))}
          />
        </div>
      ))}
      <button style={{ marginTop: 8 }} onClick={() => {
        const m = list[0]
        onChange([...value, { impl: m.name, params: defaults(m.specs) }])
      }}>+ 加一条规则</button>
    </>
  )
}

// ---- 参数表单 ----
//
// 完全由 ParamSpec 生成。加一个模块、改一个参数范围，前端不用动。

function ParamForm({
  specs, value, onChange,
}: {
  specs: ParamSpec[]
  value: Record<string, unknown>
  onChange: (v: Record<string, unknown>) => void
}) {
  if (specs.length === 0) {
    return <p className="note" style={{ marginTop: 4 }}>该实现没有参数。</p>
  }
  const set = (k: string, v: unknown) => onChange({ ...value, [k]: v })
  return (
    <div className="filters" style={{ marginTop: 6 }}>
      {specs.map((s) => (
        <Field key={s.name} label={s.name} hint={rangeHint(s)} desc={s.desc}>
          {s.kind === 'bool' ? (
            <input type="checkbox"
              checked={boolOf(value[s.name], s.default !== 0)}
              onChange={(e) => set(s.name, e.target.checked)} />
          ) : s.kind === 'string' && (s.options?.length ?? 0) > 0 ? (
            <select value={String(value[s.name] ?? s.defaultStr ?? '')}
              onChange={(e) => set(s.name, e.target.value)}>
              {s.options!.map((o) => <option key={o} value={o}>{o}</option>)}
            </select>
          ) : s.kind === 'string' ? (
            // 没有 options 的字符串参数是自由文本（如 fee.config 的 path）。
            // spec.go 说这类不该出现在 ParamSpec 里，但 fee.config 确实有一个 ——
            // 渲染成空下拉框会让它**完全没法填**，给文本框
            <input
              value={String(value[s.name] ?? s.defaultStr ?? '')}
              onChange={(e) => set(s.name, e.target.value)}
              style={{ width: 240 }}
            />
          ) : (
            <input
              type="number"
              value={String(value[s.name] ?? s.default)}
              step={s.step || (s.kind === 'int' ? 1 : 'any')}
              min={s.unbounded ? undefined : s.min}
              max={s.unbounded ? undefined : s.max}
              onChange={(e) => set(s.name,
                s.kind === 'int' ? Math.round(Number(e.target.value || 0))
                  : Number(e.target.value || 0))}
              style={{ width: 110 }}
            />
          )}
        </Field>
      ))}
    </div>
  )
}

function rangeHint(s: ParamSpec): string {
  if (s.kind === 'bool') return ''
  if (s.kind === 'string') return (s.options ?? []).join(' / ')
  if (s.unbounded) return '不限'
  return `[${fmtG(s.min)}, ${fmtG(s.max)}]`
}

function fmtG(v: number): string {
  if (Math.abs(v) >= 1e6) return v.toExponential(0)
  return String(v)
}

// ---- 小组件 ----

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="panel" style={{ margin: 0 }}>
      <h3>{title}</h3>
      {children}
    </div>
  )
}

function Field({
  label, hint, desc, children,
}: {
  label: string
  hint?: string
  desc?: string
  children: React.ReactNode
}) {
  return (
    <div className="field" title={desc}>
      <label>
        {label}
        {hint && <span className="muted"> {hint}</span>}
      </label>
      {children}
    </div>
  )
}

// ---- 纯函数 ----

function specsOf(list: ModuleSpec[], impl: string): ParamSpec[] {
  return list.find((m) => m.name === impl)?.specs ?? []
}

/** defaults 由 ParamSpec 生成一份默认参数。 */
function defaults(specs: ParamSpec[]): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const s of specs) {
    out[s.name] = s.kind === 'string' ? (s.defaultStr ?? '')
      : s.kind === 'bool' ? s.default !== 0
        : s.default
  }
  return out
}

/**
 * switchImpl 换实现时**把参数换成新实现的默认值**，而不是留着旧的。
 *
 * 留着旧参数会触发引擎的 DisallowUnknownFields —— 从 `max_positions`
 * 换到 `drawdown_halt`，旧的 `n` 还在，配置直接不合法。
 * 报错总比静默好，但在表单里根本不该产生这种状态。
 */
function switchImpl(list: ModuleSpec[], cur: ModuleValue, impl: string): ModuleValue {
  if (cur.impl === impl) return cur
  const next: ModuleValue = { impl, params: defaults(specsOf(list, impl)) }
  if (impl === 'composite') {
    next.params = undefined
    next.mode = cur.mode ?? 'union'
    next.sources = cur.sources ?? []
  }
  return next
}

function swap<T>(a: T[], i: number, j: number): T[] {
  const out = [...a]
  const t = out[i]
  out[i] = out[j]
  out[j] = t
  return out
}

function num(s: string): number {
  const n = Number(s.replaceAll('-', ''))
  return Number.isFinite(n) ? n : 0
}

function boolOf(v: unknown, dflt: boolean): boolean {
  return typeof v === 'boolean' ? v : dflt
}

/** setPath 按点号路径写一份**新的**配置，不改原对象。 */
function setPath(obj: Cfg, path: string, v: unknown): Cfg {
  const parts = path.split('.')
  const out: Cfg = { ...obj }
  let cur = out
  for (const p of parts.slice(0, -1)) {
    cur[p] = { ...(cur[p] ?? {}) }
    cur = cur[p]
  }
  cur[parts[parts.length - 1]] = v
  return out
}
