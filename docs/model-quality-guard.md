# Automatic model quality checks

Open **Quality test → Automatic checks**, select Codex models, then enable the
switch. The feature is disabled by default and applies to existing and newly
imported Codex accounts. The first check is queued within five seconds while
the account is available; subsequent checks run ten minutes after completion.
Busy accounts wait for normal account capacity. Four accounts can be checked
at once per instance, with one model at a time per account.

Each account/model has its own verdict:

- **Green:** the candy benchmark passed; this quality guard permits calls.
- **Red:** a complete response contained an unambiguous incorrect integer;
  new calls to this account/model are excluded from HTTP, WebSocket and compact
  selection. Already running business requests continue.
- **Pending:** no conclusive result yet; this guard permits calls.
- **Error:** network failure, 5xx, incomplete stream or an ungradable answer;
  the previous verdict remains unchanged. The latest diagnostic is shown.

Red models are still probed, and a passing result restores only that model.
Unselected models are unaffected. Disabling the feature or removing a model
releases its quality restrictions. Manual disables, authentication failures,
quota limits and model cooldowns remain authoritative. No two-hour expiry is
imposed: account lifetime follows actual authentication state.

## Probe and persistence

The fixed benchmark explicitly allows choosing candy shapes by touch. A
preselected draw of nine round candies and twelve stars guarantees the desired
pair; exhaustive enumeration in the test suite independently verifies the
minimum of 21. Probes use `high` reasoning, an isolated HTTP SSE request and
the exact account; no account fallback or LLM judge is used. A strict single
integer JSON answer is required; unknown formatting cannot newly block an
account. This is a quick benchmark, not proof of general model capability or
an upstream downgrade. Probes consume quota and appear in existing quality
test usage accounting.

Configuration and verdicts persist in additive `model_quality_*` tables.
Renewable per-account database leases prevent concurrent probes across
replicas. Configuration revisions, lease ownership/expiry and credential
generation prevent obsolete results from restoring restrictions. Replicas
refresh routing snapshots every five seconds; local administrator changes
apply immediately. Replaced credentials start pending. Deleted accounts are
removed by foreign-key cascade.

Probe network requests have no hard response timeout; cancellation follows
shutdown, loss of lease, changed configuration or account unavailability.
Oversized output is cancelled and remains inconclusive. A stuck upstream can
occupy one of four worker slots until cancelled; switching off cancels active
probes on their next five-second lease check.

## Rollback

Older binaries ignore these additive tables and cannot enforce quality gates.
Disable automatic checks before rolling back and verify existing account and
quota restrictions independently. No destructive database rollback is needed.
