// Codex/Responses diagnostics and presentation helpers, independent of the
// Claude diagnostics. Field meanings follow Responses and x-codex-* headers.

export interface CodexTestWindow {
  used_percent?: number;
  window_minutes?: number;
  reset_after_seconds?: number;
}

export interface CodexTestUsage {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
  reasoning_output_tokens?: number;
}

export interface CodexTestDiagnostics {
  http_status?: number;
  duration_ms?: number;
  headers_ms?: number;
  first_frame_ms?: number;
  first_content_ms?: number;
  model: string;
  response_model?: string;
  transport?: string;
  request_id?: string;
  response_id?: string;
  cf_ray?: string;
  plan_type?: string;
  turn_state_length?: number;
  turn_state_source?: 'http_headers' | 'ws_handshake' | 'response_metadata';
  safety_buffering_enabled?: boolean;
  safety_buffering_faster_model?: string;
  safety_buffered?: boolean;
  response_status?: string;
  incomplete_reason?: string;
  error_type?: string;
  error_code?: string;
  primary_window?: CodexTestWindow;
  secondary_window?: CodexTestWindow;
  usage?: CodexTestUsage;
  response_headers?: Array<{ name: string; value: string }>;
  response_body?: string;
  body_truncated?: boolean;
}

export type CodexTestWindowKind = "5h" | "7d" | "short" | "unknown";

export function codexTestTurnState(diagnostics?: CodexTestDiagnostics | null, running = false) {
  const length = diagnostics?.turn_state_length;
  if (typeof length !== 'number' || !Number.isSafeInteger(length) || length < 0) {
    return { status: running ? 'pending' : 'unknown', length: undefined } as const;
  }
  if (length === 0) {
    return { status: running && diagnostics?.transport === 'websocket' ? 'pending' : 'missing', length } as const;
  }
  return { status: length === 292 ? 'matched' : 'different', length } as const;
}

// Match backend windowMinutesToCooldown thresholds: one day / one hour.
export function codexTestWindowKind(window?: CodexTestWindow): CodexTestWindowKind {
  const minutes = window?.window_minutes;
  if (typeof minutes !== "number" || !Number.isFinite(minutes) || minutes <= 0) return "unknown";
  if (minutes >= 1440) return "7d";
  if (minutes >= 60) return "5h";
  return "short";
}

export function formatCodexTestReset(seconds?: number): string {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds < 0) return "";
  const total = Math.round(seconds);
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`;
  if (minutes > 0) return `${minutes}m`;
  return `${total}s`;
}

export function clampCodexTestPercent(value?: number): number | null {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return null;
  return Math.min(100, value);
}

export const codexTestTokenKeys = ["input_tokens", "output_tokens", "cached_input_tokens", "reasoning_output_tokens"] as const;
export type CodexTestTokenKey = (typeof codexTestTokenKeys)[number];

export function codexTestTokenMetrics(usage?: CodexTestUsage) {
  const metrics = codexTestTokenKeys.map((key) => {
    const raw = usage?.[key];
    return { key, value: typeof raw === "number" && Number.isFinite(raw) && raw >= 0 ? raw : null };
  });
  const max = Math.max(1, ...metrics.map(({ value }) => value ?? 0));
  return metrics.map((item) => ({ ...item, percent: item.value === null ? 0 : (item.value / max) * 100 }));
}

// Initial diagnostics contain headers; duration_ms identifies the final frame.
export function isFinalCodexTestDiagnostics(diagnostics?: CodexTestDiagnostics | null): boolean {
  return typeof diagnostics?.duration_ms === "number";
}

// 上游按客户端版本放行新模型:模拟版本过旧时返回这两类 400,同步到最新版本常可恢复。
// 同一文案也会出现在模型确实未对账号开放的场景,所以只能作为"可能"提示。
const CODEX_VERSION_GATE_PATTERNS = [
  /model is not supported when using codex with a chatgpt account/i,
  /requires a newer version of codex/i,
];

export function isCodexVersionGatedError(...texts: Array<string | null | undefined>): boolean {
  return texts.some((text) => Boolean(text) && CODEX_VERSION_GATE_PATTERNS.some((pattern) => pattern.test(text!)));
}

export function formatCodexTestMS(value?: number): string {
  return typeof value === "number" && Number.isFinite(value) ? `${value.toLocaleString()} ms` : "—";
}
