import { fmtDay, fmtNum } from '../api'
import type { Meta } from '../types'

export default function Overview({ meta }: { meta: Meta }) {
  const s = meta.stats
  const cards: { k: string; v: string; n?: string }[] = [
    { k: '标的', v: fmtNum(s.instruments), n: `其中 ${fmtNum(s.barInstruments)} 只有行情` },
    { k: 'bar 行数', v: fmtNum(s.barRows), n: `${fmtNum(s.steps)} 个事件时点` },
    { k: '数据区间', v: String(s.firstDay).slice(0, 4) + '–' + String(s.lastDay).slice(0, 4),
      n: `${fmtDay(s.firstDay)} ~ ${fmtDay(s.lastDay)}` },
    { k: '交易日历', v: fmtNum(s.tradingDays), n: `共 ${fmtNum(s.calendarRows)} 行（含休市日）` },
    { k: '复权因子事件', v: fmtNum(s.factorEvents), n: `覆盖 ${fmtNum(s.factorInsts)} 只` },
    { k: '分红送配', v: fmtNum(s.corpRows), n: `其中 ${fmtNum(s.corpEffective)} 条有实际影响` },
    { k: '磁盘', v: s.diskMB.toFixed(0) + ' MB', n: 'Parquet + zstd' },
    { k: '内存', v: s.memoryMB.toFixed(0) + ' MB', n: `载入耗时 ${(s.loadMS / 1000).toFixed(1)} 秒` },
  ]

  return (
    <>
      <h2>概览</h2>
      <div className="sub">
        载入于 {s.loadedAt} · 数据目录 <code>{s.dataRoot}</code>
      </div>

      <div className="cards">
        {cards.map((c) => (
          <div className="card" key={c.k}>
            <div className="k">{c.k}</div>
            <div className="v">{c.v}</div>
            {c.n && <div className="n">{c.n}</div>}
          </div>
        ))}
      </div>

      <div className="panel" style={{ marginTop: 14 }}>
        <h3>当前范围</h3>
        <table style={{ width: 'auto' }}>
          <tbody>
            <tr><td className="muted">市场</td><td>{meta.scope.market}</td></tr>
            <tr><td className="muted">频率</td><td>{meta.scope.freq}</td></tr>
            <tr><td className="muted">标的</td><td>{meta.scope.types}</td></tr>
            <tr><td className="muted">因子</td><td>{meta.scope.factors}</td></tr>
          </tbody>
        </table>
        <p className="note">
          范围是刻意收窄的。接口按多市场 / 多频率设计，实现只做 A 股日线 ——
          远期接入美股、加密货币分钟线时不需要重构核心。
        </p>
      </div>

      <div className="panel">
        <h3>数值约定</h3>
        <table style={{ width: 'auto' }}>
          <tbody>
            <tr><td className="muted">价格</td><td className="mono">×{meta.scales.price}</td><td className="muted">厘</td></tr>
            <tr><td className="muted">金额</td><td className="mono">×{meta.scales.amount}</td><td className="muted">分</td></tr>
            <tr><td className="muted">每股分红 / 送转</td><td className="mono">×{fmtNum(meta.scales.ratio)}</td><td className="muted">—</td></tr>
            <tr><td className="muted">复权因子</td><td className="mono">×{meta.scales.factor.toExponential(0)}</td><td className="muted">—</td></tr>
          </tbody>
        </table>
        <p className="note">
          <strong>全链路定点整数</strong>，接口也按整数传。这不是为了省体积，
          而是 C5 可复现性：换一台机器、换一种编译器，结果必须逐位相同 —— 浮点做不到。
          页面上显示的小数是除以上表的 scale 得来的。
        </p>
      </div>

      <div className="panel">
        <h3>这个工具怎么用</h3>
        <ul style={{ margin: 0, paddingLeft: 18, lineHeight: 1.9 }}>
          <li><b>标的列表</b> 按市场 / 板块 / 类型筛选，还能按「有无行情、有无因子」反查缺口</li>
          <li>点任意一行进 <b>K 线</b>：三种复权可切，KDJ / MACD 副图</li>
          <li>
            K 线上的<b>指标由后端引擎算出</b>，跑的是与回测逐字节相同的代码路径 ——
            你看到的就是引擎看到的
          </li>
          <li><b>复权因子</b> 与 <b>分红送配</b> 两张表互相对账，缺一边的会被标出来</li>
        </ul>
      </div>
    </>
  )
}
