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
