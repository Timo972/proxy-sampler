import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { EditableTitle } from './editable-title'

describe('EditableTitle', () => {
  it('renders the current name as a heading until it is activated', () => {
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={vi.fn()} />)

    expect(screen.getByRole('heading', { level: 1, name: 'Frankfurt sticky' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
  })

  it('saves the edited name on Enter and returns to the heading', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined)
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, 'Frankfurt residential{Enter}')

    expect(onSave).toHaveBeenCalledExactlyOnceWith('Frankfurt residential')
    expect(await screen.findByRole('heading', { level: 1 })).toBeInTheDocument()
  })

  it('trims surrounding whitespace before saving', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined)
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, '  Padded name  {Enter}')

    expect(onSave).toHaveBeenCalledExactlyOnceWith('Padded name')
  })

  it('discards the edit on Escape without saving', async () => {
    const onSave = vi.fn()
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, 'Discarded{Escape}')

    expect(onSave).not.toHaveBeenCalled()
    expect(screen.getByRole('heading', { level: 1, name: 'Frankfurt sticky' })).toBeInTheDocument()
  })

  it('saves when the input loses focus', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined)
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, 'Blurred name')
    await user.tab()

    expect(onSave).toHaveBeenCalledExactlyOnceWith('Blurred name')
  })

  it('does not call onSave when the name is unchanged', async () => {
    const onSave = vi.fn()
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    await user.type(screen.getByRole('textbox', { name: 'session name' }), '{Enter}')

    expect(onSave).not.toHaveBeenCalled()
    expect(screen.getByRole('heading', { level: 1, name: 'Frankfurt sticky' })).toBeInTheDocument()
  })

  // An empty name is rejected by the API, so the component restores the previous
  // name rather than sending a request that is bound to fail.
  it('restores the previous name when the input is emptied', async () => {
    const onSave = vi.fn()
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, '   {Enter}')

    expect(onSave).not.toHaveBeenCalled()
    expect(screen.getByRole('heading', { level: 1, name: 'Frankfurt sticky' })).toBeInTheDocument()
  })

  it('caps the input at the 100-character limit the API enforces', async () => {
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={vi.fn()} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))

    expect(screen.getByRole('textbox', { name: 'session name' })).toHaveAttribute('maxLength', '100')
  })

  it('keeps the input open and disabled while the save is in flight', async () => {
    const onSave = vi.fn().mockReturnValue(new Promise(() => undefined))
    const { rerender } = render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, 'Saving name{Enter}')
    // The parent flips to pending once the mutation it owns starts.
    rerender(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} pending />)

    expect(screen.getByRole('textbox', { name: 'session name' })).toBeDisabled()
    expect(screen.getByRole('textbox', { name: 'session name' })).toHaveValue('Saving name')
  })

  // A failed save must not silently drop the operator's text: the input stays
  // open with what they typed so they can retry or correct it.
  it('keeps the edit open when the save fails', async () => {
    const onSave = vi.fn().mockRejectedValue(new Error('nope'))
    render(<EditableTitle value="Frankfurt sticky" label="session name" onSave={onSave} />)
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Rename session name' }))
    const input = screen.getByRole('textbox', { name: 'session name' })
    await user.clear(input)
    await user.type(input, 'Failed name{Enter}')

    expect(await screen.findByRole('textbox', { name: 'session name' })).toHaveValue('Failed name')
  })
})
