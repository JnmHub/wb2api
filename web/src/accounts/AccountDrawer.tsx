import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { del, get, patch, post, type Account, type ActionResult, type Job } from '../api'
import { applyPatch } from '../events'
import { credit, num, realmLabel, span, statusLabel, tokens, until } from '../format'
import { JobToast } from './BatchBar'

const actionLabel: Record<string, string> = {
  credits: '刷新积分',
  checkin: '签到',
  'report-activity': '活跃上报',
  'daily-chat': '每日活跃',
  'daily-chat-web': '网页活跃',
  'sync-profile': '同步昵称',
  test: '测试账号',
}

export function AccountDrawer({ uid, onClose }: { uid: string; onClose: () => void }) {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['account', uid], queryFn: () => get<Account>(`/api/v2/accounts/${uid}`) })
  const a = q.data
  const [form, setForm] = useState({ enabled: true, weight: 1, note: '', models: '', proxySlot: '', product: 'workbuddy' })
  const [dirty, setDirty] = useState(false)
  const [job, setJob] = useState<string | null>(null)
  const [busy, setBusy] = useState('')
  const [result, setResult] = useState<{ name: string; ok: boolean; text: string } | null>(null)
  const [testModel, setTestModel] = useState('')

  useEffect(() => {
    if (a && !dirty) {
      setForm({
        enabled: a.enabled, weight: a.weight, note: a.note, models: a.disabled_models.join(', '),
        proxySlot: a.proxy_slot, product: a.product,
      })
    }
  }, [a, dirty])
  useEffect(() => {
    const on = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', on)
    return () => window.removeEventListener('keydown', on)
  }, [onClose])

  const save = useMutation({
    mutationFn: () =>
      patch<Account>(`/api/v2/accounts/${uid}`, {
        enabled: form.enabled, weight: Number(form.weight), note: form.note,
        disabled_models: form.models.split(/[,\s]+/).filter(Boolean), proxy_slot: form.proxySlot, product: form.product,
      }),
    onSuccess: (acc) => {
      setDirty(false)
      qc.setQueryData(['account', uid], acc)
      applyPatch(qc, { accounts: [acc] })
    },
  })
  const act = async (action: string, confirm = false) => {
    const j = await post<Job>('/api/v2/accounts/batch', { action, uids: [uid], confirm })
    qc.setQueryData(['job', j.id], j)
    setJob(j.id)
  }
  const runAction = async (name: string) => {
    setBusy(name)
    setResult(null)
    try {
      const body = name === 'test' && testModel.trim() ? { model: testModel.trim() } : {}
      const r = await post<ActionResult>(`/api/v2/accounts/${uid}/actions/${name}`, body)
      const ok = r.ok !== false && !r.error
      let text = ok ? r.reply || r.msg || '成功' : r.error || r.msg || '失败'
      if (r.elapsed_ms) text += ` · ${r.elapsed_ms} ms`
      if (r.status) text += ` · HTTP ${r.status}`
      if (r.restricted) text += ' · 已被上游风控'
      if (r.network) text += ' · 网络不通'
      setResult({ name, ok, text })
      if (ok) {
        qc.invalidateQueries({ queryKey: ['account', uid] })
        qc.invalidateQueries({ queryKey: ['accounts'] })
        qc.invalidateQueries({ queryKey: ['accounts-summary'] })
      }
    } catch (e) {
      setResult({ name, ok: false, text: (e as Error).message })
    } finally {
      setBusy('')
    }
  }
  const remove = useMutation({
    mutationFn: () => del(`/api/v2/accounts/${uid}`),
    onSuccess: () => {
      applyPatch(qc, { removed: [uid] })
      onClose()
    },
  })
  const set = <K extends keyof typeof form>(k: K, v: (typeof form)[K]) => {
    setForm((f) => ({ ...f, [k]: v }))
    setDirty(true)
  }

  return (
    <>
      <div className="scrim" onClick={onClose} />
      <aside className="drawer" role="dialog" aria-label="账号详情">
        <header>
          <div style={{ minWidth: 0 }}>
            <b>{a?.nickname ?? '…'}</b>
            <div className="dim mono">{uid}</div>
          </div>
          <span className="spacer" />
          {a && <span className={'badge ' + a.status}>{statusLabel[a.status]}</span>}
          <button className="btn sm" onClick={onClose} aria-label="关闭">✕</button>
        </header>
        <div className="body">
          {q.isError && <span className="err">{(q.error as Error).message}</span>}
          {a && (
            <>
              <div className="field">
                <label>
                  <input type="checkbox" checked={form.enabled} onChange={(e) => set('enabled', e.target.checked)} /> 参与调度
                </label>
              </div>
              <div className="field">
                <label>权重（1–100）</label>
                <input className="input" type="number" min={1} max={100} value={form.weight} onChange={(e) => set('weight', Number(e.target.value))} />
              </div>
              <div className="field">
                <label>备注</label>
                <input className="input" maxLength={200} value={form.note} onChange={(e) => set('note', e.target.value)} />
              </div>
              <div className="field">
                <label>禁用的模型（逗号分隔，精确匹配）</label>
                <input className="input" value={form.models} onChange={(e) => set('models', e.target.value)} placeholder="gpt-5.5, grok-4.7" />
              </div>
              <div className="field">
                <label>代理槽</label>
                <input className="input" value={form.proxySlot} onChange={(e) => set('proxySlot', e.target.value)} />
              </div>
              <div className="field">
                <label>出站身份</label>
                <select className="select" value={form.product} onChange={(e) => set('product', e.target.value)}>
                  <option value="workbuddy">桌面端 (workbuddy)</option>
                  <option value="vscode">VSCode 扩展</option>
                  <option value="cli">CodeBuddy CLI</option>
                </select>
              </div>

              <dl className="kv">
                <dt>区域</dt><dd>{realmLabel[a.realm]}</dd>
                <dt>Token 有效期</dt><dd>{until(a.expires_at)}</dd>
                <dt>今日</dt><dd>{num(a.today.requests)} 次 · {tokens(a.today.tokens)} tokens</dd>
                {a.credits && (a.credits.remain != null || a.credits.total != null) && (
                  <>
                    <dt>积分</dt>
                    <dd>
                      {credit(a.credits.remain)} / {credit(a.credits.total)}
                    </dd>
                  </>
                )}
                <dt>进行中</dt><dd>{a.inflight}</dd>
                {a.cooldown_for ? (<><dt>冷却</dt><dd>剩余 {span(a.cooldown_for)}</dd></>) : null}
                {a.restricted_for ? (<><dt>风控限制</dt><dd>剩余 {span(a.restricted_for)}</dd></>) : null}
                {a.refresh ? (<><dt>刷新失败</dt><dd>{a.refresh.failures} 次：{a.refresh.last_error}</dd></>) : null}
                <dt>机器码</dt><dd className="mono">{a.machine_id}</dd>
                <dt>会话 ID</dt><dd className="mono">{a.session_id}</dd>
                <dt>来源</dt><dd>{a.source}{a.enterprise_id ? ` · 企业 ${a.enterprise_id}` : ''}</dd>
              </dl>

              {a.model_cooldowns && a.model_cooldowns.length > 0 && (
                <div>
                  <label className="dim">受限模型</label>
                  <div className="chips" style={{ marginTop: 4 }}>
                    {a.model_cooldowns.map((m) => (
                      <span key={m.model} className="badge cooling">
                        {m.model} · {span(m.expires_at - Date.now() / 1000)}
                      </span>
                    ))}
                  </div>
                </div>
              )}
              <div className="field">
                <label>账号操作</label>
                <div className="actionrow">
                  <button className="btn sm" disabled={!!busy} onClick={() => runAction('credits')}>
                    {busy === 'credits' ? '刷新中…' : '刷新积分'}
                  </button>
                  {a.realm === 'cn' && (
                    <button className="btn sm" disabled={!!busy} onClick={() => runAction('checkin')}>
                      签到
                    </button>
                  )}
                  {a.realm === 'cn' && (
                    <button className="btn sm" disabled={!!busy} onClick={() => runAction('report-activity')}>
                      活跃上报
                    </button>
                  )}
                  {a.realm === 'intl' && (
                    <button className="btn sm" disabled={!!busy} onClick={() => runAction('daily-chat')}>
                      每日活跃
                    </button>
                  )}
                  {a.realm === 'intl' && (
                    <button className="btn sm" disabled={!!busy} onClick={() => runAction('daily-chat-web')}>
                      网页活跃
                    </button>
                  )}
                  <button className="btn sm" disabled={!!busy} onClick={() => runAction('sync-profile')}>
                    同步昵称
                  </button>
                  <button className="btn sm" onClick={() => act('refresh_token')}>
                    刷新 Token
                  </button>
                  <button className="btn sm" onClick={() => act('clear_cooldown')}>
                    清除冷却
                  </button>
                  <button className="btn sm danger" onClick={() => confirm('永久删除这个账号？') && remove.mutate()}>
                    删除账号
                  </button>
                </div>
                <div className="row" style={{ marginTop: 6 }}>
                  <input className="input" placeholder="测试用模型（留空 = 自动选择）" value={testModel} onChange={(e) => setTestModel(e.target.value)} style={{ flex: 1 }} />
                  <button className="btn sm" disabled={!!busy} onClick={() => runAction('test')}>
                    {busy === 'test' ? '测试中…' : '测试账号'}
                  </button>
                </div>
                {result && (
                  <div className={'actionresult ' + (result.ok ? 'ok' : 'bad')} role="status">
                    <b>{actionLabel[result.name] ?? result.name}</b> {result.text}
                  </div>
                )}
              </div>
            </>
          )}
        </div>
        <footer>
          {save.isError && <span className="err" style={{ marginRight: 'auto' }}>{(save.error as Error).message}</span>}
          <button className="btn" onClick={onClose}>关闭</button>
          <button className="btn primary" disabled={!dirty || save.isPending} onClick={() => save.mutate()}>
            保存
          </button>
        </footer>
      </aside>
      {job && <JobToast id={job} onClose={() => setJob(null)} />}
    </>
  )
}
