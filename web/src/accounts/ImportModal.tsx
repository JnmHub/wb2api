import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { post, type ImportResponse } from '../api'
import { Modal } from '../components/Modal'

/** Import accounts from the gateway's own export, a bare list, a single account
 *  object or a desktop credential. A dry run shows the same report the real
 *  import would produce, without touching a single account. */
export function ImportModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [text, setText] = useState('')
  const [realm, setRealm] = useState('')
  const [overwrite, setOverwrite] = useState(false)
  const [file, setFile] = useState('')
  const run = useMutation({
    mutationFn: (dry: boolean) => post<ImportResponse>('/api/v2/accounts/import', { document: text, realm, overwrite, dry_run: dry }),
    onSuccess: (r, dry) => {
      if (!dry && (r.result.added.length || r.result.updated.length)) {
        qc.invalidateQueries({ queryKey: ['accounts'] })
        qc.invalidateQueries({ queryKey: ['accounts-summary'] })
      }
    },
  })
  const read = async (f: File | undefined) => {
    if (!f) return
    setFile(f.name)
    setText(await f.text())
  }
  return (
    <Modal title="导入账号" onCancel={onClose} wide>
      <p className="dim" style={{ margin: 0 }}>
        接受本网关导出的 JSON、账号数组、单个账号对象或桌面端凭据文件。先点“预演”可以看清会新增、更新、跳过哪些账号，以及哪些行不合格。
      </p>
      <div className="grid2">
        <div className="field">
          <label>区域（跟随文档时按 token / domain 判断）</label>
          <select className="select" value={realm} onChange={(e) => setRealm(e.target.value)}>
            <option value="">跟随文档</option>
            <option value="intl">国际版</option>
            <option value="cn">国内版</option>
          </select>
        </div>
        <div className="field">
          <label>从文件读取</label>
          <input className="input" type="file" accept=".json,.txt,application/json,text/plain" onChange={(e) => void read(e.target.files?.[0])} />
        </div>
      </div>
      <div className="field">
        <label>{file ? `内容（来自 ${file}）` : '或直接粘贴 JSON'}</label>
        <textarea className="input mono" rows={10} value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} placeholder={'{\n  "accounts": [ { "uid": "…", "accessToken": "…", "realm": "intl" } ]\n}'} />
      </div>
      <label className="toggle">
        <input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
        <span>
          覆盖同 UID 的已有账号
          <span className="dim hint">覆盖时保留原有的添加时间、积分、签到记录和代理槽绑定。</span>
        </span>
      </label>
      {run.isError && <div className="err" role="alert">{(run.error as Error).message}</div>}
      {run.data && <Report r={run.data} />}
      <div className="row end">
        <button className="btn" onClick={onClose}>
          关闭
        </button>
        <button className="btn" disabled={!text.trim() || run.isPending} onClick={() => run.mutate(true)}>
          预演
        </button>
        <button className="btn primary" disabled={!text.trim() || run.isPending} onClick={() => run.mutate(false)}>
          {run.isPending ? '处理中…' : '导入'}
        </button>
      </div>
    </Modal>
  )
}

function Report({ r }: { r: ImportResponse }) {
  const rep = r.result
  const skipped = rep.skipped ?? []
  const invalid = rep.invalid ?? []
  return (
    <div className="actionresult" role="status">
      <div className="row">
        <b>{r.dry_run ? '预演结果' : '导入结果'}</b>
        <span className="dim">共 {r.count} 条</span>
        <span className="badge ready">新增 {rep.added.length}</span>
        <span className="badge intl">更新 {rep.updated.length}</span>
        {skipped.length > 0 && <span className="badge cooling">跳过 {skipped.length}</span>}
        {invalid.length > 0 && <span className="badge restricted">不合格 {invalid.length}</span>}
      </div>
      {skipped.length > 0 && (
        <details>
          <summary className="dim">跳过的账号</summary>
          <div className="mono small">
            {skipped.slice(0, 50).map((s, i) => (
              <div key={s.uid + i}>
                {s.uid || '?'} · {s.reason}
              </div>
            ))}
          </div>
        </details>
      )}
      {invalid.length > 0 && (
        <details>
          <summary className="dim">不合格的行</summary>
          <div className="mono small">
            {invalid.slice(0, 50).map((s, i) => (
              <div key={i}>
                #{s.index} · {s.reason}
              </div>
            ))}
          </div>
        </details>
      )}
      {rep.added.length > 0 && (
        <details>
          <summary className="dim">新增的账号</summary>
          <div className="mono small">{rep.added.map((u) => u.slice(0, 12)).join('、')}</div>
        </details>
      )}
    </div>
  )
}
