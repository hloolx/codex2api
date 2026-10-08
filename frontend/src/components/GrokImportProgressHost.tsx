import { useEffect, useState } from "react"
import OperationProgressToast from "./OperationProgressToast"
import {
  dismissGrokImportJob,
  getGrokImportJob,
  subscribeGrokImportJob,
  type GrokImportJobSnapshot,
} from "../lib/grokImportJob"
import type { OperationProgressState } from "../hooks/useOperationProgress"

function toProgress(job: GrokImportJobSnapshot): OperationProgressState {
  return {
    show: true,
    action: "grok_import",
    title: job.title,
    current: job.current,
    total: job.total,
    success: job.success,
    failed: job.failed,
    banned: 0,
    rateLimited: 0,
    deleted: 0,
    done: job.done,
    message: job.error || (job.proxyWarnings.length > 0 ? job.proxyWarnings.join(" ") : undefined),
  }
}

// 挂在 Layout 上，Grok 账号页卸载后进度条还在。
export default function GrokImportProgressHost() {
  const [job, setJob] = useState<GrokImportJobSnapshot | null>(() => getGrokImportJob())
  useEffect(() => subscribeGrokImportJob(setJob), [])
  if (!job || job.dismissed) return null
  return (
    <OperationProgressToast
      progress={toProgress(job)}
      onClose={dismissGrokImportJob}
      closable={job.done}
    />
  )
}
