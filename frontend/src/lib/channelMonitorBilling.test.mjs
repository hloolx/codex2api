import assert from 'node:assert/strict'
import test from 'node:test'

import {
  formatChannelMonitorMultiplier,
  resolveChannelMonitorRate,
} from './channelMonitorBilling.ts'

test('channel monitor rate applies the same-day peak window in its declared timezone', () => {
  const data = {
    resolved_rate_multiplier: 0.75,
    effective_rate_multiplier: 0.75,
    peak_rate_enabled: true,
    peak_start: '09:00',
    peak_end: '11:00',
    peak_rate_multiplier: 2,
    timezone: 'UTC',
  }
  assert.equal(resolveChannelMonitorRate(data, Date.UTC(2026, 0, 1, 10, 0)), 1.5)
  assert.equal(resolveChannelMonitorRate(data, Date.UTC(2026, 0, 1, 12, 0)), 0.75)
})

test('channel monitor rate falls back to the observed effective value for invalid windows', () => {
  assert.equal(resolveChannelMonitorRate({
    resolved_rate_multiplier: 1,
    effective_rate_multiplier: 1.25,
    peak_rate_enabled: true,
    peak_start: '22:00',
    peak_end: '02:00',
    peak_rate_multiplier: 2,
    timezone: 'UTC',
  }, Date.UTC(2026, 0, 1, 23, 0)), 1.25)
})

test('channel monitor multiplier formatting stays compact', () => {
  assert.equal(formatChannelMonitorMultiplier(0.7500001), '0.75')
})
