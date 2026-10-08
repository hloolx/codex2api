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
  execution?: 'disabled' | 'running' | 'unsupported' | 'account_unavailable' | 'model_cooldown' | 'scheduled' | 'waiting_account' | 'waiting_capacity' | 'queued'
  account_status?: string
  cooldown_reason?: string
  resume_at?: number
  occupied?: number
  capacity?: number
  can_retest?: boolean
}
export type ModelQualityData = {
  config: ModelQualityConfig
  models: string[]
  accounts: { id: number; name: string; available: boolean; states: ModelQualityState[] }[]
  total: number
  page: number
  page_size: number
  interval_seconds: number
  last_scan_at?: number
  scheduling_error?: boolean
}
