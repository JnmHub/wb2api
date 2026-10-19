import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { put, type ProxySlot, type SettingsView } from '../api'

type Row = Pick<ProxySlot, 'id' | 'name' | 'url' | 'enabled'> & Partial<ProxySlot>

export function ProxySection({ s }: { s: SettingsView }) {
  const qc = useQueryClient()
  const [rows, setRows] = useState<Row[]>(s.proxy_slots.map((x) => ({ ...x })))
  const [dirty, setDirty] = useState(false)
  const save = useMutation({
    mutationFn: () => put<{ slots: ProxySlot[] }>('/api/v2/proxy-slots', { slots: rows.filter((r) => r.url.trim()) }),
    onSuccess: (r) => {
      setRows(r.slots.map((x) => ({ ...x })))
      setDirty(false)
      qc.invalidateQueries({ queryKey: ['settings'] })
    },
  })
  const edit = (i: number, patch: Partial<Row>) => {
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)))
    setDirty(true)
  }
  return (
    <section className="card sec">
      <div className="row">
        <h3>代理槽</h3>
        <span className="spacer" />
        <button
          className="btn sm"
          onClick={() => {
            setRows((rs) => [...rs, { id: '', name: '', url: '', enabled: true }])
            setDirty(true)
          }}
        >
          添加
        </button>
      </div>
      <p className="dim">
        给账号绑定出口代理（http / https / socks5）。账号在"账号详情"或批量操作里选择代理槽编号绑定；槽位编号只增不回收，删除一个槽不会让账号悄悄换到别的出口。
      </p>
      {rows.length === 0 && <div className="dim pad">还没有代理槽，所有账号直连</div>}
      {rows.map((r, i) => (
        <div className="slotrow" key={r.id || 'new' + i}>
          <span className="mono dim">{r.id || '新'}</span>
          <input className="input" placeholder="名称" value={r.name} onChange={(e) => edit(i, { name: e.target.value })} />
          <input className="input mono" placeholder="http://host:port 或 socks5://host:port" value={r.url} onChange={(e) => edit(i, { url: e.target.value })} />
          <label className="toggle compact">
            <input type="checkbox" checked={r.enabled} onChange={(e) => edit(i, { enabled: e.target.checked })} aria-label="启用" />
          </label>
          <button
            className="btn sm danger"
            onClick={() => {
              setRows((rs) => rs.filter((_, j) => j !== i))
              setDirty(true)
            }}
          >
            删除
          </button>
        </div>
      ))}
      {save.isError && <div className="err">{(save.error as Error).message}</div>}
      <div className="row end">
        <button className="btn primary" disabled={!dirty || save.isPending} onClick={() => save.mutate()}>
          保存代理槽
        </button>
      </div>
    </section>
  )
}
