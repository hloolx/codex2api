import { useTranslation } from 'react-i18next'
import type { CodexClientVersionTarget } from '../types'
import { formatRelativeTime } from '../utils/time'

function CodexVersionRow({ target }: { target: CodexClientVersionTarget }) {
  const { t } = useTranslation()
  const pair = target.pairs[0]
  const source = pair?.source ?? ''
  const label = target.client_kind === 'codex-desktop' ? 'Desktop' : 'VSCode'
  const failed = target.status === 'error' || target.status === 'stale'
  return (
    <tr className="border-t border-border/50 align-top">
      <td className="py-2 pr-3">{label}<br /><span className="font-mono">{target.target_platform}</span></td>
      <td className="py-2 pr-3 font-mono">
        {pair ? <>{pair.app_version}<br />CLI {pair.cli_version}</> : t('settings.codexClientVersions.unavailable')}
      </td>
      <td className="py-2 pr-3">{source ? t(`settings.codexClientVersions.sources.${source}`, { defaultValue: source }) : '—'}</td>
      <td className="py-2">
        <span className={failed ? 'text-destructive' : undefined}>
          {target.status ? t(`settings.codexClientVersions.statuses.${target.status}`, { defaultValue: target.status }) : '—'}
        </span>
        {target.checked_at > 0 && (
          <><br />{t('settings.codexClientVersions.checkedAt')} {formatRelativeTime(new Date(target.checked_at).toISOString(), { variant: 'compact' })}</>
        )}
        {target.error && <><br /><span className="break-all text-destructive" title={target.error}>{target.error}</span></>}
      </td>
    </tr>
  )
}

export default function CodexClientVersionsPanel({ targets }: { targets: CodexClientVersionTarget[] }) {
  const { t } = useTranslation()
  if (!targets.length) return <p className="text-xs text-muted-foreground">{t('settings.codexClientVersions.empty')}</p>
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs text-muted-foreground">
        <thead className="text-foreground/80">
          <tr>
            <th className="pb-2 pr-3 font-medium">{t('settings.codexClientVersions.platform')}</th>
            <th className="pb-2 pr-3 font-medium">{t('settings.codexClientVersions.pair')}</th>
            <th className="pb-2 pr-3 font-medium">{t('settings.codexClientVersions.source')}</th>
            <th className="pb-2 font-medium">{t('settings.codexClientVersions.status')}</th>
          </tr>
        </thead>
        <tbody>{targets.map((target) => <CodexVersionRow key={`${target.client_kind}/${target.target_platform}`} target={target} />)}</tbody>
      </table>
    </div>
  )
}
