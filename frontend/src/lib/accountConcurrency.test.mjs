import assert from "node:assert/strict";
import test from "node:test";
import {
  formatAccountConcurrencyText,
  resolveAccountConcurrencyDisplay,
} from "./accountConcurrency.ts";

test("idle accounts keep the concurrency badge hidden", () => {
  assert.equal(
    resolveAccountConcurrencyDisplay({
      active_requests: 0,
      occupied_requests: 0,
      dynamic_concurrency_limit: 50,
      base_concurrency_effective: 50,
    }),
    null,
  );
  assert.equal(resolveAccountConcurrencyDisplay({}), null);
});

test("active requests are shown against the effective limit", () => {
  const display = resolveAccountConcurrencyDisplay({
    active_requests: 5,
    occupied_requests: 5,
    dynamic_concurrency_limit: 50,
    base_concurrency_effective: 50,
  });
  assert.ok(display);
  assert.equal(display.used, 5);
  assert.equal(display.limit, 50);
  assert.equal(display.degraded, false);
  assert.equal(formatAccountConcurrencyText(display), "5 / 50");
});

test("a custom account concurrency is used as the denominator", () => {
  const display = resolveAccountConcurrencyDisplay({
    active_requests: 5,
    dynamic_concurrency_limit: 10,
    base_concurrency_effective: 10,
  });
  assert.ok(display);
  assert.equal(formatAccountConcurrencyText(display), "5 / 10");
});

test("a runtime-lowered limit is flagged as degraded", () => {
  const display = resolveAccountConcurrencyDisplay({
    active_requests: 3,
    dynamic_concurrency_limit: 25,
    base_concurrency_effective: 50,
  });
  assert.ok(display);
  assert.equal(display.limit, 25);
  assert.equal(display.base, 50);
  assert.equal(display.degraded, true);
  assert.equal(formatAccountConcurrencyText(display), "3 / 25");
});

test("session slot buffering counts occupied slots against the limit", () => {
  const display = resolveAccountConcurrencyDisplay({
    active_requests: 3,
    occupied_requests: 5,
    session_slot_buffer_enabled: true,
    dynamic_concurrency_limit: 50,
    base_concurrency_effective: 50,
  });
  assert.ok(display);
  assert.equal(display.showOccupied, true);
  assert.equal(display.buffered, 2);
  assert.equal(display.used, 5);
  assert.equal(formatAccountConcurrencyText(display), "5 / 50");
});

test("without buffering the numerator stays the live request count", () => {
  const display = resolveAccountConcurrencyDisplay({
    active_requests: 3,
    occupied_requests: 5,
    session_slot_buffer_enabled: false,
    dynamic_concurrency_limit: 50,
  });
  assert.ok(display);
  assert.equal(display.used, 3);
});

test("an unknown or zero limit falls back to the bare count", () => {
  for (const limit of [undefined, 0, -1, Number.NaN]) {
    const display = resolveAccountConcurrencyDisplay({
      active_requests: 4,
      dynamic_concurrency_limit: limit,
      base_concurrency_effective: 50,
    });
    assert.ok(display);
    assert.equal(display.limit, null);
    assert.equal(display.degraded, false);
    assert.equal(formatAccountConcurrencyText(display), "4");
  }
});
