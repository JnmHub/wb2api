import { useEffect, useState } from 'react'

export function Modal(props: { title: string; children: React.ReactNode; onCancel: () => void; wide?: boolean }) {
  useEffect(() => {
    const on = (e: KeyboardEvent) => e.key === 'Escape' && props.onCancel()
    window.addEventListener('keydown', on)
    return () => window.removeEventListener('keydown', on)
  })
  return (
    <>
      <div className="scrim" onClick={props.onCancel} />
      <div className={'card modal' + (props.wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label={props.title}>
        <b>{props.title}</b>
        {props.children}
      </div>
    </>
  )
}

export function PromptDialog(props: {
  title: string
  label: string
  initial: string
  type?: string
  onOk: (v: string) => void
  onCancel: () => void
}) {
  const [v, setV] = useState(props.initial)
  return (
    <Modal title={props.title} onCancel={props.onCancel}>
      <div className="field">
        <label>{props.label}</label>
        <input
          className="input"
          autoFocus
          type={props.type ?? 'text'}
          value={v}
          onChange={(e) => setV(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && props.onOk(v)}
        />
      </div>
      <div className="row end">
        <button className="btn" onClick={props.onCancel}>
          取消
        </button>
        <button className="btn primary" onClick={() => props.onOk(v)}>
          确定
        </button>
      </div>
    </Modal>
  )
}

export function Confirm(props: { title: string; text: string; onOk: () => void; onCancel: () => void; danger?: boolean }) {
  return (
    <Modal title={props.title} onCancel={props.onCancel}>
      <span className="dim">{props.text}</span>
      <div className="row end">
        <button className="btn" onClick={props.onCancel}>
          取消
        </button>
        <button className={'btn ' + (props.danger === false ? 'primary' : 'danger')} onClick={props.onOk}>
          确认
        </button>
      </div>
    </Modal>
  )
}

/** Copies text to the clipboard and shows a short "已复制". */
export function CopyButton({ text, label = '复制' }: { text: string | (() => Promise<string>); label?: string }) {
  const [state, setState] = useState<'idle' | 'ok' | 'err'>('idle')
  return (
    <button
      className="btn sm"
      onClick={async () => {
        try {
          const value = typeof text === 'function' ? await text() : text
          await navigator.clipboard.writeText(value)
          setState('ok')
        } catch {
          setState('err')
        }
        setTimeout(() => setState('idle'), 1500)
      }}
    >
      {state === 'ok' ? '已复制' : state === 'err' ? '复制失败' : label}
    </button>
  )
}

/** A labelled on/off switch. */
export function Toggle(props: { checked: boolean; onChange: (v: boolean) => void; label: string; hint?: string; disabled?: boolean }) {
  return (
    <label className="toggle">
      <input type="checkbox" checked={props.checked} disabled={props.disabled} onChange={(e) => props.onChange(e.target.checked)} />
      <span>
        {props.label}
        {props.hint && <span className="dim hint">{props.hint}</span>}
      </span>
    </label>
  )
}
