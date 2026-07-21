import { forwardRef, type LabelHTMLAttributes } from 'react'
import { cn } from './utils'

export const Label = forwardRef<HTMLLabelElement, LabelHTMLAttributes<HTMLLabelElement>>(({ className, ...props }, ref) => (
  <label ref={ref} className={cn('label', className)} {...props} />
))
Label.displayName = 'Label'
