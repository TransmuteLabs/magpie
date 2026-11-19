# Shared request policy

The host owns response classification, retry admission and bounded waiting in
[`internal/requestpolicy`](../../internal/requestpolicy). Provider adapters
extract bounded facts and separately establish absence of effects. They do not
own a retry timer, account selector or refresh manager.

This API 1.12 implementation handles complete response facts. It does not recover
transport errors, Connect trailers or credentials. An adapter must preserve its
actual vendor code/type/message before a codec normalizes an error. Unknown or
truncated facts do not prove safe replay. Authentication refresh remains with
the existing transactional owner; reactive authentication continuity is not
advertised here.

## Settings and existing routing

`requestPolicy` uses the normal settings JSON API and CLI:

```
magpie settings set requestPolicy '{"defaults":{"maxRetries":1,"maxTotalRetries":3,"baseDelayMs":1000,"maxDelayMs":60000,"shortWaitMs":45000,"maxElapsedMs":120000}}'
```

These structured operator rules currently have no GUI editor. Other settings
saves preserve them through the shared settings owner.

`providers` maps canonical provider IDs to the same options. `models` and
`windows` within defaults/provider options override limits for exact model and
adapter window IDs. No new routing mode is introduced. Smart, order and other
existing modes retain candidate selection and order; the policy only excludes
proved unavailable account/model selections and decides whether a short wait
is permitted.

- `maxRetries` limits repeats of the same selected execution or named transport
  step. It does not turn a sibling account into the same account.
- `maxTotalRetries` bounds additional dispatches within business operations
  under one shared request root, including fallback accounts and nested
  transport permits. Independent image descriptions, search queries, advice
  requests and chat-classifier questions each get their own first dispatch.
  Retrying or falling back within one of them consumes the shared root budget.
  Switching model/provider/account, creating another child or canceling one
  does not reset consumed counts or the root clock. The default is 64;
  configured values are 0–256. A provider override cannot raise the root limit.
- `baseDelayMs`, `maxDelayMs`, `maxElapsedMs` and `jitterPercent` bound scheduling.
  The default base is one second, delay cap eight seconds and elapsed cap thirty
  minutes; existing gateway patience retains its own default schedule while
  sharing admission. Explicit delay/elapsed limits accept up to seven days so
  a long server hint can be represented and rejected without being shortened.
- `shortWaitMs` is an independent blocking threshold, default eight seconds,
  configurable from zero to five minutes. A longer reset never starts an hours
  long sleep. An eligible sibling is tried immediately in the existing order;
  if none is eligible, the caller receives a finite refusal with known timing.
- A wait also requires remaining same-account retries, elapsed time and caller
  deadline. It is cancellable. Reservations are consumed once, not refunded on
  cancellation and not charged again at the resulting dispatch.
  When only the wait cannot fit, a read-only check of the same budget permits
  an immediate eligible sibling under its own provider and model limits. The
  check walks the existing route order and skips destinations whose dispatch
  would exceed those limits or the shared root budget. An exhausted immediate-dispatch budget still
  ends the request; the check neither spends nor replenishes a permit.

Ordinary routing-group classifiers retain the caller's cancellation and shared
request budget through the context-aware chat, decision-provider and
decision-group entry points. Calibration gets its own business operation under
that root, so its first dispatch is distinct while retries still consume the
same root budget. Final SDK System One refusals use this policy for calibration
and classification. A fresh refusal contributes its upstream status and code
to usage even without token totals. A cached refusal records a rejected call
without a new upstream observation or dispatch. Contextless entry points remain
compatibility wrappers. Ordinary routing uses the context-aware path.

Each chat-classifier question has one timeout covering both effort attempts.
When that limit expires, the usage log attributes the failure to magpie's
decision-model wait. An earlier caller cancellation or parent deadline keeps
its caller attribution. The timer does not replace the account scope or
replenish the shared retry budget.

The shared classifier owner derives the effective caller/group account
intersection before looking up failure rest or an in-flight question. Those
entries are partitioned by the effective account scope; a failure in one
selection cannot suppress another selection or carry its terminal SDK fence.
Completed successful answers retain the existing question-level shared cache.

