export interface IPv6EgressConfig {
  enabled: boolean
  source_ips: string[]
  cooldown_seconds: number
  max_attempts: number
  retry_5xx: boolean
  revision: number
}
export interface IPv6EgressData {
  config: IPv6EgressConfig
  local_ips: string[]
  bindings: { account_id: number; ip: string; changed_at: number; reason: string; rotations: number }[]
  cooldowns: { ip: string; until: number }[]
  account_names: Record<string, string>
  resin_enabled: boolean
}
export function ipv6PoolCounts(data: IPv6EgressData) {
  const pool = new Set(data.config.source_ips.length ? data.config.source_ips.filter(ip => data.local_ips.includes(ip)) : data.local_ips)
  const used = new Set(data.bindings.map(row => row.ip))
  const cooling = new Set(data.cooldowns.map(row => row.ip))
  return { total: pool.size, available: [...pool].filter(ip => !used.has(ip) && !cooling.has(ip)).length, cooling: [...pool].filter(ip => cooling.has(ip)).length }
}
