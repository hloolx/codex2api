import assert from "node:assert/strict";
import test from "node:test";
import {
  ACCOUNT_AUTO_REFRESH_INTERVALS,
  ACCOUNT_AUTO_REFRESH_STORAGE_KEY,
  ACCOUNT_LIST_SORT_STORAGE_KEY,
  ACCOUNT_PAGE_STATS_MIN_REFRESH_MS,
  normalizeAccountAutoRefreshSeconds,
  readAccountAutoRefreshSeconds,
  readAccountListSort,
  shouldRefreshPageStats,
  writeAccountAutoRefreshSeconds,
  writeAccountListSort,
} from "./accountListPreferences.ts";

function createStorage(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    values,
    getItem(key) {
      return values.has(key) ? values.get(key) : null;
    },
    setItem(key, value) {
      values.set(key, String(value));
    },
    removeItem(key) {
      values.delete(key);
    },
  };
}

function createBrokenStorage() {
  return {
    getItem() {
      throw new Error("storage unavailable");
    },
    setItem() {
      throw new Error("storage unavailable");
    },
    removeItem() {
      throw new Error("storage unavailable");
    },
  };
}

test("account list sort defaults to the server order", () => {
  assert.deepEqual(readAccountListSort(createStorage()), {
    key: null,
    dir: "desc",
  });
});

test("account list sort round-trips a chosen column", () => {
  const storage = createStorage();
  writeAccountListSort({ key: "importTime", dir: "asc" }, storage);
  assert.deepEqual(readAccountListSort(storage), {
    key: "importTime",
    dir: "asc",
  });
  writeAccountListSort({ key: "group", dir: "desc" }, storage);
  assert.deepEqual(readAccountListSort(storage), { key: "group", dir: "desc" });
  writeAccountListSort({ key: "id", dir: "asc" }, storage);
  assert.deepEqual(readAccountListSort(storage), { key: "id", dir: "asc" });
});

test("restoring the default sort clears the stored value", () => {
  const storage = createStorage();
  writeAccountListSort({ key: "usage", dir: "desc" }, storage);
  writeAccountListSort({ key: null, dir: "desc" }, storage);
  assert.equal(storage.values.has(ACCOUNT_LIST_SORT_STORAGE_KEY), false);
  assert.deepEqual(readAccountListSort(storage), { key: null, dir: "desc" });
});

test("invalid stored sorts fall back to the default", () => {
  for (const raw of ["not json", '{"key":"unknown","dir":"asc"}', "{}", "null"]) {
    const storage = createStorage({ [ACCOUNT_LIST_SORT_STORAGE_KEY]: raw });
    assert.deepEqual(readAccountListSort(storage), { key: null, dir: "desc" });
  }
  const storage = createStorage({
    [ACCOUNT_LIST_SORT_STORAGE_KEY]: '{"key":"requests","dir":"sideways"}',
  });
  assert.deepEqual(readAccountListSort(storage), {
    key: "requests",
    dir: "desc",
  });
});

test("sort preference tolerates unavailable storage", () => {
  const storage = createBrokenStorage();
  assert.deepEqual(readAccountListSort(storage), { key: null, dir: "desc" });
  assert.doesNotThrow(() =>
    writeAccountListSort({ key: "today", dir: "asc" }, storage),
  );
});

test("auto refresh is off by default and accepts only offered intervals", () => {
  assert.deepEqual([...ACCOUNT_AUTO_REFRESH_INTERVALS], [0, 5, 10, 30]);
  assert.equal(readAccountAutoRefreshSeconds(createStorage()), 0);
  for (const raw of ["7", "abc", "-5", "300"]) {
    const storage = createStorage({ [ACCOUNT_AUTO_REFRESH_STORAGE_KEY]: raw });
    assert.equal(readAccountAutoRefreshSeconds(storage), 0);
  }
  assert.equal(normalizeAccountAutoRefreshSeconds(10), 10);
  assert.equal(normalizeAccountAutoRefreshSeconds("30"), 30);
  assert.equal(normalizeAccountAutoRefreshSeconds(15), 0);
});

test("auto refresh interval round-trips and turning it off clears storage", () => {
  const storage = createStorage();
  writeAccountAutoRefreshSeconds(5, storage);
  assert.equal(storage.getItem(ACCOUNT_AUTO_REFRESH_STORAGE_KEY), "5");
  assert.equal(readAccountAutoRefreshSeconds(storage), 5);
  writeAccountAutoRefreshSeconds(0, storage);
  assert.equal(storage.values.has(ACCOUNT_AUTO_REFRESH_STORAGE_KEY), false);
  assert.equal(readAccountAutoRefreshSeconds(storage), 0);
});

test("auto refresh preference tolerates unavailable storage", () => {
  const storage = createBrokenStorage();
  assert.equal(readAccountAutoRefreshSeconds(storage), 0);
  assert.doesNotThrow(() => writeAccountAutoRefreshSeconds(10, storage));
});

test("page stats are throttled during auto refresh", () => {
  const start = 1_000_000;
  assert.equal(ACCOUNT_PAGE_STATS_MIN_REFRESH_MS, 30_000);
  assert.equal(shouldRefreshPageStats(start, start + 5_000), false);
  assert.equal(shouldRefreshPageStats(start, start + 29_999), false);
  assert.equal(shouldRefreshPageStats(start, start + 30_000), true);
  assert.equal(shouldRefreshPageStats(0, start), true);
  assert.equal(shouldRefreshPageStats(start, start + 1_000, 1_000), true);
});
