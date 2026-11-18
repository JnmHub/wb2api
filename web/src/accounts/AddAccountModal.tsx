import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Input, Segmented, Select, Space, Typography } from 'antd'
import { get, post, type LoginPoll, type LoginStart, type Realm } from '../api'
import { CopyButton, Modal } from '../components/Modal'

/**
 * Browser (OAuth device) login: the gateway asks the upstream for a state and
 * an auth URL; the operator opens that URL, signs in, and the gateway polls
 * until the token arrives and the account has been added to the pool.
 */
export function AddAccountModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [realm, setRealm] = useState<Realm>('intl')
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
  const [callbackURL, setCallbackURL] = useState('')
  const complete = useMutation({
    mutationFn: () =>
      post<{ ok: boolean; uid: string; nickname?: string }>('/api/v2/codearts/login/complete', {
        attempt: state,
        url: callbackURL,
      }),
    onSuccess: () => {
      setCallbackURL('')
      void qc.invalidateQueries({ queryKey: ['login-poll', state] })
      void qc.invalidateQueries({ queryKey: ['accounts'] })
    },
  })

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
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        {!state && (
          <>
            <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
              {realm === 'codearts'
                ? '用华为云账号完成登录后，网关会自动领取 STS 凭据、写入账号，并拉取一次模型目录。'
                : '用浏览器完成 WorkBuddy 登录后，账号会自动写进网关。国内版首次登录会顺带签到一次。'}
            </Typography.Paragraph>
            <div>
              <Typography.Text type="secondary">渠道</Typography.Text>
              <div style={{ marginTop: 4 }}>
                <Segmented
                  value={realm}
                  onChange={(v) => setRealm(v as Realm)}
                  options={[
                    { value: 'intl', label: '国际版' },
                    { value: 'cn', label: '国内版' },
                    { value: 'codearts', label: '华为 CodeArts' },
                  ]}
                />
              </div>
            </div>
            {realm !== 'codearts' && (
              <div>
                <Typography.Text type="secondary">登录身份</Typography.Text>
                <Select
                  style={{ width: '100%' }}
                  value={platform}
                  onChange={setPlatform}
                  options={[
                    { value: 'CLI', label: 'CLI（默认）' },
                    { value: 'VSCode', label: 'VSCode 扩展' },
                  ]}
                />
              </div>
            )}
            {start.isError && <Alert type="error" showIcon message={String(start.error)} />}
            <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
              <Button onClick={onClose}>取消</Button>
              <Button type="primary" loading={start.isPending} onClick={() => start.mutate()}>
                开始登录
              </Button>
            </Space>
          </>
        )}

        {state && (
          <>
            {status === 'ok' ? (
              <Alert
                type="success"
                showIcon
                message={`登录成功：${poll.data?.nickname || poll.data?.uid || ''}`}
                description={poll.data?.uid && <span className="mono">{poll.data.uid}</span>}
              />
            ) : (
              <>
                <div>
                  <Typography.Text type="secondary">在浏览器里打开下面的链接并完成登录</Typography.Text>
                  <Input.TextArea className="mono" readOnly autoSize={{ minRows: 1, maxRows: 3 }} value={start.data?.authUrl ?? ''} />
                </div>
                <Space wrap>
                  <Button type="primary" size="small" href={start.data?.authUrl} target="_blank">
                    打开登录页
                  </Button>
                  {start.data?.authUrl && <CopyButton text={start.data.authUrl} label="复制链接" />}
                  <Typography.Text type="secondary">
                    {status === 'expired'
                      ? '登录窗口已过期，请重新开始。'
                      : status === 'error'
                        ? poll.data?.message || '登录失败'
                        : poll.data?.message || '等待浏览器完成登录…'}
                  </Typography.Text>
                </Space>
                {status !== 'expired' && status !== 'error' && (
                  <Typography.Text type="secondary">登录完成后这里会自动跳转，不需要刷新页面。</Typography.Text>
                )}
                {realm === 'codearts' && (
                  <div>
                    <Typography.Text type="secondary">
                      部署在服务器上时，浏览器打不开 127.0.0.1 的回调地址。把登录后浏览器地址栏那一行
                      （<span className="mono">…/oauth/callback?code=…</span>）粘到这里即可完成登录。
                    </Typography.Text>
                    <Input.TextArea
                      className="mono"
                      autoSize={{ minRows: 2, maxRows: 4 }}
                      placeholder="http://127.0.0.1:52917/oauth/callback?code=…&state=…"
                      value={callbackURL}
                      onChange={(e) => setCallbackURL(e.target.value)}
                    />
                    <Space style={{ marginTop: 8 }}>
                      <Button
                        size="small"
                        disabled={!callbackURL.trim()}
                        loading={complete.isPending}
                        onClick={() => complete.mutate()}
                      >
                        用这个地址完成登录
                      </Button>
                      {complete.data && <Typography.Text type="success">已登录：{complete.data.uid}</Typography.Text>}
                      {complete.error && (
                        <Typography.Text type="danger">{(complete.error as Error).message}</Typography.Text>
                      )}
                    </Space>
                  </div>
                )}
              </>
            )}
            <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
              <Button onClick={close}>{status === 'ok' ? '完成' : '取消'}</Button>
            </Space>
          </>
        )}
      </Space>
    </Modal>
  )
}
