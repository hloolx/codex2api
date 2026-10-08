import type { BatchUpdateAccountsRequest, CodexFingerprintMode } from "../types";

export interface BuildBatchMetadataUpdateOptions {
  ids: number[];
  updateTags: boolean;
  tags: string[];
  updateGroups: boolean;
  groupIds: number[];
  updateScoreBias: boolean;
  scoreBias: number | null;
  updateBaseConcurrency: boolean;
  baseConcurrency: number | null;
  updateKeepConcurrency?: boolean;
  keepConcurrency?: boolean;
  updateSchedulerPriority: boolean;
  schedulerPriority: number | null;
  updateCodexFingerprintMode?: boolean;
  codexFingerprintMode?: CodexFingerprintMode;
  updateTimezone?: boolean;
  timezone?: string;
}

export function buildBatchMetadataUpdate({
  ids,
  updateTags,
  tags,
  updateGroups,
  groupIds,
  updateScoreBias,
  scoreBias,
  updateBaseConcurrency,
  baseConcurrency,
  updateKeepConcurrency,
  keepConcurrency,
  updateSchedulerPriority,
  schedulerPriority,
  updateCodexFingerprintMode,
  codexFingerprintMode,
  updateTimezone,
  timezone,
}: BuildBatchMetadataUpdateOptions): BatchUpdateAccountsRequest {
  const payload: BatchUpdateAccountsRequest = { ids: [...ids] };
  if (updateTags) payload.tags = [...tags];
  if (updateGroups) payload.group_ids = [...groupIds];
  if (updateScoreBias) payload.score_bias_override = scoreBias;
  if (updateBaseConcurrency)
    payload.base_concurrency_override = baseConcurrency;
  if (updateKeepConcurrency)
    payload.keep_concurrency_on_degrade = keepConcurrency ?? false;
  if (updateSchedulerPriority) payload.scheduler_priority = schedulerPriority;
  if (updateCodexFingerprintMode)
    payload.codex_fingerprint_mode = codexFingerprintMode ?? "off";
  if (updateTimezone) payload.timezone = (timezone ?? "").trim();
  return payload;
}
