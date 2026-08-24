import { useEffect, useRef, useState, type KeyboardEvent } from 'react'
import { Pencil } from 'lucide-react'

import { Input } from './ui/input'

// maxNameLength mirrors the API's own limit, so an over-long name is stopped
// here rather than bouncing back as a 400. The API counts Unicode code points,
// so this has to as well: the HTML maxLength attribute counts UTF-16 code
// units, which would cut a name of non-BMP characters (emoji, say) off at half
// the length the API actually accepts.
const maxNameLength = 100

function clampToCodePoints(value: string, limit: number): string {
  const codePoints = Array.from(value)
  return codePoints.length <= limit ? value : codePoints.slice(0, limit).join('')
}

interface EditableTitleProps {
  value: string
  // label names the thing being renamed ("session name", "run name"). It labels
  // the input and builds the trigger's accessible name.
  label: string
  onSave: (name: string) => Promise<unknown>
  pending?: boolean
}

export function EditableTitle({ value, label, onSave, pending }: EditableTitleProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(value)
  const input = useRef<HTMLInputElement>(null)
  // saving guards the blur handler: committing on Enter moves focus off the
  // input, which would otherwise fire blur and save a second time.
  const saving = useRef(false)

  useEffect(() => {
    if (editing) {
      input.current?.focus()
      input.current?.select()
    }
  }, [editing])

  const startEditing = () => {
    setDraft(value)
    setEditing(true)
  }

  const commit = async () => {
    if (saving.current) return
    const name = draft.trim()
    // An unchanged or empty name has nothing to save; drop back to the heading
    // showing the name the server still holds.
    if (name === '' || name === value) {
      setEditing(false)
      return
    }
    saving.current = true
    try {
      await onSave(name)
      setEditing(false)
    } catch {
      // Leave the field open with the operator's text so they can retry or
      // correct it; the page surfaces the error alongside.
    } finally {
      saving.current = false
    }
  }

  const keyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') {
      event.preventDefault()
      void commit()
      return
    }
    if (event.key === 'Escape') {
      event.preventDefault()
      setEditing(false)
    }
  }

  if (!editing) {
    return (
      <h1 className="editable-title">
        <button type="button" className="editable-title-trigger" aria-label={`Rename ${label}`} onClick={startEditing}>
          <span>{value}</span>
          <Pencil className="editable-title-icon" size={16} aria-hidden="true" />
        </button>
      </h1>
    )
  }

  return (
    <h1 className="editable-title">
      <Input
        ref={input}
        className="editable-title-input"
        aria-label={label}
        value={draft}
        disabled={pending}
        onChange={(event) => setDraft(clampToCodePoints(event.target.value, maxNameLength))}
        onKeyDown={keyDown}
        onBlur={() => void commit()}
      />
    </h1>
  )
}
