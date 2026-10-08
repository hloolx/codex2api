import type { GrokSSOImportItem } from "../types"

// Grok 导入任务挂在模块上，而不是账号页的组件状态里。
// 离开 Grok 账号页后组件会卸载，进行中的分片请求和右上角进度必须继续。

export interface GrokImportChunkResult {
  total: number
  imported: number
  failed: number
  items: GrokSSOImportItem[]
  proxies_imported?: number
  proxies_skipped?: number
  proxy_warning?: string
}

export type GrokImportJobSource = "sso-paste" | "pool"

export interface GrokImportJobSnapshot {
  seq: number
  source: GrokImportJobSource
  title: string
  current: number
  total: number
  success: number
  failed: number
  items: GrokSSOImportItem[]
  chunkDone: number
  chunkTotal: number
  running: boolean
  done: boolean
  dismissed: boolean
  finishedAt: number
  error?: string
  proxyImported: number
  proxySkipped: number
  proxyWarnings: string[]
  proxyCarried: boolean
}

export interface StartGrokImportJobInput {
  title: string
  totalItems: number
  source: GrokImportJobSource
  chunks: Array<() => Promise<GrokImportChunkResult>>
}

type Listener = (job: GrokImportJobSnapshot | null) => void

let seq = 0
let snapshot: GrokImportJobSnapshot | null = null
let running = false
const listeners = new Set<Listener>()

function publish(next: GrokImportJobSnapshot | null) {
  snapshot = next
  for (const listener of listeners) listener(next)
}

export function getGrokImportJob(): GrokImportJobSnapshot | null {
  return snapshot
}

export function isGrokImportRunning(): boolean {
  return running
}

export function subscribeGrokImportJob(listener: Listener): () => void {
  listeners.add(listener)
  listener(snapshot)
  return () => {
    listeners.delete(listener)
  }
}

// 完成后才允许关掉浮层。进行中关掉会让人以为任务停了，请求其实还在后台跑。
export function dismissGrokImportJob() {
  if (!snapshot || snapshot.running || snapshot.dismissed) return
  publish({ ...snapshot, dismissed: true })
}

export function resetGrokImportJobForTests() {
  seq += 1
  running = false
  snapshot = null
  listeners.clear()
}

export function startGrokImportJob(input: StartGrokImportJobInput): Promise<void> {
  if (running) {
    return Promise.reject(new Error("已有 Grok 导入任务在进行"))
  }
  if (input.chunks.length === 0) {
    return Promise.resolve()
  }
  const jobSeq = ++seq
  running = true
  const initial: GrokImportJobSnapshot = {
    seq: jobSeq,
    source: input.source,
    title: input.title,
    current: 0,
    total: input.totalItems,
    success: 0,
    failed: 0,
    items: [],
    chunkDone: 0,
    chunkTotal: input.chunks.length,
    running: true,
    done: false,
    dismissed: false,
    finishedAt: 0,
    proxyImported: 0,
    proxySkipped: 0,
    proxyWarnings: [],
    proxyCarried: false,
  }
  publish(initial)

  const task = (async () => {
    const merged: GrokImportJobSnapshot = { ...initial, items: [] }
    try {
      for (let i = 0; i < input.chunks.length; i++) {
        if (jobSeq !== seq) return
        const res = await input.chunks[i]()
        if (jobSeq !== seq) return
        merged.current += res.total ?? 0
        merged.success += res.imported ?? 0
        merged.failed += res.failed ?? 0
        merged.items = merged.items.concat(res.items ?? [])
        merged.total = Math.max(input.totalItems, merged.current)
        merged.chunkDone = i + 1
        if (res.proxies_imported !== undefined) {
          merged.proxyCarried = true
          merged.proxyImported += res.proxies_imported
          merged.proxySkipped += res.proxies_skipped ?? 0
          const warning = res.proxy_warning?.trim()
          if (warning && !merged.proxyWarnings.includes(warning)) {
            merged.proxyWarnings.push(warning)
          }
        }
        publish({ ...merged, items: merged.items.slice() })
      }
      if (jobSeq !== seq) return
      running = false
      publish({
        ...merged,
        items: merged.items.slice(),
        running: false,
        done: true,
        finishedAt: Date.now(),
      })
    } catch (err) {
      if (jobSeq !== seq) return
      running = false
      const message = err instanceof Error && err.message ? err.message : String(err)
      publish({
        ...merged,
        items: merged.items.slice(),
        running: false,
        done: true,
        finishedAt: Date.now(),
        error: message,
        total: Math.max(input.totalItems, merged.current),
      })
    }
  })()

  return task
}
