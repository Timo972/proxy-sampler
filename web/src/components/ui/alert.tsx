import type { HTMLAttributes } from 'react'
import { cn } from './utils'

export function Alert({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div role="alert" className={cn('alert', className)} {...props} />
}
