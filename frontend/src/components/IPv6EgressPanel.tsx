import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, CheckCircle2, Globe, RefreshCw } from 'lucide-react'
import { api } from '../api'
import { ipv6PoolCounts, type IPv6EgressConfig, type IPv6EgressData } from '../lib/ipv6Egress'
import { getErrorMessage } from '../utils/error'
import { Button } from './ui/button'
import { Switch } from './ui/switch'
import { DraftNumberInput } from './ui/draft-number-input'
import { Textarea } from './ui/textarea'
import { Input } from './ui/input'

export default function IPv6EgressPanel() {
  const { t } = useTranslation()
  const [data, setData] = useState<IPv6EgressData | null>(null)
  const [draft, setDraft] = useState<IPv6EgressConfig | null>(null)
  const [sources, setSources] = useState('')
  const [search, setSearch] = useState('')
  const [error, setError] = useState('')
  const [refreshError, setRefreshError] = useState('')
  const [busy, setBusy] = useState(false)
  const [saved, setSaved] = useState(false)
  const dirty = useRef(false)
  const mounted = useRef(true)
  function accept(next: IPv6EgressData, reset = false) {
    setData(next)
    if (reset || !dirty.current) { setDraft(next.config); setSources(next.config.source_ips.join('\n')); dirty.current = false }
  }
  useEffect(() => {
    mounted.current = true
    const controller = new AbortController()
    let pending = false
    async function refresh() {
      if (pending) return
      pending = true
      try { const next = await api.getIPv6Egress(controller.signal); if (mounted.current) { accept(next); setRefreshError('') } }
      catch (e) { if (!controller.signal.aborted && mounted.current) setRefreshError(getErrorMessage(e)) }
      finally { pending = false }
    }
    void refresh()
    const timer = window.setInterval(() => void refresh(), 10000)
    return () => { mounted.current = false; controller.abort(); window.clearInterval(timer) }
  }, [])
  function change(patch: Partial<IPv6EgressConfig>) { if (draft) { dirty.current = true; setSaved(false); setDraft({ ...draft, ...patch }) } }
  async function save(enabled?: boolean) {
    if (!draft || !data || busy) return
    setBusy(true); setError(''); setSaved(false)
    try {
      const toggling = enabled !== undefined
      const next = await api.setIPv6Egress(toggling ? { ...data.config, enabled } : { ...draft, source_ips: sources.split(/[\s,]+/).filter(Boolean) })
      if (mounted.current) {
        accept(next, !toggling)
        if (toggling && dirty.current) setDraft(previous => previous ? { ...previous, enabled: next.config.enabled, revision: next.config.revision } : next.config)
        setSaved(!dirty.current)
      }
    } catch (e) { if (mounted.current) setError(getErrorMessage(e)) }
    finally { if (mounted.current) setBusy(false) }
  }
  const counts = data ? ipv6PoolCounts(data) : null
  const rows = data?.bindings.filter(row => `${row.account_id} ${data.account_names?.[row.account_id] || ''} ${row.ip}`.toLowerCase().includes(search.toLowerCase())) || []
  return <section aria-labelledby="ipv6-egress-heading" className="rounded-xl border border-border bg-card p-4 sm:p-5 space-y-5">
    <div className="flex items-start justify-between gap-4">
      <div className="min-w-0 space-y-1.5"><h2 id="ipv6-egress-heading" className="flex items-center gap-2 font-semibold"><Globe className="size-4 shrink-0" />{t('ipv6Egress.title')}</h2><p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{t('ipv6Egress.description')}</p></div>
      <Switch aria-label={t('ipv6Egress.enable')} checked={data?.config.enabled ?? false} onCheckedChange={value => void save(value)} disabled={!draft || busy || !!data?.resin_enabled} />
    </div>
    {(error || refreshError) && <p role="alert" className="flex items-start gap-2 text-sm text-red-700 dark:text-red-300"><AlertTriangle className="size-4 shrink-0 mt-0.5" />{error || refreshError}</p>}
    {!data || !draft ? <p role="status" className="text-sm text-muted-foreground">{t('ipv6Egress.loading')}</p> : <>
      <div className="flex flex-wrap gap-x-5 gap-y-2 text-sm tabular-nums"><strong>{t(data.config.enabled ? 'ipv6Egress.on' : 'ipv6Egress.off')}</strong><span>{t('ipv6Egress.counts', counts!)}</span></div>
      {data.resin_enabled && <p className="text-sm text-amber-800 dark:text-amber-200">{t('ipv6Egress.resin')}</p>}
      {!data.local_ips.length && <p className="text-sm text-amber-800 dark:text-amber-200">{t('ipv6Egress.noLocal')}</p>}
      <div className="grid gap-5 lg:grid-cols-2">
        <div className="space-y-2"><label htmlFor="ipv6-egress-sources" className="text-sm font-medium">{t('ipv6Egress.sources')}</label><Textarea id="ipv6-egress-sources" rows={4} value={sources} disabled={busy} onChange={e => { dirty.current = true; setSaved(false); setSources(e.target.value) }} placeholder={t('ipv6Egress.sourcesPlaceholder')} className="font-mono text-xs break-all" /><p className="text-xs leading-relaxed text-muted-foreground">{t('ipv6Egress.sourcesHint')}</p></div>
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2"><label htmlFor="ipv6-egress-cooldown" className="text-sm font-medium">{t('ipv6Egress.cooldown')}</label><DraftNumberInput id="ipv6-egress-cooldown" value={draft.cooldown_seconds} min={30} max={86400} disabled={busy} onValueChange={cooldown_seconds => change({ cooldown_seconds })} /></div>
            <div className="space-y-2"><label htmlFor="ipv6-egress-attempts" className="text-sm font-medium">{t('ipv6Egress.attempts')}</label><DraftNumberInput id="ipv6-egress-attempts" value={draft.max_attempts} min={1} max={5} disabled={busy} onValueChange={max_attempts => change({ max_attempts })} /></div>
          </div>
          <label className="flex items-center justify-between gap-4 text-sm"><span>{t('ipv6Egress.retry5xx')}</span><Switch checked={draft.retry_5xx} disabled={busy} onCheckedChange={retry_5xx => change({ retry_5xx })} /></label>
          <p className="text-xs leading-relaxed text-muted-foreground">{t('ipv6Egress.retryHint')}</p>
          <div className="flex flex-wrap items-center gap-3"><Button disabled={busy || !dirty.current} onClick={() => void save()}>{busy && <RefreshCw className="size-4 animate-spin" />}{t('ipv6Egress.save')}</Button>{saved && <span role="status" className="flex items-center gap-1.5 text-sm text-emerald-800 dark:text-emerald-300"><CheckCircle2 className="size-4" />{t('ipv6Egress.saved')}</span>}</div>
        </div>
      </div>
      <div className="border-t border-border pt-4 space-y-3">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"><h3 className="text-sm font-semibold">{t('ipv6Egress.bindings', { count: data.bindings.length })}</h3><Input value={search} onChange={e => setSearch(e.target.value)} aria-label={t('ipv6Egress.search')} placeholder={t('ipv6Egress.search')} className="sm:max-w-xs" /></div>
        {!rows.length && <p className="text-sm text-muted-foreground">{t(search ? 'ipv6Egress.noMatch' : 'ipv6Egress.empty')}</p>}
        <div className="max-h-96 overflow-y-auto divide-y divide-border">
          {rows.map(row => { const cooling = data.cooldowns.some(item => item.ip === row.ip); return <div key={row.account_id} className="grid gap-1.5 py-3 sm:grid-cols-2 sm:gap-4">
            <div className="min-w-0"><p className="break-all text-sm font-medium">{data.account_names?.[row.account_id] || `#${row.account_id}`}</p><p className="break-all font-mono text-xs text-muted-foreground mt-1">{row.ip}</p></div>
            <div className="text-xs text-muted-foreground space-y-1"><p className={cooling ? 'text-amber-800 dark:text-amber-200' : ''}>{t(cooling ? 'ipv6Egress.waiting' : 'ipv6Egress.assigned')} · {t('ipv6Egress.rotations', { count: row.rotations })}</p><p className="break-words">{t(`ipv6Egress.reason.${row.reason}`, { defaultValue: row.reason })} · <time className="tabular-nums">{new Date(row.changed_at * 1000).toLocaleString()}</time></p></div>
          </div> })}
        </div>
      </div>
    </>}
  </section>
}
