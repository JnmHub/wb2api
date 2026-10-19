import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Input, Space, Tag, Typography } from 'antd'
import { get, post, type DesktopFile, type DesktopResult } from '../api'
import { Modal } from '../components/Modal'

/**
 * Import accounts the WorkBuddy desktop client already logged in. The files are
 * encrypted (at-rest envelope); the operator supplies the protector key once and
 * it stays in the gateway's memory only.
 */
export function DesktopImportModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [keyText, setKeyText] = useState('')
  const q = useQuery({ queryKey: ['desktop'], queryFn: () => get<DesktopResult>('/api/v2/accounts/desktop'), retry: false })
  const act = useMutation({
    mutationFn: (body: Record<string, unknown>) => post<DesktopResult>('/api/v2/accounts/desktop', body),
    onSuccess: (r) => {
      qc.setQueryData(['desktop'], r)
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['accounts-summary'] })
    },
  })
  const st = q.data?.status
  const files = q.data?.files ?? []
  const imported = act.data?.imported ?? []
  return (
    <Modal title="从桌面客户端导入" onCancel={onClose} wide>
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
          读取本机 WorkBuddy 桌面客户端的登录凭据。文件是加密的，密钥只保存在网关内存里，不会写盘。自动从客户端进程内存回收密钥尚未移植，请粘贴 64 位 hex 保护密钥或 44 字符 base64 的 atRestSecretKey。
        </Typography.Paragraph>
        {st && (
          <Space wrap size={[10, 4]}>
            <Typography.Text type="secondary">keyblob：{st.keyblob_found ? '已找到' : '未找到'}</Typography.Text>
            {st.key_id && <Typography.Text className="mono">keyId {st.key_id}</Typography.Text>}
            <Tag color={st.key_cached ? 'success' : 'warning'}>{st.key_cached ? '密钥已导入' : '尚未导入密钥'}</Tag>
            {st.key_source && <Typography.Text type="secondary">来源 {st.key_source}</Typography.Text>}
          </Space>
        )}
        {st?.error && <Typography.Text type="secondary">{st.error}</Typography.Text>}
        <div>
          <Typography.Text type="secondary">at-rest 密钥</Typography.Text>
          <Space.Compact style={{ width: '100%' }}>
            <Input.Password
              className="mono"
              value={keyText}
              onChange={(e) => setKeyText(e.target.value)}
              placeholder="64 位 hex 或 44 字符 base64"
            />
            <Button type="primary" disabled={!keyText.trim() || act.isPending} onClick={() => act.mutate({ keyText: keyText.trim() })}>
              导入密钥
            </Button>
            {st?.key_cached && (
              <Button onClick={() => act.mutate({ forgetKey: true })}>
                忘记密钥
              </Button>
            )}
          </Space.Compact>
        </div>
        {q.isError && <Alert type="error" showIcon message={String(q.error)} />}

        <Card
          size="small"
          title="发现的凭据"
          extra={
            <Button
              size="small"
              type="primary"
              disabled={act.isPending || files.filter((f) => f.valid).length === 0}
              onClick={() => act.mutate({ all: true })}
            >
              全部导入
            </Button>
          }
        >
          {files.length === 0 && <Typography.Text type="secondary">没有找到桌面凭据文件（需要桌面客户端登录过）。</Typography.Text>}
          <Space direction="vertical" size={8} style={{ width: '100%' }}>
            {files.map((f) => (
              <FileRow key={f.path} f={f} busy={act.isPending} onImport={() => act.mutate({ path: f.path, realm: f.realm })} />
            ))}
          </Space>
        </Card>

        {imported.length > 0 && (
          <Alert
            type={imported.some((r) => r.error) ? 'warning' : 'success'}
            showIcon
            message="导入结果"
            description={imported.map((r) => (
              <div key={r.path}>{r.ok ? `✓ ${r.nickname || r.uid}（${r.realm}）` : `✕ ${r.path}：${r.error}`}</div>
            ))}
          />
        )}
        {act.isError && <Alert type="error" showIcon message={String(act.error)} />}
        <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>关闭</Button>
        </Space>
      </Space>
    </Modal>
  )
}

function FileRow({ f, busy, onImport }: { f: DesktopFile; busy: boolean; onImport: () => void }) {
  return (
    <div className="catline" style={{ paddingTop: 0 }}>
      <Tag color={f.valid ? 'success' : f.needs_key ? 'warning' : 'error'}>{f.valid ? '可导入' : f.needs_key ? '需要密钥' : '不可用'}</Tag>
      <span className="mono">{f.file}</span>
      <Typography.Text type="secondary">
        {f.realm_name}
        {f.uid ? ` · ${f.nickname || f.uid.slice(0, 8)}` : ''}
      </Typography.Text>
      {f.error && <Typography.Text type="secondary">{f.error}</Typography.Text>}
      <span className="spacer" />
      <Button size="small" disabled={!f.valid || busy} onClick={onImport}>
        导入
      </Button>
    </div>
  )
}
