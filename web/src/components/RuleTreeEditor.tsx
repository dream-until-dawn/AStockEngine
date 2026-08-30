import type {
  IndInstance, ModuleCatalog, Operand, ParamSpec, RuleTreeCfg, TreeNode,
} from '../types'

// 规则树编辑器：指标表 + 三棵决策树。
//
// # 三棵树各管什么
//
//   是否买进（buy）   什么时候算一次机会
//   是否有效（valid） 这次机会要不要真的下注
//   是否卖出（sell）  什么时候离场
//
// buy + valid 为真 → 真实开仓。
// buy 为真但 valid 为假 → **虚拟持仓**：不动资金，但记成持有，
// 于是在它的卖出信号出现之前不会再次触发买入。
//
// 这解决的是一个真实的麻烦：「MACD 在零轴上方」这类条件会连续多根 bar
// 为真，只用 buy 树的话每根 bar 都会尝试开仓。有了虚拟持仓，
// 「这次被过滤掉了」与「还没轮到」就区分开了。
//
// # 可选列由启用了哪些指标决定
//
// 条件左右侧的下拉里，除了行情列（close/high/...），
// 只会出现**上面指标表里声明过的**指标字段 ——
// 加了 KDJ 才有 kdj.K / kdj.D / kdj.J。字段名来自 /api/modules 的
// 指标目录，前端不维护。

type Tree = 'buy' | 'valid' | 'sell'

const TREES: { key: Tree; title: string; hint: string }[] = [
  { key: 'buy', title: '是否买进', hint: '为真时算作一次机会' },
  {
    key: 'valid', title: '是否有效',
    hint: '与买进同时为真才真的下单；为假则记虚拟持仓（不动资金，但在卖出前不再买入）',
  },
  { key: 'sell', title: '是否卖出', hint: '为真时平仓；虚拟持仓也在这里清掉' },
]

export default function RuleTreeEditor({
  cfg, onChange, cat,
}: {
  cfg: RuleTreeCfg
  onChange: (c: RuleTreeCfg) => void
  cat: ModuleCatalog
}) {
  const inds = cfg.indicators ?? []
  const setInds = (v: IndInstance[]) => onChange({ ...cfg, indicators: v })

  return (
    <div style={{ display: 'grid', gap: 12 }}>
      <div>
        <div className="k" style={{ marginBottom: 6 }}>
          指标 · {inds.length} 个
          <span className="muted">
            {' '}—— 条件里能引用的列由它决定，加了 KDJ 才有 kdj.K / kdj.D / kdj.J
          </span>
        </div>
        {inds.map((ind, i) => (
          <IndRow
            key={i} ind={ind} cat={cat}
            onChange={(v) => setInds(inds.map((x, j) => (j === i ? v : x)))}
            onRemove={() => setInds(inds.filter((_, j) => j !== i))}
            dup={inds.filter((x) => x.name === ind.name).length > 1}
          />
        ))}
        <button style={{ marginTop: 6 }} onClick={() => {
          const k = cat.indicator[0]
          setInds([...inds, {
            name: uniqueName(inds, k.name), kind: k.name, params: defaults(k.specs),
          }])
        }}>+ 加一个指标</button>
      </div>

      {TREES.map(({ key, title, hint }) => (
        <div key={key} style={{
          borderTop: '1px solid var(--border)', paddingTop: 10,
        }}>
          <div className="k" style={{ marginBottom: 4 }}>
            {title}
            <span className="muted"> —— {hint}</span>
          </div>
          <NodeEditor
            node={cfg[key] ?? null}
            onChange={(n) => onChange({ ...cfg, [key]: n })}
            cat={cat} inds={inds} depth={0}
            rootOf={key}
          />
        </div>
      ))}

      <p className="note">
        价格列一律是<strong>后复权价</strong>，与指标同基准。
        拿原始价去和均线比，除权日会凭空产生一次穿越 —— 那不是行情，是分红。
      </p>
    </div>
  )
}

// ---- 指标行 ----

