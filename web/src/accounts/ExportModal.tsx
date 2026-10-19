import { useState } from 'react'
import { Modal } from '../components/Modal'

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
      <p className="dim" style={{ margin: 0 }}>
        导出成 JSON 文件，可以再导入到本网关，也兼容旧版 Python 网关的导入格式。
      </p>
      <div className="field">
        <label>区域</label>
        <div className="chips">
          {[
            ['', '全部'],
            ['intl', '国际版'],
            ['cn', '国内版'],
          ].map(([v, label]) => (
            <button key={v} className="chip" aria-pressed={realm === v} onClick={() => setRealm(v)}>
              {label}
            </button>
          ))}
        </div>
      </div>
      <label className="toggle">
        <input type="checkbox" checked={secrets} onChange={(e) => setSecrets(e.target.checked)} />
        <span>
          包含 Token 等凭据
          <span className="dim hint">关掉后导出的是脱敏副本：可以看账号清单，不能直接导入使用。</span>
        </span>
      </label>
      <div className="row end">
        <button className="btn" onClick={onClose}>
          取消
        </button>
        <a className="btn primary" href={url} download onClick={onClose}>
          下载
        </a>
      </div>
    </Modal>
  )
}
