import i18n from '../i18n'
import { readTimezonePreference, resolveDisplayTimezone, writeTimezonePreference } from '../lib/displayTimezone'

export { AUTO_DISPLAY_TIMEZONE } from '../lib/displayTimezone'

export interface RelativeTimeOptions {
  variant?: 'long' | 'compact'
  includeSeconds?: boolean
  fallback?: string
}

export function formatRelativeTime(dateStr?: string | null, options: RelativeTimeOptions = {}): string {
  const {
    variant = 'long',
    includeSeconds = false,
    fallback = '-',
  } = options

  if (!dateStr) {
    return fallback
  }

  const timestamp = new Date(dateStr).getTime()
  if (Number.isNaN(timestamp)) {
    return fallback
  }

  const diff = Math.max(0, Date.now() - timestamp)
  const seconds = Math.floor(diff / 1000)

  if (includeSeconds && seconds < 60) {
    return variant === 'compact'
      ? i18n.t('common.secondsAgoCompact', { count: seconds })
      : i18n.t('common.secondsAgoLong', { count: seconds })
  }

  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) {
    return i18n.t('common.justNow')
  }

  if (minutes < 60) {
    return variant === 'compact'
      ? i18n.t('common.minutesAgoCompact', { count: minutes })
      : i18n.t('common.minutesAgoLong', { count: minutes })
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return variant === 'compact'
      ? i18n.t('common.hoursAgoCompact', { count: hours })
      : i18n.t('common.hoursAgoLong', { count: hours })
  }

  const days = Math.floor(hours / 24)
  return variant === 'compact'
    ? i18n.t('common.daysAgoCompact', { count: days })
    : i18n.t('common.daysAgoLong', { count: days })
}

/** 当前生效的显示时区(IANA)。未保存选择或选择"自动"时跟随浏览器时区,取不到则 UTC。 */
export function getTimezone(): string {
  return resolveDisplayTimezone()
}

/** 已保存的显示时区偏好:IANA 名称,或 AUTO_DISPLAY_TIMEZONE 表示跟随浏览器。 */
export function getTimezonePreference(): string {
  return readTimezonePreference()
}

/** 保存显示时区偏好到 localStorage;传 AUTO_DISPLAY_TIMEZONE 清除保存值(跟随浏览器)。 */
export function setTimezone(tz: string): void {
  writeTimezonePreference(tz)
  dateTimeFormatters.clear()
}

const dateTimeFormatters = new Map<string, Intl.DateTimeFormat>()

function dateTimeFormatter(timeZone: string): Intl.DateTimeFormat {
  let fmt = dateTimeFormatters.get(timeZone)
  if (!fmt) {
    fmt = new Intl.DateTimeFormat('sv-SE', {
      timeZone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hourCycle: 'h23',
    })
    dateTimeFormatters.set(timeZone, fmt)
  }
  return fmt
}

/**
 * 以当前显示时区格式化时间,输出 YYYY-MM-DD HH:mm:ss。
 *
 * 函数名沿用历史命名;实际使用 getTimezone() 返回的显示时区,不固定为北京时间。
 * 使用 Intl.DateTimeFormat 换算,无论后端返回 UTC(带 Z)还是带偏移(+08:00)都能正确显示,
 * 避免手动加减偏移导致的重复转换。
 */
export function formatBeijingTime(dateStr?: string | null, fallback = '-'): string {
  if (!dateStr) return fallback

  const date = new Date(dateStr)
  if (Number.isNaN(date.getTime())) return fallback

  // sv-SE locale 输出格式为 "YYYY-MM-DD HH:mm:ss"，正好是目标格式
  return dateTimeFormatter(getTimezone()).format(date)
}
