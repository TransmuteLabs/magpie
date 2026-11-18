# Shared request recovery policy

Status: response recovery and final refusal disposition implemented in API 1.12;
combined acceptance is pending. Reactive authentication continuity remains a
separate proposal.

Date: 2026-11-15.

Updated: 2026-11-16.

## Problem

Before this change, provider modules shared account selection, transactional
credential refresh and execution receipts, but had no explicit shared contract
for retry budgets and backoff. Adding a retry loop to each provider would duplicate
policy, hide attempts from the host and multiply the gateway's outer retries.

An unexpected authentication refusal also differs from expiry preflight.
Refreshing credentials during an existing call changes the account generation.
The current call deliberately pins its original scope. Merely replacing its
token would revoke subsequent events, callbacks and result publication.

The CLIProxyAPI comparison supplies regression scenarios, not a second runtime
dependency or a claim that its consolidated work in progress is accepted.

## Decision

The host owns one reusable request-policy engine. Its global defaults can be
overridden for a provider. The same resolved policy and remaining budget apply
to an outer route and its nested request recovery. Provider settings can reduce
or increase bounded attempts and delays; they cannot authorize uncertain work
to execute again.

The engine owns attempt admission, elapsed and attempt limits, exponential
backoff, jitter, Retry-After handling, cancellable waits and retry diagnostics.
The transport driver executes only the request for which permission was issued.
Both Go and JS modules use that contract. A native module uses the same owner.

The provider supplies vendor-specific classification: authentication rejection,
temporary rate refusal, exhausted quota, unsupported request or content refusal.
It also supplies the applicable no-effect evidence. HTTP status alone does not
prove that an operation is safe to repeat. A configured policy cannot override
that boundary.

Provider rules match bounded response facts through one shared matcher:
HTTP status, vendor error code/type, message, selected response headers,
operation and phase. All specified conditions must match, with explicit
precedence. The same 401, 403 or 429 can describe different failures; an unknown
message is not automatically an authentication, quota or temporary-rate
verdict. Classification also distinguishes request-, model- and account-scoped
failures so a malformed request cannot rest every account. Raw error bodies
and credentials are not copied into retry diagnostics.

An HTTP 200 response can still carry a vendor refusal in its JSON envelope or
stream trailer. Adapters extract those facts before classifying success.
Machine codes are compared as complete values, not digit substrings in request
identifiers. Provider code rules take precedence over broad prose matches.
Malformed or truncated error envelopes retain an unknown outcome rather than
becoming an empty, successful or safely retryable response.

Recovery is scoped to a named transport step. Retrying the download of an
already generated asset must not repeat generation, tool execution, the complete
handler or another account's request. Polling a declared job and OAuth device
authorization are separate workflows; their continuation state is not an
implicit retry of the operation that created them.

## Operator settings and server timing

The operator can configure retry count, total request budget, initial delay,
maximum delay, elapsed-time budget and jitter. Global defaults and provider
overrides use the same resolver. Changing an account, model or provider cannot
replenish the logical request's consumed budget. Documentation and ordinary
route tests must state which additional dispatches spend each limit.

The policy represents seconds, minutes and hours as account eligibility
boundaries, separately from the time a request may block. Waiting briefly,
trying the next eligible account and returning a timed refusal are distinct
actions. The existing routing mode supplies candidate order; the policy does
not introduce another strategy selector. A bounded short-wait threshold and
the remaining request deadline decide whether waiting is useful.

An account with an hour until reset does not hold a request while another
eligible account is ready. The host tries that sibling immediately. A
forty-second reset can justify waiting when the configured short-wait
threshold and remaining budget admit it. If no eligible candidate exists and
the wait does not fit, the host returns a finite refusal retaining the known
reset time. Representing a long quota window never authorizes an hours-long
blocking wait.

An explicit server retry time is a lower bound. Standard `Retry-After` can be
delta seconds or an HTTP date. Provider adapters also decode their documented
body fields or message formats into bounded duration or absolute-time facts,
with the original units and response phase preserved. They do not fabricate
HTTP headers or give a global text matcher authority over unrelated vendors.
An unrecognized or malformed hint remains unknown.

The host combines applicable server hints with the configured backoff. It
does not send before the greatest applicable lower bound. If that time exceeds
the allowed delay or remaining elapsed budget, it denies an immediate retry
and retains the timing reason for diagnostics. Cancellation interrupts a wait
without making its already consumed budget available again. Device polling,
quota reset observations and paid-job continuation keep their own declared
workflow semantics.

## Multiple accounts and failure disposition

Classification, same-account retry, candidate selection and persistent
availability are separate decisions. A request or content refusal must not
enumerate every account. An account-specific quota or credential refusal must
not disable its siblings. A model entitlement failure must retain the
account/model pair to which it applies; it is not evidence that every model or
every account is unavailable.

The free SWE allowance in Devin is a motivating case, not a hardcoded core
exception. An adapter identifies the applicable quota window and its proved
reset time. The host keys availability to the selected account generation and
declared exact model selectors. A window ID is an account-local label, not
proof of a bucket shared across accounts or egress addresses. Exhausting that
window must not block a separate paid allowance or an unrelated account.
Multiple applicable exhausted windows
constrain the next eligible time together. An unknown reset cannot acquire an
invented duration from an unrelated usage window.

