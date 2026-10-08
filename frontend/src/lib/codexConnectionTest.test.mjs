import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import ts from "typescript";
import {
  clampCodexTestPercent,
  codexTestTokenMetrics,
  codexTestTurnState,
  codexTestWindowKind,
  formatCodexTestMS,
  formatCodexTestReset,
  isCodexVersionGatedError,
  isFinalCodexTestDiagnostics,
} from "./codexConnectionTest.ts";

test("turn state distinguishes exact length, absent headers, and unknown transport failures", () => {
  assert.equal(codexTestTurnState({ turn_state_length: 292 }).status, "matched");
  assert.deepEqual(codexTestTurnState({ turn_state_length: 312 }), { status: "different", length: 312 });
  assert.equal(codexTestTurnState({ turn_state_length: 0, transport: "http" }, true).status, "missing");
  assert.equal(codexTestTurnState({ turn_state_length: 0, transport: "websocket" }, true).status, "pending");
  assert.equal(codexTestTurnState({ turn_state_length: 0, transport: "websocket" }, false).status, "missing");
  assert.equal(codexTestTurnState(null, true).status, "pending");
  assert.equal(codexTestTurnState(null, false).status, "unknown");
  for (const value of [-1, 292.5, Infinity, "292"]) {
    assert.equal(codexTestTurnState({ turn_state_length: value }).status, "unknown");
  }
});

test("version-gated upstream errors are recognised in any provided text", () => {
  const failedEvent = JSON.stringify({
    response: {
      error: {
        message: "The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.",
        type: "invalid_request_error",
      },
      status: "failed",
      status_code: 400,
    },
    type: "response.failed",
  });
  assert.equal(isCodexVersionGatedError(failedEvent), true);
  assert.equal(isCodexVersionGatedError(undefined, `{"detail":"The 'gpt-5.5' model requires a newer version of Codex"}`), true);
  assert.equal(isCodexVersionGatedError("usage_limit_reached", null, ""), false);
});

test("window kind follows the backend minute thresholds", () => {
  assert.equal(codexTestWindowKind({ window_minutes: 300 }), "5h");
  assert.equal(codexTestWindowKind({ window_minutes: 10080 }), "7d");
  assert.equal(codexTestWindowKind({ window_minutes: 1440 }), "7d");
  assert.equal(codexTestWindowKind({ window_minutes: 30 }), "short");
  assert.equal(codexTestWindowKind({ used_percent: 5 }), "unknown");
  assert.equal(codexTestWindowKind(undefined), "unknown");
});

test("reset countdown formats coarse units and rejects garbage", () => {
  assert.equal(formatCodexTestReset(45), "45s");
  assert.equal(formatCodexTestReset(90), "1m");
  assert.equal(formatCodexTestReset(3600), "1h");
  assert.equal(formatCodexTestReset(4980), "1h 23m");
  assert.equal(formatCodexTestReset(90000), "1d 1h");
  assert.equal(formatCodexTestReset(-1), "");
  assert.equal(formatCodexTestReset(undefined), "");
  assert.equal(formatCodexTestReset(Number.NaN), "");
});

test("percent clamp keeps the bar inside its track", () => {
  assert.equal(clampCodexTestPercent(12.5), 12.5);
  assert.equal(clampCodexTestPercent(240), 100);
  assert.equal(clampCodexTestPercent(-3), null);
  assert.equal(clampCodexTestPercent(undefined), null);
});

test("token metrics scale bars against the largest observed count", () => {
  const metrics = codexTestTokenMetrics({ input_tokens: 40, output_tokens: 10, cached_input_tokens: 20 });
  assert.deepEqual(metrics.map((m) => m.key), ["input_tokens", "output_tokens", "cached_input_tokens", "reasoning_output_tokens"]);
  assert.equal(metrics[0].percent, 100);
  assert.equal(metrics[1].percent, 25);
  assert.equal(metrics[2].percent, 50);
  assert.equal(metrics[3].value, null);
  assert.equal(metrics[3].percent, 0);
  assert.equal(codexTestTokenMetrics(undefined).every((m) => m.value === null), true);
});

test("final diagnostics are recognised by duration_ms only", () => {
  assert.equal(isFinalCodexTestDiagnostics({ model: "gpt-5.4", http_status: 200 }), false);
  assert.equal(isFinalCodexTestDiagnostics({ model: "gpt-5.4", duration_ms: 0 }), true);
  assert.equal(isFinalCodexTestDiagnostics(null), false);
  assert.equal(formatCodexTestMS(1234), "1,234 ms");
  assert.equal(formatCodexTestMS(undefined), "—");
});

test("账号测连只在点击后发送，编辑模型和内容不会自动重测", () => {
  const source = readFileSync(new URL("../components/TestConnectionModal.tsx", import.meta.url), "utf8");
  const file = ts.createSourceFile("TestConnectionModal.tsx", source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  let requests = 0;
  const visit = (node) => {
    if (ts.isCallExpression(node) && node.expression.getText(file) === "fetch") {
      requests++;
      let parent = node.parent;
      const ancestors = [];
      while (parent) {
        ancestors.push(parent);
        parent = parent.parent;
      }
      assert.ok(ancestors.some((item) => ts.isVariableDeclaration(item) && item.name.getText(file) === "startTest"));
      assert.ok(!ancestors.some((item) => ts.isCallExpression(item) && item.expression.getText(file) === "useEffect"));
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  assert.equal(requests, 1);
  assert.ok(source.includes('>("idle")'));
  assert.ok(source.includes("onClick={startTest}"));
  assert.ok(source.includes("prompt: testContent"));
});
