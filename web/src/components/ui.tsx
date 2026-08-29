import { useEffect, useState, type ReactNode } from 'react'
import type { EnumItem } from '../types'

// ---- 数据获取 ----

export type Async<T> = { data?: T; err?: string; loading: boolean }

/** 简单的取数 hook。deps 变化就重取，并忽略过期响应。 */
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): Async<T> {
  const [s, setS] = useState<Async<T>>({ loading: true })
  useEffect(() => {
    let alive = true
    setS((p) => ({ ...p, loading: true, err: undefined }))
    fn().then(
      (d) => alive && setS({ data: d, loading: false }),
      (e) => alive && setS({ err: String(e?.message ?? e), loading: false }),
    )
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return s
}

export function Loading({ what = '数据' }: { what?: string }) {
  return <div className="loading">正在载入{what}…</div>
}

export function ErrBox({ msg }: { msg: string }) {
  return <div className="error">{msg}</div>
}

// ---- 表单件 ----

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="field">
      <label>{label}</label>
      {children}
    </div>
  )
}

export function EnumSelect({
  label, items, value, onChange, allLabel = '全部',
}: {
  label: string
  items: EnumItem[] | undefined
  value: string
  onChange: (v: string) => void
  allLabel?: string
}) {
  return (
    <Field label={label}>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{allLabel}</option>
        {items?.map((i) => (
          <option key={i.code} value={i.code}>{i.label}</option>
        ))}
      </select>
    </Field>
  )
}

/** 三态：不过滤 / 有 / 无。核对时「哪些标的没有行情」比「有行情的」更值得看。 */
export function TriSelect({
  label, value, onChange, yes = '有', no = '无',
}: {
  label: string; value: string; onChange: (v: string) => void; yes?: string; no?: string
}) {
  return (
    <Field label={label}>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">不限</option>
        <option value="1">{yes}</option>
        <option value="0">{no}</option>
      </select>
    </Field>
  )
}

export function DayInput({
  label, value, onChange, placeholder = 'YYYYMMDD',
}: {
  label: string; value: string; onChange: (v: string) => void; placeholder?: string
}) {
  return (
    <Field label={label}>
      <input
        value={value}
        placeholder={placeholder}
        style={{ width: 100 }}
        onChange={(e) => onChange(e.target.value.replace(/[^0-9]/g, ''))}
      />
    </Field>
  )
}

// ---- 表格 ----

export type Col<T> = {
  key: string
  title: string
  num?: boolean
  /** 服务端可排序的列才给 sort key */
  sort?: string
  render: (row: T) => ReactNode
}

export function DataTable<T>({
  cols, rows, sort, order, onSort, onRowClick, empty = '没有符合条件的记录',
}: {
  cols: Col<T>[]
  rows: T[]
  sort?: string
  order?: string
  onSort?: (key: string) => void
  onRowClick?: (row: T) => void
  empty?: string
}) {
  if (rows.length === 0) return <div className="empty">{empty}</div>
  return (
    <div className="tablewrap">
      <table>
        <thead>
          <tr>
            {cols.map((c) => (
              <th
                key={c.key}
                className={`${c.num ? 'num' : ''} ${c.sort && onSort ? '' : 'nosort'}`}
                onClick={() => c.sort && onSort && onSort(c.sort)}
              >
                {c.title}
                {c.sort && sort === c.sort && (
                  <span className="arrow">{order === 'desc' ? '↓' : '↑'}</span>
                )}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr
              key={i}
              className={onRowClick ? 'clickable' : ''}
              onClick={() => onRowClick?.(r)}
            >
              {cols.map((c) => (
                <td key={c.key} className={c.num ? 'num' : ''}>{c.render(r)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function Pager({
  total, page, pageSize, onPage, onPageSize,
}: {
  total: number; page: number; pageSize: number
  onPage: (p: number) => void; onPageSize?: (n: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="pager">
      <span className="muted">
        共 {total.toLocaleString('zh-CN')} 条 · 第 {page} / {pages} 页
      </span>
      <span className="grow" />
      {onPageSize && (
        <select value={pageSize} onChange={(e) => onPageSize(Number(e.target.value))}>
          {[20, 50, 100, 200, 500].map((n) => (
            <option key={n} value={n}>{n} / 页</option>
          ))}
        </select>
      )}
      <button disabled={page <= 1} onClick={() => onPage(1)}>«</button>
      <button disabled={page <= 1} onClick={() => onPage(page - 1)}>上一页</button>
      <button disabled={page >= pages} onClick={() => onPage(page + 1)}>下一页</button>
      <button disabled={page >= pages} onClick={() => onPage(pages)}>»</button>
    </div>
  )
}

/** 把当前结果导出成 CSV。核对时经常要贴到别处比对。 */
export function downloadCSV(filename: string, header: string[], rows: (string | number)[][]) {
  const esc = (v: string | number) => {
    const s = String(v)
    return /[",\n]/.test(s) ? `"${s.replaceAll('"', '""')}"` : s
  }
  const body = [header, ...rows].map((r) => r.map(esc).join(',')).join('\n')
  // BOM：Excel 不加它会把 UTF-8 中文认成乱码
  const blob = new Blob(['﻿' + body], { type: 'text/csv;charset=utf-8' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = filename
  a.click()
  setTimeout(() => URL.revokeObjectURL(a.href), 1000)
}
