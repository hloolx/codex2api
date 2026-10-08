import { useCallback, useEffect, useMemo, useState } from 'react'
import { getTimezone } from '../utils/time'
import { useNavigate } from 'react-router-dom'
import {
  AlertTriangle,
  CheckCircle2,
  Clock3,
  Gauge,
  Loader2,
  RadioTower,
  RefreshCw,
  Settings2,
  TimerReset,
  XCircle,
} from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import {
  formatChannelMonitorMultiplier,
  resolveChannelMonitorRate,
  validChannelMonitorMultiplier,
} from '../lib/channelMonitorBilling'
import type {
  ChannelMonitorCard,
  ChannelMonitorListResponse,
  ChannelMonitorStatus,
} from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardAction, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

const EMPTY_RESPONSE: ChannelMonitorListResponse = { items: [], generated_at: '' }

export default function ChannelMonitor() {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const [probing, setProbing] = useState<Set<number>>(new Set())
  const [clock, setClock] = useState(() => Date.now())
  const load = useCallback(() => api.getChannelMonitors(), [])
  const { data, loading, error, reload, reloadSilently } = useDataLoader<ChannelMonitorListResponse>({
    initialData: EMPTY_RESPONSE,
    load,
  })

  useEffect(() => {
    const poll = window.setInterval(() => { void reloadSilently() }, 30_000)
    const tick = window.setInterval(() => setClock(Date.now()), 60_000)
    return () => {
      window.clearInterval(poll)
      window.clearInterval(tick)
    }
  }, [reloadSilently])

  const items = useMemo(
    () => [...data.items].sort((left, right) => {
      const severity: Record<ChannelMonitorStatus, number> = { failed: 0, degraded: 1, unknown: 2, operational: 3 }
      return severity[left.status] - severity[right.status] || left.name.localeCompare(right.name)
    }),
    [data.items],
  )
  const summary = useMemo(() => ({
    total: items.length,
    operational: items.filter((item) => item.status === 'operational').length,
    degraded: items.filter((item) => item.status === 'degraded').length,
    failed: items.filter((item) => item.status === 'failed').length,
  }), [items])

  const probeNow = async (accountID: number) => {
    setProbing((current) => new Set(current).add(accountID))
    try {
      await api.probeChannelMonitor(accountID)
      showToast('渠道健康与倍率探测完成')
      await reloadSilently()
    } catch (cause) {
      showToast(`立即探测失败: ${getErrorMessage(cause)}`, 'error')
    } finally {
      setProbing((current) => {
        const next = new Set(current)
        next.delete(accountID)
        return next
      })
    }
  }

  return (
    <StateShell
      variant="page"
      loading={loading && data.items.length === 0}
      error={data.items.length === 0 ? error : null}
      onRetry={() => void reload()}
      isEmpty={!loading && !error && data.items.length === 0}
      emptyTitle="暂无启用的渠道监控"
      emptyDescription="前往账号管理，在 Responses API 渠道的“渠道监控配置”中开启。"
      action={<Button onClick={() => navigate('/accounts')}><Settings2 className="size-4" />前往账号管理</Button>}
      loadingTitle="正在加载渠道监控"
      loadingDescription="读取健康历史与上游倍率快照"
      errorTitle="渠道监控加载失败"
    >
      <>
        <PageHeader
          title="渠道监控"
          description="Responses API 渠道的主动可用性、响应延迟与上游计费倍率。"
          actions={
            <div className="flex items-center gap-3 max-sm:w-full max-sm:flex-col max-sm:items-stretch">
              <span className="text-xs text-muted-foreground max-sm:text-center">
                更新于 {formatTime(data.generated_at)}
              </span>
              <Button variant="outline" onClick={() => void reload()} disabled={loading}>
                <RefreshCw className={cn('size-3.5', loading && 'animate-spin')} />
                刷新
              </Button>
            </div>
          }
        />

        {error && data.items.length > 0 ? (
          <div className="mb-4 flex items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive" role="alert">
            <span className="truncate">{error}</span>
            <Button variant="outline" size="sm" onClick={() => void reload()}>重试</Button>
          </div>
        ) : null}

        <div className="mb-5 grid grid-cols-2 gap-2.5 sm:grid-cols-4">
          <SummaryCard label="已监控" value={summary.total} icon={<RadioTower className="size-4" />} tone="neutral" />
          <SummaryCard label="运行正常" value={summary.operational} icon={<CheckCircle2 className="size-4" />} tone="success" />
          <SummaryCard label="响应较慢" value={summary.degraded} icon={<AlertTriangle className="size-4" />} tone="warning" />
          <SummaryCard label="探测异常" value={summary.failed} icon={<XCircle className="size-4" />} tone="danger" />
        </div>

        <TooltipProvider delayDuration={120}>
          <div className="grid gap-4 md:grid-cols-2 2xl:grid-cols-3">
            {items.map((item) => (
              <MonitorCard
                key={item.account_id}
                item={item}
                now={clock}
                probing={probing.has(item.account_id)}
                onProbe={() => void probeNow(item.account_id)}
              />
            ))}
          </div>
        </TooltipProvider>
      </>
    </StateShell>
  )
}

