import { useEffect, useState } from 'react'
import { runApi } from '../api'
import RuleTreeEditor, { blankRuleTree } from './RuleTreeEditor'
import UniversePicker from './UniversePicker'
import type {
  FeeFile, FeeRule, Meta, ModuleCatalog, ModuleSpec, ModuleValue, ParamSpec, UniverseSpec,
} from '../types'

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

  // 计价货币随市场变。**这不只是个标签**：加密的初始资金默认 1,000 USDT、
  // A 股默认 20,000 元，把 20000 当成 USDT 填进去，回测会以 20 倍的
  // 本金起跑，而报告上每个数字都看着正常
  const isCrypto = (cfg.data?.market ?? 'ashare') === 'crypto'
  const cur = isCrypto ? 'USDT' : '元'
  const defCash = isCrypto ? 100_000 : 2_000_000

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

      <Section title={`离场规则 · ${(cfg.exit ?? []).length} 条 · 止损 / 止盈`}>
        <RuleChain
          list={cat.exit}
          value={(cfg.exit ?? []) as ModuleValue[]}
          onChange={(v) => set('exit', v)}
          note={cat.notes.exit}
          empty="没有离场规则 —— 只有策略自己发的卖出信号能平仓。"
          addLabel="+ 加一条离场规则"
        />
      </Section>

      <Section title={`风控链 · ${(cfg.risk ?? []).length} 条 · 开仓限制`}>
        <RuleChain
          list={cat.risk}
          value={(cfg.risk ?? []) as ModuleValue[]}
          onChange={(v) => set('risk', v)}
          note={cat.notes.risk}
          empty="没有风控规则 —— 订单不受拦截。"
          addLabel="+ 加一条风控规则"
        />
        <p className="note">
          注意 <code>drawdown_halt</code> 是<strong>账户级</strong>的：
          从峰值权益回撤到阈值后<strong>停止开新仓</strong>，
          它<strong>不会平掉已有持仓</strong>。逐仓的止损止盈在上面那一段。
        </p>
      </Section>

      <Section title="撮合与成本">
        <div style={{ display: 'grid', gap: 10 }}>
          <ModuleSlot label="市场规则" list={cat.market}
            value={(cfg.market ?? { impl: 'ashare' }) as ModuleValue}
            onChange={(v) => set('market', v)} />
          <FeeSlot cat={cat}
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
          <Field label={`初始资金（${cur}）`}>
            <input
              value={(cfg.portfolio?.initial_cash_cents ?? defCash) / 100}
              onChange={(e) => set('portfolio.initial_cash_cents',
                Math.round(Number(e.target.value || 0) * 100))}
              style={{ width: 130 }} />
          </Field>
          {!isCrypto && (
            <Field label="红利税（ppm）">
              <input value={cfg.portfolio?.dividend_tax_ppm ?? 0}
                onChange={(e) => set('portfolio.dividend_tax_ppm', num(e.target.value))}
                style={{ width: 100 }} />
            </Field>
          )}
          {isCrypto && (
            <>
              <Field label="杠杆（倍）">
                <input value={cfg.portfolio?.leverage ?? 1}
                  onChange={(e) => set('portfolio.leverage', num(e.target.value))}
                  style={{ width: 80 }} />
              </Field>
              <Field label="维持保证金率（ppm）">
                <input value={cfg.portfolio?.maint_margin_ppm ?? 5000}
                  onChange={(e) => set('portfolio.maint_margin_ppm', num(e.target.value))}
                  style={{ width: 100 }} />
              </Field>
            </>
          )}
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
        {isCrypto && (
          <p className="note">
            加密永续固定<strong>逐仓 + 双向持仓</strong>：每个仓位有自己的一份保证金，
            爆仓只吃掉那一份、不牵连余额与其他仓位；同一标的可同时持多与持空，
            各自独立记账、独立爆仓。杠杆越高保证金越少，
            也就越容易在一次回撤里被强平 —— 强平会出现在结果页的账本告警里。
            红利税不适用（永续没有分红送配）。
          </p>
        )}
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
  const isTree = value.impl === 'rule_tree'
  const sources = value.sources ?? []
  return (
    <>
      <div className="filters">
        <Field label="实现">
          <select value={value.impl}
            onChange={(e) => onChange(switchImpl(cat.strategy, value, e.target.value))}>
            <ImplOptions list={cat.strategy} />
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

      {isTree && (
        <div style={{ marginTop: 8 }}>
          <RuleTreeEditor
            cfg={asTree(value.params)}
            onChange={(c) => onChange({ ...value, params: c as any })}
            cat={cat}
          />
        </div>
      )}

      {!isComposite && !isTree && (
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
                  <ImplOptions list={cat.strategy} />
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
          <ImplOptions list={list} />
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

// ---- 费率 ----
//
// 费率不能只给一个填路径的文本框。它是 A 股策略的主要亏损来源之一 ——
// 实测 macd_cross 的摩擦占初始资金 **20.45%**（费用 11.56% + 滑点 8.89%），
// 而它此前藏在一个用户从没打开过的 JSON 文件里。
//
// 这里做三件事：选文件、**看见实际生效的规则**、改佣金。
// 只开放佣金是因为它是券商定的（各家万 0.85 到万 3）；
// 印花税与过户费是监管费率且按生效日期分段，
// 做成可随手填的数字等于邀请用户拿 2023 年的税率去算 2007 年的回测。

function FeeSlot({
  cat, value, onChange,
}: {
  cat: ModuleCatalog
  value: ModuleValue
  onChange: (v: ModuleValue) => void
}) {
  const [files, setFiles] = useState<FeeFile[]>([])
  const [open, setOpen] = useState(false)
  useEffect(() => { runApi.fees().then((d) => setFiles(d.files), () => {}) }, [])

  const params = (value.params ?? {}) as Record<string, any>
  const cur = files.find((f) => f.path === params.path)
  const isConfig = value.impl === 'config'

  const setP = (k: string, v: unknown) =>
    onChange({ ...value, params: { ...params, [k]: v } })

  return (
    <div>
      <div className="filters">
        <span className="k" style={{ minWidth: 68 }}>费率</span>
        <select value={value.impl}
          onChange={(e) => onChange(switchImpl(cat.fee, value, e.target.value))}>
          <ImplOptions list={cat.fee} />
        </select>
        {isConfig && (
          <select value={String(params.path ?? '')}
            onChange={(e) => setP('path', e.target.value)} style={{ minWidth: 220 }}>
            {!files.some((f) => f.path === params.path) && (
              <option value={String(params.path ?? '')}>
                {String(params.path ?? '（未选）')}
              </option>
            )}
            {files.map((f) => (
              <option key={f.path} value={f.path}>
                {f.name || f.path}{f.err ? '（读不了）' : ''}
              </option>
            ))}
          </select>
        )}
        {isConfig && cur && (
          <button onClick={() => setOpen((v) => !v)}>
            {open ? '收起费率明细' : `看费率明细（${cur.rules.length} 条）`}
          </button>
        )}
      </div>

      {isConfig && cur?.err && <div className="error">{cur.err}</div>}
      {isConfig && cur?.description && (
        <p className="note" style={{ marginTop: 6 }}>{cur.description}</p>
      )}

      {isConfig && (
        <div className="filters" style={{ marginTop: 6 }}>
          <ParamField spec={specsOf(cat.fee, 'config').find((x) => x.name === 'commission_ppm')!}>
            <input type="number" step={0.5} min={0}
              value={String(params.commission_ppm ?? 0)}
              onChange={(e) => setP('commission_ppm', Number(e.target.value || 0))}
              style={{ width: 110 }} />
          </ParamField>
          <ParamField
            spec={specsOf(cat.fee, 'config').find((x) => x.name === 'commission_min_yuan')!}>
            <input type="number" step={1} min={-1}
              value={String(params.commission_min_yuan ?? -1)}
              onChange={(e) => setP('commission_min_yuan', Number(e.target.value))}
              style={{ width: 110 }} />
          </ParamField>
          <div className="field" style={{ maxWidth: 300 }}>
            <label>&nbsp;</label>
            <div className="muted" style={{ fontSize: 11, lineHeight: 1.5 }}>
              只开放<strong>佣金</strong>：它是券商定的，各家万 0.85 到万 3 都有。
              印花税与过户费是监管费率、按生效日期分段（2005/2007/2008/2023
              各调过一次，2008-09-19 还从双边改成单边），要改请直接改文件。
            </div>
          </div>
        </div>
      )}

      {isConfig && open && cur && <FeeRules rules={cur.rules} params={params} />}
      {!isConfig && (
        <ParamForm specs={specsOf(cat.fee, value.impl)} value={params}
          onChange={(p) => onChange({ ...value, params: p })} />
      )}
    </div>
  )
}

function FeeRules({
  rules, params,
}: {
  rules: FeeRule[]
  params: Record<string, any>
}) {
  const ovPPM = Number(params.commission_ppm ?? 0)
  const ovMin = Number(params.commission_min_yuan ?? -1)
  return (
    <div className="tablewrap" style={{ marginTop: 8 }}>
      <table>
        <thead>
          <tr>
            <th>费用</th><th>适用</th><th>方向</th><th>生效区间</th>
            <th className="num">费率</th><th className="num">每笔最低</th><th>说明</th>
          </tr>
        </thead>
        <tbody>
          {rules.map((r, i) => {
            const isComm = r.kind === 'commission'
            const ppm = isComm && ovPPM > 0 ? ovPPM : (r.ratePpm ?? 0)
            const min = isComm && ovMin >= 0 ? ovMin * 100 : (r.minCents ?? 0)
            const overridden = isComm && (ovPPM > 0 || ovMin >= 0)
            return (
              <tr key={i}>
                <td>
                  {kindLabel(r.kind)}
                  {overridden && <span className="tag warn" style={{ marginLeft: 4 }}>已覆盖</span>}
                </td>
                <td>{(r.instrumentTypes ?? []).join('/') || '全部'}</td>
                <td>{sideLabel(r.side)}</td>
                <td className="mono">{rangeLabel(r.from, r.to)}</td>
                <td className="num mono">{ppm ? `${(ppm / 10000).toFixed(3)}%` : '—'}</td>
                <td className="num mono">{min ? `${(min / 100).toFixed(2)} 元` : '无'}</td>
                <td className="muted" style={{ fontSize: 11, maxWidth: 380 }}>{r.note}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

const KINDS: Record<string, string> = {
  commission: '佣金', stamp_duty: '印花税', transfer_fee: '过户费',
  trading_fee: '交易费', funding: '资金费率',
}
function kindLabel(k: string): string { return KINDS[k] ? `${KINDS[k]}（${k}）` : k }
function sideLabel(s: string): string {
  return s === 'buy' ? '买入' : s === 'sell' ? '卖出' : '双边'
}
function rangeLabel(from?: number, to?: number): string {
  if (!from && !to) return '始终'
  const f = from ? String(from) : '—'
  const t = to ? String(to) : '至今'
  return `${f} ~ ${t}`
}

// ---- 规则链（风控与离场共用）----

function RuleChain({
  list, value, onChange, note, empty, addLabel,
}: {
  list: ModuleSpec[]
  value: ModuleValue[]
  onChange: (v: ModuleValue[]) => void
  note?: string
  empty: string
  addLabel: string
}) {
  return (
    <>
      {note && <p className="note">{note}</p>}
      {value.length === 0 && <p className="note">{empty}</p>}
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
              <ImplOptions list={list} />
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
      }}>{addLabel}</button>
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
        <ParamField key={s.name} spec={s}>
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
        </ParamField>
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

/**
 * ParamField 参数的标签用**中文说明**做主标题，英文名做副标。
 *
 * 此前 desc 只塞进了 title 属性（要悬停才看得到），标签是
 * `slots` / `pct` / `bps` 这样的英文标识符 —— 没读过源码的人无从判断
 * 该填什么。desc 在引擎里早就写好了，只是没显示出来。
 */
function ParamField({ spec: sp, children }: { spec: ParamSpec; children: React.ReactNode }) {
  const range = rangeHint(sp)
  return (
    <div className="field" style={{ maxWidth: 260 }}>
      <label style={{ lineHeight: 1.5 }}>
        <strong style={{ color: 'var(--text)' }}>{shortDesc(sp)}</strong>
        <br />
        <code className="muted">{sp.name}</code>
        {range && <span className="muted"> {range}</span>}
      </label>
      {children}
      {longDesc(sp) && (
        <div className="muted" style={{ fontSize: 11, lineHeight: 1.45 }}>
          {longDesc(sp)}
        </div>
      )}
    </div>
  )
}

// 说明的第一句做标题，其余做补充。引擎里的 desc 有长有短 ——
// 「快线周期」四个字就够当标题，而「把资金等分成多少份，同时也是
// 最多持有的标的数」显然该拆开。用第一个逗号/句号切
function splitDesc(d: string): [string, string] {
  // **括号内不断句**：「佣金费率覆盖（百万分之一；万 2.5 填 250）」
  // 在分号处切开会留下一个不闭合的括号
  let depth = 0
  for (let i = 0; i < d.length; i++) {
    const c = d[i]
    if (c === '（' || c === '(') depth++
    else if (c === '）' || c === ')') depth--
    else if (depth === 0 && '，。：；'.includes(c)) {
      return i > 16 ? [d, ''] : [d.slice(0, i), d.slice(i + 1)]
    }
  }
  return [d, '']
}
function shortDesc(s: ParamSpec): string {
  return s.desc ? splitDesc(s.desc)[0] : s.name
}
function longDesc(s: ParamSpec): string {
  return s.desc ? splitDesc(s.desc)[1] : ''
}

// ---- 纯函数 ----

function specsOf(list: ModuleSpec[], impl: string): ParamSpec[] {
  return list.find((m) => m.name === impl)?.specs ?? []
}

/**
 * ImplOptions 把下拉项渲染成「英文名（中文说明）」。
 *
 * 英文名要留着 —— 它是配置 JSON 里真正写的东西，也是文档与报错里
 * 出现的名字。只显示中文会让用户在 JSON 里对不上号。
 */
function ImplOptions({ list }: { list: ModuleSpec[] }) {
  return (
    <>
      {list.map((m) => (
        <option key={m.name} value={m.name}>
          {m.name}{m.desc ? `（${m.desc}）` : ''}
        </option>
      ))}
    </>
  )
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
  if (impl === 'rule_tree') {
    // 规则树的配置是结构不是标量，给一份能直接跑的默认（MACD 金叉买死叉卖）
    next.params = blankRuleTree() as any
    return next
  }
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

/** asTree 把 params 当规则树读；形状不对时给一份默认，避免编辑器崩在半路。 */
function asTree(p: unknown): import('../types').RuleTreeCfg {
  const t = p as import('../types').RuleTreeCfg | undefined
  if (!t || !Array.isArray(t.indicators) || !t.buy) return blankRuleTree()
  return t
}
