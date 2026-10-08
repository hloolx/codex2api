import { useTranslation } from 'react-i18next'
import { Info } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { formatImageStudioQuota } from '@/lib/imageStudioQuota'

const UNIT_MODE_TITLE: Record<string, string> = {
  per_image: 'settings.pricing.imageBilling.perImage',
  per_video: 'settings.pricing.mediaBilling.perVideo',
  per_second: 'settings.pricing.mediaBilling.perSecond',
}

// ImageBillingCost 展示按单位计费(按张/按次/按秒)的用户金额与计费依据;
// count 是落库的计费单位数(张/视频数/秒),与 mode 对应。
export function ImageBillingCost({ count = 0, unitPrice = 0, userBilled, accountBilled, mode = 'per_image', media = false }: {
  count?: number
  unitPrice?: number
  userBilled: number
  accountBilled?: number
  mode?: string
  /** Grok Imagine rows: upstream cost is per media unit rather than token-based. */
  media?: boolean
}) {
  const { t } = useTranslation()
  const values = { count, seconds: count, price: formatImageStudioQuota(unitPrice), total: formatImageStudioQuota(userBilled) }
  const formula = mode === 'per_second' ? 'usage.videoBillingPerSecond' : mode === 'per_video' ? 'usage.videoBillingPerVideo' : 'usage.imageBillingFormula'
  const noCharge = mode === 'per_image' ? 'usage.imageBillingNoCharge' : 'usage.videoBillingNoCharge'
  const upstream = media || mode !== 'per_image' ? 'usage.mediaUpstreamCost' : 'usage.imageUpstreamCost'
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="inline-flex items-center gap-1.5 rounded-md px-1.5 py-1 text-[13px] font-semibold tabular-nums text-emerald-600 hover:bg-muted/60 dark:text-emerald-400">
          {formatImageStudioQuota(userBilled)}<Info className="size-3.5 text-muted-foreground" />
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 space-y-1.5 p-3 text-xs">
        <p className="font-semibold">{t(UNIT_MODE_TITLE[mode] ?? UNIT_MODE_TITLE.per_image)}</p>
        <p>{t(formula, values)}</p>
        {count === 0 ? <p>{t(noCharge)}</p> : null}
        {accountBilled != null ? <p className="border-t pt-1.5">{t(upstream, { amount: formatImageStudioQuota(accountBilled) })}</p> : null}
      </TooltipContent>
    </Tooltip>
  )
}
