import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { get, post, type AgentClient, type AgentsView } from '../api'

/**
 * Point installed AI CLIs at this gateway. Every file is backed up before it is
 * touched and restore puts the pre-integration bytes back. Writes are only
 * accepted from loopback, so the panel must be opened on the host itself.
 */
export function AgentsSection() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['agents'], queryFn: () => get<AgentsView>('/api/v2/agents'), retry: false })
  const [key, setKey] = useState('')
  const [model, setModel] = useState('')
  const apply = useMutation({
    mutationFn: (v: { client: string }) =>
      post<{ clients: AgentClient[] }>('/api/v2/agents/apply', {
        client: v.client,
        base_url: q.data?.base_url,
        api_key: key,
        model,
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['agents'] }),
  })
  const restore = useMutation({
    mutationFn: (client: string) => post<{ clients: AgentClient[] }>('/api/v2/agents/restore', { client }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['agents'] }),
  })
  const list = q.data?.clients ?? []
  return (
    <section className="card sec">
      <h3>智能体配置</h3>
      <p className="dim">
        把 Claude Code / Codex / OpenCode / DSH / Crush / ZCode 指向本网关。接管前会备份原文件，恢复时按备份还原；只接受本机请求。ZCode 会边运行边重写配置，接管前请先退出它。
      </p>
      <div className="row">
        <div className="field">
          <label>网关地址</label>
          <input className="input" value={q.data?.base_url ?? ''} readOnly aria-label="网关地址" />
        </div>
        <div className="field">
          <label>智能体 API Key</label>
          <input className="input" type="password" value={key} onChange={(e) => setKey(e.target.value)} placeholder="sk-…" aria-label="智能体使用的 API Key" />
        </div>
        <div className="field">
          <label>默认模型（可选）</label>
          <input className="input mono" value={model} onChange={(e) => setModel(e.target.value)} placeholder="glm-5.3-flash" aria-label="智能体默认模型" />
        </div>
      </div>
      {q.data && !q.data.loopback && <div className="err">当前不是本机访问，接管/恢复会被服务端拒绝。</div>}
      {apply.isError && <div className="err">{(apply.error as Error).message}</div>}
      {restore.isError && <div className="err">{(restore.error as Error).message}</div>}
      {list.map((c) => (
        <div key={c.id} className="row" style={{ borderTop: '1px solid var(--border)', padding: '8px 0', alignItems: 'flex-start' }}>
          <div style={{ minWidth: 220 }}>
            <b>{c.label}</b>
            <div className="dim hint">{c.desc}</div>
            <div className="dim mono" style={{ fontSize: 11, wordBreak: 'break-all' }} title={c.config_paths.join('\n')}>
              {c.config_paths.join(' · ')}
            </div>
          </div>
          <span className={c.installed ? 'badge ready' : 'badge'}>{c.installed ? '已安装' : '未检测到'}</span>
          {c.configured && <span className="badge ready">已接管</span>}
          {c.applied?.external_change && <span className="badge cooling">接管后被改过</span>}
          {c.applied && <span className="dim">模型 {c.applied.model || '默认'}</span>}
          <span className="spacer" />
          <button
            className="btn sm primary"
            disabled={!key.trim() || apply.isPending || !q.data?.loopback}
            onClick={() => apply.mutate({ client: c.id })}
          >
            {apply.isPending && apply.variables?.client === c.id ? '写入中…' : c.configured ? '重新接管' : '接管'}
          </button>
          <button
            className="btn sm"
            disabled={!c.configured || restore.isPending || !q.data?.loopback}
            onClick={() => restore.mutate(c.id)}
          >
            {restore.isPending && restore.variables === c.id ? '恢复中…' : '恢复'}
          </button>
        </div>
      ))}
      {q.isError && <div className="err">{(q.error as Error).message}</div>}
    </section>
  )
}
