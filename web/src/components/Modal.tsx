import { useState } from 'react'
import { Button, Input, Modal as AntModal, Switch } from 'antd'

/**
 * Shared modal shell. Pages own their action buttons (they render them in the
 * body), so this keeps the footer empty and only provides the antd chrome,
 * Escape/close behaviour and a responsive width.
 */
export function Modal(props: { title: string; children: React.ReactNode; onCancel: () => void; wide?: boolean }) {
  return (
    <AntModal
      open
      title={props.title}
      onCancel={props.onCancel}
      footer={null}
      width={props.wide ? 720 : 480}
      destroyOnHidden
      maskClosable
    >
      {props.children}
    </AntModal>
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
    <AntModal
      open
      title={props.title}
      onCancel={props.onCancel}
      onOk={() => props.onOk(v)}
      okText="确定"
      cancelText="取消"
      destroyOnHidden
    >
      <div style={{ marginBottom: 6 }}>
        <span className="dim">{props.label}</span>
      </div>
      <Input
        autoFocus
        type={props.type ?? 'text'}
        value={v}
        onChange={(e) => setV(e.target.value)}
        onPressEnter={() => props.onOk(v)}
      />
    </AntModal>
  )
}

export function Confirm(props: { title: string; text: string; onOk: () => void; onCancel: () => void; danger?: boolean }) {
  return (
    <AntModal
      open
      title={props.title}
      onCancel={props.onCancel}
      onOk={props.onOk}
      okText="确认"
      cancelText="取消"
      okButtonProps={{ danger: props.danger !== false }}
      destroyOnHidden
    >
      <span className="dim">{props.text}</span>
    </AntModal>
  )
}

/** Copies text to the clipboard and shows a short "已复制". */
export function CopyButton({ text, label = '复制' }: { text: string | (() => Promise<string>); label?: string }) {
  const [state, setState] = useState<'idle' | 'ok' | 'err'>('idle')
  return (
    <Button
      size="small"
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
    </Button>
  )
}

/** A labelled on/off switch. */
export function Toggle(props: { checked: boolean; onChange: (v: boolean) => void; label: string; hint?: string; disabled?: boolean }) {
  return (
    <div className="toggle-row">
      <Switch size="small" checked={props.checked} disabled={props.disabled} onChange={props.onChange} />
      <div className="toggle-text">
        <b>{props.label}</b>
        {props.hint && <span className="dim hint">{props.hint}</span>}
      </div>
    </div>
  )
}
