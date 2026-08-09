import { zodResolver } from '@hookform/resolvers/zod'
import { ChevronDown, LoaderCircle, Plus, Trash2 } from 'lucide-react'
import { cloneElement, type ReactElement, useState } from 'react'
import { Controller, useFieldArray, useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'

import { APIError, type AxisSpec, type CreateRunRequest, useCreateRun, useRunConfig } from '../lib/api'
import { variantCount } from '../lib/variation'
import { Alert } from './ui/alert'
import { Button } from './ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from './ui/collapsible'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from './ui/dialog'
import { Input } from './ui/input'
import { Label } from './ui/label'
import { Select } from './ui/select'

export const MAX_VARIANTS = 128

const positiveInteger = /^[1-9]\d*$/
const nonNegativeInteger = /^\d+$/

const axisSchema = z
  .object({
    name: z.string().trim().min(1, 'Axis name is required'),
    kind: z.enum(['list', 'range', 'random']),
    values: z.string(),
    from: z.string(),
    to: z.string(),
    count: z.string(),
    length: z.string(),
  })
  .superRefine((axis, ctx) => {
    if (axis.kind === 'list') {
      const values = parseListValues(axis.values)
      if (values.length < 1) {
        ctx.addIssue({ code: 'custom', path: ['values'], message: 'Add at least one value' })
      }
    } else if (axis.kind === 'range') {
      if (!nonNegativeInteger.test(axis.from)) ctx.addIssue({ code: 'custom', path: ['from'], message: 'Enter a whole number' })
      if (!nonNegativeInteger.test(axis.to)) ctx.addIssue({ code: 'custom', path: ['to'], message: 'Enter a whole number' })
      if (nonNegativeInteger.test(axis.from) && nonNegativeInteger.test(axis.to) && Number(axis.to) < Number(axis.from)) {
        ctx.addIssue({ code: 'custom', path: ['to'], message: '"To" must be greater than or equal to "from"' })
      }
    } else {
      if (!positiveInteger.test(axis.count)) ctx.addIssue({ code: 'custom', path: ['count'], message: 'Count must be a positive integer' })
      if (axis.length !== '' && !positiveInteger.test(axis.length)) ctx.addIssue({ code: 'custom', path: ['length'], message: 'Length must be a positive integer' })
    }
  })

const formSchema = z.object({
  name: z.string().trim().min(1, 'Name is required').max(100, 'Name must be 100 characters or fewer'),
  template: z.string().min(1, 'Proxy template is required'),
  mode: z.string().refine((value) => value === 'sticky' || value === 'pool', 'Choose a sampling mode'),
  cadence: z.string().regex(positiveInteger, 'Cadence must be a positive integer'),
  axes: z
    .array(axisSchema)
    .min(1, 'Add at least one axis')
    .superRefine((axes, ctx) => {
      // Axis names become object keys, so duplicates would silently overwrite
      // one another and drop axes from the run. Reject them, flagging the
      // duplicate row.
      const seen = new Set<string>()
      axes.forEach((axis, index) => {
        const name = axis.name.trim()
        if (name === '') return
        if (seen.has(name)) {
          ctx.addIssue({ code: 'custom', message: 'Axis names must be unique', path: [index, 'name'] })
        } else {
          seen.add(name)
        }
      })
    }),
  probes: z.string().refine((value) => value === '' || positiveInteger.test(value), 'Enter a positive integer or leave blank').refine((value) => value === '' || Number(value) <= 255, 'Probes cannot exceed 255'),
  probeTarget: z.string().refine((value) => value === '' || isHTTPURL(value), 'Enter an HTTP or HTTPS URL'),
  dialTimeout: z.string().refine((value) => value === '' || positiveInteger.test(value), 'Enter a positive integer or leave blank').refine((value) => value === '' || Number(value) >= 100, 'Timeout must be at least 100 ms'),
  maxSamples: z.string().refine((value) => value === '' || positiveInteger.test(value), 'Enter a positive integer or leave blank'),
  maxDuration: z.string().refine((value) => value === '' || positiveInteger.test(value), 'Enter a positive integer or leave blank'),
})

type FormValues = z.input<typeof formSchema>
type ParsedFormValues = z.output<typeof formSchema>
type AxisRowValues = FormValues['axes'][number]

const defaultAxisRow: AxisRowValues = { name: '', kind: 'list', values: '', from: '', to: '', count: '', length: '' }

const defaults: FormValues = {
  name: '',
  template: '',
  mode: '',
  cadence: '30',
  axes: [],
  probes: '',
  probeTarget: '',
  dialTimeout: '',
  maxSamples: '',
  maxDuration: '',
}

export function NewRunDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const create = useCreateRun()
  const config = useRunConfig()
  const maxVariants = config.data?.max_variants_per_run ?? MAX_VARIANTS
  const [advanced, setAdvanced] = useState(false)
  const [serverError, setServerError] = useState<string | null>(null)
  const form = useForm<FormValues, unknown, ParsedFormValues>({ resolver: zodResolver(formSchema), defaultValues: defaults })
  const axesFieldArray = useFieldArray({ control: form.control, name: 'axes' })
  const watchedAxes = useWatch({ control: form.control, name: 'axes' }) ?? []

  const builtAxes = buildAxesRecord(watchedAxes)
  const count = variantCount(builtAxes)
  const overCap = count > maxVariants

  const changeOpen = (next: boolean) => {
    if (create.isPending) return
    if (!next) {
      form.reset(defaults)
      setAdvanced(false)
      setServerError(null)
      create.reset()
      onClose()
    }
  }

  const submit = form.handleSubmit(async (values) => {
    if (overCap) return
    setServerError(null)
    const request: CreateRunRequest = {
      name: values.name.trim(),
      template: values.template,
      axes: buildAxesRecord(values.axes),
      mode: values.mode as 'sticky' | 'pool',
      cadence_seconds: Number(values.cadence),
      ...(values.probes ? { probes_per_sample: Number(values.probes) } : {}),
      ...(values.probeTarget ? { probe_target: values.probeTarget } : {}),
      ...(values.dialTimeout ? { dial_timeout_ms: Number(values.dialTimeout) } : {}),
      ...(values.maxSamples ? { max_samples: Number(values.maxSamples) } : {}),
      ...(values.maxDuration ? { max_duration_seconds: Number(values.maxDuration) } : {}),
    }
    try {
      await create.mutateAsync(request)
      changeOpen(false)
    } catch (error) {
      setServerError(error instanceof APIError ? error.message : 'The run could not be started. Try again.')
    } finally {
      create.reset()
    }
  })

  return (
    <Dialog open={open} onOpenChange={changeOpen}>
      <DialogContent
        aria-describedby="new-run-description"
        closeDisabled={create.isPending}
        onEscapeKeyDown={(event) => { if (create.isPending) event.preventDefault() }}
        onPointerDownOutside={(event) => { if (create.isPending) event.preventDefault() }}
      >
        <div className="dialog-heading">
          <DialogTitle>New parameter-variation run</DialogTitle>
          <DialogDescription id="new-run-description">Expand a proxy template across one or more axes and sample every resulting variant.</DialogDescription>
        </div>
        <form onSubmit={submit} noValidate>
          {serverError && <Alert className="form-alert">{serverError}</Alert>}
          <fieldset disabled={create.isPending} className="form-fields">
            <Field label="Run name" error={form.formState.errors.name?.message}>
              <Input autoFocus id="run-name" placeholder="Experiment name" {...form.register('name')} />
            </Field>
            <Field label="Proxy template" error={form.formState.errors.template?.message} hint="Use {axis_name} placeholders, e.g. socks5h://u-cc-{country}:pw@gate:1080">
              <Input id="proxy-template" spellCheck={false} placeholder="socks5h://u-cc-{country}:pw@gate:1080" {...form.register('template')} />
            </Field>
            <div className="form-grid">
              <Controller
                control={form.control}
                name="mode"
                render={({ field, fieldState }) => (
                  <Field label="Sampling mode" error={fieldState.error?.message}>
                    <Select id="sampling-mode" {...field}>
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
            </div>

            <div className="axes-builder">
              <div className="axes-builder-heading">
                <span className="label">Axes</span>
                <Button type="button" variant="secondary" size="small" onClick={() => axesFieldArray.append({ ...defaultAxisRow })}>
                  <Plus aria-hidden="true" size={14} /> Add axis
                </Button>
              </div>
              {form.formState.errors.axes?.root?.message && <p className="field-error" role="alert">{form.formState.errors.axes.root.message}</p>}
              {axesFieldArray.fields.length === 0 && <p className="field-hint">No axes yet. Add at least one to expand this run into variants.</p>}
              {axesFieldArray.fields.map((axisField, index) => (
                <AxisRow
                  key={axisField.id}
                  index={index}
                  form={form}
                  kind={watchedAxes[index]?.kind ?? 'list'}
                  onRemove={() => axesFieldArray.remove(index)}
                />
              ))}
              <p className={overCap ? 'field-error' : 'field-hint'} role={overCap ? 'alert' : undefined}>
                {count} variants{overCap ? ` — exceeds the maximum of ${maxVariants}. Remove or narrow an axis.` : ''}
              </p>
            </div>

            <Collapsible open={advanced} onOpenChange={setAdvanced} className="advanced-settings">
              <CollapsibleTrigger asChild>
                <Button type="button" variant="quiet" className="advanced-trigger" aria-expanded={advanced}>
                  Advanced settings <ChevronDown aria-hidden="true" className={advanced ? 'rotate' : undefined} size={16} />
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent className="advanced-content">
                <div className="form-grid">
                  <Field label="Probes per sample (optional)" error={form.formState.errors.probes?.message}>
                    <Input id="probes" type="number" min="1" max="255" inputMode="numeric" {...form.register('probes')} />
                  </Field>
                  <Field label="Dial timeout (milliseconds, optional)" error={form.formState.errors.dialTimeout?.message}>
                    <Input id="dial-timeout" type="number" min="100" inputMode="numeric" {...form.register('dialTimeout')} />
                  </Field>
                </div>
                <Field label="Probe target URL (optional)" error={form.formState.errors.probeTarget?.message}>
                  <Input id="probe-target" type="url" spellCheck={false} {...form.register('probeTarget')} />
                </Field>
                <div className="form-grid">
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
            <Button type="submit" variant="primary" disabled={create.isPending || overCap}>
              {create.isPending ? <><LoaderCircle className="spin" aria-hidden="true" size={16} /> Starting run…</> : 'Start run'}
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function AxisRow({
  index,
  form,
  kind,
  onRemove,
}: {
  index: number
  form: ReturnType<typeof useForm<FormValues, unknown, ParsedFormValues>>
  kind: AxisRowValues['kind']
  onRemove: () => void
}) {
  const n = index + 1
  const axisErrors = form.formState.errors.axes?.[index]
  return (
    <div className="axis-row">
      <div className="axis-row-fields">
        <Field label={`Axis ${n} name`} error={axisErrors?.name?.message}>
          <Input id={`axis-${n}-name`} placeholder="country" {...form.register(`axes.${index}.name` as const)} />
        </Field>
        <Field label={`Axis ${n} kind`}>
          <Select id={`axis-${n}-kind`} {...form.register(`axes.${index}.kind` as const)}>
            <option value="list">List</option>
            <option value="range">Range</option>
            <option value="random">Random</option>
          </Select>
        </Field>
        {kind === 'list' && (
          <Field label={`Axis ${n} values`} error={axisErrors?.values?.message} hint="Comma-separated">
            <Input id={`axis-${n}-values`} placeholder="de, us, fr, jp" {...form.register(`axes.${index}.values` as const)} />
          </Field>
        )}
        {kind === 'range' && (
          <div className="form-grid">
            <Field label={`Axis ${n} from`} error={axisErrors?.from?.message}>
              <Input id={`axis-${n}-from`} type="number" inputMode="numeric" {...form.register(`axes.${index}.from` as const)} />
            </Field>
            <Field label={`Axis ${n} to`} error={axisErrors?.to?.message}>
              <Input id={`axis-${n}-to`} type="number" inputMode="numeric" {...form.register(`axes.${index}.to` as const)} />
            </Field>
          </div>
        )}
        {kind === 'random' && (
          <div className="form-grid">
            <Field label={`Axis ${n} count`} error={axisErrors?.count?.message}>
              <Input id={`axis-${n}-count`} type="number" min="1" inputMode="numeric" {...form.register(`axes.${index}.count` as const)} />
            </Field>
            <Field label={`Axis ${n} length (optional)`} error={axisErrors?.length?.message}>
              <Input id={`axis-${n}-length`} type="number" min="1" inputMode="numeric" {...form.register(`axes.${index}.length` as const)} />
            </Field>
          </div>
        )}
      </div>
      <Button type="button" variant="quiet" size="small" onClick={onRemove}>
        <Trash2 aria-hidden="true" size={14} /> Remove axis
      </Button>
    </div>
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

function parseListValues(values: string): string[] {
  return values.split(',').map((v) => v.trim()).filter(Boolean)
}

function buildAxesRecord(rows: readonly Partial<AxisRowValues>[]): Record<string, AxisSpec> {
  const axes: Record<string, AxisSpec> = {}
  for (const row of rows) {
    const name = row?.name?.trim()
    if (!name) continue
    if (row?.kind === 'range') {
      const from = Number(row.from)
      const to = Number(row.to)
      if (row.from === '' || row.to === '' || Number.isNaN(from) || Number.isNaN(to)) continue
      axes[name] = { kind: 'range', from, to }
    } else if (row?.kind === 'random') {
      const count = Number(row.count)
      if (row.count === '' || Number.isNaN(count)) continue
      const length = row.length ? Number(row.length) : undefined
      axes[name] = { kind: 'random', count, ...(length !== undefined && !Number.isNaN(length) ? { length } : {}) }
    } else {
      axes[name] = { kind: 'list', values: parseListValues(row?.values ?? '') }
    }
  }
  return axes
}

function isHTTPURL(value: string) {
  try {
    const parsed = new URL(value)
    return parsed.protocol === 'http:' || parsed.protocol === 'https:'
  } catch {
    return false
  }
}
