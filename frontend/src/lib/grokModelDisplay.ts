import type { AccountRow } from "../types";

type ModelAccount = Pick<AccountRow, "models" | "grok_models" | "grok_auth_kind">;

// OAuth / CLI 只提供 grok-4.5 及以上。grok-4 与 grok-3 / grok-2 一代只属于 API Key 公开目录。
export const OAUTH_GROK_PRESET_MODELS = [
  "grok-4.7",
  "grok-4.7-fast",
  "grok-4.6",
  "grok-4.5",
];

export const API_KEY_GROK_PRESET_MODELS = [
  ...OAUTH_GROK_PRESET_MODELS,
  "grok-4",
  "grok-3-fast",
  "grok-3",
  "grok-2",
];

const OAUTH_EXCLUDED_GROK_MODELS = new Set([
  "grok-4",
  "grok-4-fast",
  "grok-3",
  "grok-3-fast",
  "grok-3-mini",
  "grok-2",
]);

export const DEFAULT_GROK_TEST_MODELS = API_KEY_GROK_PRESET_MODELS;

export function grokPresetModels(authKind?: string): string[] {
  return authKind === "api_key"
    ? [...API_KEY_GROK_PRESET_MODELS]
    : [...OAUTH_GROK_PRESET_MODELS];
}

export function grokModelAllowedForAuthKind(model: string, authKind?: string): boolean {
  if (authKind === "api_key") return true;
  return !OAUTH_EXCLUDED_GROK_MODELS.has(model.trim().toLowerCase());
}

export function filterGrokModelsForAuthKind(models: string[], authKind?: string): string[] {
  if (authKind === "api_key") return [...models];
  return models.filter((model) => grokModelAllowedForAuthKind(model, authKind));
}

// Reading this must never mutate or populate the operator's model list.
// A saved model list stays visible even when the synced catalog is narrower:
// OAuth catalogs often list only grok-4.7, while the operator still listed
// grok-4.6 / grok-4.5 / grok-4.7-fast. Names outside both the catalog and the
// built-in preset stay hidden. An authoritative empty catalog stays empty.
// A catalog entry is not added unless the model list contains it.
export function grokDisplayModels(account: ModelAccount): string[] {
  const raw = account.models ?? [];
  const configured = filterGrokModelsForAuthKind(raw, account.grok_auth_kind);
  const catalog = account.grok_models;
  if (!catalog || catalog.status === "unknown") return [...configured];
  const catalogModels = filterGrokModelsForAuthKind(
    catalog.models,
    account.grok_auth_kind,
  );
  if (raw.length === 0) return [...catalogModels];
  if (catalogModels.length === 0) return [];
  const allowed = new Set(configured.map((model) => model.toLowerCase()));
  const preset = new Set(
    grokPresetModels(account.grok_auth_kind).map((model) => model.toLowerCase()),
  );
  const seen = new Set<string>();
  const result: string[] = [];
  const push = (model: string) => {
    const key = model.toLowerCase();
    if (seen.has(key)) return;
    seen.add(key);
    result.push(model);
  };
  for (const model of catalogModels) {
    if (allowed.has(model.toLowerCase())) push(model);
  }
  for (const model of configured) {
    if (preset.has(model.toLowerCase())) push(model);
  }
  return result;
}

// A known but empty catalog means the account exposes no text model; only an
// unknown catalog may fall back to the static defaults.
export function grokConnectionTestModels(account: ModelAccount): string[] {
  const models = grokDisplayModels(account).filter(
    (m) => m.trim() !== "" && !m.toLowerCase().includes("image"),
  );
  if (models.length > 0) return models;
  return account.grok_models && account.grok_models.status !== "unknown"
    ? []
    : grokPresetModels(account.grok_auth_kind);
}

export function grokModelSummaryTitle(account: ModelAccount): string {
  const source = account.models?.length ? "Model list" : "Automatic";
  const summary = account.grok_models;
  return [source, summary?.status ?? "unknown", summary?.updated_at ?? ""].filter(Boolean).join(" · ");
}
