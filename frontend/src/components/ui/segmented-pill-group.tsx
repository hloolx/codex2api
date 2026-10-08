import type { ReactNode } from 'react'
import { Button } from './button'
import { cn } from '@/lib/utils'

// Shared version of the segmented control used by Settings.
export function SegmentedPillGroup<T extends string>({
  value, onChange, options, label, disabled = false, className,
}: {
  value: T
  onChange: (value: T) => void
  options: Array<{ label: ReactNode; value: T; icon?: ReactNode; disabled?: boolean; title?: string }>
  label: string
  disabled?: boolean
  className?: string
}) {
  return (
    <div role="group" aria-label={label} className={cn('flex gap-1 rounded-xl border border-border/60 bg-muted/40 p-1', className)}>
      {options.map(option => (
        <Button
          key={option.value}
          type="button"
          variant="ghost"
          disabled={disabled || option.disabled}
          title={option.title}
          aria-pressed={value === option.value}
          onClick={() => onChange(option.value)}
          className={cn(
            'min-w-0 flex-1 gap-1.5 px-2 text-xs motion-reduce:transition-none',
            value === option.value
              ? 'bg-background text-foreground shadow-sm hover:bg-background'
              : 'text-muted-foreground hover:text-foreground',
          )}
        >
          {option.icon}
          {option.label}
        </Button>
      ))}
    </div>
  )
}
