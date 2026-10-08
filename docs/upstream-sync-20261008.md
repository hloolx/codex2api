# Upstream integration: 2026-10-08

## Compared revisions

| Reference | Revision | Commit date | Description |
| --- | --- | --- | --- |
| Fork main | `b402c611` | 2026-09-25 | Verified BPS models per API key; `v2.9.8-45-gb402c611` |
| Shared ancestor | `de41a5e3` | 2026-09-17 | Last upstream baseline present in the fork |
| Upstream main | `b1bfe1b4` | 2026-10-07 | `v3.0.7-9-gb1bfe1b4` |

Sources: [fork](https://github.com/hloolx/codex2api),
[upstream revision](https://github.com/james-6-23/codex2api/commit/b1bfe1b4),
[upstream changelog](https://github.com/james-6-23/codex2api/blob/b1bfe1b4/CHANGELOG.md).

Before integration, the histories contained 37 fork-only commits and 226
upstream-only commits (173 excluding merge commits). Upstream changed 500 files,
with 75,014 insertions and 5,126 deletions since the shared ancestor. These are
history and source-size comparisons, not a measure of runtime compatibility.
The ModelTrace bank accounts for 31,185 added lines.

## Included upstream changes

- Paired Codex Desktop/VS Code app and CLI identities, persistent version caches,
  Windows attestation, and opt-in unified maintenance identities.
- Daybreak capabilities, model detection, model catalog updates, billing fixes,
  ProMax plan recognition, and upstream response-model auditing.
- Native Responses relay WebSocket support, selection cancellation, concurrency
  reporting, and optional preservation of concurrency on degraded accounts.
- Durable image jobs, staged image memory admission, retention and cleanup.
- Grok client/protocol updates and Antigravity model, thinking and error handling.
- Usage query bounds, typed atomics for 32-bit targets, dependency fixes,
  timezone selection, connection tests, and account-management improvements.

## Fork compatibility decisions

The upstream history is merged in full. Its removal of BPS is intentionally
adapted: the user explicitly requested retention of the existing fork adapter.
The fork keeps its global enable switch, per-key policies, exact-model probes,
controlled fallback, history provenance, cooldowns, and image hosting. Removed
upstream BPS settings and its separate adapter are not reintroduced.

Managed State, capture admission limits, supply signals, and credential-level
State injection remain available. Credential loading, outbox refresh, scheduler
updates, the manual editor, HTTP injection, and WebSocket injection are retained.
Upstream's removal of the Usage-page State marker remains in effect. Preserving
these features does not assert that replayed State improves upstream behavior.

Material integration fixes:

- Combine State coverage with the new live concurrency caps.
- Preserve BPS settings alongside the new PostgreSQL settings parameters and
  the prompt-filter update guards. CI now exercises their combined persistence.
- Convert fork-only admission counters and tests to upstream's typed atomics.
- Keep route budgets, initial eligibility errors, and selection bookkeeping
  while adopting cancellation and exclusion-aware queue waiting.
- Spool the route layer's canonical replay payload for durable image jobs. An
  in-memory replay copy otherwise defeats upstream's memory release while waiting.
- Respect detector contexts before route-layer payload rules, and sanitize
  rule-injected service tiers before native dispatch.
- Keep connection-test completion notifications after the SSE body closes while
  adopting upstream's manual start, version synchronization, and detector UI.

## Validation

Local results:

- `go test -json -count=1 -timeout 8m ./...`: 8,971 tests/subtests passed,
  28 explicitly skipped. The final detector/tier change also passed a complete
  `./proxy` rerun, including its added regression test.
- `go vet ./...`: passed.
- Windows amd64, Linux amd64 and Linux ARMv7 builds: passed.
- Frontend unit tests: 386 passed. Typechecking and production build: passed.
- BPS route browser interactions: desktop/mobile passed.
- State dashboard, accounts, renewal, settings and saved filters:
  desktop/mobile in light/dark themes passed.
- SVG browser regressions: five passed; one optional local sample skipped.

Browser tests use synthetic account data. PostgreSQL, Redis and race-detector
jobs run in GitHub Actions; unavailable local service-dependent tests are skipped
explicitly rather than counted as executed. CI outcomes are attached to the PR.

The integration uses a separate worktree and branch. Existing local development
changes are preserved. No production service is restarted or deployed by this
source integration.
