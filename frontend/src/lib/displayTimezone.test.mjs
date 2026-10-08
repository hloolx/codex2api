import assert from 'node:assert/strict'
import test from 'node:test'
import {
  AUTO_DISPLAY_TIMEZONE,
  DISPLAY_TIMEZONE_STORAGE_KEY,
  browserTimezone,
  displayTimezoneOptions,
  formatUtcOffset,
  isValidTimezone,
  readTimezonePreference,
  resolveDisplayTimezone,
  timezoneOffsetMinutes,
  writeTimezonePreference,
} from './displayTimezone.ts'

function memoryStore(initial = {}) {
  const data = new Map(Object.entries(initial))
  return {
    data,
    getItem: (k) => (data.has(k) ? data.get(k) : null),
    setItem: (k, v) => data.set(k, String(v)),
    removeItem: (k) => data.delete(k),
  }
}

test('no saved choice follows the browser timezone instead of Asia/Shanghai', () => {
  const store = memoryStore()
  assert.equal(readTimezonePreference(store), AUTO_DISPLAY_TIMEZONE)
  assert.equal(resolveDisplayTimezone(readTimezonePreference(store)), browserTimezone())
})

test('an explicit saved zone wins over the browser zone', () => {
  const store = memoryStore({ [DISPLAY_TIMEZONE_STORAGE_KEY]: 'Pacific/Auckland' })
  assert.equal(readTimezonePreference(store), 'Pacific/Auckland')
  assert.equal(resolveDisplayTimezone(readTimezonePreference(store)), 'Pacific/Auckland')
})

test('invalid or blank saved values fall back to automatic', () => {
  for (const bad of ['', '   ', 'Mars/Olympus', 'UTC+12', AUTO_DISPLAY_TIMEZONE]) {
    const store = memoryStore({ [DISPLAY_TIMEZONE_STORAGE_KEY]: bad })
    assert.equal(readTimezonePreference(store), AUTO_DISPLAY_TIMEZONE, bad)
  }
})

test('choosing automatic clears the saved value; choosing a zone stores it', () => {
  const store = memoryStore()
  writeTimezonePreference('Europe/London', store)
  assert.equal(store.data.get(DISPLAY_TIMEZONE_STORAGE_KEY), 'Europe/London')
  writeTimezonePreference(AUTO_DISPLAY_TIMEZONE, store)
  assert.equal(store.data.has(DISPLAY_TIMEZONE_STORAGE_KEY), false)
  writeTimezonePreference('Not/AZone', store)
  assert.equal(store.data.has(DISPLAY_TIMEZONE_STORAGE_KEY), false)
})

test('storage failures never throw', () => {
  const broken = {
    getItem() { throw new Error('denied') },
    setItem() { throw new Error('denied') },
    removeItem() { throw new Error('denied') },
  }
  assert.equal(readTimezonePreference(broken), AUTO_DISPLAY_TIMEZONE)
  assert.doesNotThrow(() => writeTimezonePreference('UTC', broken))
  assert.equal(readTimezonePreference(null), AUTO_DISPLAY_TIMEZONE)
})

test('offsets honour daylight saving', () => {
  // New Zealand: NZST (+12) in July, NZDT (+13) in January.
  assert.equal(timezoneOffsetMinutes('Pacific/Auckland', new Date('2026-07-01T00:00:00Z')), 720)
  assert.equal(timezoneOffsetMinutes('Pacific/Auckland', new Date('2026-01-01T00:00:00Z')), 780)
  assert.equal(timezoneOffsetMinutes('America/New_York', new Date('2026-01-15T12:00:00Z')), -300)
  assert.equal(timezoneOffsetMinutes('America/New_York', new Date('2026-07-15T12:00:00Z')), -240)
  assert.equal(timezoneOffsetMinutes('Asia/Kolkata', new Date('2026-07-15T12:00:00Z')), 330)
  assert.equal(timezoneOffsetMinutes('UTC', new Date('2026-07-15T12:34:56.789Z')), 0)
})

test('offset labels', () => {
  assert.equal(formatUtcOffset(780), 'UTC+13:00')
  assert.equal(formatUtcOffset(-300), 'UTC-05:00')
  assert.equal(formatUtcOffset(330), 'UTC+05:30')
  assert.equal(formatUtcOffset(0), 'UTC+00:00')
})

test('picker options are sorted by current offset and keep an uncommon saved zone', () => {
  const at = new Date('2026-01-01T00:00:00Z')
  const options = displayTimezoneOptions('America/Denver', at)
  assert.ok(options.some((o) => o.value === 'America/Denver'))
  const custom = displayTimezoneOptions('Africa/Nairobi', at)
  assert.ok(custom.some((o) => o.value === 'Africa/Nairobi' && o.label === 'UTC+03:00 · Africa/Nairobi'))
  const auckland = options.find((o) => o.value === 'Pacific/Auckland')
  assert.equal(auckland?.label, 'UTC+13:00 · Pacific/Auckland')
  const offsets = options.map((o) => timezoneOffsetMinutes(o.value, at))
  assert.deepEqual(offsets, [...offsets].sort((a, b) => a - b))
  assert.equal(new Set(options.map((o) => o.value)).size, options.length)
  assert.ok(!options.some((o) => o.value === AUTO_DISPLAY_TIMEZONE))
})

test('isValidTimezone', () => {
  assert.equal(isValidTimezone('Pacific/Auckland'), true)
  assert.equal(isValidTimezone('UTC'), true)
  assert.equal(isValidTimezone(' UTC'), false)
  assert.equal(isValidTimezone('Nowhere/Place'), false)
  assert.equal(isValidTimezone(undefined), false)
})
