export function normalizeVersion(version?: string | null): string {
  return (version || '')
    .trim()
    .replace(/^refs\/tags\//i, '')
    .replace(/^v/i, '')
}

export function versionLabel(version?: string | null): string | null {
  if (!version) return null
  return version.startsWith('v') || version.startsWith('V') ? version : `v${version}`
}

function parseVersion(version: string): [number, number, number] | null {
  const normalized = normalizeVersion(version).split(/[+-]/, 1)[0]
  if (!normalized) return null
  const parts = normalized.split('.')
  if (parts.length === 0 || parts.length > 3) return null

  const parsed: [number, number, number] = [0, 0, 0]
  for (let i = 0; i < parts.length; i += 1) {
    if (!/^\d+$/.test(parts[i])) return null
    parsed[i] = Number(parts[i])
  }
  return parsed
}

function isReleaseVersion(version?: string | null): boolean {
  const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(normalizeVersion(version))
  if (!match || !match.slice(1, 4).every((part) => Number.isSafeInteger(Number(part)))) return false
  return !match[4]?.split('.').some((part) => /^0\d+$/.test(part))
}

export function resolveBuildVersions(frontendVersion: string, backendVersion?: string | null) {
  const versionMismatch = isReleaseVersion(frontendVersion)
    && isReleaseVersion(backendVersion)
    && normalizeVersion(frontendVersion) !== normalizeVersion(backendVersion)

  return {
    frontendVersion,
    // Preserve the existing release label when it already matches the service.
    currentVersion: versionMismatch ? versionLabel(normalizeVersion(backendVersion))! : frontendVersion,
    versionMismatch,
  }
}

function compareVersions(a: string, b: string): number {
  const parsedA = parseVersion(a)
  const parsedB = parseVersion(b)
  if (!parsedA && !parsedB) return normalizeVersion(a).localeCompare(normalizeVersion(b))
  if (!parsedA) return -1
  if (!parsedB) return 1

  for (let i = 0; i < 3; i += 1) {
    if (parsedA[i] < parsedB[i]) return -1
    if (parsedA[i] > parsedB[i]) return 1
  }
  return 0
}

export function hasRemoteUpdate(currentVersion: string, latestVersion?: string | null): boolean {
  if (!latestVersion || currentVersion === 'dev') return false
  return compareVersions(currentVersion, latestVersion) < 0
}
