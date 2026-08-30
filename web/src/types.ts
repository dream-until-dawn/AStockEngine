// 与 engine/cmd/server 的 JSON 一一对应。
//
// 价格、金额、因子一律是**定点整数** —— 服务端不传浮点，
// 因为「数据准不准」正是这个工具要回答的问题，不能在传输层就先丢精度。
// 除以 meta.scales 里对应的 scale 才是人读的数值。

export type EnumItem = { code: number; label: string }

export type Meta = {
  enums: {
    market: EnumItem[]
    exchange: EnumItem[]
    type: EnumItem[]
    board: EnumItem[]
    status: EnumItem[]
    currency: EnumItem[]
    adj: { code: string; label: string }[]
  }
  scales: { price: number; amount: number; ratio: number; factor: number }
  stats: {
    instruments: number
    barRows: number
    barInstruments: number
    steps: number
    calendarRows: number
    tradingDays: number
    factorEvents: number
    factorInsts: number
    corpRows: number
    corpEffective: number
    firstDay: number
    lastDay: number
    memoryMB: number
    diskMB: number
    loadMS: number
    loadedAt: string
    feeName: string
    dataRoot: string
  }
  scope: { market: string; freq: string; types: string; factors: string }
}

export type Instrument = {
  id: number
  symbol: string
  name: string
  market: number
  exchange: number
  type: number
  board: number
  trackedBoard: number
  priceScale: number
  qtyScale: number
  quoteCcy: number
  minOrderQty: number
  qtyStep: number
  listDate: number
  delistDate: number
  status: number
  bars: number
  firstDay: number
  lastDay: number
  factorEvents: number
  corpActions: number
}

export type Paged<T> = { total: number; page: number; pageSize: number; rows: T[] }

export type FactorRow = {
  exDate: number
  factor: number
  ratio?: number
}

export type CorpRow = {
  exDate: number
  cashBeforeTax: number
  stockDividend: number
  stockTransfer: number
  rightsRatio: number
  rightsPrice: number
  hasEffect: boolean
}

export type InstrumentDetail = {
  instrument: Instrument
  factors: FactorRow[]
  corpActions: CorpRow[]
  reconcile: { factorOnly: number[] | null; corpOnly: number[] | null }
}

export type IndicatorSpec = {
  key: string
  label: string
  pane: 'main' | 'macd' | 'kdj'
  names: string[]
}

export type KBar = {
  d: number
  o: number
  h: number
  l: number
  c: number
  rawC: number
  v: number
  amt: number
  pre: number
  susp: boolean
  st: boolean
  limitUp: number
  limitDn: number
  factor: number
  ex: boolean
  /** 所选复权基准下的指标值 */
  ind: Record<string, number[]>
  ready: Record<string, boolean>
  /** 回测基准（后复权）下的同一批指标。所选基准就是后复权时不下发。 */
  indBt?: Record<string, number[]>
  readyBt?: Record<string, boolean>
}

export type Kline = {
  instrument: Instrument
  adj: string
  btAdj: string
  sameAsBT: boolean
  indicators: IndicatorSpec[]
  bars: KBar[]
  engine: {
    steps: number
    runs: number
    warmupBars: number
    returned: number
    priceScale: number
  }
}

// ---- 回测结果（v0.3.1）----
//
// 服务端跑完一次回测返回的全部东西。金额一律定点整数（分），
// 与行情价格一样，除以 scale 才是元。

export type ConfigItem = {
  name: string
  title: string
  config: unknown
  error?: string
}

export type Drawdown = {
  peak_day: number
  trough_day: number
  recovery_day: number
  ratio: number
  peak_cents: number
  trough_cents: number
  trough_steps: number
  recovery_steps: number
}

export type TradeStats = {
  round_trips: number
  wins: number
  losses: number
  flat: number
  win_rate: number
  profit_factor: number
  avg_win_cents: number
  avg_loss_cents: number
  avg_hold_days: number
  bonus_trips: number
  open_positions: number
  open_qty: number
}

export type BenchmarkStats = {
  name: string
  covered: number
  total: number
  return: number
  excess: number
  beta: number
  alpha: number
  information_ratio: number
  strategy_return: number
}

