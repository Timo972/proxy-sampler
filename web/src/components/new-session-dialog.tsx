import { zodResolver } from '@hookform/resolvers/zod'
import { ChevronDown, LoaderCircle } from 'lucide-react'
import { cloneElement, type ReactElement, useRef, useState } from 'react'
import { Controller, useForm } from 'react-hook-form'
import { useNavigate } from 'react-router-dom'
import { z } from 'zod'

import { APIError, type CreateSessionRequest, useCreateSession } from '../lib/api'
import { Alert } from './ui/alert'
import { Button } from './ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from './ui/collapsible'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from './ui/dialog'
import { Input } from './ui/input'
import { Label } from './ui/label'
import { Select } from './ui/select'

const positiveInteger = /^[1-9]\d*$/
const formSchema = z.object({
  name: z.string().trim().min(1, 'Name is required').max(100, 'Name must be 100 characters or fewer'),
  proxy: z.string().min(1, 'Proxy connection string is required'),
  mode: z.string().refine((value) => value === 'sticky' || value === 'pool', 'Choose a sampling mode'),
  cadence: z.string().regex(positiveInteger, 'Cadence must be a positive integer'),
  probes: z.string().regex(positiveInteger, 'Probes must be a positive integer').refine((value) => Number(value) <= 255, 'Probes cannot exceed 255'),
  probeTarget: z.string().refine((value) => value === '' || isHTTPURL(value), 'Enter an HTTP or HTTPS URL'),
  dialTimeout: z.string().regex(positiveInteger, 'Timeout must be a positive integer').refine((value) => Number(value) >= 100, 'Timeout must be at least 100 ms'),
  maxSamples: z.string().refine((value) => value === '' || positiveInteger.test(value), 'Enter a positive integer or leave blank'),
  maxDuration: z.string().refine((value) => value === '' || positiveInteger.test(value), 'Enter a positive integer or leave blank'),
})

type FormValues = z.input<typeof formSchema>
type ParsedFormValues = z.output<typeof formSchema>

const defaults: FormValues = {
  name: '',
  proxy: '',
  mode: '',
  cadence: '30',
  probes: '',
  probeTarget: 'https://speed.cloudflare.com/cdn-cgi/trace',
  dialTimeout: '10000',
  maxSamples: '',
  maxDuration: '',
}