function SummaryCard({
  label,
  value,
  icon,
  tone,
}: {
  label: string
  value: number
  icon: React.ReactNode
  tone: 'neutral' | 'success' | 'warning' | 'danger'
}) {
  const tones = {
    neutral: 'bg-primary/10 text-primary',
    success: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
    warning: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
    danger: 'bg-destructive/10 text-destructive',
  }
  return (
    <Card className="gap-0 shadow-2xs">
      <CardContent className="flex items-center gap-3 p-3.5 sm:p-4">
        <div className={cn('flex size-9 shrink-0 items-center justify-center rounded-lg', tones[tone])}>{icon}</div>
        <div className="min-w-0">
          <p className="text-[11px] font-medium text-muted-foreground">{label}</p>
          <p className="text-xl font-bold tabular-nums text-foreground">{value}</p>
        </div>
      </CardContent>
    </Card>
  )
}

function MonitorCard({
  item,
  now,
  probing,
  onProbe,
}: {
  item: ChannelMonitorCard
  now: number
  probing: boolean
  onProbe: () => void
}) {
  const rate = resolveChannelMonitorRate(item.billing.data, now)
  const host = formatHost(item.base_url)
  const status = monitorStatusMeta(item.status)
  return (
    <Card className="group gap-0 overflow-hidden border-border/80 shadow-2xs transition-colors duration-200 hover:border-primary/30">
      <CardHeader className="gap-1.5 border-b border-border/60 bg-muted/15 p-4">
        <div className="flex min-w-0 items-center gap-2">
          <span className={cn('size-2 shrink-0 rounded-full ring-4', status.dot, status.ring)} />
          <CardTitle className="truncate text-sm" title={item.name}>{item.name}</CardTitle>
        </div>
        <p className="truncate font-mono text-[11px] text-muted-foreground" title={item.base_url}>{host}</p>
        <CardAction className="flex items-center gap-1.5">
          <BillingRateBadge item={item} rate={rate} />
          <Badge variant="outline" className={cn('text-[10px]', status.badge)}>{status.label}</Badge>
        </CardAction>
      </CardHeader>

      <CardContent className="space-y-4 p-4">
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="min-w-0 truncate font-mono text-foreground" title={item.model}>{item.model}</span>
          <span className="shrink-0 text-muted-foreground">每 {item.interval_minutes} 分钟</span>
        </div>

        <div className="grid grid-cols-2 divide-x divide-border rounded-lg border border-border/70 bg-muted/15 py-2.5">
          <Metric label="总耗时" value={formatLatency(item.latency_ms)} icon={<Clock3 className="size-3" />} />
          <Metric label="7 天可用" value={item.availability_7d == null ? '—' : `${item.availability_7d.toFixed(2)}%`} icon={<Gauge className="size-3" />} />
        </div>

        <MonitorTimeline checks={item.recent_checks} checks7d={item.checks_7d} />

        {item.message ? (
          <div className={cn(
            'line-clamp-2 rounded-md px-2.5 py-2 text-[11px] leading-relaxed',
            item.status === 'failed' ? 'bg-destructive/7 text-destructive' : 'bg-muted/30 text-muted-foreground',
          )} title={item.message}>
            {item.http_status ? `HTTP ${item.http_status} · ` : ''}{item.message}
          </div>
        ) : null}

        <div className="flex items-center justify-between gap-3 border-t border-border/60 pt-3">
          <div className="min-w-0 text-[11px] text-muted-foreground">
            <p className="truncate">上次：{formatDateTime(item.last_checked_at)}</p>
            <p className="truncate">下次：{formatDateTime(item.next_check_at)}</p>
          </div>
          <Button variant="outline" size="sm" onClick={onProbe} disabled={probing} className="shrink-0 cursor-pointer">
            {probing ? <Loader2 className="size-3.5 animate-spin" /> : <TimerReset className="size-3.5" />}
            立即探测
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

const MONITOR_TIMELINE_LENGTH = 60

function MonitorTimeline({
  checks,
  checks7d,
}: {
  checks: ChannelMonitorCard['recent_checks']
  checks7d: number
}) {
  const recent = checks.slice(-MONITOR_TIMELINE_LENGTH)
  const slots: Array<ChannelMonitorCard['recent_checks'][number] | null> = [
    ...Array.from({ length: MONITOR_TIMELINE_LENGTH - recent.length }, () => null),
    ...recent,
  ]
  const statusCounts = recent.reduce<Record<ChannelMonitorStatus, number>>((counts, check) => {
    counts[check.status] += 1
    return counts
  }, { unknown: 0, operational: 0, degraded: 0, failed: 0 })
  const timelineLabel = recent.length === 0
    ? '最近 60 个状态槽位，暂无探测记录'
    : `最近 60 个状态槽位：正常 ${statusCounts.operational}，较慢 ${statusCounts.degraded}，异常 ${statusCounts.failed}，待探测 ${statusCounts.unknown}`

  return (
    <div className="space-y-1.5 border-t border-border/60 pt-3">
      <div className="flex items-center justify-between text-[10px] font-medium uppercase text-muted-foreground">
        <span>最近 60 个状态</span>
        <span className="tabular-nums">{checks7d > 0 ? `7 天 ${checks7d} 次` : '暂无历史'}</span>
      </div>
      <div
        className="flex h-5 w-full items-end gap-[2px]"
        role="img"
        aria-label={timelineLabel}
      >
        {slots.map((check, index) => (
          <span
            key={check ? `${check.checked_at}-${index}` : `empty-${index}`}
            className={cn('min-w-0 flex-1 rounded-[2px]', monitorTimelineBarClass(check?.status))}
            title={check
              ? `${formatDateTime(check.checked_at)} · ${monitorStatusMeta(check.status).label} · ${formatLatency(check.latency_ms)}`
              : undefined}
            aria-hidden="true"
          />
        ))}
      </div>
      <div className="flex justify-between text-[9px] uppercase text-muted-foreground/80" aria-hidden="true">
        <span>过去</span>
        <span>现在</span>
      </div>
    </div>
  )
}

function BillingRateBadge({ item, rate }: { item: ChannelMonitorCard; rate: number | null }) {
  const billing = item.billing
  const data = billing.data
  const rateLabel = rate == null ? billingStatusLabel(billing.status) : `×${formatChannelMonitorMultiplier(rate)}`
  const tone = billing.status === 'ok'
    ? 'border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'
    : billing.status === 'failed'
      ? 'border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-300'
      : 'border-border bg-muted text-muted-foreground'
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          className={cn('cursor-help rounded-md border px-2 py-1 font-mono text-xs font-bold tabular-nums transition-colors duration-200', tone)}
          aria-label={`上游计费倍率 ${rateLabel}`}
        >
          {rateLabel}
        </button>
      </TooltipTrigger>
      <TooltipContent side="bottom" className="max-w-[min(22rem,calc(100vw-2rem))] space-y-1.5 px-3 py-2.5">
        <p className="font-semibold">上游计费倍率</p>
        {data ? (
          <>
            <DetailLine label="分组倍率" value={formatOptionalMultiplier(data.group_rate_multiplier)} />
            {data.user_rate_multiplier != null ? <DetailLine label="用户倍率" value={formatOptionalMultiplier(data.user_rate_multiplier)} /> : null}
            <DetailLine label="基础倍率" value={formatOptionalMultiplier(data.resolved_rate_multiplier)} />
            <DetailLine
              label="峰值规则"
              value={data.peak_rate_enabled
                ? `${data.peak_start || '?'}–${data.peak_end || '?'} · ×${formatChannelMonitorMultiplier(data.peak_rate_multiplier ?? 1)} ${data.timezone || ''}`
                : '未启用'}
            />
            <DetailLine label="当前倍率" value={rate == null ? '—' : `×${formatChannelMonitorMultiplier(rate)}`} />
          </>
        ) : <p>{billingStatusLabel(billing.status)}</p>}
        <div className="mt-2 border-t border-background/20 pt-2 text-background/75">
          <p>状态：{billingStatusLabel(billing.status)}</p>
          <p>上次成功：{formatDateTime(billing.success_at)}</p>
          <p>下次探测：{formatDateTime(billing.next_check_at)}</p>
          {billing.message ? <p className="mt-1 break-words">{billing.message}</p> : null}
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

function Metric({ label, value, icon }: { label: string; value: string; icon: React.ReactNode }) {
  return (
    <div className="min-w-0 px-2 text-center">
      <div className="mb-1 flex items-center justify-center gap-1 text-[10px] text-muted-foreground">{icon}{label}</div>
      <p className="truncate text-xs font-semibold tabular-nums text-foreground" title={value}>{value}</p>
    </div>
  )
}

function DetailLine({ label, value }: { label: string; value: string }) {
  return <p className="flex items-start justify-between gap-4"><span className="text-background/70">{label}</span><span className="text-right font-mono">{value}</span></p>
}

function monitorStatusMeta(status: ChannelMonitorStatus) {
  switch (status) {
    case 'operational': return { label: '正常', dot: 'bg-emerald-500', ring: 'ring-emerald-500/10', badge: 'border-emerald-500/30 text-emerald-600 dark:text-emerald-400' }
    case 'degraded': return { label: '较慢', dot: 'bg-amber-500', ring: 'ring-amber-500/10', badge: 'border-amber-500/30 text-amber-600 dark:text-amber-400' }
    case 'failed': return { label: '异常', dot: 'bg-destructive', ring: 'ring-destructive/10', badge: 'border-destructive/30 text-destructive' }
    default: return { label: '待探测', dot: 'bg-slate-400', ring: 'ring-slate-400/10', badge: 'text-muted-foreground' }
  }
}

function monitorTimelineBarClass(status?: ChannelMonitorStatus) {
  switch (status) {
    case 'operational': return 'h-full bg-emerald-500'
    case 'degraded': return 'h-[65%] bg-amber-500'
    case 'failed': return 'h-[35%] bg-destructive'
    default: return 'h-[15%] bg-muted-foreground/25'
  }
}

function billingStatusLabel(status: ChannelMonitorCard['billing']['status']) {
  switch (status) {
    case 'ok': return '倍率已探测'
    case 'unsupported': return '不支持倍率'
    case 'failed': return '倍率探测失败'
    default: return '倍率待探测'
  }
}

function formatOptionalMultiplier(value?: number) {
  const normalized = validChannelMonitorMultiplier(value)
  return normalized == null ? '—' : `×${formatChannelMonitorMultiplier(normalized)}`
}

function formatLatency(value: number) {
  if (!Number.isFinite(value) || value <= 0) return '—'
  if (value < 1000) return `${Math.round(value)} ms`
  return `${(value / 1000).toFixed(value >= 10_000 ? 1 : 2)} s`
}

function formatHost(value: string) {
  try {
    const url = new URL(value)
    return `${url.host}${url.pathname === '/' ? '' : url.pathname}`
  } catch {
    return value || '—'
  }
}

function formatDateTime(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '—'
  return date.toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', timeZone: getTimezone() })
}

function formatTime(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return '—'
  return date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', timeZone: getTimezone() })
}
