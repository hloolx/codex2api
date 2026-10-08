import type { ReactNode } from 'react'
import { getTimezone } from '../utils/time'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Area,
  Bar,
  BarChart,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { Card, CardContent } from '@/components/ui/card'
import StateShell from './StateShell'
import type { ChartAggregation } from '../types'
import { getBucketConfig, type TimeRangeKey } from '../lib/timeRange'

interface DashboardUsageChartsProps {
  chartData: ChartAggregation | null
  refreshedAt: number | null
  refreshIntervalMs: number
  timeRange: TimeRangeKey
  loading?: boolean
}

interface TimelinePoint {
  label: string
  fullLabel: string
  requests: number
  avgLatency: number | null
  inputTokens: number
  outputTokens: number
  reasoningTokens: number
  cachedTokens: number
  errors4xx: number
  errors5xx: number
}

interface ModelRankingPoint {
  model: string
  shortModel: string
  requests: number
}

const chartMargin = { top: 8, right: 12, left: 8, bottom: 0 }
const gridColor = 'var(--color-border)'
const axisColor = 'var(--color-muted-foreground)'
const tooltipContentStyle = {
  backgroundColor: 'var(--color-card)',
  border: '1px solid var(--color-border)',
  borderRadius: '12px',
  boxShadow: '0 10px 30px rgba(0, 0, 0, 0.12)',
  padding: '10px 14px',
}
const tooltipLabelStyle = { color: 'var(--color-foreground)', fontWeight: 600 }
const tooltipItemStyle = { color: 'var(--color-foreground)' }
const tokenBreakdownTooltipPosition = { y: -160 }
const compactNumberFormatter = new Intl.NumberFormat(undefined, {
  notation: 'compact',
  maximumFractionDigits: 1,
})

