// 账号列表的页面偏好：排序（标签页级）与自动刷新间隔（浏览器级）。
//
// 排序存 sessionStorage：同一标签页内切换菜单、F5 刷新都会保留，关闭标签页或
// 新开标签页回到默认排序，避免“记住了但自己不知道”的长期意外。
// 自动刷新间隔是用户显式开启的选项且按钮上始终可见，存 localStorage。

export const ACCOUNT_LIST_SORT_STORAGE_KEY = "codex2api:accounts:sort";
export const ACCOUNT_AUTO_REFRESH_STORAGE_KEY =
  "codex2api:accounts:auto-refresh-seconds";

export const ACCOUNT_LIST_SORT_KEYS = [
  "requests",
  "today",
  "usage",
  "importTime",
  "schedulerPriority",
  "group",
  "id",
] as const;
export type AccountListSortKey = (typeof ACCOUNT_LIST_SORT_KEYS)[number];
export type AccountListSortDir = "asc" | "desc";
export type AccountListSort = {
  key: AccountListSortKey | null;
  dir: AccountListSortDir;
};

// 0 表示关闭自动刷新。
export const ACCOUNT_AUTO_REFRESH_INTERVALS = [0, 5, 10, 30] as const;
export type AccountAutoRefreshSeconds =
  (typeof ACCOUNT_AUTO_REFRESH_INTERVALS)[number];

// page-stats 会对近 7 天 usage_logs 做多条聚合且没有服务端缓存，
// 自动刷新时最多按这个间隔重拉，短间隔只刷新列表本身。
export const ACCOUNT_PAGE_STATS_MIN_REFRESH_MS = 30_000;

type PreferenceStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

const DEFAULT_SORT: AccountListSort = { key: null, dir: "desc" };

function resolveStorage(
  kind: "session" | "local",
  storage?: PreferenceStorage,
): PreferenceStorage | null {
  if (storage) return storage;
  if (typeof window === "undefined") return null;
  try {
    return kind === "session" ? window.sessionStorage : window.localStorage;
  } catch {
    return null;
  }
}

function isSortKey(value: unknown): value is AccountListSortKey {
  return (
    typeof value === "string" &&
    (ACCOUNT_LIST_SORT_KEYS as readonly string[]).includes(value)
  );
}

export function readAccountListSort(
  storage?: PreferenceStorage,
): AccountListSort {
  try {
    const raw = resolveStorage("session", storage)?.getItem(
      ACCOUNT_LIST_SORT_STORAGE_KEY,
    );
    if (!raw) return { ...DEFAULT_SORT };
    const parsed = JSON.parse(raw) as { key?: unknown; dir?: unknown };
    if (!isSortKey(parsed.key)) return { ...DEFAULT_SORT };
    return { key: parsed.key, dir: parsed.dir === "asc" ? "asc" : "desc" };
  } catch {
    return { ...DEFAULT_SORT };
  }
}

export function writeAccountListSort(
  sort: AccountListSort,
  storage?: PreferenceStorage,
): void {
  try {
    const target = resolveStorage("session", storage);
    if (!target) return;
    if (sort.key === null) {
      target.removeItem(ACCOUNT_LIST_SORT_STORAGE_KEY);
      return;
    }
    target.setItem(
      ACCOUNT_LIST_SORT_STORAGE_KEY,
      JSON.stringify({ key: sort.key, dir: sort.dir }),
    );
  } catch {
    // Keep the in-memory sort working when storage is unavailable.
  }
}

export function normalizeAccountAutoRefreshSeconds(
  value: unknown,
): AccountAutoRefreshSeconds {
  const parsed = typeof value === "number" ? value : Number(value);
  return (ACCOUNT_AUTO_REFRESH_INTERVALS as readonly number[]).includes(parsed)
    ? (parsed as AccountAutoRefreshSeconds)
    : 0;
}

export function readAccountAutoRefreshSeconds(
  storage?: PreferenceStorage,
): AccountAutoRefreshSeconds {
  try {
    const raw = resolveStorage("local", storage)?.getItem(
      ACCOUNT_AUTO_REFRESH_STORAGE_KEY,
    );
    return raw ? normalizeAccountAutoRefreshSeconds(raw) : 0;
  } catch {
    return 0;
  }
}

export function writeAccountAutoRefreshSeconds(
  seconds: AccountAutoRefreshSeconds,
  storage?: PreferenceStorage,
): void {
  try {
    const target = resolveStorage("local", storage);
    if (!target) return;
    if (seconds === 0) {
      target.removeItem(ACCOUNT_AUTO_REFRESH_STORAGE_KEY);
      return;
    }
    target.setItem(ACCOUNT_AUTO_REFRESH_STORAGE_KEY, String(seconds));
  } catch {
    // Keep the in-memory interval working when storage is unavailable.
  }
}

// shouldRefreshPageStats 判断自动刷新这一轮是否顺带重拉 page-stats。
export function shouldRefreshPageStats(
  lastRefreshAt: number,
  now: number,
  minIntervalMs: number = ACCOUNT_PAGE_STATS_MIN_REFRESH_MS,
): boolean {
  return now - lastRefreshAt >= minIntervalMs;
}
