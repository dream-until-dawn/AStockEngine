import type { RunFill, RunResult } from './types'

// 最近一次回测的结果，放在模块级。
//
// 不用 Context：这是**单页应用里的一份全局数据**，K 线页要读它来叠成交标注，
// 而 K 线页与回测页是两条路由。用 Context 得把 Provider 提到 App 顶层、
// 再让每个消费者订阅，为一份只写一次的数据不值当。
//
// 代价是刷新页面就没了 —— 可以接受：回测本身只要 200 毫秒，重跑即可。

let current: RunResult | null = null
const listeners = new Set<() => void>()

export function setRun(r: RunResult | null) {
  current = r
  listeners.forEach((fn) => fn())
}

export function getRun(): RunResult | null {
  return current
}

export function subscribe(fn: () => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

/** 取某标的在本次回测里的成交，按时间升序。 */
export function fillsFor(instrumentId: number): RunFill[] {
  if (!current) return []
  return current.fills.filter((f) => f.id === instrumentId)
}