export type Metrics = {
  steps: number
  from_day: number
  to_day: number
  years: number
  initial_cents: number
  final_cents: number
  total_return: number
  annual_return: number
  annual_reliable: boolean
  annual_vol: number
  downside_vol: number
  sharpe: number
  sortino: number
  calmar: number
  max_drawdown: Drawdown
  turnover_cents: number
  turnover: number
  fee_cents: number
  slippage_cents: number
  trades: TradeStats
  benchmark?: BenchmarkStats
  trading_days_per_year: number
  risk_free_ppm: number
}

export type CurvePoint = {
  d: number
  equity: number
  cash: number
  positions: number
  signals: number
  fills: number
  rejects: number
  /** 基准净值，已归一化到初始资金；0 表示该日基准无数据（不要连线） */
  bench?: number
}

export type RunFill = {
  d: number; id: number; symbol: string; name: string
  side: 'buy' | 'sell'
  price: number; qty: number; amount: number
  fee: number; slippage: number; tag: string
}

export type RunReject = {
  d: number; id: number; symbol: string; name: string
  side: 'buy' | 'sell'
  qty: number; reason: string; rule?: string; detail: string
}

export type RoundTrip = {
  id: number; symbol: string; name: string
  openDay: number; closeDay: number
  qty: number; cost: number; proceed: number; pnl: number
  holdDays: number; fromBonus: boolean
}

export type RunResult = {
  name: string
  config: unknown
  stats: {
    steps: number; durationMs: number; instruments: number
    signals: number; fills: number; rejects: number
  }
  fingerprint: {
    input: string; output: string; data: string
    engine: string; reproducible: boolean
  }
  metrics: Metrics
  curve: CurvePoint[]
  fills: RunFill[]
  rejections: RunReject[]
  rejectTotal: number
  rejectBy: Record<string, number>
  roundTrips: RoundTrip[]
  warnings?: string[]
  // disclosures 本次回测**已知未计入**的成本与机制。
  // 必须显示 —— 漏算的成本不报错，只让结果一致地偏乐观
  disclosures?: string[]
}

// ---- 标的池 ----

export type UniverseSpec = {
  symbols?: string[]
  market?: string[]
  type?: string
  board?: string[]
  exchange?: string[]
  status?: string
  require_factor?: boolean
  limit?: number
}

export type UniversePreview = {
  count: number
  /** 其中在数据里真的有行情的只数 —— 命中数不等于能跑的数 */
  withBars: number
  limit: number
  overLimit: boolean
  truncated: number
  sample: {
    id: number; symbol: string; name: string
    type: number; board: number
    bars: number; firstDay: number; lastDay: number
  }[]
}

// ---- 单步调试会话（v0.4）----

export interface SessionBrief {
  id: string; name: string; step: number; totalSteps: number; created: string
}

export interface DbgSignal {
  id: number; symbol: string; name: string
  kind: string; side: string; strength: number; tag?: string
}

export interface DbgOrder {
  id: number; symbol: string; name: string; side: string; qty: number; tag?: string
}

export interface DbgPending {
  id: number; symbol: string; name: string; side: string; qty: number
  signalDay: number; priceRef: string; tried: number; maxSteps: number; tag?: string
}

export interface DbgPosition {
  id: number; symbol: string; name: string
  qty: number; available: number; cost: number
  last: number; value: number; pnl: number; suspended: boolean
}

/** 一步的全部动静。无事发生的步不会出现在列表里 —— 只计入 quiet 计数。 */
export interface StepEvent {
  step: number; day: number
  equity: number; cash: number; positions: number
  signals?: DbgSignal[]
  orders?: DbgOrder[]
  fills?: RunFill[]
  rejects?: RunReject[]
}

export interface SessionState {
  id: string; name: string
  step: number; totalSteps: number
  day: number; firstDay: number; lastDay: number
  done: boolean; started: boolean; universe: number
  equity: number; cash: number; initial: number; peak: number
  realized: number; fee: number; slippage: number
  holdings: DbgPosition[]
  pending: DbgPending[]
  indicators: string[]
  warnings?: string[]
  disclosures?: string[]
}

/** 三种停法可叠加，谁先到停在谁。until 是最有用的那个。 */
export interface StepReq {
  n?: number
  day?: number
  until?: '' | 'signal' | 'fill' | 'reject' | 'end'
}

export interface StepResult {
  state: SessionState
  steps: StepEvent[]
  advanced: number
  quiet: number
  stoppedBy: string
  truncated: boolean
  elapsedMs: number
}

