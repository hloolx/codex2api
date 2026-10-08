import type { ChannelMonitorBillingData } from '../types'

export function resolveChannelMonitorRate(
  data: ChannelMonitorBillingData | undefined,
  now: number,
): number | null {
  if (!data) return null
  const base = validChannelMonitorMultiplier(data.resolved_rate_multiplier)
  if (base == null) return validChannelMonitorMultiplier(data.effective_rate_multiplier)
  if (!data.peak_rate_enabled) return base

  const start = parseMinute(data.peak_start)
  const end = parseMinute(data.peak_end)
  const peak = validChannelMonitorMultiplier(data.peak_rate_multiplier)
  const minute = minuteInTimeZone(now, data.timezone)
  if (start == null || end == null || peak == null || minute == null || start >= end) {
    return validChannelMonitorMultiplier(data.effective_rate_multiplier)
  }
  return minute >= start && minute < end ? base * peak : base
}

export function validChannelMonitorMultiplier(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}

export function formatChannelMonitorMultiplier(value: number): string {
  return Number(value.toPrecision(8)).toLocaleString(undefined, { maximumFractionDigits: 6 })
}

function parseMinute(value?: string): number | null {
  const match = /^(\d{2}):(\d{2})$/.exec(value || '')
  if (!match) return null
  const hour = Number(match[1])
  const minute = Number(match[2])
  return hour < 24 && minute < 60 ? hour * 60 + minute : null
}

function minuteInTimeZone(now: number, timeZone?: string): number | null {
  if (!timeZone) return null
  try {
    const parts = new Intl.DateTimeFormat('en-GB', {
      timeZone,
      hour: '2-digit',
      minute: '2-digit',
      hourCycle: 'h23',
    }).formatToParts(new Date(now))
    const hour = Number(parts.find((part) => part.type === 'hour')?.value)
    const minute = Number(parts.find((part) => part.type === 'minute')?.value)
    return Number.isInteger(hour) && Number.isInteger(minute) ? hour * 60 + minute : null
  } catch {
    return null
  }
}
