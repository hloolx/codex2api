import { useEffect, useId, useMemo, useState } from 'react'
import { Activity, Clock3, Gauge, Loader2, RadioTower, Save } from 'lucide-react'
import type { AccountRow, ChannelMonitorConfig } from '../types'
import { api } from '../api'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import Modal from './Modal'
import StateShell from './StateShell'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

interface ChannelMonitorConfigDialogProps {
  account: AccountRow | null
  show: boolean
  onClose: () => void
  onSaved?: () => void
}

export default function ChannelMonitorConfigDialog({
  account,
  show,
  onClose,
  onSaved,
}: ChannelMonitorConfigDialogProps) {
  const { showToast } = useToast()
  const modelListId = useId()
  const [config, setConfig] = useState<ChannelMonitorConfig | null>(null)
  const [enabled, setEnabled] = useState(false)
  const [intervalInput, setIntervalInput] = useState('5')
  const [model, setModel] = useState('')
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => {
    if (!show || !account) return undefined
    const controller = new AbortController()
    setLoading(true)
    setError('')
    setConfig(null)
    void api.getChannelMonitorConfig(account.id, controller.signal)
      .then((next) => {
        setConfig(next)
        setEnabled(next.enabled)
        setIntervalInput(String(next.interval_minutes || 5))
        setModel(next.model || next.available_models[0] || '')
      })
      .catch((cause: unknown) => {
        if (cause instanceof DOMException && cause.name === 'AbortError') return
        setError(getErrorMessage(cause))
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false)
      })
    return () => controller.abort()
  }, [account, reloadKey, show])

  const interval = Number.parseInt(intervalInput, 10)
  const intervalValid = Number.isInteger(interval) && interval >= 1 && interval <= 60
  const modelValid = !enabled || Boolean(model.trim())
  const canSave = Boolean(config) && !loading && !saving && intervalValid && modelValid
  const dailyCalls = useMemo(
    () => intervalValid ? Math.ceil(1440 / interval) : 0,
    [interval, intervalValid],
  )

  const handleSave = async () => {
    if (!account || !canSave) return
    setSaving(true)
    try {
      await api.updateChannelMonitorConfig(account.id, {
        enabled,
        interval_minutes: interval,
        model: model.trim(),
      })
      showToast(enabled ? '渠道监控已保存，首次探测已排队' : '渠道监控已关闭')
      onSaved?.()
      onClose()
    } catch (cause) {
      showToast(`保存失败: ${getErrorMessage(cause)}`, 'error')
    } finally {
      setSaving(false)
    }
  }

  if (!account) return null

  return (
    <Modal
      show={show}
      title={
        <span className="inline-flex items-center gap-2">
          <RadioTower className="size-5 text-primary" />
          渠道监控配置
        </span>
      }
      onClose={() => { if (!saving) onClose() }}
      contentClassName="sm:max-w-[560px]"
      footer={
        <>
          <Button variant="outline" onClick={onClose} disabled={saving}>取消</Button>
          <Button onClick={() => void handleSave()} disabled={!canSave}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
            保存配置
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <p className="text-sm leading-relaxed text-muted-foreground">
          配置该 Responses API 渠道的监控开关、健康探测间隔与探测模型。
        </p>
        <div className="flex items-center justify-between gap-3 rounded-lg border border-border/70 bg-muted/25 p-3.5">
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-foreground">
              {account.name || account.email || `ID ${account.id}`}
            </p>
            <p className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground">
              {account.base_url || `账号 ID ${account.id}`}
            </p>
          </div>
          <Badge variant="outline">Responses API</Badge>
        </div>

        <StateShell
          loading={loading}
          error={error || null}
          onRetry={() => setReloadKey((value) => value + 1)}
          loadingTitle="正在读取监控配置"
          loadingDescription="同步探测模型与当前开关状态"
          errorTitle="监控配置加载失败"
        >
          {config ? (
            <div className="space-y-4">
              <div className="flex items-center justify-between gap-4 rounded-lg border border-border bg-card p-4 shadow-2xs">
                <div className="min-w-0 space-y-1">
                  <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                    <Activity className="size-4 text-emerald-500" />
                    启用渠道监控
                  </div>
                  <p className="text-xs leading-relaxed text-muted-foreground">
                    关闭后停止探测并隐藏监控卡片，已有历史与设置会保留。
                  </p>
                </div>
                <Switch
                  checked={enabled}
                  onCheckedChange={setEnabled}
                  aria-label="启用渠道监控"
                />
              </div>

              <div className="grid gap-4 rounded-lg border border-border bg-card p-4 shadow-2xs sm:grid-cols-2">
                <div className="space-y-2">
                  <label htmlFor="channel-monitor-interval" className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
                    <Clock3 className="size-3.5 text-amber-500" />
                    健康探测间隔
                  </label>
                  <div className="relative">
                    <Input
                      id="channel-monitor-interval"
                      type="number"
                      min={1}
                      max={60}
                      step={1}
                      value={intervalInput}
                      onChange={(event) => setIntervalInput(event.target.value)}
                      disabled={!enabled}
                      aria-invalid={!intervalValid}
                      className="pr-12 tabular-nums"
                    />
                    <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-muted-foreground">分钟</span>
                  </div>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">
                    允许 1–60 分钟{enabled && intervalValid ? `，约 ${dailyCalls} 次/天` : ''}。
                  </p>
                </div>

                <div className="space-y-2">
                  <label htmlFor="channel-monitor-model" className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
                    <Gauge className="size-3.5 text-sky-500" />
                    探测模型
                  </label>
                  <Input
                    id="channel-monitor-model"
                    list={modelListId}
                    value={model}
                    onChange={(event) => setModel(event.target.value)}
                    placeholder="选择或输入文本模型"
                    disabled={!enabled}
                    aria-invalid={!modelValid}
                    autoComplete="off"
                  />
                  <datalist id={modelListId}>
                    {config.available_models.map((item) => <option key={item} value={item} />)}
                  </datalist>
                  <p className="text-[11px] leading-relaxed text-muted-foreground">
                    可选择账号模型，也可输入上游支持的其他文本模型。
                  </p>
                </div>
              </div>

              <div className="rounded-lg border border-sky-500/20 bg-sky-500/5 px-3 py-2.5 text-xs leading-relaxed text-muted-foreground">
                健康探测走真实 Responses 请求；倍率通过上游 <code className="font-mono text-foreground">/v1/sub2api/billing</code> 每 30 分钟独立探测。倍率不支持或失败不会影响渠道健康状态。
              </div>
            </div>
          ) : null}
        </StateShell>
      </div>
    </Modal>
  )
}
