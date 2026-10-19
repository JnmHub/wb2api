import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { del, get, type LogEntry, type LogList } from '../api'

const MAX_KEPT = 2000
const SHOWN = 600

/** Appends a new batch to the kept lines, bounded and without duplicates. */
export function mergeLogs(kept: LogEntry[], batch: LogEntry[]): LogEntry[] {
  if (batch.length === 0) return kept
  const lastId = kept.length ? kept[kept.length - 1].id : 0
  const fresh = batch.filter((e) => e.id > lastId)
  if (fresh.length === 0) return kept
  const all = kept.concat(fresh)
  return all.length > MAX_KEPT ? all.slice(all.length - MAX_KEPT) : all
}

export function LogsPage() {
  const [level, setLevel] = useState('')
  const [tag, setTag] = useState('')
  const [q, setQ] = useState('')
  const [follow, setFollow] = useState(true)
  const [paused, setPaused] = useState(false)
  const [lines, setLines] = useState<LogEntry[]>([])
  const [tags, setTags] = useState<string[]>([])
  const lastId = useRef(0)
  const box = useRef<HTMLDivElement>(null)
  const filterKey = `${level}|${tag}|${q}`

  // A changed filter starts over: the kept lines were selected by the old one.
  useEffect(() => {
    lastId.current = 0
    setLines([])
  }, [filterKey])

  const params = useMemo(() => {
    const p = new URLSearchParams()
    if (level) p.set('level', level)
    if (tag) p.set('tag', tag)
    if (q) p.set('q', q)
    return p
  }, [level, tag, q])

  const poll = useQuery({
    queryKey: ['logs', filterKey],
    queryFn: async () => {
      const p = new URLSearchParams(params)
      p.set('after', String(lastId.current))
      p.set('limit', '1000')
      return get<LogList>('/api/v2/logs?' + p)
    },
    refetchInterval: paused ? false : 2000,
    gcTime: 0,
  })

  useEffect(() => {
    const r = poll.data
    if (!r) return
    if (r.entries.length) lastId.current = r.entries[r.entries.length - 1].id
    setLines((cur) => mergeLogs(cur, r.entries))
    setTags(r.tags ?? [])
  }, [poll.data])

  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [lines, follow])

  const shown = lines.length > SHOWN ? lines.slice(lines.length - SHOWN) : lines
  const clear = async () => {
    await del('/api/v2/logs')
    lastId.current = 0
    setLines([])
    void poll.refetch()
  }
  return (
    <div className="page logs">
      <div className="toolbar">
        <select className="select" value={level} onChange={(e) => setLevel(e.target.value)} aria-label="级别">
          <option value="">全部级别</option>
          <option value="WARN">警告及以上</option>
          <option value="ERROR">仅错误</option>
        </select>
        <select className="select" value={tag} onChange={(e) => setTag(e.target.value)} aria-label="模块">
          <option value="">全部模块</option>
          {tags.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>
        <input className="input" style={{ width: 240 }} placeholder="搜索日志" value={q} onChange={(e) => setQ(e.target.value)} aria-label="搜索日志" />
        <label className="toggle compact">
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> 跟随最新
        </label>
        <button className="btn sm" onClick={() => setPaused((p) => !p)}>
          {paused ? '继续' : '暂停'}
        </button>
        <span className="spacer" />
        <span className="dim">
          {lines.length} 行{paused ? '（已暂停）' : ''} · 内存环形缓冲，重启后清空
        </span>
        <a className="btn sm" href={'/api/v2/logs/export?' + params} download>
          导出
        </a>
        <button className="btn sm danger" onClick={clear}>
          清空
        </button>
      </div>
      <div className="card logbox mono" ref={box} role="log" aria-live="off">
        {shown.map((e) => (
          <div key={e.id} className={'logline ' + e.level.toLowerCase()}>
            <span className="dim">{e.time}</span> <span className={'lvl ' + e.level.toLowerCase()}>{e.level}</span>{' '}
            <span className="tag">[{e.tag}]</span> <span className="msg">{e.msg}</span>
          </div>
        ))}
        {shown.length === 0 && <div className="dim pad">{poll.isPending ? '加载中…' : poll.isError ? '读取失败' : '没有符合条件的日志'}</div>}
      </div>
    </div>
  )
}
