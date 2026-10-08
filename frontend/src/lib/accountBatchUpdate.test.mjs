import assert from "node:assert/strict";
import test from "node:test";

import { buildBatchMetadataUpdate } from "./accountBatchUpdate.ts";

test("buildBatchMetadataUpdate includes enabled scheduler fields", () => {
  const payload = buildBatchMetadataUpdate({
    ids: [3, 7],
    updateTags: false,
    tags: ["ignored"],
    updateGroups: false,
    groupIds: [99],
    updateScoreBias: true,
    scoreBias: 25,
    updateBaseConcurrency: true,
    baseConcurrency: 4,
    updateSchedulerPriority: true,
    schedulerPriority: 10,
  });

  assert.deepEqual(payload, {
    ids: [3, 7],
    score_bias_override: 25,
    base_concurrency_override: 4,
    scheduler_priority: 10,
  });
});

test("buildBatchMetadataUpdate omits the fingerprint mode unless explicitly enabled", () => {
  const untouched = buildBatchMetadataUpdate({
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateCodexFingerprintMode: false,
    codexFingerprintMode: "session",
  });

  assert.deepEqual(untouched, { ids: [1] });

  const applied = buildBatchMetadataUpdate({
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateCodexFingerprintMode: true,
    codexFingerprintMode: "session",
  });

  assert.deepEqual(applied, { ids: [1], codex_fingerprint_mode: "session" });
});

test("buildBatchMetadataUpdate sends null only for enabled reset fields", () => {
  const payload = buildBatchMetadataUpdate({
    ids: [5],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: true,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: true,
    schedulerPriority: null,
  });

  assert.deepEqual(payload, {
    ids: [5],
    score_bias_override: null,
    scheduler_priority: null,
  });
});

test("buildBatchMetadataUpdate binds a trimmed timezone only when enabled", () => {
  const base = {
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
  };
  assert.deepEqual(
    buildBatchMetadataUpdate({ ...base, updateTimezone: true, timezone: " America/New_York " }),
    { ids: [1], timezone: "America/New_York" },
  );
  assert.deepEqual(
    buildBatchMetadataUpdate({ ...base, updateTimezone: true, timezone: "" }),
    { ids: [1], timezone: "" },
  );
  assert.deepEqual(
    buildBatchMetadataUpdate({ ...base, updateTimezone: false, timezone: "Asia/Tokyo" }),
    { ids: [1] },
  );
});

for (const mode of ['single_machine_multi_window']) {
  test(`batch fingerprint ${mode} is opt-in`, () => {
    const base = { ids: [1, 2], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null, codexFingerprintMode: mode };
    assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexFingerprintMode: true }), { ids: [1, 2], codex_fingerprint_mode: mode });
    assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexFingerprintMode: false }), { ids: [1, 2] });
  });
}

test("buildBatchMetadataUpdate sends keep_concurrency_on_degrade only when enabled", () => {
  const base = {
    ids: [5],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
  };

  assert.deepEqual(
    buildBatchMetadataUpdate({ ...base, keepConcurrency: true }),
    { ids: [5] },
  );
  assert.deepEqual(
    buildBatchMetadataUpdate({ ...base, updateKeepConcurrency: true }),
    { ids: [5], keep_concurrency_on_degrade: false },
  );
  assert.deepEqual(
    buildBatchMetadataUpdate({
      ...base,
      updateKeepConcurrency: true,
      keepConcurrency: true,
    }),
    { ids: [5], keep_concurrency_on_degrade: true },
  );
});