function IndRow({
  ind, cat, onChange, onRemove, dup,
}: {
  ind: IndInstance
  cat: ModuleCatalog
  onChange: (v: IndInstance) => void
  onRemove: () => void
  dup: boolean
}) {
  const kind = cat.indicator.find((k) => k.name === ind.kind)
  return (
    <div style={{
      border: '1px solid var(--border)', borderRadius: 4, padding: 8, marginTop: 6,
    }}>
      <div className="filters">
        <div className="field">
          <label>名字 <span className="muted">条件里按它引用</span></label>
          <input
            value={ind.name}
            onChange={(e) => onChange({ ...ind, name: e.target.value })}
            style={{ width: 120 }}
          />
        </div>
        <div className="field">
          <label>种类</label>
          <select value={ind.kind} onChange={(e) => {
            const k = cat.indicator.find((x) => x.name === e.target.value)!
            // 换种类就换默认参数 —— 留着旧参数会让引擎的未知字段校验失败
            onChange({ ...ind, kind: k.name, params: defaults(k.specs) })
          }}>
            {cat.indicator.map((k) => (
              <option key={k.name} value={k.name}>{k.name}（{k.desc}）</option>
            ))}
          </select>
        </div>
        {(kind?.specs ?? []).map((sp) => (
          <div className="field" key={sp.name}>
            <label>
              {sp.desc || sp.name} <code className="muted">{sp.name}</code>
            </label>
            <input
              type="number" step={sp.step || 1}
              min={sp.unbounded ? undefined : sp.min}
              max={sp.unbounded ? undefined : sp.max}
              value={String(ind.params?.[sp.name] ?? sp.default)}
              onChange={(e) => onChange({
                ...ind,
                params: { ...(ind.params ?? {}), [sp.name]: Number(e.target.value || 0) },
              })}
              style={{ width: 90 }}
            />
          </div>
        ))}
        <button onClick={onRemove}>移除</button>
      </div>
      <div className="muted" style={{ fontSize: 11, marginTop: 4 }}>
        可用列：{(kind?.fields ?? []).map((f) => `${ind.name}.${f}`).join('  ')}
        {dup && <span className="warn"> ⚠ 名字重复，条件里的引用会有歧义</span>}
      </div>
    </div>
  )
}

// ---- 树节点 ----

function NodeEditor({
  node, onChange, cat, inds, depth, rootOf,
}: {
  node: TreeNode | null
  onChange: (n: TreeNode | null) => void
  cat: ModuleCatalog
  inds: IndInstance[]
  depth: number
  rootOf?: Tree
}) {
  if (!node) {
    return (
      <div className="filters">
        <span className="muted">
          {rootOf === 'valid' ? '空 —— 视为恒真（买进即下单）' : '空'}
        </span>
        <button onClick={() => onChange(blankCond(cat, inds))}>+ 条件</button>
        <button onClick={() => onChange({ op: 'and', children: [blankCond(cat, inds)] })}>
          + 条件组
        </button>
      </div>
    )
  }

  const isGroup = !!node.op
  const pad = { marginLeft: depth > 0 ? 14 : 0 }

  if (!isGroup) {
    return (
      <div style={pad}>
        <CondEditor
          node={node} onChange={onChange} cat={cat} inds={inds}
          onWrap={() => onChange({ op: 'and', children: [node, blankCond(cat, inds)] })}
        />
      </div>
    )
  }

  const kids = node.children ?? []
  return (
    <div style={{
      ...pad, borderLeft: '2px solid var(--accent-soft)', paddingLeft: 10, marginTop: 4,
    }}>
      <div className="filters">
        <select value={node.op} onChange={(e) => {
          const op = e.target.value as 'and' | 'or' | 'not'
          // not 只能有一个子节点 —— 多的截掉，免得配置直接不合法
          onChange({ ...node, op, children: op === 'not' ? kids.slice(0, 1) : kids })
        }}>
          <option value="and">全部满足（AND）</option>
          <option value="or">任一满足（OR）</option>
          <option value="not">取反（NOT）</option>
        </select>
        {node.op !== 'not' && (
          <>
            <button onClick={() => onChange({ ...node, children: [...kids, blankCond(cat, inds)] })}>
              + 条件
            </button>
            <button onClick={() => onChange({
              ...node, children: [...kids, { op: 'and', children: [blankCond(cat, inds)] }],
            })}>+ 子组</button>
          </>
        )}
        <button onClick={() => onChange(null)}>删除整组</button>
      </div>
      {kids.map((c, i) => (
        <div key={i} style={{ display: 'flex', alignItems: 'flex-start', gap: 6 }}>
          <div style={{ flex: 1, minWidth: 0 }}>
            <NodeEditor
              node={c} cat={cat} inds={inds} depth={depth + 1}
              onChange={(n) => {
                const next = n === null
                  ? kids.filter((_, j) => j !== i)
                  : kids.map((x, j) => (j === i ? n : x))
                // 组空了就把组本身删掉，别留一个恒真的空 AND
                onChange(next.length === 0 ? null : { ...node, children: next })
              }}
            />
          </div>
        </div>
      ))}
    </div>
  )
}

// ---- 条件 ----

