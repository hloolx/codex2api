import assert from 'node:assert/strict'
import { test } from 'node:test'
import { ipv6PoolCounts } from './ipv6Egress.ts'

test('IPv6 availability excludes bound, cooling and removed addresses', () => {
  const data = { config: { source_ips: ['2604::1', '2604::2', '2604::3', '2604::4', '2604::5'] }, local_ips: ['2604::1', '2604::2', '2604::3', '2604::4'], bindings: [{ ip: '2604::1' }, { ip: '2604::2' }], cooldowns: [{ ip: '2604::2' }, { ip: '2604::3' }] }
  assert.deepEqual(ipv6PoolCounts(data), { total: 4, available: 1, cooling: 2 })
})