export function NewSessionDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const navigate = useNavigate()
  const create = useCreateSession()
  const manuallyEditedProbes = useRef(false)
  const [advanced, setAdvanced] = useState(false)
  const [serverError, setServerError] = useState<string | null>(null)
  const form = useForm<FormValues, unknown, ParsedFormValues>({ resolver: zodResolver(formSchema), defaultValues: defaults })

  const changeOpen = (next: boolean) => {
    if (create.isPending) return
    if (!next) {
      form.reset(defaults)
      manuallyEditedProbes.current = false
      setAdvanced(false)
      setServerError(null)
      create.reset()
    }
    onOpenChange(next)
  }

  const submit = form.handleSubmit(async (values) => {
    setServerError(null)
    const request: CreateSessionRequest = {
      name: values.name.trim(),
      proxy: values.proxy,
      mode: values.mode as 'sticky' | 'pool',
      cadence_seconds: Number(values.cadence),
      probes_per_sample: Number(values.probes),
      ...(values.probeTarget ? { probe_target: values.probeTarget } : {}),
      ...(values.dialTimeout ? { dial_timeout_ms: Number(values.dialTimeout) } : {}),
      ...(values.maxSamples ? { max_samples: Number(values.maxSamples) } : {}),
      ...(values.maxDuration ? { max_duration_seconds: Number(values.maxDuration) } : {}),
    }
    form.resetField('proxy', { defaultValue: '' })
    try {
      const session = await create.mutateAsync(request)
      form.reset(defaults)
      onOpenChange(false)
      navigate(`/sessions/${session.id}`)
    } catch (error) {
      setServerError(error instanceof APIError ? error.message : 'The session could not be started. Try again.')
    } finally {
      create.reset()
    }
  })

  return (
    <Dialog open={open} onOpenChange={changeOpen}>
      <DialogContent
        aria-describedby="new-session-description"
        closeDisabled={create.isPending}
        onEscapeKeyDown={(event) => { if (create.isPending) event.preventDefault() }}
        onPointerDownOutside={(event) => { if (create.isPending) event.preventDefault() }}
      >
        <div className="dialog-heading">
          <DialogTitle>New sampling session</DialogTitle>
          <DialogDescription id="new-session-description">Start a durable measurement run against one proxy endpoint.</DialogDescription>
        </div>
        <form onSubmit={submit} noValidate>
          {serverError && <Alert className="form-alert">{serverError}</Alert>}
          <fieldset disabled={create.isPending} className="form-fields">
            <Field label="Session name" error={form.formState.errors.name?.message}>
              <Input autoFocus id="session-name" placeholder="Provider or endpoint name" {...form.register('name')} />
            </Field>
            <Field label="Proxy connection string" error={form.formState.errors.proxy?.message} hint="Supports socks5h:// and http:// connection strings.">
              <Input
                id="proxy-connection-string"
                type="password"
                autoComplete="off"
                spellCheck={false}
                placeholder="socks5h://user:password@host:port"
                {...form.register('proxy')}
              />
            </Field>
            <div className="form-grid">
              <Controller
                control={form.control}
                name="mode"
                render={({ field, fieldState }) => (
                  <Field label="Sampling mode" error={fieldState.error?.message}>
                    <Select
                      id="sampling-mode"
                      {...field}
                      onChange={(event) => {
                        field.onChange(event)
                        if (!manuallyEditedProbes.current) {
                          form.setValue('probes', event.target.value === 'pool' ? '8' : event.target.value === 'sticky' ? '3' : '', { shouldValidate: true })
                        }
                      }}
                    >
                      <option value="">Select mode</option>
                      <option value="sticky">Sticky</option>
                      <option value="pool">Pool</option>
                    </Select>
                  </Field>
                )}
              />
              <Field label="Cadence (seconds)" error={form.formState.errors.cadence?.message}>
                <Input id="cadence" type="number" min="1" inputMode="numeric" {...form.register('cadence')} />
              </Field>
              <Field label="Probes per sample" error={form.formState.errors.probes?.message}>
                <Input
                  id="probes"
                  type="number"
                  min="1"
                  max="255"
                  inputMode="numeric"
                  {...form.register('probes', { onChange: () => { manuallyEditedProbes.current = true } })}
                />
              </Field>
            </div>
            <Collapsible open={advanced} onOpenChange={setAdvanced} className="advanced-settings">
              <CollapsibleTrigger asChild>
                <Button type="button" variant="quiet" className="advanced-trigger" aria-expanded={advanced}>
                  Advanced settings <ChevronDown aria-hidden="true" className={advanced ? 'rotate' : undefined} size={16} />
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent className="advanced-content">
                <Field label="Probe target URL" error={form.formState.errors.probeTarget?.message}>
                  <Input id="probe-target" type="url" spellCheck={false} {...form.register('probeTarget')} />
                </Field>
                <div className="form-grid">
                  <Field label="Dial timeout (milliseconds)" error={form.formState.errors.dialTimeout?.message}>
                    <Input id="dial-timeout" type="number" min="100" inputMode="numeric" {...form.register('dialTimeout')} />
                  </Field>
                  <Field label="Maximum samples (optional)" error={form.formState.errors.maxSamples?.message}>
                    <Input id="max-samples" type="number" min="1" inputMode="numeric" {...form.register('maxSamples')} />
                  </Field>
                  <Field label="Maximum duration (seconds, optional)" error={form.formState.errors.maxDuration?.message}>
                    <Input id="max-duration" type="number" min="1" inputMode="numeric" {...form.register('maxDuration')} />
                  </Field>
                </div>
              </CollapsibleContent>
            </Collapsible>
          </fieldset>
          <div className="dialog-actions">
            <Button type="button" variant="quiet" disabled={create.isPending} onClick={() => changeOpen(false)}>Cancel</Button>
            <Button type="submit" variant="primary" disabled={create.isPending}>
              {create.isPending ? <><LoaderCircle className="spin" aria-hidden="true" size={16} /> Starting session…</> : 'Start session'}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function Field({ label, error, hint, children }: { label: string; error?: string; hint?: string; children: ReactElement<{ id?: string }> }) {
  const id = children.props.id
  const hintID = hint && id ? `${id}-hint` : undefined
  const errorID = error && id ? `${id}-error` : undefined
  const control = cloneElement(children, {
    'aria-invalid': error ? true : undefined,
    'aria-describedby': [hintID, errorID].filter(Boolean).join(' ') || undefined,
  } as Record<string, unknown>)
  return (
    <div className="field">
      <Label htmlFor={id}>{label}</Label>
      {control}
      {hint && <p className="field-hint" id={hintID}>{hint}</p>}
      {error && <p className="field-error" id={errorID} role="alert">{error}</p>}
    </div>
  )
}

function isHTTPURL(value: string) {
  try {
    const parsed = new URL(value)
    return parsed.protocol === 'http:' || parsed.protocol === 'https:'
  } catch {
    return false
  }
}
