# ADR 0013: fence account usage and journal business cancellation

Date: 2026-11-13
Status: Implemented with scoped independent review; combined SDK checkpoint passed.

## Context

An entitlement change can invalidate a provider's earlier quota reading
without replacing its credentials. A cache key alone cannot stop an
in-flight read from publishing stale allowance, and clearing one process's
cache cannot invalidate another host. Models can also consume different
quota windows on the same account.

Separately, canceling a vendor operation can itself change remote state.
Using the target operation's identity for that cancellation would reuse its
execution journal and prevent a distinct durable cancellation intent.

## Negotiation and ownership

These guarantees require SDK API `{major:1, minor:4}`. Modules depending on
them declare `compatibility.hostAPI: "^1.4"`. An older host refuses the
descriptor instead of silently ignoring quota applicability. The native ABI
remains 1; no capability name or `component.configure` field is added.

The host owns selected-account scope, persistent revisions, admission and
operation receipts. The module owns vendor billing identities, mutation
ordering, unsettled operations, high-water marks and reconciliation. No
vendor controller, reset policy or general storage API moves into core.

Three identities remain independent:

| Identity | Meaning | Changed by |
| --- | --- | --- |
| `Scope.Generation` | Selected credentials and account replacement | Sign-in, refresh or explicit replacement |
| Usage revision | Quota invalidation for that account | Accepted `usage.invalidate` callback |
| `Scope.Operation` | This business write's intent | A new execution, mutation or cancellation |

Usage invalidation changes neither credential generation nor expiry.
Credential refresh preserves the account's usage revision and dirty state.

## Quota applicability

`UsageWindow` adds `models`, `notModels` and `aside`. The provider adapter
maps them to existing `QuotaWindow.matches` and `Aside`, using the existing
case-insensitive ID set and bare/`@context-size` matching. Other suffixes
remain distinct model names. There is no glob, prefix or vendor inference.

Omitted or empty selector lists mean account-wide applicability. Both lists
cannot be nonempty. Windows and each selector list are bounded to 1024
entries; selector IDs are at most 300 UTF-8 bytes, with empty, whitespace,
control and case-fold duplicate IDs refused. Known quota percentages must
be finite and within 0–100. The Go and JS SDKs validate these result fields;
the host validates them again against the actual leased manifest.
Contracts below 1.4 refuse even explicitly empty applicability fields.

An `aside` window is informational and contributes no routing limit. Its
unknown reading does not conceal an otherwise known ordinary quota.
Unknown ordinary quota still prevents a fresh acknowledgment and remains
unavailable as known allowance.

## Invalidation and read barrier

An accepted declared WRITE may call:

```json
{"method":"usage.invalidate","params":{}}
```

The callback returns `{"revision":"1"}`. Revision strings are canonical
unsigned decimal integers, bounded by uint64. There are no account,
provider, path or revision selectors in the callback. Scope is inherited
from the current call.

Admission requires API 1.4, `account-auth`, `usage-reporter`, a selected
account at its exact credential generation, WRITE component authority and
the same operation's durable accepted receipt. READ actions, recalls,
unaccepted operations, stale accounts and explicit selectors are refused.

API 1.4 READ actions cannot use `state.cas`. Declared `account-auth`
lifecycle calls `auth.start`, `auth.finish` and `auth.logout` may still
update private scoped continuation state without acquiring financial-write
receipts. This exception grants neither usage invalidation nor delegated
WRITE actions. Recall's read-only marker continues to deny CAS, and a
nested component READ does not inherit the root auth-method exception.
Host-owned refresh already has its separate WRITE authority.

The module invalidates after its acceptance receipt and **before** starting
the entitlement mutation. It must withhold a fresh usage acknowledgment
while that mutation or its vendor high-water mark is unsettled. A concurrent
read returns deferred, an error, historical data or unknown ordinary quota
until the module can assert that the reading follows the mutation.
Echoing a revision alone does not establish freshness.

The host captures the current account revision in `UsageRequest.revision`.
Caller credentials cannot be supplied through those params, and a supplied
conflicting revision is refused. The module echoes the revision in
`UsageResult.revision`.

A read can acknowledge only its captured revision at the same credential
generation. Freshness requires all of the following:

