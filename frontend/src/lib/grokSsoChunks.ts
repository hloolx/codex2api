// 与后端 grokSSOImportMaxTokens 对齐。SSO 每条都要跑完 device flow，
// 单次请求超过这个数会被整批拒绝，所以大量导入必须在客户端先切开。
export const GROK_SSO_IMPORT_CHUNK_SIZE = 50

export interface GrokSSOImportSplit {
  total: number
  payloads: string[]
}

// splitGrokSSOImport 把粘贴内容或 sso.txt 切成后端可接受的批次。
// JSON（{"accounts":[...]}）按账号切，纯文本按非空行切。
export function splitGrokSSOImport(
  raw: string,
  chunkSize = GROK_SSO_IMPORT_CHUNK_SIZE,
): GrokSSOImportSplit {
  const size = chunkSize > 0 ? chunkSize : GROK_SSO_IMPORT_CHUNK_SIZE
  const trimmed = raw.trim()
  if (!trimmed) return { total: 0, payloads: [] }

  if (trimmed.startsWith("{")) {
    let doc: { accounts?: unknown }
    try {
      doc = JSON.parse(trimmed) as { accounts?: unknown }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      throw new Error(`解析 SSO 导入 JSON 失败: ${message}`)
    }
    const accounts = Array.isArray(doc.accounts) ? doc.accounts : []
    const payloads: string[] = []
    for (let i = 0; i < accounts.length; i += size) {
      payloads.push(JSON.stringify({ accounts: accounts.slice(i, i + size) }))
    }
    return { total: accounts.length, payloads }
  }

  const lines = trimmed
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"))
  const payloads: string[] = []
  for (let i = 0; i < lines.length; i += size) {
    payloads.push(lines.slice(i, i + size).join("\n"))
  }
  return { total: lines.length, payloads }
}
