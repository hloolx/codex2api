import assert from "node:assert/strict";
import test from "node:test";

import {
  ACCOUNT_LIST_FULL_POOL_SCAN_MAX,
  isLargePoolSortDisabled,
  resolveDisabledAccountSorts,
  toAccountListApiSort,
} from "./accountListSort.ts";

test("account list sort keys map to the paged API", () => {
  assert.equal(toAccountListApiSort(null), undefined);
  assert.equal(toAccountListApiSort("requests"), "requests");
  assert.equal(toAccountListApiSort("today"), "today");
  assert.equal(toAccountListApiSort("importTime"), "created_at");
  assert.equal(toAccountListApiSort("schedulerPriority"), "scheduler_priority");
  assert.equal(toAccountListApiSort("updated"), "updated_at");
  assert.equal(toAccountListApiSort("id"), "id");
});

test("large pools disable usage-log sorts from the API or pool size", () => {
  assert.deepEqual(resolveDisabledAccountSorts(undefined, 20), []);
  assert.deepEqual(
    resolveDisabledAccountSorts(undefined, ACCOUNT_LIST_FULL_POOL_SCAN_MAX + 1),
    ["requests", "today"],
  );
  assert.deepEqual(
    resolveDisabledAccountSorts([], ACCOUNT_LIST_FULL_POOL_SCAN_MAX + 1),
    [],
  );
  assert.deepEqual(resolveDisabledAccountSorts(["requests", "today"], 3), [
    "requests",
    "today",
  ]);
  assert.equal(isLargePoolSortDisabled("requests", ["requests", "today"]), true);
  assert.equal(isLargePoolSortDisabled("today", ["requests", "today"]), true);
  assert.equal(isLargePoolSortDisabled("usage", ["requests", "today"]), false);
  assert.equal(isLargePoolSortDisabled("group", ["requests", "today"]), false);
});
