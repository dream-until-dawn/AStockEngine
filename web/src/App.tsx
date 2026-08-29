import { useEffect, useState } from 'react'
import { api } from './api'
import { ErrBox, Loading, useAsync } from './components/ui'
import Overview from './views/Overview'
import Instruments from './views/Instruments'
import Kline from './views/Kline'
import CalendarView from './views/Calendar'
import Factors from './views/Factors'
import CorpActions from './views/CorpActions'
import type { Meta } from './types'

// 路由用 hash，不引路由库：视图就六个，参数就一个 id。
// 少一个依赖，也让 URL 可以直接手改（核对时经常这么干）。
function useHash(): string {
  const [h, setH] = useState(() => location.hash.slice(1) || '/overview')
  useEffect(() => {
    const on = () => setH(location.hash.slice(1) || '/overview')
    addEventListener('hashchange', on)
    return () => removeEventListener('hashchange', on)
  }, [])
  return h
}

const NAV = [
  { path: '/overview', label: '概览' },
  { path: '/instruments', label: '标的列表' },
  { path: '/calendar', label: '交易日历' },
  { path: '/factors', label: '复权因子' },
  { path: '/corp-actions', label: '分红送配' },
]

export default function App() {
  const hash = useHash()
  const meta = useAsync<Meta>(() => api.meta(), [])

  const nav = NAV.find((n) => hash.startsWith(n.path))
  const klineMatch = /^\/kline\/([^/?]+)/.exec(hash)

  return (
    <>
      <aside className="sidebar">
        <div className="brand">
          <h1>AStockEngine</h1>
          <div className="scope">
            {meta.data ? `${meta.data.scope.market} · ${meta.data.scope.freq}` : '数据核对'}
          </div>
        </div>
        {NAV.map((n) => (
          <a key={n.path} href={`#${n.path}`} className={nav?.path === n.path ? 'on' : ''}>
            {n.label}
          </a>
        ))}
        {klineMatch && (
          <a href={hash} className="on">K 线 · {klineMatch[1]}</a>
        )}
        <div className="foot">
          {meta.data && (
            <>
              {meta.data.stats.instruments.toLocaleString('zh-CN')} 只标的<br />
              {meta.data.stats.firstDay} ~ {meta.data.stats.lastDay}<br />
              <span title={meta.data.stats.dataRoot}>费率 {meta.data.stats.feeName}</span>
            </>
          )}
        </div>
      </aside>

      <main className="main">
        {meta.loading && <Loading what="元数据" />}
        {meta.err && (
          <ErrBox msg={`无法连接后端服务：${meta.err}\n\n请先启动：cd engine && go run ./cmd/server`} />
        )}
        {meta.data && <Route hash={hash} meta={meta.data} />}
      </main>
    </>
  )
}

function Route({ hash, meta }: { hash: string; meta: Meta }) {
  const k = /^\/kline\/([^/?]+)/.exec(hash)
  if (k) return <Kline key={k[1]} id={k[1]} meta={meta} />
  if (hash.startsWith('/instruments')) return <Instruments meta={meta} />
  if (hash.startsWith('/calendar')) return <CalendarView />
  if (hash.startsWith('/factors')) return <Factors meta={meta} />
  if (hash.startsWith('/corp-actions')) return <CorpActions meta={meta} />
  return <Overview meta={meta} />
}