export default function DashboardUsageCharts({
  chartData: serverData,
  refreshedAt,
  refreshIntervalMs,
  timeRange,
  loading = false,
}: DashboardUsageChartsProps) {
  const { t } = useTranslation()
  const { bucketMinutes, bucketCount } = getBucketConfig(timeRange)
  const isLive = timeRange === '1h'
  const lastUpdatedAtLabel = formatClockTime(refreshedAt)
  const useFullDate = bucketMinutes >= 360

  // 将服务端聚合数据映射为图表渲染格式（极轻量，无聚合计算）
  const displayData = useMemo(() => {
    if (!serverData) return { timelineData: [] as TimelinePoint[], modelData: [] as ModelRankingPoint[], sampleCount: 0 }

    const totalRequests = serverData.timeline.reduce((sum, p) => sum + p.requests, 0)

    const timelineData: TimelinePoint[] = serverData.timeline.map((point) => {
      const d = new Date(point.bucket)
      return {
        label: useFullDate ? formatDateLabel(d, bucketMinutes) : formatMinuteLabel(d),
        fullLabel: formatFullLabel(d, bucketMinutes),
        requests: point.requests,
        avgLatency: point.avg_latency > 0 ? Math.round(point.avg_latency) : null,
        inputTokens: point.input_tokens,
        outputTokens: point.output_tokens,
        reasoningTokens: point.reasoning_tokens,
        cachedTokens: point.cached_tokens,
        errors4xx: point.errors_4xx,
        errors5xx: point.errors_5xx,
      }
    })

    const modelData: ModelRankingPoint[] = serverData.models
      .slice()
      .sort((a, b) => b.requests - a.requests)
      .slice(0, 5)
      .map((m) => ({
        model: m.model,
        shortModel: truncateLabel(m.model, 22),
        requests: m.requests,
      }))

    return { timelineData, modelData, sampleCount: totalRequests }
  }, [serverData, useFullDate, bucketMinutes])

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <h3 className="text-base font-semibold text-foreground">{t('dashboard.usageCharts')}</h3>
          <p className="mt-1 text-sm text-muted-foreground">{t('dashboard.usageChartsDesc', { count: displayData.sampleCount.toLocaleString() })}</p>
          {isLive && (
            <p className="mt-1 text-xs text-muted-foreground">
              {t('dashboard.liveWindowDesc', {
                hours: (bucketCount * bucketMinutes) / 60,
                minutes: bucketMinutes,
                seconds: Math.round(refreshIntervalMs / 1000),
                time: lastUpdatedAtLabel,
              })}
            </p>
          )}
        </div>
      </div>

      {loading ? (
        <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
          {[0, 1, 2, 3].map((i) => (
            <Card key={i} className="py-0">
              <CardContent className="p-6">
                <div className="mb-5 space-y-2">
                  <div className="h-4 w-32 rounded-md bg-muted animate-pulse" />
                  <div className="h-3 w-48 rounded-md bg-muted/60 animate-pulse" />
                </div>
                <div className="h-[280px] flex items-end gap-2 px-4 pb-4">
                  {[40, 65, 30, 80, 55, 70, 45, 60, 35, 75, 50, 68].map((h, j) => (
                    <div
                      key={j}
                      className="flex-1 rounded-t-md bg-muted/50 animate-pulse"
                      style={{ height: `${h}%`, animationDelay: `${j * 80}ms` }}
                    />
                  ))}
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      ) : displayData.sampleCount === 0 ? (
        <Card>
          <CardContent className="p-6">
            <StateShell
              variant="section"
              isEmpty
              emptyTitle={t('dashboard.chartsEmptyTitle')}
              emptyDescription={t('dashboard.chartsEmptyDesc')}
            >
              <></>
            </StateShell>
          </CardContent>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
          <ChartCard title={t('dashboard.requestTrend')} description={t('dashboard.requestTrendDesc')}>
            <ResponsiveContainer width="100%" height="100%">
              <ComposedChart data={displayData.timelineData} margin={chartMargin}>
                <defs>
                  <linearGradient id="dashboard-request-gradient" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="5%" stopColor="var(--color-primary)" stopOpacity={0.28} />
                    <stop offset="95%" stopColor="var(--color-primary)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                <YAxis tickFormatter={formatCompactNumber} tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} allowDecimals={false} tickCount={8} width={58} />
                <Tooltip
                  position={{ y: 10 }}
                  formatter={(value) => formatNumber(value)}
                  labelFormatter={(_, payload) => getTooltipLabel(payload, 'fullLabel')}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                  itemSorter={(item) => (item.dataKey === 'requests' ? -1 : 1)}
                />
                <Legend wrapperStyle={{ paddingTop: 4, fontSize: 12 }} />
                <Area
                  type="monotone"
                  dataKey="requests"
                  name={t('dashboard.seriesRequests')}
                  stroke="var(--color-primary)"
                  fill="url(#dashboard-request-gradient)"
                  strokeWidth={2.5}
                />
                <Line
                  type="monotone"
                  dataKey="errors4xx"
                  name={t('dashboard.series4xxErrors')}
                  stroke="var(--color-destructive)"
                  strokeWidth={2}
                  dot={false}
                  activeDot={{ r: 4 }}
                />
                <Line
                  type="monotone"
                  dataKey="errors5xx"
                  name={t('dashboard.series5xxErrors')}
                  stroke="hsl(36 90% 55%)"
                  strokeWidth={2}
                  dot={false}
                  activeDot={{ r: 4 }}
                />
              </ComposedChart>
            </ResponsiveContainer>
          </ChartCard>

          <ChartCard title={t('dashboard.latencyTrend')} description={t('dashboard.latencyTrendDesc')}>
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={displayData.timelineData} margin={chartMargin}>
                <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                <YAxis tickFormatter={formatDurationTick} tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} width={66} />
                <Tooltip
                  position={{ y: 10 }}
                  formatter={(value) => formatDuration(value)}
                  labelFormatter={(_, payload) => getTooltipLabel(payload, 'fullLabel')}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                />
                <Line
                  type="monotone"
                  dataKey="avgLatency"
                  name={t('dashboard.seriesAvgLatency')}
                  stroke="hsl(var(--info))"
                  strokeWidth={2.5}
                  dot={false}
                  connectNulls
                  activeDot={{ r: 4 }}
                />
              </LineChart>
            </ResponsiveContainer>
          </ChartCard>

          <ChartCard title={t('dashboard.tokenBreakdown')} description={t('dashboard.tokenBreakdownDesc')}>
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={displayData.timelineData} margin={chartMargin}>
                <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                <YAxis tickFormatter={formatCompactNumber} tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} width={58} />
                <Tooltip
                  position={tokenBreakdownTooltipPosition}
                  allowEscapeViewBox={{ y: true }}
                  offset={20}
                  formatter={(value) => formatNumber(value)}
                  labelFormatter={(_, payload) => getTooltipLabel(payload, 'fullLabel')}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                />
                <Legend wrapperStyle={{ paddingTop: 12, fontSize: 12, color: axisColor }} />
                <Bar dataKey="inputTokens" stackId="tokens" name={t('dashboard.seriesInputTokens')} fill="hsl(var(--info))" radius={[0, 0, 4, 4]} />
                <Bar dataKey="outputTokens" stackId="tokens" name={t('dashboard.seriesOutputTokens')} fill="hsl(var(--success))" minPointSize={4} />
                <Bar dataKey="reasoningTokens" stackId="tokens" name={t('dashboard.seriesReasoningTokens')} fill="hsl(36 90% 55%)" minPointSize={4} />
                <Bar dataKey="cachedTokens" stackId="tokens" name={t('dashboard.seriesCachedTokens')} fill="hsl(262 83% 58%)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </ChartCard>

          <ChartCard title={t('dashboard.modelRanking')} description={t('dashboard.modelRankingDesc')}>
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={displayData.modelData} layout="vertical" margin={{ top: 8, right: 12, left: 0, bottom: 0 }}>
                <CartesianGrid horizontal={false} stroke={gridColor} strokeDasharray="4 4" />
                <XAxis type="number" tickFormatter={formatCompactNumber} tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} allowDecimals={false} />
                <YAxis dataKey="shortModel" type="category" width={80} tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} />
                <Tooltip
                  position={{ y: 10 }}
                  formatter={(value) => formatNumber(value)}
                  labelFormatter={(_, payload) => getTooltipLabel(payload, 'model')}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                />
                <Bar dataKey="requests" name={t('dashboard.seriesRequestCount')} fill="hsl(var(--success))" radius={[0, 8, 8, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </ChartCard>
        </div>
      )}
    </div>
  )
}

