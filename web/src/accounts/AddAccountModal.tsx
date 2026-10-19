import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { get, post, type LoginPoll, type LoginStart } from '../api'
import { CopyButton, Modal } from '../components/Modal'

/**
 * Browser (OAuth device) login: the gateway asks the upstream for a state and
 * an auth URL; the operator opens that URL, signs in, and the gateway polls
 * until the token arrives and the account has been added to the pool.
 */
export function AddAccountModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [realm, setRealm] = useState<'intl' | 'cn'>('intl')
  const [platform, setPlatform] = useState('CLI')
  const start = useMutation({
    mutationFn: () => post<LoginStart>('/api/v2/accounts/login/start', { realm, platform }),
  })
  const state = start.data?.state ?? ''
  const poll = useQuery({
    queryKey: ['login-poll', state],
    enabled: !!state,
    queryFn: () => get<LoginPoll>('/api/v2/accounts/login/poll?state=' + encodeURIComponent(state)),
    refetchInterval: (q) => (q.state.data && q.state.data.status !== 'pending' ? false : 2000),
    gcTime: 0,
  })
  const status = poll.data?.status

  useEffect(() => {
    if (status === 'ok') {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['accounts-summary'] })
    }
  }, [status, qc])

  const cancel = useMutation({ mutationFn: () => post('/api/v2/accounts/login/cancel', { state }) })
  const close = () => {
    if (state && status !== 'ok') cancel.mutate()
    onClose()
  }

  return (
    <Modal title="添加账号" onCancel={close} wide>
      {!state && (
        <>
          <p className="dim" style={{ margin: 0 }}>
            用浏览器完成 WorkBuddy 登录后，账号会自动写进网关。国内版首次登录会顺带签到一次。
          </p>
          <div className="field">
            <label>区域</label>
            <div className="chips">
              {(['intl', 'cn'] as const).map((r) => (
                <button key={r} className="chip" aria-pressed={realm === r} onClick={() => setRealm(r)}>
                  {r === 'intl' ? '国际版' : '国内版'}
                </button>
              ))}
            </div>
          </div>
          <div className="field">
            <label>登录身份</label>
            <select className="select" value={platform} onChange={(e) => setPlatform(e.target.value)}>
              <option value="CLI">CLI（默认）</option>
              <option value="VSCode">VSCode 扩展</option>
            </select>
          </div>
          {start.isError && <div className="err">{(start.error as Error).message}</div>}
          <div className="row end">
            <button className="btn" onClick={onClose}>
              取消
            </button>
            <button className="btn primary" disabled={start.isPending} onClick={() => start.mutate()}>
              {start.isPending ? '正在获取登录链接…' : '开始登录'}
            </button>
          </div>
        </>
      )}

      {state && (
        <>
          {status === 'ok' ? (
            <div className="actionresult ok">
              登录成功：{poll.data?.nickname || poll.data?.uid}
              {poll.data?.uid ? <div className="dim mono">{poll.data.uid}</div> : null}
            </div>
          ) : (
            <>
              <div className="field">
                <label>在浏览器里打开下面的链接并完成登录</label>
                <div className="secret mono" style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                  <a href={start.data?.authUrl} target="_blank" rel="noreferrer" style={{ wordBreak: 'break-all', flex: 1 }}>
                    {start.data?.authUrl}
                  </a>
                </div>
              </div>
              <div className="row">
                <a className="btn primary sm" href={start.data?.authUrl} target="_blank" rel="noreferrer">
                  打开登录页
                </a>
                {start.data?.authUrl && <CopyButton text={start.data.authUrl} label="复制链接" />}
                <span className="spacer" />
                <span className="dim">
                  {status === 'expired'
                    ? '登录窗口已过期，请重新开始。'
                    : status === 'error'
                      ? poll.data?.message || '登录失败'
                      : poll.data?.message || '等待浏览器完成登录…'}
                </span>
              </div>
              {status !== 'expired' && status !== 'error' && <div className="dim">登录完成后这里会自动跳转，不需要刷新页面。</div>}
            </>
          )}
          <div className="row end">
            <button className="btn" onClick={close}>
              {status === 'ok' ? '完成' : '取消'}
            </button>
          </div>
        </>
      )}
    </Modal>
  )
}
