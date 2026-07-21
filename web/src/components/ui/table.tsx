import { forwardRef, type HTMLAttributes, type TableHTMLAttributes, type TdHTMLAttributes, type ThHTMLAttributes } from 'react'
import { cn } from './utils'

export const Table = forwardRef<HTMLTableElement, TableHTMLAttributes<HTMLTableElement>>(({ className, ...props }, ref) => <table ref={ref} className={cn('table', className)} {...props} />)
Table.displayName = 'Table'
export const TableHeader = (props: HTMLAttributes<HTMLTableSectionElement>) => <thead {...props} />
export const TableBody = (props: HTMLAttributes<HTMLTableSectionElement>) => <tbody {...props} />
export const TableRow = forwardRef<HTMLTableRowElement, HTMLAttributes<HTMLTableRowElement>>((props, ref) => <tr ref={ref} {...props} />)
TableRow.displayName = 'TableRow'
export const TableHead = (props: ThHTMLAttributes<HTMLTableCellElement>) => <th scope="col" {...props} />
export const TableCell = (props: TdHTMLAttributes<HTMLTableCellElement>) => <td {...props} />
