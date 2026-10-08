// 账号并发徽标的展示口径。
//
// 调度准入按「占用槽位 >= dynamic_concurrency_limit」判断：dynamic 是配置并发
// （账号覆盖 > 分组 > 全局默认）再经健康度、额度保护等运行时下调后的实际上限。
// 因此分母用实际上限，分子在开启会话缓冲槽时用占用槽位，其余情况用真实在途。
export type AccountConcurrencyInput = {
  active_requests?: number
  occupied_requests?: number
  session_slot_buffer_enabled?: boolean
  dynamic_concurrency_limit?: number
  base_concurrency_effective?: number
}

export type AccountConcurrencyDisplay = {
  active: number
  occupied: number
  buffered: number
  // 开启会话缓冲槽时展示占用口径。
  showOccupied: boolean
  // 分子：与准入判断同口径的已用槽位。
  used: number
  // 实际并发上限；未知或 <= 0 时为 null，此时只展示分子。
  limit: number | null
  // 配置并发；未知或 <= 0 时为 null。
  base: number | null
  // 实际上限低于配置值（被运行时临时下调）。
  degraded: boolean
}

function positiveInteger(value: number | undefined): number | null {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    return null
  }
  return Math.floor(value)
}

// resolveAccountConcurrencyDisplay 返回徽标数据；没有在途/占用时返回 null，
// 与原先「空闲账号不显示并发徽标」的行为保持一致。
export function resolveAccountConcurrencyDisplay(
  account: AccountConcurrencyInput,
): AccountConcurrencyDisplay | null {
  const active = Math.max(0, account.active_requests ?? 0)
  const occupied = Math.max(active, account.occupied_requests ?? active)
  if (occupied === 0) return null

  const showOccupied = account.session_slot_buffer_enabled === true
  const limit = positiveInteger(account.dynamic_concurrency_limit)
  const base = positiveInteger(account.base_concurrency_effective)
  return {
    active,
    occupied,
    buffered: occupied - active,
    showOccupied,
    used: showOccupied ? occupied : active,
    limit,
    base,
    degraded: limit !== null && base !== null && limit < base,
  }
}

export function formatAccountConcurrencyText(
  display: AccountConcurrencyDisplay,
): string {
  return display.limit === null
    ? String(display.used)
    : `${display.used} / ${display.limit}`
}