- No result error, historical `asOf` or deferred flag.
- At least one ordinary window.
- Every ordinary window has a known numeric share, no error and no `asOf`.
- The echoed revision matches both the request and the current Store.

The host atomically persists that acknowledgment. A stale read cannot
acknowledge a newer invalidation, even through another process. Invalidation
without a later fresh acknowledgment leaves the account dirty, including
after an interrupted or failed mutation.

`provider.execute` checks readiness before dispatch and rechecks dirty state
under the same Store lock that writes its dispatched execution receipt.
The second check closes the gap between an optimistic readiness check and
a concurrent mutation. A dirty account returns `usage_refresh_required`
with `outcome: "not-started"` before inference dispatch.

This does not revoke inference that was already admitted before
invalidation. The module's vendor controller remains responsible for its
own in-flight business operations and reconciliation.

## Persistence and cached allowance

The private account document upgrades to schema 3 on first invalidation.
It stores each account's revision and acknowledged revision alongside,
separately from, credentials and refresh receipts. The existing strict
cross-process Store lock and atomic write/sync path serialize invalidation,
acknowledgment and inference admission. Both usage members must be present
and nonnull unsigned integers; an empty object cannot default to a clean
revision. Missing or impossible schema-3 metadata, stale scope and revision
exhaustion fail closed. Reusing a clean
acknowledgment confirms directory durability before admitting it.
An older host cannot silently strip schema-3 metadata.

Provider usage keys include the revision, while model cache identity stays
unchanged. A quota carries an internal Store-backed proof of its account
generation, revision and validated freshness. Cached reads, pending waiters,
routing, display and history paths check that proof before reuse. Changing
one account invalidates that account's allowance entries; other account
entries remain. Published map snapshots are not mutated in place.

Serialized SDK quota history does not carry live authority. After restart,
it cannot restore known routing allowance without a fresh scoped proof.
This conservative cold-start behavior prevents a revisionless disk snapshot
from bypassing a cross-process invalidation. Stored history remains
historical display data.

## Business cancellation

For API 1.4, `operation.cancel` is a unary business WRITE:

- `Scope.Operation = C` identifies this cancellation intent.
- `OperationRequest.OperationID = T` identifies its target, or `reference`
  carries a bounded vendor reference.
- A supplied `C` equal to `T` is refused. An omitted intent is generated by
  the host; the host also defaults its attempt as for other writes.
- Selected-account scope and ordinary execution-journal rules apply.
- The only allowed stream event is the accepted receipt for `C`.
- The host journals `C` through dispatch and completion or uncertainty.
  It does not alter `T`'s journal or interpret the vendor's target outcome.

RPC context cancellation remains transport cancellation. It does not create
a business cancellation intent, and control-slot scheduling is unchanged.
`operation.status` remains a read. Modules at API 1.3 and below retain their
previous unjournaled `operation.cancel` control behavior.

## Source and verification

Public DTOs and validation live in
[`types.go`](../../pkg/extension/types.go),
[`usage_contract.go`](../../pkg/extension/usage_contract.go) and
[`usage.js`](../../sdk/js/usage.js). Host ownership lives in
[`usage_store.go`](../../internal/extension/usage_store.go),
[`usage_runtime.go`](../../internal/extension/usage_runtime.go) and
[`Lease.Call`](../../internal/extension/manager.go).
Provider projection and cache proof checks live in
[`extension_provider.go`](../../internal/provider/extension_provider.go) and
[`extension_usage_barrier.go`](../../internal/provider/extension_usage_barrier.go).

Offline regressions use actual executable workers, private Store documents,
foreign child processes and synthetic quota results. They cover accepted
WRITE authority, dirty inference refusal, concurrent revisions, stale and
nonfresh reads, credential-refresh preservation, corrupt/exhausted revisions,
pending waiters, disk history, applicability, informational unknowns,
separate cancellation journals and legacy cancellation compatibility.
Negative overlays remove admission, selectors, revision comparison,
intent separation and strict metadata decoding individually and require
behavioral assertion failures.

Terminal combined source hashes, gate results and platform limits belong in
[the SDK 1.4 validation record](0014-sdk14-validation.md). Synthetic and
cross-compiled checks do not establish live vendor behavior or native
execution on an untested platform.
