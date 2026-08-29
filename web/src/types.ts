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
