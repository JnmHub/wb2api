import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Collapse, Input, Select, Space, Tag, Typography, Upload } from 'antd'
import { Upload as UploadIcon, X } from 'lucide-react'
import { post, type ImportReport, type ImportResponse } from '../api'
import { Modal, Toggle } from '../components/Modal'

type Picked = { name: string; text: string }

/** Import accounts from the gateway's own export, a bare list, a single account
 *  object or a desktop credential. A dry run shows the same report the real
 *  import would produce, without touching a single account. Multiple files are
 *  imported one after another and their reports are merged. */
export function ImportModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [text, setText] = useState('')
  const [realm, setRealm] = useState('')
  const [overwrite, setOverwrite] = useState(false)
  const [picked, setPicked] = useState<Picked[]>([])
  const docs = () => (picked.length ? picked.map((p) => p.text) : text.trim() ? [text] : [])
  const canSubmit = picked.length > 0 || text.trim().length > 0
  const run = useMutation({
    mutationFn: async (dry: boolean) => {
      const agg: ImportResponse = { count: 0, dry_run: dry, result: { added: [], updated: [], skipped: [], invalid: [] } }
      for (const document of docs()) {
        const r = await post<ImportResponse>('/api/v2/accounts/import', { document, realm, overwrite, dry_run: dry })
        mergeReport(agg.result, r.result)
        agg.count += r.count
      }
      return agg
    },
    onSuccess: (r, dry) => {
      if (!dry && (r.result.added.length || r.result.updated.length)) {
        qc.invalidateQueries({ queryKey: ['accounts'] })
        qc.invalidateQueries({ queryKey: ['accounts-summary'] })
      }
    },
  })
  const read = async (f: File | undefined) => {
    if (!f) return
    const content = await f.text()
    setPicked((cur) => [...cur.filter((p) => p.name !== f.name), { name: f.name, text: content }])
  }
  return (
    <Modal title="导入账号" onCancel={onClose} wide>
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Typography.Paragraph type="secondary" style={{ margin: 0 }}>
          接受本网关导出的 JSON、账号数组、单个账号对象或桌面端凭据文件。先点“预演”可以看清会新增、更新、跳过哪些账号，以及哪些行不合格。
        </Typography.Paragraph>
        <Space wrap>
          <div>
            <Typography.Text type="secondary">区域（跟随文档时按 token / domain 判断）</Typography.Text>
            <Select
              style={{ width: 180 }}
              value={realm}
              onChange={setRealm}
              options={[
                { value: '', label: '跟随文档' },
                { value: 'intl', label: '国际版' },
                { value: 'cn', label: '国内版' },
              ]}
            />
          </div>
          <div>
            <Typography.Text type="secondary">从文件读取</Typography.Text>
            <div>
              <Upload
                accept=".json,.txt,application/json,text/plain"
                multiple
                showUploadList={false}
                beforeUpload={(f) => {
                  void read(f as File)
                  return false
                }}
              >
                <Button icon={<UploadIcon size={14} />}>选择文件（可多选）</Button>
              </Upload>
            </div>
          </div>
        </Space>
        {picked.length > 0 && (
          <Space wrap size={[6, 6]}>
            {picked.map((p) => (
              <Tag key={p.name} closable onClose={() => setPicked((cur) => cur.filter((x) => x.name !== p.name))} closeIcon={<X size={11} />}>
                {p.name}
              </Tag>
            ))}
            <Button size="small" type="link" onClick={() => setPicked([])}>
              清空文件
            </Button>
            <Typography.Text type="secondary">将按顺序导入 {picked.length} 个文件</Typography.Text>
          </Space>
        )}
        <div>
          <Typography.Text type="secondary">{picked.length ? '或继续粘贴 JSON（选择文件时忽略）' : '或直接粘贴 JSON'}</Typography.Text>
          <Input.TextArea
            className="mono"
            rows={picked.length ? 4 : 10}
            value={text}
            onChange={(e) => setText(e.target.value)}
            spellCheck={false}
            placeholder={'{\n  "accounts": [ { "uid": "…", "accessToken": "…", "realm": "intl" } ]\n}'}
          />
        </div>
        <Toggle
          label="覆盖同 UID 的已有账号"
          hint="覆盖时保留原有的添加时间、积分、签到记录和代理槽绑定。"
          checked={overwrite}
          onChange={setOverwrite}
        />
        {run.isError && <Alert type="error" showIcon message={String(run.error)} />}
        {run.data && <Report r={run.data} />}
        <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>关闭</Button>
          <Button disabled={!canSubmit || run.isPending} onClick={() => run.mutate(true)}>
            预演
          </Button>
          <Button type="primary" disabled={!canSubmit} loading={run.isPending} onClick={() => run.mutate(false)}>
            导入
          </Button>
        </Space>
      </Space>
    </Modal>
  )
}

function mergeReport(dst: ImportReport, src: ImportReport) {
  dst.added.push(...(src.added ?? []))
  dst.updated.push(...(src.updated ?? []))
  dst.skipped.push(...(src.skipped ?? []))
  dst.invalid.push(...(src.invalid ?? []))
}

function Report({ r }: { r: ImportResponse }) {
  const rep = r.result
  const skipped = rep.skipped ?? []
  const invalid = rep.invalid ?? []
  return (
    <Card size="small" title={r.dry_run ? '预演结果' : '导入结果'} extra={<Typography.Text type="secondary">共 {r.count} 条</Typography.Text>}>
      <Space wrap size={[6, 6]}>
        <Tag color="success">新增 {rep.added.length}</Tag>
        <Tag color="processing">更新 {rep.updated.length}</Tag>
        {skipped.length > 0 && <Tag color="warning">跳过 {skipped.length}</Tag>}
        {invalid.length > 0 && <Tag color="error">不合格 {invalid.length}</Tag>}
      </Space>
      {(skipped.length > 0 || invalid.length > 0 || rep.added.length > 0) && (
        <Collapse
          ghost
          style={{ marginTop: 8 }}
          items={[
            ...(skipped.length
              ? [{
                  key: 'skipped',
                  label: `跳过的账号（${skipped.length}）`,
                  children: (
                    <div className="mono small">
                      {skipped.slice(0, 50).map((s, i) => (
                        <div key={s.uid + i}>
                          {s.uid || '?'} · {s.reason}
                        </div>
                      ))}
                    </div>
                  ),
                }]
              : []),
            ...(invalid.length
              ? [{
                  key: 'invalid',
                  label: `不合格的行（${invalid.length}）`,
                  children: (
                    <div className="mono small">
                      {invalid.slice(0, 50).map((s, i) => (
                        <div key={i}>
                          #{s.index} · {s.reason}
                        </div>
                      ))}
                    </div>
                  ),
                }]
              : []),
            ...(rep.added.length
              ? [{
                  key: 'added',
                  label: `新增的账号（${rep.added.length}）`,
                  children: <div className="mono small">{rep.added.map((u) => u.slice(0, 12)).join('、')}</div>,
                }]
              : []),
          ]}
        />
      )}
    </Card>
  )
}
