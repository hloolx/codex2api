/**
 * Display timezone preference for the admin dashboard and the public portals.
 *
 * The choice is per browser (localStorage). With no saved choice, or when the
 * saved choice is "auto", timestamps follow the viewer's browser timezone and
 * fall back to UTC when the runtime cannot report one. This is independent of
 * the server's TZ, which only governs backend local-time behaviour such as
 * daily-limit reset boundaries.
 */
export const DISPLAY_TIMEZONE_STORAGE_KEY = 'codex2api_timezone'
export const AUTO_DISPLAY_TIMEZONE = 'auto'
export const FALLBACK_DISPLAY_TIMEZONE = 'UTC'

/** Common IANA zones offered in the picker; any valid IANA name is accepted. */
export const COMMON_DISPLAY_TIMEZONES: readonly string[] = [
  'UTC',
  'Pacific/Honolulu',
  'America/Anchorage',
  'America/Los_Angeles',
  'America/Denver',
  'America/Chicago',
  'America/New_York',
  'America/Sao_Paulo',
  'Europe/London',
  'Europe/Paris',
  'Europe/Berlin',
  'Europe/Amsterdam',
  'Europe/Rome',
  'Europe/Moscow',
  'Asia/Dubai',
  'Asia/Kolkata',
  'Asia/Bangkok',
  'Asia/Shanghai',
  'Asia/Hong_Kong',
  'Asia/Taipei',
  'Asia/Singapore',
  'Asia/Tokyo',
  'Asia/Seoul',
  'Australia/Sydney',
  'Pacific/Auckland',
]

interface TimezoneStore {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

function defaultStore(): TimezoneStore | null {
  try {
    return (globalThis as { localStorage?: TimezoneStore }).localStorage ?? null
  } catch {
    return null
  }
}

const validityCache = new Map<string, boolean>()

/** Reports whether the runtime accepts `tz` as an IANA timezone name. */
export function isValidTimezone(tz: unknown): tz is string {
  if (typeof tz !== 'string') return false
  const name = tz.trim()
  if (!name || name !== tz) return false
  const cached = validityCache.get(name)
  if (cached !== undefined) return cached
  let valid = false
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: name })
    valid = true
  } catch {
    valid = false
  }
  validityCache.set(name, valid)
  return valid
}

/** The browser's own timezone, or UTC when it cannot be determined. */
export function browserTimezone(): string {
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone
    if (isValidTimezone(tz)) return tz
  } catch {
    /* fall through */
  }
  return FALLBACK_DISPLAY_TIMEZONE
}

/** Saved preference: an IANA name, or "auto" when nothing valid is saved. */
export function readTimezonePreference(store: TimezoneStore | null = defaultStore()): string {
  let raw: string | null = null
  try {
    raw = store?.getItem(DISPLAY_TIMEZONE_STORAGE_KEY) ?? null
  } catch {
    raw = null
  }
  const value = raw?.trim() ?? ''
  if (!value || value === AUTO_DISPLAY_TIMEZONE) return AUTO_DISPLAY_TIMEZONE
  return isValidTimezone(value) ? value : AUTO_DISPLAY_TIMEZONE
}

/** Persists a preference; "auto", empty, or invalid names clear the saved choice. */
export function writeTimezonePreference(preference: string, store: TimezoneStore | null = defaultStore()): void {
  const value = preference?.trim() ?? ''
  try {
    if (!value || value === AUTO_DISPLAY_TIMEZONE || !isValidTimezone(value)) {
      store?.removeItem(DISPLAY_TIMEZONE_STORAGE_KEY)
    } else {
      store?.setItem(DISPLAY_TIMEZONE_STORAGE_KEY, value)
    }
  } catch {
    /* storage unavailable: the choice simply is not remembered */
  }
}

/** The IANA timezone timestamps should be rendered in. */
export function resolveDisplayTimezone(preference: string = readTimezonePreference()): string {
  if (preference && preference !== AUTO_DISPLAY_TIMEZONE && isValidTimezone(preference)) return preference
  return browserTimezone()
}

/** Offset of `tz` from UTC in minutes at instant `at`, honouring daylight saving. */
export function timezoneOffsetMinutes(tz: string, at: Date = new Date()): number {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: tz,
    hourCycle: 'h23',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).formatToParts(at)
  const part = (type: Intl.DateTimeFormatPartTypes) => Number(parts.find((p) => p.type === type)?.value ?? 0)
  const wallClockAsUTC = Date.UTC(part('year'), part('month') - 1, part('day'), part('hour') % 24, part('minute'), part('second'))
  const wholeSeconds = Math.floor(at.getTime() / 1000) * 1000
  return Math.round((wallClockAsUTC - wholeSeconds) / 60000)
}

/** Formats an offset in minutes as "UTC+13:00" / "UTC-05:00". */
export function formatUtcOffset(minutes: number): string {
  const sign = minutes < 0 ? '-' : '+'
  const abs = Math.abs(minutes)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `UTC${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
}

/** "UTC+13:00 · Pacific/Auckland", using the offset in effect at `at`. */
export function timezoneOptionLabel(tz: string, at: Date = new Date()): string {
  try {
    return `${formatUtcOffset(timezoneOffsetMinutes(tz, at))} · ${tz}`
  } catch {
    return tz
  }
}

export interface DisplayTimezoneOption {
  value: string
  label: string
}

/**
 * Picker entries: the common zones (plus the saved one when it is not common),
 * ordered by their current offset. The "auto" entry is added by the caller so
 * its label can be localised.
 */
export function displayTimezoneOptions(preference: string = readTimezonePreference(), at: Date = new Date()): DisplayTimezoneOption[] {
  const zones = new Set(COMMON_DISPLAY_TIMEZONES.filter(isValidTimezone))
  if (preference !== AUTO_DISPLAY_TIMEZONE && isValidTimezone(preference)) zones.add(preference)
  return [...zones]
    .map((tz) => {
      let offset = 0
      try {
        offset = timezoneOffsetMinutes(tz, at)
      } catch {
        offset = 0
      }
      return { tz, offset }
    })
    .sort((a, b) => a.offset - b.offset || a.tz.localeCompare(b.tz))
    .map(({ tz, offset }) => ({ value: tz, label: `${formatUtcOffset(offset)} · ${tz}` }))
}
