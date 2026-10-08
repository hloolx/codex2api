import { useEffect, useMemo } from 'react'
import { api } from '../api'
import type { AccountLiveStateResponse, AccountStateModel } from '../types'
import { subscribeStateChange } from '../lib/stateSync'
import { syncStateClock } from './useStateClock'

// Merge a live poll response into an account list. Rows whose live concurrency
// counters did not change keep their object identity, and a no-op poll returns the
// original array, so the 1s polling cadence cannot defeat row-level memoization.
export function mergeAccountLiveState<T extends {
  state_models?: AccountStateModel[]
  id: number
  active_requests?: number
  occupied_requests?: number
  session_slot_buffer_enabled?: boolean
  dynamic_concurrency_limit?: number
  base_concurrency_effective?: number
}>(
  current: T[],
  response: AccountLiveStateResponse,
): T[] {
  let changed = false
  const next = current.map((account) => {
    const live = response.accounts[String(account.id)]
    const activeRequests = live?.active_requests ?? 0
    const occupiedRequests = live?.occupied_requests ?? activeRequests
    const slotBufferEnabled = response.session_slot_buffer_enabled === true
    const stateModels = response.accounts[String(account.id)]?.state_models
    // Concurrency caps are optional so an older backend keeps the list values.
    const dynamicLimit = typeof live?.dynamic_concurrency_limit === 'number'
      ? live.dynamic_concurrency_limit
      : account.dynamic_concurrency_limit
    const baseConcurrency = typeof live?.base_concurrency_effective === 'number'
      ? live.base_concurrency_effective
      : account.base_concurrency_effective
    if (
      (account.active_requests ?? 0) === activeRequests &&
      (account.occupied_requests ?? account.active_requests ?? 0) === occupiedRequests &&
      (account.session_slot_buffer_enabled ?? false) === slotBufferEnabled &&
      JSON.stringify(account.state_models) === JSON.stringify(stateModels) &&
      account.dynamic_concurrency_limit === dynamicLimit &&
      account.base_concurrency_effective === baseConcurrency
    ) return account
    changed = true
    return {
      ...account,
      active_requests: activeRequests,
      occupied_requests: occupiedRequests,
      session_slot_buffer_enabled: slotBufferEnabled,
      state_models: stateModels,
      dynamic_concurrency_limit: dynamicLimit,
      base_concurrency_effective: baseConcurrency,
    }
  })
  return changed ? next : current
}

export function useAccountLiveState(
  ids: number[],
  apply: (response: AccountLiveStateResponse) => void,
  enabled = true,
) {
  const idsKey = useMemo(() => ids.join(','), [ids])

  useEffect(() => {
    if (!enabled) return undefined
    let stopped = false
    let timer: number | undefined
    let controller: AbortController | undefined
    let generation = 0

    const poll = async () => {
      if (stopped) return
      if (timer !== undefined) window.clearTimeout(timer)
      const version = ++generation
      if (document.hidden) {
        timer = window.setTimeout(poll, 30000)
        return
      }
      controller?.abort()
      controller = new AbortController()
      try {
        const response = await api.getAccountLiveState(
          idsKey ? idsKey.split(',').map(Number) : [],
          controller.signal,
        )
        if (!stopped && version === generation && !controller.signal.aborted) {
          syncStateClock(response.server_time)
          apply(response)
        }
      } catch {
        // The next tick retries; live counters must never disrupt the page.
      }
      if (!stopped && version === generation) timer = window.setTimeout(poll, 1000)
    }

    const refresh = () => { if (!document.hidden) void poll() }
    const unsubscribe = subscribeStateChange(refresh)
    document.addEventListener('visibilitychange', refresh)
    void poll()
    return () => {
      stopped = true
      unsubscribe()
      document.removeEventListener('visibilitychange', refresh)
      controller?.abort()
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [apply, enabled, idsKey])
}
