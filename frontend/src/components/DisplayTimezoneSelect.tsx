import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Select } from '@/components/ui/select'
import {
  AUTO_DISPLAY_TIMEZONE,
  browserTimezone,
  displayTimezoneOptions,
  readTimezonePreference,
  timezoneOptionLabel,
  writeTimezonePreference,
} from '@/lib/displayTimezone'

interface DisplayTimezoneSelectProps {
  className?: string
  triggerClassName?: string
  compact?: boolean
  'aria-label'?: string
}

/**
 * Per-browser display timezone picker. Timestamps are formatted outside React
 * state, so a change reloads the page to re-render everything consistently.
 */
export default function DisplayTimezoneSelect({ className, triggerClassName, compact, 'aria-label': ariaLabel }: DisplayTimezoneSelectProps) {
  const { t } = useTranslation()
  const preference = readTimezonePreference()

  const options = useMemo(() => {
    const now = new Date()
    const browserLabel = timezoneOptionLabel(browserTimezone(), now)
    const zones = displayTimezoneOptions(preference, now)
    return [
      {
        value: AUTO_DISPLAY_TIMEZONE,
        label: `${t('settings.timezoneAuto')} · ${browserLabel}`,
        triggerLabel: compact ? browserLabel : undefined,
      },
      ...zones.map((zone) => ({ ...zone, triggerLabel: compact ? zone.label : undefined })),
    ]
  }, [preference, t, compact])

  return (
    <Select
      aria-label={ariaLabel ?? t('settings.timezone')}
      className={className}
      triggerClassName={triggerClassName}
      compact={compact}
      value={preference}
      onValueChange={(value) => {
        if (value === preference) return
        writeTimezonePreference(value)
        window.location.reload()
      }}
      options={options}
    />
  )
}
