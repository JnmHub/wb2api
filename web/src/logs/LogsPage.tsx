import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button, Card, Input, Select, Space, Switch, Tag, Typography } from 'antd'
import { Download, Pause, Play, Trash2 } from 'lucide-react'
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
    <div className="page">
      <div className="page-head">
        <div>
          <Typography.Title level={3} style={{ margin: 0 }}>
            日志
          </Typography.Title>
          <Typography.Text type="secondary">
            {lines.length} 行{paused ? '（已暂停）' : ''} · 内存环形缓冲，重启后清空
          </Typography.Text>
        </div>
        <Space wrap>
          <Space size={6}>
            <Switch size="small" checked={follow} onChange={setFollow} />
            <Typography.Text type="secondary">跟随最新</Typography.Text>
          </Space>
          <Button icon={paused ? <Play size={14} /> : <Pause size={14} />} onClick={() => setPaused((p) => !p)}>
            {paused ? '继续' : '暂停'}
          </Button>
          <Button icon={<Download size={14} />} href={'/api/v2/logs/export?' + params} download>
            导出
          </Button>
          <Button danger icon={<Trash2 size={14} />} onClick={clear}>
            清空
          </Button>
        </Space>
      </div>

      <Card styles={{ body: { padding: 12 } }} style={{ marginBottom: 12 }}>
        <Space wrap size={10}>
          <Select
            value={level}
            style={{ width: 150 }}
            onChange={setLevel}
            options={[
              { value: '', label: '全部级别' },
              { value: 'WARN', label: '警告及以上' },
              { value: 'ERROR', label: '仅错误' },
            ]}
          />
          <Select
            value={tag}
            style={{ width: 160 }}
            onChange={setTag}
            options={[{ value: '', label: '全部模块' }, ...tags.map((t) => ({ value: t, label: t }))]}
          />
          <Input.Search allowClear style={{ width: 260 }} placeholder="搜索日志" value={q} onChange={(e) => setQ(e.target.value)} />
          {paused && <Tag color="warning">已暂停刷新</Tag>}
        </Space>
      </Card>

      <Card styles={{ body: { padding: 0 } }}>
        <div className="logbox mono" ref={box} role="log" aria-live="off" style={{ borderRadius: 8 }}>
          {shown.map((e) => (
            <div key={e.id} className={'logline ' + e.level.toLowerCase()}>
              <span className="dim">{e.time}</span> <span className={'lvl ' + e.level.toLowerCase()}>{e.level}</span>{' '}
              <span className="tag">[{e.tag}]</span> <span className="msg">{e.msg}</span>
            </div>
          ))}
          {shown.length === 0 && (
            <div className="dim pad">{poll.isPending ? '加载中…' : poll.isError ? '读取失败' : '没有符合条件的日志'}</div>
          )}
        </div>
      </Card>
    </div>
  )
}
