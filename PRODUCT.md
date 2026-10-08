# Codex2API automatic model quality guard

This brief covers the automatic-check addition to the existing administration
interface, not the entire proxy product.

Administrators import short-lived Codex accounts and choose which models to
check. An optional switch enables an initial check and a ten-minute repeat
schedule. Results belong to an account/model pair: green permits routing; red
pauses that model only; pending and unselected models remain available under
the existing account rules. Red models continue to be tested and recover on a
pass. Network/5xx/incomplete/ungradable outcomes preserve the prior verdict.
Disabling or deselecting releases only quality restrictions. Manual account
disables, authentication failures and quota restrictions remain independent.
An expected two-hour account lifetime is not an automatic expiry policy.

The addition lives under **Quality test → Automatic checks** and preserves the
existing studio, presets and history. It extends the current DESIGN.md system:
shared controls, Chinese/Traditional Chinese/English translations, inherited
light/dark themes, responsive account/model groups and labeled status icons.
No new visual world, brand or animation system is introduced.

The quality bar is a readable operational interface: admins must find the
switch, models, verdict, last/next check, error reason and retest action quickly.
Status text must meet 4.5:1 contrast in both themes and remain understandable
without color. Phone layouts must not overflow.

Browser review fixtures use only synthetic `example.test` identities and
invented states. Those fixtures validate presentation and interactions, not
the accuracy of a real upstream model's benchmark performance.

## Dedicated host IPv6 egress

The Proxies administration page adds optional dedicated host IPv6 egress for
native Codex accounts. Administrators choose existing local addresses, address
cooldown, retry limits, and whether transient 5xx responses rotate the source.
Temporary rate limits and connection failures can rotate an account independently.
Authentication, hard quota, input errors and quality verdicts stay separate.
The panel must expose configuration, pool availability, persisted account/IP
bindings, the last rotation cause and time, and clear failed-save/empty states.
Disabling restores existing proxy routing. Addresses are never created by the
application; exhausted pools fail closed. Requests with visible output or reported
usage are not replayed. The existing Operate design system and shared controls
remain authoritative; this is a narrow expansion of an incumbent administration
page, with no new visual-world or comp selection.
