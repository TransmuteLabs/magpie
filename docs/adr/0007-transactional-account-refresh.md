# ADR 0007: host-owned credential refresh

Status: implemented; acceptance evidence is recorded separately from the design.

Rotating refresh tokens cannot be protected by a CAS performed after a
module RPC. Two hosts can exchange the same generation before either saves,
and canceling the RPC can discard credentials after the remote token has
already rotated. The host therefore owns the complete refresh transaction.

## Contract

A module that relies on this guarantee requires SDK API 1.3 and
`compatibility.hostAPI: "^1.3"`. The capability remains `account-auth`, the
native ABI remains 1, and `AuthRefreshRequest`, `AuthResult` and `Frame.Auth`
retain their wire formats. Old modules continue to work on the new host.

`Manager.RefreshAccount` and all `Manager.Call`/`Lease.Call` invocations of
`auth.refresh` enter the same coordinator. A module still returns replacement
credentials in its unary result. The host publishes them and returns only a
receipt containing the account ID, optional display name and expiry.
Credentials are not returned through the public refresh result.
An upgrade must drain refresh work already dispatched by an older binary;
compatibility admission cannot retroactively protect such an exchange.

`Account.ExpiresAt` has the same Unix millisecond unit as
`AuthResult.ExpiresAt`; zero means unknown. Negative expiry is refused.
Sign-in publication and refresh publication retain expiry with credentials.
An explicit account replacement supplies its own expiry, rather than
inheriting the old token's lifetime. Deletion clears credentials and expiry.
Renewal scheduling and provider execution policy remain separate adapters.

## Ownership and persistence

The owner takes a strict OS lock scoped to the Store, module, provider and
account, independently of credential generation. Paths are anchored and
hashed, directories are private, and lock files must be regular private
files. Unsupported locks and symlinks fail closed. The global Store lock is
held only for short reads/writes, so scoped callbacks and other accounts can
progress during a refresh.

After acquiring both worker slots and rereading the exact generation, the
owner writes a dispatched fence before sending a single RPC. The owner checks
the leased package, enablement and effective configuration, subscription owner
and current account discovery at admission. A caller that already selected a
worker generation binds renewal to that generation. The triggering operation
supplies its login policy where applicable, and account-bound tools also
check their proxy choice. The host generates the operation ID independently
of any request, session or attempt ID. The fence and completed receipt are
private metadata in the atomic account document.
Schema 2 and strict unknown-field decoding prevent old hosts from silently
discarding this metadata.

Publication atomically replaces credentials and expiry, increments the
generation and writes the sanitized completed receipt. Ordinary replacement
and deletion preserve the refresh record; a late refresh cannot overwrite a
new generation or resurrect a deleted account. Previous refresh records are
archived in the operation journal before their replacement. Active refresh
records are authoritative for operation inspection and cannot be changed
through the ordinary journal or module state APIs.

Same-process callers coalesce; other hosts wait on the OS lock and reread.
A retry of a successfully published generation returns its receipt without
another exchange or generation increment. An orphaned dispatch is persisted
as uncertain and blocks the same generation. Only a transport-proven or
module-declared `not-started` outcome permits a later intentional attempt.
There is no automatic refresh retry loop.

## Cancellation and shutdown

After the stdin writer drains prior work, the owner rechecks the dispatched
record and current authority before committing a prepared frame to the
writer. Other frames can proceed during the check. If one takes the writer
first, the refresh repeats admission for the same fenced operation. The
writer starts pipe I/O after the authority locks have been released.
Cancellation before this handoff prevents the exchange. The owner reports
`not-started` only after any existing fence has been durably finalized in
that state. If finalization fails, the outcome is uncertain and the storage
error is retained.
After handoff, a request can leave while the owner retains the selected
package, configuration and network-policy context through publication.
Stopping the idle renewal loop interrupts owner lookup and releases a wait
on a foreground refresh that is still queued. The loop waits for an exchange
that has crossed handoff under its existing deadline.
The RPC has a 60-second limit inside a 70-second owned lifetime; Store lock
waits retain their existing bounded, fail-closed behavior. At most 64
refresh owners are admitted per Manager.

The Manager tracks owners and independently pins their worker generations.
Shutdown closes admission, cancels owned work and joins publication/fencing
before closing clients. A Close caller whose deadline expires receives its
context error while the tracked shutdown continues. Dispatched operations
without a valid result remain uncertain. An already received valid result is
published independently of the original caller's cancellation.
Ordinary RPCs are stopped by closing their clients after that refresh join;
shutdown never waits for an ordinary lease to become idle before sending the
cancellation that lets it finish. Transport draining during application
handover remains a separate owner.

Unary refresh results have their own bounded account-result decoder. The
smaller scoped host-callback envelope limit does not apply to a replacement
credential result; valid credentials within the account-document budget must
remain publishable after the exchange.

## Limits and verification

The host cannot recover an unseen token after a process dies between remote
rotation and local publication. Its guarantee is a durable replay fence and
explicit reauthorization, not distributed exactly-once success. Native
modules still share the host crash domain.

Offline regressions use synthetic credentials, actual executable workers,
real Store files and child processes. They cover coalescing, independent
accounts, cancel-after-exchange, replacement/deletion, malformed results,
not-started history, failed publication, process death before/after commit,
shutdown, pinned configuration, expiry, strict locks and reader fences.
Vendor protocol parity and actual Windows/macOS lock execution require their
own platform evidence; Linux execution is not evidence for those platforms.