Transport repairs using `forwardOnce`'s resend callback also admit each
additional send against the same root before wire accounting or dispatch.
This includes resending a remote Magpie request after a dictionary miss.
Admission retains the repeated request's cancellation, selected account/model
and operation. A permitted nested SDK dispatch receives one fresh route permit,
so it consumes that admission once. The individual repair still decides whether
replay is safe; budget admission does not authorize a new business operation.

Responses compaction uses one budget for its summary request. Role repair
and text fallback share the initial dispatch's operation and consume the
same retry limits, including when the gateway enters routing again. A local
summary needs no upstream dispatch.

Models apply after provider/default options, followed by window overrides.
All applicable exhausted windows constrain eligibility together. When several
window policies apply, the most restrictive wait/count limits win. Window
options do not define response classification rules. Existing cooldown and
account eligibility remain separate from consumed retry counts.

## Classification and final disposition

Rules match HTTP status, actual machine code/type, bounded message, selected
headers, operation and phase. Specified predicates are ANDed. Operator provider
rules precede global rules; model-specific operator rules precede general
rules. Within each operator list the first match wins. Vendor code/type rules
precede vendor prose rules; adapters order each group explicitly, with specific
budget prose ahead of generic pace wording. Digits in request IDs are not
machine codes. Diagnostics retain the rule ID, not the raw message or headers.

Classes are unknown, transient-rate, quota, auth-refusal, request-refusal,
content-refusal and transient-outage. Scope is request, model, account or
unknown. Model scope means the selected account/model pair; it does not disable
that model on all accounts. Unknown scope is terminal. In particular, a
source-IP limit is not automatically an account limit. This contract does not
claim a proved egress identity or global shared-bucket authority.

Legacy gateway classifiers continue to own legacy cooldown/fallback decisions.
Configured message rules do not partially replace only their retry predicate.
Opted-in SDK final refusals use the shared classification and disposition path
instead of those legacy cooldown branches.
Valid empty legacy error envelopes retain status-owned busy retries.
An optional OpenAI `code: null` is absent machine-code evidence; a valid type
or message still participates in classification. A null-only, malformed,
duplicate-key or unrelated envelope does not establish complete response facts.

The `request-policy` capability, negotiated at API 1.12, admits
`request.recover` for the original selected provider.execute/tool.call/module.call
invocation. `DecideRequest`/`decideRequest` always classify a refusal, even with
empty evidence. `RequestWithPolicy`/`requestWithPolicy` repeat only their named
HTTP request after a host permit. They return transport errors unchanged and
perform no automatic redirect forwarding or credential changes. Private
component delegation does not gain this callback implicitly.

A positive permit must name the exact requested step, use action `wait`, and
agree with the request's complete response facts and explicit safe-replay
evidence. The public Go and JS helpers reject an invalid or contradictory
positive result before another HTTP send. A known terminal denial reason or
terminal class/scope cannot authorize a positive retry. Unknown diagnostic
reason strings remain forward compatible; a reason whitelist is not part of
this contract. Classification-only results grant no replay authority.

The callback retains the selected model from the original host-owned
`provider.execute` request and applies its model overrides. Callback facts
cannot select a different model. Durable acceptance contradicts
`rejected-before-effect` evidence and prevents a business repeat, including
acceptance observed during the host wait. A separately named `read-only`
download may still retry after an earlier paid step. Published output and
revoked selection remain barriers for both kinds of recovery.
When a valid recovery callback arrives after account or window access is
revoked, the recovery owner records the terminal decision against the
original scope before returning the refusal.
Disabling a module revokes retry authority for its active calls. Re-enabling
it gives retry authority to new calls in the new enablement generation.

A callback result is an intermediate step diagnostic. It cannot become the
final response's routing authority. For an entire execution refused before
its effects, return `ExecuteResult.refusal` using
`RefusedExecution`/`refusedExecution`, with fresh step/facts/rules/evidence and
optional windows. The host checks API negotiation, the original selected scope,
`outcome: not-started`, matching response status and absence of accepted/output
publication before recording `not-started` in the operation journal. An
invalid or contradictory refusal leaves an `uncertain` receipt. A repeat that
reaches the same operation identity stops at that receipt before fallback.
The accepted/uncertain/completed fence returns 409 before routing can continue.