function CondEditor({
  node, onChange, cat, inds, onWrap,
}: {
  node: TreeNode
  onChange: (n: TreeNode | null) => void
  cat: ModuleCatalog
  inds: IndInstance[]
  onWrap: () => void
}) {
  const cols = columns(cat, inds)
  return (
    <div className="filters" style={{ marginTop: 4, alignItems: 'center' }}>
      <OperandPick
        value={node.left!} onChange={(o) => onChange({ ...node, left: o })}
        cols={cols} allowValue={false}
      />
      <select value={node.cmp} onChange={(e) => onChange({ ...node, cmp: e.target.value })}>
        {cat.cmps.map((c) => <option key={c.code} value={c.code}>{c.label}</option>)}
      </select>
      <OperandPick
        value={node.right!} onChange={(o) => onChange({ ...node, right: o })}
        cols={cols} allowValue
      />
      <button onClick={onWrap} title="把这个条件包进一个条件组">分组</button>
      <button onClick={() => onChange(null)}>删除</button>
    </div>
  )
}

/** column 是一个可选列：行情列或某个指标的某个字段。 */
type column = { key: string; label: string; operand: Operand }

function columns(cat: ModuleCatalog, inds: IndInstance[]): column[] {
  const out: column[] = cat.barFields.map((f) => ({
    key: `bar.${f}`, label: barLabel(f), operand: { kind: 'bar', field: f },
  }))
  for (const ind of inds) {
    const k = cat.indicator.find((x) => x.name === ind.kind)
    for (const f of k?.fields ?? []) {
      out.push({
        key: `ind.${ind.name}.${f}`, label: `${ind.name}.${f}`,
        operand: { kind: 'ind', ind: ind.name, field: f },
      })
    }
  }
  return out
}

const BAR_LABELS: Record<string, string> = {
  close: '收盘价', open: '开盘价', high: '最高价', low: '最低价',
  preclose: '前收盘', volume: '成交量', amount: '成交额(元)',
}
function barLabel(f: string): string { return `${BAR_LABELS[f] ?? f}（${f}）` }

function OperandPick({
  value, onChange, cols, allowValue,
}: {
  value: Operand
  onChange: (o: Operand) => void
  cols: column[]
  allowValue: boolean
}) {
  const isValue = value.kind === 'value'
  const cur = isValue ? '__value__'
    : value.kind === 'bar' ? `bar.${value.field}` : `ind.${value.ind}.${value.field}`
  const missing = !isValue && !cols.some((c) => c.key === cur)
  return (
    <>
      <select
        value={missing ? '__missing__' : cur}
        onChange={(e) => {
          if (e.target.value === '__value__') {
            onChange({ kind: 'value', value: 0 })
            return
          }
          const c = cols.find((x) => x.key === e.target.value)
          if (c) onChange(c.operand)
        }}
        style={{ minWidth: 140 }}
      >
        {/* 引用了已删除的指标时保留一个占位项并标出来 ——
            静默换成别的列会让用户以为自己没改过 */}
        {missing && <option value="__missing__">⚠ {cur}（指标已删除）</option>}
        {cols.map((c) => <option key={c.key} value={c.key}>{c.label}</option>)}
        {allowValue && <option value="__value__">— 填一个值 —</option>}
      </select>
      {isValue && (
        <input
          type="number" step="any"
          value={String(value.value ?? 0)}
          onChange={(e) => onChange({ kind: 'value', value: Number(e.target.value || 0) })}
          style={{ width: 100 }}
        />
      )}
    </>
  )
}

// ---- 纯函数 ----

function blankCond(cat: ModuleCatalog, inds: IndInstance[]): TreeNode {
  const cols = columns(cat, inds)
  return {
    left: cols[0].operand,
    cmp: 'gt',
    right: { kind: 'value', value: 0 },
  }
}

function defaults(specs: ParamSpec[]): Record<string, number> {
  const out: Record<string, number> = {}
  for (const s of specs) out[s.name] = s.default
  return out
}

function uniqueName(inds: IndInstance[], base: string): string {
  if (!inds.some((i) => i.name === base)) return base
  for (let n = 2; ; n++) {
    const c = `${base}${n}`
    if (!inds.some((i) => i.name === c)) return c
  }
}

/** blankRuleTree 给一份能直接跑的默认配置：MACD 金叉买、死叉卖。 */
export function blankRuleTree(): RuleTreeCfg {
  const dif: Operand = { kind: 'ind', ind: 'macd', field: 'DIF' }
  const dea: Operand = { kind: 'ind', ind: 'macd', field: 'DEA' }
  return {
    indicators: [{ name: 'macd', kind: 'macd', params: { short: 12, long: 26, signal: 9 } }],
    buy: { left: dif, cmp: 'cross_above', right: dea },
    valid: null,
    sell: { left: dif, cmp: 'cross_below', right: dea },
  }
}
