import { forwardRef, type ButtonHTMLAttributes } from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from './utils'

const buttonVariants = cva('button', {
  variants: {
    variant: { primary: 'button-primary', secondary: 'button-secondary', quiet: 'button-quiet', danger: 'button-danger' },
    size: { default: 'button-default', small: 'button-small', icon: 'button-icon' },
  },
  defaultVariants: { variant: 'secondary', size: 'default' },
})

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement>, VariantProps<typeof buttonVariants> {}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(({ className, variant, size, ...props }, ref) => (
  <button ref={ref} className={cn(buttonVariants({ variant, size }), className)} {...props} />
))
Button.displayName = 'Button'