An outbound executor can also negotiate `request-handoff` alongside
`request-policy` to report exhausted local admission with
`HandoffExecution`/`handoffExecution`. This final `429` has phase `admission`,
whole-execution rejection evidence, and no usage, vendor rules or availability
windows. The gateway tries the next eligible candidate immediately within the
existing dispatch budget. When no candidate can be dispatched it returns the
refusal and optional client retry hint. Local admission creates neither an
upstream failure observation nor an account cooldown. Host versions that lack
the capability reject modules that require it during manifest validation.

The common selector distinguishes bounded same-account waiting, moving to the
next already eligible candidate, and terminal refusal. Request/content refusal
never enumerates the pool. Quota/auth/model entitlement can select an eligible
sibling only with whole-execution rejection evidence. The module never supplies
replacement credentials or chooses the account. Known unavailable windows skip
only matching selections; a free-model refusal leaves an independent paid model
and another account eligible.

## Timing, availability and diagnostics

`retryAfterMs` is a nonnegative typed duration in milliseconds (at most thirty
days). `resetAt` is an absolute RFC3339 timestamp. The adapter extracts these
from a documented vendor field/message; it must not fabricate Retry-After or
copy a paid quota reset into a free allowance. The host uses its own clock and
the greatest applicable lower bound from backoff, typed hints, standard
Retry-After delta/date, legacy fractional seconds and rate-limit reset headers.
A bound that cannot fit is retained in `nextAllowedAt`, not shortened.
Public next-allowed values use canonical UTC wall-clock instants. Internally,
typed durations, Retry-After delta/fractional seconds and duration-valued reset
headers retain an elapsed-time floor anchored to the observation's monotonic
reading. Absolute resetAt, Retry-After dates and timestamp-valued reset headers
retain a separate wall-clock deadline. Mixed hints, applicable windows and
same-ID refreshes preserve the strongest bound in each domain; eligibility
requires both bounds to expire. Cached refusals carry these original bounds
back into scheduling without rounding remaining durations or turning their UTC
diagnostic projection into new timing authority. Elapsed-budget admission and
the scheduler retain their existing owners.

Final refusal windows use `id`, exact `models` or `notModels`, and optional
typed timing. Their applicability follows the SDK usage-window convention.
A window ID is only a label within the selected account. Availability is a
bounded, in-memory observation cache keyed by account generation and installed
package/configuration identity. It stores only proved future reset times;
unknown resets have no guessed persistent cooldown. Restart loses these
observations and permits a fresh probe; it does not rewrite saved quotas.

Callback and final-decision diagnostics identify the named step, action,
class/scope/rule, selected account/generation/operation, known next-allowed time
and consumed root/provider counts. They contain no raw message, body, headers
or credentials. Upstream wire diagnostics remain a separate observation owner;
neither wire observations nor earlier callback classifications authorize replay.

Once a selected, bounded recovery request reaches `recoverRequest`, the same
diagnostic owner records its terminal result even if initial admission fails.
Revoked package/configuration/account/lease/network/window authority reports
`revoked`; cancellation reports `canceled`, and published output reports
`unsafe`. Before classification, class and scope remain unknown and no rule
is invented. Reading the consumed counts does not admit or replenish a retry.
The original callback error is returned with no permit. Invalid or unowned
callbacks rejected before this owner acquire no policy-diagnostic authority;
an outer dispatcher refusal is not a new recovery attempt.

Validated complete fresh final refusal facts also supply bounded sanitized
original status/code/type to that common observation owner, separately from the
host's routing status, rule and class. Original HTTP200 business refusals retain
status200 even when the client receives429. Host-generated refusal responses
carry private response provenance, so their generated error fields are never
parsed as upstream facts. A transport cache hit carries no new upstream
observation. At preselection a terminal cache hit follows the existing rejected
request ledger path once, with no transport attempt. A later selection clears
the call's observation while the previous actual try retains its own.