function ChartCard({ title, description, children }: { title: string; description: string; children: ReactNode }) {
  return (
    <Card className="py-0 border-border/70 bg-card shadow-2xs transition-all duration-200 hover:border-border">
      <CardContent className="p-3.5 sm:p-5">
        <div className="mb-3 sm:mb-4">
          <h4 className="text-sm font-bold tracking-tight text-foreground">{title}</h4>
          <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground/90">{description}</p>
        </div>
        <div className="h-[220px] sm:h-[280px]">{children}</div>
      </CardContent>
    </Card>
  )
}

function formatMinuteLabel(date: Date): string {
  const hours = String(date.getHours()).padStart(2, '0')
  const minutes = String(date.getMinutes()).padStart(2, '0')
  return `${hours}:${minutes}`
}

function formatDateLabel(date: Date, bucketMinutes: number): string {
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  if (bucketMinutes >= 1440) {
    return `${month}-${day}`
  }
  const hour = String(date.getHours()).padStart(2, '0')
  return `${month}-${day} ${hour}:00`
}

function formatFullLabel(date: Date, bucketMinutes: number): string {
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  const hour = String(date.getHours()).padStart(2, '0')
  const minute = String(date.getMinutes()).padStart(2, '0')
  if (bucketMinutes >= 1440) {
    return `${date.getFullYear()}-${month}-${day}`
  }
  return `${month}-${day} ${hour}:${minute}`
}

function formatClockTime(value: number | null): string {
  if (!value) return '--:--:--'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '--:--:--'
  return date.toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    timeZone: getTimezone(),
  })
}

function truncateLabel(value: string, maxLength: number): string {
  if (value.length <= maxLength) return value
  return `${value.slice(0, maxLength - 1)}…`
}

function formatCompactNumber(value: number | string): string {
  const numericValue = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(numericValue)) return '0'
  return compactNumberFormatter.format(numericValue)
}

function formatNumber(value: unknown): string {
  const numericValue = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(numericValue)) return '0'
  return numericValue.toLocaleString()
}

function formatDuration(value: unknown): string {
  const numericValue = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(numericValue) || numericValue <= 0) return '-'
  if (numericValue >= 1000) {
    return `${(numericValue / 1000).toFixed(numericValue >= 10000 ? 0 : 1)}s`
  }
  return `${Math.round(numericValue)}ms`
}

function formatDurationTick(value: number | string): string {
  const numericValue = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(numericValue)) return '0ms'
  return formatDuration(numericValue)
}

function getTooltipLabel(payload: readonly { payload?: Record<string, unknown> }[] | undefined, key: string): string {
  const tooltipPayload = payload?.[0]?.payload
  const rawValue = tooltipPayload?.[key]
  return typeof rawValue === 'string' && rawValue ? rawValue : ''
}
