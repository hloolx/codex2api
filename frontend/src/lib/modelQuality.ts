export type ModelQualityConfig = { enabled: boolean; models: string[]; revision: number }
export type ModelQualityState = {
  account_id: number
  model: string
  status: 'pending' | 'pass' | 'fail' | 'unsupported'
  last_outcome: string
  reason: string
  checked_at: number
  next_check_at: number
  running: boolean
}
export type ModelQualityData = {
  config: ModelQualityConfig
  models: string[]
  accounts: { id: number; name: string; available: boolean; states: ModelQualityState[] }[]
  total: number
  page: number
  page_size: number
  interval_seconds: number
}
