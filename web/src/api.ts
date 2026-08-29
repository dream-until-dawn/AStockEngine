import type { Instrument, InstrumentDetail, Kline, Meta, Paged } from './types'

export class ApiError extends Error {}

async function get<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '' || v === 'all') continue
    u.set(k, String(v))
  }
  const qs = u.toString()
  const res = await fetch(`/api${path}${qs ? '?' + qs : ''}`)
  const text = await res.text()
  if (!res.ok) {
    // 服务端的错误体是 {"error": "..."}，把它原样透出来 ——
    // 那句话往往就是答案（比如「指标不能用前复权喂入」）
    try {
      throw new ApiError(JSON.parse(text).error ?? text)
    } catch (e) {
      if (e instanceof ApiError) throw e
      throw new ApiError(`HTTP ${res.status}: ${text.slice(0, 200)}`)
    }
  }
  return JSON.parse(text) as T
}

export const api = {
  meta: () => get<Meta>('/meta'),
  instruments: (p: Record<string, unknown>) => get<Paged<Instrument>>('/instruments', p),
  instrument: (id: number | string) => get<InstrumentDetail>(`/instruments/${id}`),
  calendar: (p: Record<string, unknown>) =>
    get<Paged<{ market: number; date: number; isTradingDay: boolean }>>('/calendar', p),
  factors: (p: Record<string, unknown>) =>
    get<Paged<{
      id: number; symbol: string; name: string
      exDate: number; factor: number; ratio: number; hasCorp: boolean
    }>>('/factors', p),
  corpActions: (p: Record<string, unknown>) =>
    get<Paged<{
      id: number; symbol: string; name: string; exDate: number
      cashBeforeTax: number; stockDividend: number; stockTransfer: number
      rightsRatio: number; rightsPrice: number; hasEffect: boolean; hasFactor: boolean
    }>>('/corp-actions', p),
  kline: (id: number | string, p: Record<string, unknown>) => get<Kline>(`/kline/${id}`, p),
}

// ---- 格式化 ----

/** 定点整数 → 字符串。不用 toFixed 之外的任何近似，scale 是 10 的幂，除法精确。 */
export function fx(v: number, scale: number, digits?: number): string {
  if (!Number.isFinite(v)) return '—'
  const d = digits ?? Math.round(Math.log10(scale))
  return (v / scale).toFixed(d)
}

/** YYYYMMDD → YYYY-MM-DD */
export function fmtDay(d: number): string {
  if (!d) return '—'
  const s = String(d)
  return `${s.slice(0, 4)}-${s.slice(4, 6)}-${s.slice(6, 8)}`
}

/** YYYY-MM-DD → YYYYMMDD，供输入框回传 */
export function parseDay(s: string): number {
  const n = Number(s.replaceAll('-', ''))
  return Number.isFinite(n) ? n : 0
}

export function fmtNum(v: number, digits = 0): string {
  if (!Number.isFinite(v)) return '—'
  return v.toLocaleString('zh-CN', { minimumFractionDigits: digits, maximumFractionDigits: digits })
}

/** 大数缩写：成交量动辄上亿，完整数字反而看不清量级 */
export function fmtCompact(v: number): string {
  const a = Math.abs(v)
  if (a >= 1e8) return (v / 1e8).toFixed(2) + '亿'
  if (a >= 1e4) return (v / 1e4).toFixed(2) + '万'
  return fmtNum(v)
}

export function labelOf(items: { code: number; label: string }[] | undefined, code: number): string {
  return items?.find((i) => i.code === code)?.label ?? String(code)
}

// ---- 回测 ----

export const runApi = {
  configs: () =>
    get<{ dir: string; configs: import('./types').ConfigItem[] }>('/configs'),
  backtest: async (cfg: unknown): Promise<import('./types').RunResult> => {
    const res = await fetch('/api/backtest', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: typeof cfg === 'string' ? cfg : JSON.stringify(cfg),
    })
    const text = await res.text()
    if (!res.ok) {
      try {
        throw new ApiError(JSON.parse(text).error ?? text)
      } catch (e) {
        if (e instanceof ApiError) throw e
        throw new ApiError(`HTTP ${res.status}: ${text.slice(0, 400)}`)
      }
    }
    return JSON.parse(text)
  },
}
