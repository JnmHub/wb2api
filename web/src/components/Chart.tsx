import { useMemo, useState } from 'react'

export interface Bar {
  label: string
  value: number
  detail?: string
}

/**
 * A small dependency-free bar chart. Thousands of points stay cheap because it
 * draws plain SVG rects and one shared hover readout, not a DOM node per tooltip.
 */
export function BarChart({ bars, format }: { bars: Bar[]; format: (n: number) => string }) {
  const [hover, setHover] = useState<number | null>(null)
  const W = 1000
  const H = 180
  const pad = { l: 8, r: 8, t: 10, b: 22 }
  const max = useMemo(() => Math.max(1, ...bars.map((b) => b.value)), [bars])
  if (bars.length === 0) return <div className="dim" style={{ padding: 28, textAlign: 'center' }}>这个时间范围内没有数据</div>
  const bw = (W - pad.l - pad.r) / bars.length
  const every = Math.ceil(bars.length / 10)
  const h = hover != null ? bars[hover] : null
  return (
    <div style={{ position: 'relative' }}>
      <svg className="chart" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label="用量柱状图"
        onMouseLeave={() => setHover(null)}>
        {[0.25, 0.5, 0.75, 1].map((f) => (
          <line key={f} x1={pad.l} x2={W - pad.r} y1={pad.t + (1 - f) * (H - pad.t - pad.b)} y2={pad.t + (1 - f) * (H - pad.t - pad.b)}
            stroke="var(--border)" strokeWidth="1" vectorEffect="non-scaling-stroke" />
        ))}
        {bars.map((b, i) => {
          const bh = (b.value / max) * (H - pad.t - pad.b)
          return (
            <g key={i} onMouseEnter={() => setHover(i)}>
              <rect x={pad.l + i * bw} y={pad.t} width={bw} height={H - pad.t - pad.b} fill="transparent" />
              <rect x={pad.l + i * bw + bw * 0.12} y={H - pad.b - bh} width={Math.max(1, bw * 0.76)} height={bh}
                fill={hover === i ? 'var(--accent)' : 'var(--accent)'} opacity={hover === i ? 1 : 0.72} rx={1.5} />
              {i % every === 0 && (
                <text x={pad.l + i * bw + bw / 2} y={H - 6} fontSize="14" textAnchor="middle" fill="var(--dim)">
                  {b.label}
                </text>
              )}
            </g>
          )
        })}
      </svg>
      <div className="dim" style={{ minHeight: 20, fontSize: 12.5 }} aria-live="polite">
        {h ? `${h.label} · ${format(h.value)}${h.detail ? ' · ' + h.detail : ''}` : `最大 ${format(max)}`}
      </div>
    </div>
  )
}
