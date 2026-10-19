import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
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
      <p className="dim" style={{ margin: 0 }}>
        读取本机 WorkBuddy 桌面客户端的登录凭据。文件是加密的，密钥只保存在网关内存里，不会写盘。自动从客户端进程内存回收密钥尚未移植，请粘贴 64 位 hex 保护密钥或 44 字符 base64 的 atRestSecretKey。
      </p>
      {st && (
        <div className="row dim">
          <span>keyblob：{st.keyblob_found ? '已找到' : '未找到'}</span>
          {st.key_id && <span className="mono">keyId {st.key_id}</span>}
          <span className={st.key_cached ? 'badge ready' : 'badge cooling'}>{st.key_cached ? '密钥已导入' : '尚未导入密钥'}</span>
          {st.key_source && <span>来源 {st.key_source}</span>}
        </div>
      )}
      {st?.error && <div className="dim">{st.error}</div>}
      <div className="field">
        <label>at-rest 密钥</label>
        <div className="row">
          <input
            className="input mono"
            style={{ flex: 1 }}
            type="password"
            value={keyText}
            onChange={(e) => setKeyText(e.target.value)}
            placeholder="64 位 hex 或 44 字符 base64"
          />
          <button className="btn primary" disabled={!keyText.trim() || act.isPending} onClick={() => act.mutate({ keyText: keyText.trim() })}>
            导入密钥
          </button>
          {st?.key_cached && (
            <button className="btn sm" onClick={() => act.mutate({ forgetKey: true })}>
              忘记密钥
            </button>
          )}
        </div>
      </div>
      {q.isError && <div className="err">{(q.error as Error).message}</div>}
      <div className="row">
        <b>发现的凭据</b>
        <span className="spacer" />
        <button className="btn primary sm" disabled={act.isPending || files.filter((f) => f.valid).length === 0} onClick={() => act.mutate({ all: true })}>
          全部导入
        </button>
      </div>
      {files.length === 0 && <div className="dim pad">没有找到桌面凭据文件（需要桌面客户端登录过）。</div>}
      {files.map((f) => (
        <FileRow key={f.path} f={f} busy={act.isPending} onImport={() => act.mutate({ path: f.path, realm: f.realm })} />
      ))}
      {imported.length > 0 && (
        <div className="actionresult" role="status">
          {imported.map((r) => (
            <div key={r.path}>
              {r.ok ? `✓ ${r.nickname || r.uid}（${r.realm}）` : `✕ ${r.path}：${r.error}`}
            </div>
          ))}
        </div>
      )}
      {act.isError && <div className="err">{(act.error as Error).message}</div>}
      <div className="row end">
        <button className="btn" onClick={onClose}>
          关闭
        </button>
      </div>
    </Modal>
  )
}

function FileRow({ f, busy, onImport }: { f: DesktopFile; busy: boolean; onImport: () => void }) {
  return (
    <div className="row" style={{ borderTop: '1px solid var(--border)', paddingTop: 8 }}>
      <span className={f.valid ? 'badge ready' : f.needs_key ? 'badge cooling' : 'badge restricted'}>
        {f.valid ? '可导入' : f.needs_key ? '需要密钥' : '不可用'}
      </span>
      <span className="mono">{f.file}</span>
      <span className="dim">
        {f.realm_name}
        {f.uid ? ` · ${f.nickname || f.uid.slice(0, 8)}` : ''}
      </span>
      {f.error && <span className="dim">{f.error}</span>}
      <span className="spacer" />
      <button className="btn sm" disabled={!f.valid || busy} onClick={onImport}>
        导入
      </button>
    </div>
  )
}
