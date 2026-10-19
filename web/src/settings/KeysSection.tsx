import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { del, get, patch, post, type KeyView, type SettingsView } from '../api'
import { Confirm, CopyButton, Modal } from '../components/Modal'
import { realmLabel } from '../format'

interface Created extends KeyView {
  key: string
}

export function KeysSection({ s }: { s: SettingsView }) {
  const qc = useQueryClient()
  const refresh = () => qc.invalidateQueries({ queryKey: ['settings'] })
  const [editing, setEditing] = useState<KeyView | 'new' | null>(null)
  const [created, setCreated] = useState<Created | null>(null)
  const [removing, setRemoving] = useState<KeyView | null>(null)
  const toggle = useMutation({
    mutationFn: (k: KeyView) => patch(`/api/v2/keys/${k.id}`, { enabled: !k.enabled }),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: (k: KeyView) => del(`/api/v2/keys/${k.id}`),
    onSuccess: () => {
      setRemoving(null)
      refresh()
    },
  })
  const live = s.keys.filter((k) => !k.deleted)
  const gone = s.keys.filter((k) => k.deleted)
  return (
    <section className="card sec">
      <div className="row">
        <h3>API Key</h3>
        <span className="spacer" />
        <button className="btn primary sm" onClick={() => setEditing('new')}>
          新建 Key
        </button>
      </div>
      {live.length === 0 && (
        <p className="warn" role="note">
          还没有任何 Key：网关此刻不校验调用方，任何能访问端口的客户端都能使用你的账号。建议先建一个。
        </p>
      )}
      <div className="keytable" role="table" aria-label="API Key 列表">
        {live.map((k) => (
          <div className="keyrow" role="row" key={k.id}>
            <div>
              <b>{k.name}</b>
              <div className="dim mono">{k.key_preview}</div>
            </div>
            <span>{k.realm ? <span className={'badge ' + k.realm}>{realmLabel[k.realm]}</span> : <span className="dim">跟随面板</span>}</span>
            <span className="dim" title={k.models.join('\n')}>
              {k.models.length ? `${k.models.length} 条模型规则` : '不限模型'}
            </span>
            <span className="dim">{k.concurrency_limit ? `并发 ≤ ${k.concurrency_limit}` : '并发不限'}</span>
            <label className="toggle compact">
              <input type="checkbox" checked={k.enabled} onChange={() => toggle.mutate(k)} aria-label={`${k.name} 启用`} />
            </label>
            <span className="actions">
              <CopyButton label="复制 Key" text={async () => (await get<{ key: string }>(`/api/v2/keys/${k.id}/secret`)).key} />
              <button className="btn sm" onClick={() => setEditing(k)}>
                编辑
              </button>
              <button className="btn sm danger" onClick={() => setRemoving(k)}>
                删除
              </button>
            </span>
          </div>
        ))}
        {live.length === 0 && <div className="dim pad">暂无 Key</div>}
      </div>
      {gone.length > 0 && (
        <details className="dim">
          <summary>已删除的 Key（{gone.length}，保留以归属历史用量）</summary>
          {gone.map((k) => (
            <div key={k.id} className="mono">
              {k.name} · {k.id} · 删除于 {k.deleted_at}
            </div>
          ))}
        </details>
      )}

      {editing && (
        <KeyEditor
          initial={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(c) => {
            setEditing(null)
            if (c) setCreated(c)
            refresh()
          }}
        />
      )}
      {created && (
        <Modal title="新 Key 已创建" onCancel={() => setCreated(null)}>
          <span className="dim">完整的 Key 只在这里显示一次（之后仍可在列表里"复制 Key"）。</span>
          <div className="secret mono" data-testid="new-key">{created.key}</div>
          <div className="row end">
            <CopyButton text={created.key} />
            <button className="btn primary" onClick={() => setCreated(null)}>
              完成
            </button>
          </div>
        </Modal>
      )}
      {removing && (
        <Confirm
          title={`删除 Key「${removing.name}」`}
          text="Key 立即失效，不可恢复；历史用量仍归属在这个名字下。"
          onCancel={() => setRemoving(null)}
          onOk={() => remove.mutate(removing)}
        />
      )}
    </section>
  )
}

function KeyEditor(props: { initial: KeyView | null; onClose: () => void; onSaved: (created: Created | null) => void }) {
  const k = props.initial
  const [name, setName] = useState(k?.name ?? '')
  const [realm, setRealm] = useState<string>(k?.realm ?? '')
  const [models, setModels] = useState((k?.models ?? []).join('\n'))
  const [conc, setConc] = useState(String(k?.concurrency_limit ?? 0))
  const [secret, setSecret] = useState('')
  const m = useMutation({
    mutationFn: async () => {
      const body: Record<string, unknown> = {
        name, realm, models: models.split(/[\s,]+/).filter(Boolean), concurrency_limit: Number(conc || 0),
      }
      if (!k) {
        if (secret.trim()) body.key = secret.trim()
        return { created: await post<Created>('/api/v2/keys', body) }
      }
      await patch(`/api/v2/keys/${k.id}`, body)
      return { created: null }
    },
    onSuccess: (r) => props.onSaved(r.created),
  })
  return (
    <Modal title={k ? '编辑 Key' : '新建 Key'} onCancel={props.onClose} wide>
      <div className="grid2">
        <div className="field">
          <label>名称</label>
          <input className="input" autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：cursor / ci" />
        </div>
        <div className="field">
          <label>绑定出口</label>
          <select className="select" value={realm} onChange={(e) => setRealm(e.target.value)}>
            <option value="">跟随面板切换</option>
            <option value="intl">国际版</option>
            <option value="cn">国内版</option>
          </select>
        </div>
        <div className="field">
          <label>并发上限（0 = 不限）</label>
          <input className="input" type="number" min={0} value={conc} onChange={(e) => setConc(e.target.value)} />
        </div>
        {!k && (
          <div className="field">
            <label>自定义 Key（留空 = 自动生成）</label>
            <input className="input mono" value={secret} onChange={(e) => setSecret(e.target.value)} placeholder="至少 4 个字符" />
          </div>
        )}
      </div>
      <div className="field">
        <label>允许的模型（每行一条，支持 * 通配；留空 = 不限）</label>
        <textarea className="input mono" rows={4} value={models} onChange={(e) => setModels(e.target.value)} placeholder={'glm-*\ndeepseek-v4.1-flash'} />
      </div>
      {m.isError && <div className="err">{(m.error as Error).message}</div>}
      <div className="row end">
        <button className="btn" onClick={props.onClose}>
          取消
        </button>
        <button className="btn primary" disabled={m.isPending} onClick={() => m.mutate()}>
          保存
        </button>
      </div>
    </Modal>
  )
}
