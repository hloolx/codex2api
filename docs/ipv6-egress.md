# Dedicated host IPv6 egress

The **Proxies → Dedicated host IPv6 egress** panel assigns an existing public
IPv6 source address to each native Codex account. It is disabled by default.
Enabling it overrides the account/group/global forward proxy for Codex traffic;
disabling restores those saved proxy settings. Resin remains the higher-priority
egress and must be disabled before enabling this feature.

The container must already be able to bind these addresses. On Linux, host
networking shares the host network namespace. The application does not create
addresses, change routes, require `NET_ADMIN`, or fall back to IPv4. An explicit
allowlist is recommended to reserve the host's primary address and addresses
used by other applications. An empty allowlist selects all visible public IPv6
addresses. Every replica sharing the database must see the same address pool.

Bindings, cooldowns and configuration live in the database. Atomic claims and
a unique address constraint prevent concurrent workers or blue/green instances
from assigning the same current address to different accounts. A delayed failure
from an old address cannot rotate a newer assignment again. Deleting an account
removes its current binding; disabling an account does not discard its binding.

The default cooldown is 600 seconds and the direct exchange attempts up to three
addresses for the selected account. Existing gateway account-selection/retry
policies remain separate. Temporary 429 responses and failures establishing a
connection rotate the source. Optional 500/502/503/504 handling also covers
explicit upstream error frames before output. Authentication failures, hard quota
exhaustion, invalid requests and policy rejections do not rotate addresses.
Neither a 5xx nor a rotation is a model-quality failure or proof of IP throttling.

HTTP, WebSocket, maintenance requests and OAuth use the account's source binding.
OAuth refresh is never replayed transparently. HTTP and WebSocket stream preflight
buffers only creation metadata, up to 64 KiB; it stops at the first output event.
Visible text, reasoning and tool events are never replayed by this feature.
Failures reporting output usage are also not replayed. Existing active streams
keep their connection when another request rotates the account; new exchanges
use the new address and WebSocket continuation reuse respects the source route.

The panel reports pool availability, account/IP bindings, rotation counts and the
last rotation reason. Usage audit labels record the actual IPv6 source, and
rotation logs contain only account ID, old/new address and a fixed reason label.
An exhausted or unavailable pool fails closed until an address becomes available.

Admin API: `GET /api/admin/ipv6-egress`, `PUT /api/admin/ipv6-egress`.
Configuration writes require the current `revision`; stale writes return 409.
The enable switch saves only enablement, while address/cooldown/retry drafts use
the explicit save button. Automatic status refresh never clears a failed write.

For rollout, keep the feature disabled during migration and overlap, validate the
new instance, switch ingress, drain and stop the old instance, then enable a
verified address pool. Disable the feature to restore the previous proxy routing
without reverting the image. A version rollback must also disable it before
traffic is handed back to a version that does not implement dedicated IPv6.
