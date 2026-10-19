import { useState } from 'react'
import { Button, Segmented, Space, Typography } from 'antd'
import { Modal, Toggle } from '../components/Modal'

/** Download the accounts as the gateway's own JSON export. */
export function ExportModal({ onClose }: { onClose: () => void }) {
  const [realm, setRealm] = useState('')
  const [secrets, setSecrets] = useState(true)
  const p = new URLSearchParams()
  if (realm) p.set('realm', realm)
  if (!secrets) p.set('secrets', '0')
  const url = '/api/v2/accounts/export?' + p
  return (
    <Modal title="导出账号" onCancel={onClose}>
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
          导出成 JSON 文件，可以再导入到本网关，也兼容旧版 Python 网关的导入格式。
        </Typography.Paragraph>
        <div>
          <Typography.Text type="secondary">区域</Typography.Text>
          <div style={{ marginTop: 4 }}>
            <Segmented
              value={realm}
              onChange={(v) => setRealm(String(v))}
              options={[
                { value: '', label: '全部' },
                { value: 'intl', label: '国际版' },
                { value: 'cn', label: '国内版' },
              ]}
            />
          </div>
        </div>
        <Toggle
          label="包含 Token 等凭据"
          hint="关掉后导出的是脱敏副本：可以看账号清单，不能直接导入使用。"
          checked={secrets}
          onChange={setSecrets}
        />
        <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>取消</Button>
          <Button type="primary" href={url} download onClick={onClose}>
            下载
          </Button>
        </Space>
      </Space>
    </Modal>
  )
}
