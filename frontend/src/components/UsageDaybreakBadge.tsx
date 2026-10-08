import { Badge } from '@/components/ui/badge'

interface UsageDaybreakBadgeProps {
  program?: string
}

export default function UsageDaybreakBadge({ program }: UsageDaybreakBadgeProps) {
  const red = program === 'daybreak_red'
  const blue = program === 'daybreak_blue'
  if (!red && !blue) return null

  return (
    <Badge
      variant="outline"
      className={red
        ? 'border-transparent bg-red-500/12 text-[11px] font-semibold text-red-700 dark:bg-red-500/20 dark:text-red-300'
        : 'border-transparent bg-blue-500/12 text-[11px] font-semibold text-blue-700 dark:bg-blue-500/20 dark:text-blue-300'}
    >
      {red ? 'Daybreak Red' : 'Daybreak Blue'}
    </Badge>
  )
}
