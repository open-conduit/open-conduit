import { useEffect, useId, useRef, useState, type ReactNode } from 'react'
import { useStore, dismissToast } from '../data/store'

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="kbd">{children}</kbd>
}

/** Popover menu with roving focus. Items are buttons. */
export function Menu({ label, trigger, children, align = 'end', className }: { label: string; trigger: ReactNode; children: (close: () => void) => ReactNode; align?: 'start' | 'end'; className?: string }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  const btn = useRef<HTMLButtonElement>(null)
  const id = useId()
  useEffect(() => {
    if (!open) return
    const first = ref.current?.querySelector<HTMLButtonElement>('[role=menuitem]:not([aria-disabled=true])')
    first?.focus()
    const onDoc = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node) && !btn.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])
  const close = () => {
    setOpen(false)
    btn.current?.focus()
  }
  const onKey = (e: React.KeyboardEvent) => {
    const items = [...(ref.current?.querySelectorAll<HTMLButtonElement>('[role=menuitem]') ?? [])]
    const i = items.indexOf(document.activeElement as HTMLButtonElement)
    if (e.key === 'Escape') {
      e.stopPropagation()
      close()
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      items[(i + 1) % items.length]?.focus()
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      items[(i - 1 + items.length) % items.length]?.focus()
    } else if (e.key === 'Tab') setOpen(false)
  }
  return (
    <div className={'menu-wrap ' + (className ?? '')}>
      <button ref={btn} type="button" className="btn btn-quiet menu-trigger" aria-haspopup="menu" aria-expanded={open} aria-controls={id} aria-label={label} onClick={(e) => (e.stopPropagation(), setOpen((o) => !o))}>
        {trigger}
      </button>
      {open && (
        <div ref={ref} id={id} role="menu" className={'menu menu-' + align} onKeyDown={onKey} onClick={(e) => e.stopPropagation()}>
          {children(close)}
        </div>
      )}
    </div>
  )
}

export function MenuItem({ children, onSelect, disabled, why, destructive, hint }: { children: ReactNode; onSelect: () => void; disabled?: boolean; why?: string; destructive?: boolean; hint?: ReactNode }) {
  return (
    <button type="button" role="menuitem" aria-disabled={disabled || undefined} className={'menu-item' + (destructive ? ' is-destructive' : '')} onClick={() => !disabled && onSelect()} title={disabled ? why : undefined}>
      <span className="menu-label">{children}</span>
      {hint && <span className="menu-hint">{hint}</span>}
      {disabled && why && <span className="menu-why">{why}</span>}
    </button>
  )
}
export const MenuRule = () => <div className="menu-rule" role="separator" />

/** Modal built on <dialog>: focus is trapped and Escape closes natively. */
export function Dialog({ open, onClose, title, children, wide, tone }: { open: boolean; onClose: () => void; title: string; children: ReactNode; wide?: boolean; tone?: 'danger' | 'signal' }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (open && !d.open) d.showModal()
    if (!open && d.open) d.close()
  }, [open])
  return (
    <dialog ref={ref} className={'dialog' + (wide ? ' is-wide' : '') + (tone ? ' tone-' + tone : '')} onClose={onClose} onCancel={onClose} aria-labelledby={title ? 'dlg-title' : undefined} onClick={(e) => e.target === ref.current && onClose()}>
      {open && (
        <div className="dialog-body">
          <header className="dialog-head">
            <h2 id="dlg-title">{title}</h2>
            <button type="button" className="btn btn-quiet btn-icon" onClick={onClose} aria-label="Close">
              ✕
            </button>
          </header>
          {children}
        </div>
      )}
    </dialog>
  )
}

export function Toasts() {
  const toasts = useStore((s) => s.toasts)
  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={'toast tone-' + t.tone}>
          <div className="toast-text">{t.text}</div>
          {t.receipt && (
            <div className="toast-receipt">
              <span className="sim-tag">Simulated</span>
              <code>
                {t.receipt.method} {t.receipt.path.replace(/work%2F([0-9a-f]{8})[0-9a-f]{56}/g, 'work%2F$1…')}
              </code>
            </div>
          )}
          <button type="button" className="toast-x" onClick={() => dismissToast(t.id)} aria-label="Dismiss">
            ✕
          </button>
        </div>
      ))}
    </div>
  )
}

export function Segmented<T extends string>({ value, options, onChange, label }: { value: T; options: { value: T; label: ReactNode; count?: number }[]; onChange: (v: T) => void; label: string }) {
  return (
    <div className="segmented" role="radiogroup" aria-label={label}>
      {options.map((o) => (
        <button key={o.value} type="button" role="radio" aria-checked={value === o.value} className={value === o.value ? 'is-on' : ''} onClick={() => onChange(o.value)}>
          {o.label}
          {o.count !== undefined && <span className="seg-count">{o.count}</span>}
        </button>
      ))}
    </div>
  )
}

export function Spinner({ label = 'Loading' }: { label?: string }) {
  return <span className="spinner" role="progressbar" aria-label={label} />
}

export function Copy({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      type="button"
      className="btn btn-quiet btn-xs"
      onClick={() => {
        void navigator.clipboard?.writeText(text)
        setDone(true)
        setTimeout(() => setDone(false), 1400)
      }}
    >
      {done ? 'Copied' : label}
    </button>
  )
}

/** Renders `code` spans inside short strings such as titles. */
export function Inline({ text }: { text: string }) {
  return (
    <>
      {text.split(/(`[^`]+`)/).map((p, i) => (p.length > 2 && p.startsWith('`') && p.endsWith('`') ? <code key={i}>{p.slice(1, -1)}</code> : p))}
    </>
  )
}
