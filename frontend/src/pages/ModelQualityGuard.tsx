import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CheckCircle2, Circle, CircleAlert, RefreshCw, X, XCircle } from 'lucide-react'
import { api } from '../api'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import { Switch } from '../components/ui/switch'
import Pagination from '../components/Pagination'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import type { ModelQualityConfig, ModelQualityData } from '../lib/modelQuality'
import './model-quality.css'

export default function ModelQualityGuard() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [data, setData] = useState<ModelQualityData>()
  const [error, setError] = useState('')
  const [saveError, setSaveError] = useState('')
  const [saving, setSaving] = useState(false)
  const [loading, setLoading] = useState(true)
  const [retesting, setRetesting] = useState('')
  const [page, setPage] = useState(1)
  const [search, setSearch] = useState('')
  const [query, setQuery] = useState('')
  const [revision, setRevision] = useState(0)
  const requestVersion = useRef(0)
  const alive = useRef(true)
  const changing = useRef(false)
  useEffect(() => { alive.current = true; return () => { alive.current = false; requestVersion.current++ } }, [])
  useEffect(() => { const timer = setTimeout(() => { setQuery(search); setPage(1) }, 250); return () => clearTimeout(timer) }, [search])
  const reload = useCallback(() => setRevision(value => value + 1), [])
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout>
    async function load() {
      if (changing.current) { timer = setTimeout(load, 5000); return }
      const version = ++requestVersion.current
      try {
        const result = await api.getModelQuality(page, query, controller.signal)
        if (controller.signal.aborted || version !== requestVersion.current) return
        setData(result); setError('')
        const lastPage = Math.max(1, Math.ceil(result.total / result.page_size))
        if (page > lastPage) setPage(lastPage)
      } catch (err) {
        if (!controller.signal.aborted && version === requestVersion.current) setError(getErrorMessage(err))
      } finally {
        if (!controller.signal.aborted) { setLoading(false); timer = setTimeout(load, 5000) }
      }
    }
    setLoading(true)
    void load()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [page, query, revision])

  async function save(config: ModelQualityConfig) {
    if (changing.current) return
    changing.current = true; requestVersion.current++; setSaving(true); setSaveError('')
    try {
      const result = await api.setModelQuality(config)
      if (!alive.current) return
      setData(current => current ? { ...current, config: result.config } : current)
      showToast(t('modelQuality.saved'))
    } catch (err) { if (alive.current) setSaveError(getErrorMessage(err)) }
    finally { changing.current = false; if (alive.current) { setSaving(false); reload() } }
  }

  async function retest(id: number, model: string) {
    setRetesting(`${id}:${model}`)
    try { await api.retestModelQuality(id, model); if (alive.current) { showToast(t('modelQuality.queued')); reload() } }
    catch (err) { if (alive.current) showToast(getErrorMessage(err), 'error') }
    finally { if (alive.current) setRetesting('') }
  }

  const config = data?.config
  const time = (seconds: number) => seconds ? new Date(seconds * 1000).toLocaleString() : t('modelQuality.notYet')
  return <div className="model-quality" aria-busy={loading || saving}>
    {error || saveError ? <div role="alert" className="quality-test-error">{saveError || error}<Button size="sm" variant="outline" onClick={() => { setSaveError(''); reload() }}>{t('common.retry')}</Button></div> : null}
    <section className="model-quality-settings" aria-labelledby="model-quality-title">
      <div className="model-quality-toggle">
        <div><h2 id="model-quality-title">{t('modelQuality.title')}</h2><p id="model-quality-description">{t('modelQuality.description')}</p></div>
        <div className="model-quality-switch"><span>{t(saving ? 'common.saving' : config?.enabled ? 'modelQuality.enabled' : 'modelQuality.disabled')}</span><Switch aria-label={t('modelQuality.title')} aria-describedby="model-quality-description" checked={config?.enabled ?? false} disabled={!config || saving || (!config.enabled && config.models.length === 0)} onCheckedChange={enabled => config && void save({ ...config, enabled })} /></div>
      </div>
      <div className="model-quality-models">
        <label htmlFor="quality-add-model">{t('modelQuality.models')}</label>
        <div className="model-quality-model-controls"><Select id="quality-add-model" value="" placeholder={t('modelQuality.addModel')} disabled={!data || saving || (config?.models.length ?? 0) >= 20} options={(data?.models ?? []).filter(model => !config?.models.includes(model)).map(model => ({ value: model, label: model }))} onValueChange={model => config && void save({ ...config, models: [...config.models, model] })} />
          <span>{t('modelQuality.interval')}</span></div>
        <div className="model-quality-selected">{config?.models.map(model => <span key={model}>{model}<Button size="icon-xs" variant="ghost" disabled={saving} aria-label={t('modelQuality.removeModel', { model })} onClick={() => { const models = config.models.filter(item => item !== model); void save({ ...config, models, enabled: config.enabled && models.length > 0 }) }}><X className="size-3.5" /></Button></span>)}</div>
        <p>{t(config?.models.length ? 'modelQuality.unselectedHint' : 'modelQuality.selectHint')}</p>
      </div>
      <p className="model-quality-policy"><CircleAlert className="size-4" />{t('modelQuality.policy')}</p>
      {!config?.enabled ? <p className="model-quality-off">{t('modelQuality.offHint')}</p> : null}
    </section>
    <section className="model-quality-results" aria-labelledby="quality-accounts-title">
      <div className="model-quality-toolbar"><h2 id="quality-accounts-title">{t('modelQuality.accounts')}</h2><div><Input value={search} onChange={event => setSearch(event.target.value)} placeholder={t('modelQuality.search')} aria-label={t('modelQuality.search')} /><Button size="sm" variant="outline" onClick={reload} disabled={loading}><RefreshCw className={loading ? 'size-4 animate-spin' : 'size-4'} />{t('common.refresh')}</Button></div></div>
      <div className="model-quality-legend"><span className="model-quality-pass"><CheckCircle2 />{t('modelQuality.pass')}</span><span className="model-quality-fail"><XCircle />{t('modelQuality.fail')}</span><span><Circle />{t('modelQuality.pending')}</span></div>
      {!data ? <p role="status" className="model-quality-empty">{t(error ? 'modelQuality.loadFailed' : 'common.loading')}</p> : data.accounts.length === 0 ? <p className="model-quality-empty">{t(query ? 'modelQuality.noMatch' : 'modelQuality.empty')}</p> : <div className="model-quality-account-list">{data.accounts.map(account => <article key={account.id} className="model-quality-account">
        <div className="model-quality-account-name"><strong>{account.name}</strong><span>#{account.id}{!account.available ? ` · ${t('modelQuality.unavailable')}` : ''}</span></div>
        {account.states.length === 0 ? <p>{t('modelQuality.noSelected')}</p> : <div className="model-quality-verdicts">{account.states.map(state => {
          const active = Boolean(config?.enabled)
          const status = active ? state.status : 'pending'
          const Icon = status === 'pass' ? CheckCircle2 : status === 'fail' ? XCircle : Circle
          return <div key={state.model} className="model-quality-verdict">
            <div className="model-quality-verdict-heading"><strong>{state.model}</strong><span className={`model-quality-${status}`}><Icon />{t(active ? `modelQuality.${status}` : 'modelQuality.notMonitoring')}</span></div>
            <div className="model-quality-verdict-detail"><span>{t('modelQuality.lastCheck', { time: time(state.checked_at) })}</span><span>{state.running ? t('modelQuality.running') : !active ? t('modelQuality.offRow') : state.status === 'unsupported' ? t('modelQuality.unsupportedHint') : !account.available ? t('modelQuality.waitingAccount') : t('modelQuality.nextCheck', { time: state.next_check_at ? time(state.next_check_at) : t('modelQuality.soon') })}</span></div>
            {state.reason && active ? <p className={state.last_outcome === 'error' ? 'model-quality-inconclusive' : ''}>{state.last_outcome === 'error' ? <CircleAlert className="size-3.5" /> : null}{state.reason}</p> : null}
            <Button size="sm" variant="outline" disabled={!active || state.running || !account.available || state.status === 'unsupported' || retesting !== ''} onClick={() => void retest(account.id, state.model)}><RefreshCw className={state.running || retesting === `${account.id}:${state.model}` ? 'size-3.5 animate-spin' : 'size-3.5'} />{t('modelQuality.retest')}</Button>
          </div>
        })}</div>}
      </article>)}</div>}
      {data ? <Pagination page={page} totalPages={Math.ceil(data.total / data.page_size)} onPageChange={setPage} totalItems={data.total} pageSize={data.page_size} /> : null}
    </section>
  </div>
}