The host owns whether a proved not-started execution can move to another
eligible account. The module cannot select replacement credentials through a
retry callback. Accepted, uncertain or partially published work remains fenced
through account and provider switches.

A classification reported for an earlier transport step is not the final
response's routing authority. Final disposition must be bound to the actual
refusal and its selected invocation. Tests must inspect both the reported
scope and the accounts actually contacted by the ordinary gateway. Diagnostic
scope alone does not prove that fallback or cooldown behavior follows it.

## Future authentication continuity

API 1.12 covers response recovery and final refusal disposition. It does not
grant reactive credential refresh. The following requirements describe a
separate future contract; the existing transactional expiry preflight remains
the current refresh owner.

A scoped recovery callback uses the existing transactional refresh owner.
It cannot select another account or accept replacement credentials from the
module. Concurrent callers join the existing owner. A retry uses only the exact
credential successor proved by its durable receipt.

The original wire scope, operation, attempt and journal identity remain
immutable. A distinct invocation authority tracks the effective credential
generation. Every event, callback, nested component and publication check must
use that authority consistently. Manual replacement, deletion, account pause,
package change or configuration change revokes recovery.

The existing journal equality and terminal-state rules remain in force.
Accepted or uncertain work never becomes not-started because recovery failed.
A callback may retry a separately identified safe read after a paid step; it
cannot repeat that paid step. Output already published to the caller also
prevents replay of the execution producing it.

## Implementation and acceptance

The current implementation is described by the
[shared request-policy reference](../subsystems/request-policy.md).
`internal/requestpolicy` owns classification, budgets, timing and availability;
the ordinary gateway/provider adapters retain account selection and final
disposition. The negotiated `request.recover` callback and the public Go/JS
helpers recover an explicitly named HTTP step. They do not refresh credentials
or retry an entire handler.

Scoped independent reviews have accepted selected-model/clock/budget behavior,
the exact CAS-successor publication boundary, fresh refusal observations and
the MiniMax named-read integration on their respective frozen sources. These
reviews do not establish combined host/module acceptance. The final combined
source still requires the whole-repository, runtime parity and installed-module
gates below. Expiry preflight remains the transactional refresh path; an
unexpected 401 has no reactive-refresh contract in API 1.12.

The implementation follows this sequence:

1. Record existing behavior and the donor scenarios before moving policy code.
2. Lock shared attempt/backoff policy with deterministic clock and random-source
   tests. Reuse existing gateway classifications and preserve defaults.
3. Add provider overrides and a single host entry for the policy. Avoid a
   second module-owned retry counter, timer or refresh manager.
4. Add the negotiated Go/JS response-recovery contract with explicit final
   refusal authority. Keep reactive credential continuity separate and do not
   advertise it while callbacks or publication reject the proved successor.
5. Connect provider adapters through the shared entry, then remove their
   superseded loops. Keep workflow polling explicit.

## Required behavioral verification

- Default policy, provider override, disabled retries, invalid settings and
  elapsed/attempt limits.
- Equal HTTP statuses with different vendor messages/codes produce distinct
  classifications; a provider-specific rule does not affect another provider.
- Equal vendor codes with different messages, headers or phases retain their
  distinct meanings; malformed or truncated envelopes cannot authorize retry.
- Retry-After and declared body timing as minimum delays; delta seconds,
  absolute dates, invalid hints and a lower bound beyond the remaining budget.
- Ordinary dispatch timestamps prove that waiting is honored, while
  deterministic clock tests cover the numerical boundaries.
- Shared outer and nested budget; cancellation during backoff.
- Two-account ordinary routes cover account quota, credential refusal, model
  entitlement, request/content refusal and unknown errors; a switch preserves
  the shared budget and leaves unrelated account/model state unchanged.
- A free-model window with a seconds/minutes/hours reset exercises configured
  wait, next-account and timed-refusal actions, including a short-wait
  threshold. The paid allowance and a sibling account remain independent.
- Generation/configuration replacement, pause, logout and removal revoke both
  dispatch and publication.
- A media sequence performs generation once when an asset read is recovered.
- A partial stream is delivered once and never triggers another execution.
- Measured usage, explicit zero, request diagnostics and routing traces retain
  one owner through recovery and cancellation.
- Both SDK runtimes and the ordinary gateway/MCP/search/media paths exercise
  the contract.

The proposed authentication-continuity contract additionally requires:

- Concurrent authentication refusals use one refresh exchange.
- Saturated worker slots do not deadlock a nested refresh.
- A second authentication refusal does not start another recovery loop.
- Every event, callback and publication admits only the exact proved credential
  successor while retaining the original operation and journal identity.

## Rejected approaches

- Independent provider retry loops: duplicate budgets and inconsistent safety.
- Retrying every 401, 429 or 5xx: status does not establish absence of effects.
- Reinvoking a complete handler after refreshing: repeats earlier paid steps.
- Mutating the original scope or weakening journal equality: breaks replay
  fencing and usage attribution.
- A timer inside a module deciding when to refresh a rotating grant: creates
  another refresh owner outside the host transaction.
