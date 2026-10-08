import assert from "node:assert/strict";
import test from "node:test";
import { autoRefreshCountdownStep } from "./autoRefreshCountdown.ts";

test("countdown shows whole seconds and wakes at the next second boundary", () => {
  const deadline = 100_000;
  assert.deepEqual(autoRefreshCountdownStep(deadline, deadline - 10_000), { remaining: 10, delay: 1000 });
  assert.deepEqual(autoRefreshCountdownStep(deadline, deadline - 9_400), { remaining: 10, delay: 400 });
  assert.deepEqual(autoRefreshCountdownStep(deadline, deadline - 9_000), { remaining: 9, delay: 1000 });
  assert.deepEqual(autoRefreshCountdownStep(deadline, deadline - 1), { remaining: 1, delay: 1 });
});

test("countdown reaches zero at the deadline", () => {
  assert.deepEqual(autoRefreshCountdownStep(5_000, 5_000), { remaining: 0, delay: 0 });
  assert.deepEqual(autoRefreshCountdownStep(5_000, 7_500), { remaining: 0, delay: 0 });
});

test("walking the countdown shows every second once", () => {
  const deadline = 30_000;
  const shown = [];
  let now = 0;
  for (;;) {
    const step = autoRefreshCountdownStep(deadline, now);
    shown.push(step.remaining);
    if (step.remaining === 0) break;
    now += step.delay;
  }
  assert.deepEqual(shown, [30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0]);
});