export interface DbgIndicator {
  key: string
  names?: string[]
  values?: number[]
  /** false 时 values 是垃圾（预热未完成）——界面必须把这件事说出来 */
  ready: boolean
}

export interface Inspect {
  id: number; symbol: string; name: string; day: number
  hasBar: boolean
  open: number; high: number; low: number; close: number; preclose: number
  volume: number; amount: number
  suspended: boolean; isST: boolean
  limitUp: number; limitDn: number; hasLimit: boolean
  adjClose: number; priceScale: number
  held: number; available: number; cost: number
  indicators: DbgIndicator[]
  pending?: DbgPending[]
}

// ---- 模块目录（v1.0 可视化装配）----
//
// **前端不维护任何一份模块清单或参数默认值。**
// 全部来自 /api/modules，也就是引擎 registry 里的同一份 ParamSpec。
// 抄一份到前端会立刻分叉：引擎加了风控规则前端不知道，
// 引擎把上限从 100 改成 500 前端还在按 100 拦 —— 表单填得出、引擎不认。

export interface ParamSpec {
  name: string
  kind: 'int' | 'float' | 'bool' | 'string'
  desc: string
  default: number
  min: number
  max: number
  step: number
  defaultStr?: string
  options?: string[]
  /** true 表示没有上下限（引擎里 min==max 就是「不限」的写法） */
  unbounded: boolean
}

export interface ModuleSpec {
  name: string
  /** 一句话中文说明，来自引擎的注册表 */
  desc: string
  specs: ParamSpec[]
}

export interface ModuleCatalog {
  strategy: ModuleSpec[]
  sizer: ModuleSpec[]
  risk: ModuleSpec[]
  /** 离场规则：止损 / 止盈 / 移动止损。与 risk 是两类东西 */
  exit: ModuleSpec[]
  slippage: ModuleSpec[]
  fee: ModuleSpec[]
  market: ModuleSpec[]
  enums: Record<string, { code: string; label: string }[]>
  notes: Record<string, string>
  /** 指标目录。规则树的条件从这里取可选列 */
  indicator: IndicatorKind[]
  /** 可用作条件左右侧的行情列 */
  barFields: string[]
  /** 比较符及其中文名 */
  cmps: { code: string; label: string }[]
}

/** 配置里的一个模块槽位：选一个实现 + 给它一段参数。 */
export interface ModuleValue {
  impl: string
  params?: Record<string, unknown>
  /** 仅 strategy.impl === 'composite' 时有意义 */
  sources?: ModuleValue[]
  mode?: string
}

// ---- 费率 ----

export interface FeeRule {
  kind: string
  instrumentTypes?: string[]
  boards?: string[]
  side: string
  from?: number
  to?: number
  ratePpm?: number
  perShareCents?: number
  flatCents?: number
  minCents?: number
  note?: string
}

export interface FeeFile {
  path: string
  name: string
  description?: string
  rules: FeeRule[]
  err?: string
}

// ---- 规则树（决策树策略）----
//
// 三棵树：是否买进 / 是否有效 / 是否卖出。
// 买入 + 有效 → 真实开仓；买入 + 无效 → **虚拟持仓**（不动资金，
// 但在卖出信号出现前不再触发买入）。

/** IndicatorKind 指标目录里的一种指标。与 K 线视图的 IndicatorSpec 无关 */
export interface IndicatorKind {
  name: string
  desc: string
  /** 输出字段，如 KDJ 的 ["K","D","J"]。**条件的可选列由它决定** */
  fields: string[]
  specs: ParamSpec[]
}

/** 配置里声明的一个指标实例。name 是用户起的名字，条件按它引用 */
export interface IndInstance {
  name: string
  kind: string
  params?: Record<string, number>
}

export interface Operand {
  kind: 'bar' | 'ind' | 'value'
  field?: string
  ind?: string
  value?: number
}

/** 树节点：有 op 就是逻辑组，否则是条件 */
export interface TreeNode {
  op?: 'and' | 'or' | 'not'
  children?: TreeNode[]
  left?: Operand
  cmp?: string
  right?: Operand
}

export interface RuleTreeCfg {
  indicators: IndInstance[]
  buy: TreeNode | null
  valid?: TreeNode | null
  sell: TreeNode | null
}
