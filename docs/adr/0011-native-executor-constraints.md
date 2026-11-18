# Keep native-only requests with their owning executor

Date: 2026-11-13
Status: Implemented; bounded independent review and SDK checkpoint passed.

## Context

A canonical conversation can omit vendor-only controls or signed history.
Transporting private metadata only to its originating module protects that
metadata, but does not stop another provider from executing the incomplete
canonical projection after group routing or fallback.

## Decision

The decoded request may set `Executor: SameModuleExecutor`
(`executor: "same-module"` on the SDK wire). Empty keeps ordinary routing.
This policy is valid only for canonical conversational operations, not
catalog discovery or tools. The linked codec contract negotiates
`ExecutorContract()` at codec API 0.4; an installed module must also declare
SDK API 1.4, inbound decoding and outbound execution.

The host derives the executor owner from the authenticated request's pinned
inbound lease. A codec cannot name another module, select an account or
construct that authority through JSON metadata or an HTTP header.
Linked codecs without an installed-module lease fail closed when requesting
this policy.

The gateway filters resolved candidates, held/capped alternatives and
unlisted checkpoint candidates before execution-side selection. Late
attempts recheck compatibility. Provider signing, dispatch and the SDK
transport also check the live lease and host namespace before credentials,
body consumption or execution. An unavailable compatible executor produces
a refusal, preserving ordinary key/model/budget guards. Empty policies do
not remove an inherited constraint, and a nested native lease cannot replace
the original constrained lease.
Explicit null, non-string and unknown policy values fail decoding rather
than becoming an empty policy.

Native response encoding and matching outbound work retain the original
module generation through the existing pinned lease. The module still owns
receipt validation against the actual account, generation, model and native
request. This generic host policy neither validates a vendor signature nor
permits replay after an accepted operation.

## Verification boundaries

The regression set covers explicit negotiation, legacy and inbound-only
refusal, a closed/canceled lease, another host/module, nested-policy
inheritance and JS worker validation. The real executable fixture goes
through `Server.Front`, normal provider selection, groups, key aliases and
fallback with synthetic providers.

Terminal gate results and source hashes belong in the SDK 1.4 validation
record. Live Devin-client/vendor compatibility remains a separate module
acceptance requirement.

Independent probes first reproduced a nested empty-policy bypass: the
constraint retained lease A while dispatch used a replacement inbound slot
B for the same module and host. Admission now checks the exact original
lease, and empty inherited policies validate that binding before continuing.
The unchanged independent probes pass after the fix. The focused race
gates pass with 300 unit/runtime events across ×20 and 96 gateway/bridge
events across ×3. Independent executor/usage closure passes 160 events
across ×20. The [combined checkpoint](0014-sdk14-validation.md) records
the exact SDK source and separates application acceptance.
