# External provider adapters

The executable module and RPC owners are `internal/extension`. Provider
projection, authentication helpers and normalized transport are owned by
`internal/provider/extension_*.go`. Existing OpenCode plugins retain their
current owner and transport.

## Discovery and account policy

`ExtensionProviders` reads enabled installed manifests with
`outbound-executor` or an API 1.11 account-only descriptor. The shared
`ProviderDescriptor.AccountOnly` classification means empty protocols,
models and operations; manifest admission also requires `account-auth`.
Such accounts are visible to ordinary sign-in, selected search/tools, usage
and account actions, but have no inference endpoints or routable models.
A mixed module's discovery/executor capability does not grant these methods
to the service descriptor. Host admission and both author SDKs refuse those
calls before the handler; catalog reads and refreshes return no models and
never invoke discovery for the service. Generation, configuration, pause and
package fences remain the same as other selected accounts. Paid `accountTasks`
remain inference-owned work: a live execution parent and selected model are
required, so account-only descriptors cannot declare them, including in mixed
modules. Ordinary declared account actions remain available. `ExtensionProviderID` gives each module/provider pair a
stable core namespace; a module cannot replace a built-in by naming its
provider after one. `Accounts` projects the first account in use, with the
others available through `AlsoOn`. An entirely paused provider remains
visible, while `OwnPaused` prevents routing through it.

Discovery selects the eligible packages, their enablement and their saved
configuration versions in one checked catalog view. It releases the catalog
read lock before projecting accounts. Dispatch and model publication check
the current selection again.

Authenticated accounts come from `Manager.Store`, with their exact ID and
credential generation. Discovery, idle renewal and outside history each
read one checked account snapshot for the selected providers of a module.
Discovery reads account labels for display. Renewal and outside history
only project IDs, generations and expiration from that snapshot. Each
operation still checks its selected generation at admission.
Core remembers selection, order and on/off state in
its existing account metadata, without copying credentials there. Account
discovery off excludes authenticated extensions before their store is read.
Modules without `account-auth` are public providers and remain available.
Account IDs must remain distinct under the core's existing case-insensitive
account policy. An unreadable module configuration or account store, or an
ambiguous account set, excludes that module from discovery and logs the
module and cause. Discovery fails if the shared installed catalog or core
account policy cannot be read. The aggregate login list discovers SDK
providers once and marks the first enabled account active for each provider.

### Sign-in before the first account

Providers → Add → Subscriptions reads sign-in grants from the active SDK
catalog, without starting the module. A provider can appear here before it
has an account or routable models. Providers with an existing `window:page`
grant open their own sign-in page until the package declares a
`provider:signin` binding.

Sign-in choices and signed-in account rows keep the provider name declared
in the SDK manifest. They use the same checked package icon as Installed
when one is available.

A root manifest requests `provider-signin` alongside `account-auth` and
declares one `authBindings` grant with `from: "provider:signin"` per provider
it offers. Its polling methods declare labels and any text, secret or
selection fields the core form needs. The form sends only those fields.
Reauthorization also requires a grant and the selected account generation.
Both the form and the window bridge use the same credential broker. The form
has its own flow authority, bound to the selected package, configuration and
enablement generation. A flow cannot cross between the form and a plugin
window. A live flow also checks the worker's effective configuration before
dispatch and publication, including replacements at the same CAS version.
The journal retains its existing on-disk schema and numeric config version.

The account ID remains the key for routing, quota and reauthorization. A
valid authentication or refresh result name is stored with the account
and displayed in Providers and Usage, with the user's alias taking precedence.
A separate per-account label file records only the public name and its
generation. Credential storage retains its existing schema for rollback.
Existing accounts can reuse a successful refresh receipt for their current
generation. Without one, the next named sign-in or refresh
supplies the label.

After completion, the account appears in the existing provider catalog,
model list and Usage. Accounts disabled globally suppress SDK sign-in choices
and their operations. See [`ProviderSignIns`](../../internal/extension/provider_signin.go),
the [Providers route](../../internal/gui/providers_extension_signin.go) and
the [HTTP regression](../../internal/gui/providers_extension_signin_test.go).

For migration, `magpie plugin sdk auth import <module> <provider>
<saved-account-key>` prepares one current community sign-in for an installed
module. The provider sign-in grant declares `saved-signin` with one required
secret `record` field. This method is available only to the local import
command, including at the SDK dispatch boundary. Its SDK `auth.start`
returns a continuation without an interactive step or polling delay.
`auth.finish` verifies that continuation on the same module lease and
returns the new credential. State reads and writes during verification share
a temporary, bounded store, discarded when the verification ends. Modules
choose which saved formats they can verify. Verification uses the selected
source account's current proxy. Unreadable provider settings and a proxy
change before dispatch refuse the import.

The command returns a ticket and account summary. Prepared credentials stay
in a separate private namespace omitted by Providers, the gateway, usage
polling and the account observation stamp. The ticket expires after ten
minutes or at the imported credential's earlier expiry. Access to the ticket store and gateway
maintenance, including startup, remove expired copies. `magpie plugin sdk
auth activate <module> <provider> <ticket>` rechecks the package, module
configuration, enablement and account generation. Both preparation and
activation check the current source record under the community store's
writer lock immediately before their local writes. The digest uses compact
JSON with consistent HTML escaping, so rewriting the source file can keep
its tickets valid. It includes the community sign-in epoch, so signing in
again with the same key invalidates an earlier ticket. The SDK verifier
receives the credential record with the epoch removed. Malformed UTF-8 is
refused before decoding the source. A changed, unavailable or occupied
source refuses the operation. Activation preserves the existing
account store schema and the legacy sign-in for rollback. If the publication
is visible but disk finalization fails, the command returns the active
account summary with a warning. Rechecking the same ticket can complete
local setup without creating a new credential generation. Activation checks
the current installed plugin list under its writers' lock. An absent list
allows standalone activation. An unreadable or invalid list refuses it.
Activation adds the SDK account under its own provider ID. Routing
references and account policies still name the legacy provider and need to
be checked before a cutover.

Completing a sign-in or activating a saved sign-in registers a
missing account policy for the credential generation in the receipt.
Registration preserves existing pause and order settings. If policy
storage is unavailable, the committed sign-in remains complete.
The request can cancel while waiting for policy storage. The activation
keeps its ticket until local registration succeeds. Repeating activation
with the same ticket or reading the managed flow's completion again can
register the missing policy without repeating vendor authentication.
If `magpie plugin sdk auth finish` cannot register local policy, its
credential-free result includes the committed generation, owner revision
when present, and a warning. Use `magpie plugin sdk auth register <module>
<provider> <account> <generation> <owner-revision-or-empty>` to finish
local setup. The command checks the exact current generation and owner.

`magpie plugin sdk auth transfer <module> <provider> <ticket>...` takes
prepared tickets for the complete community subscription. Its ownership
record keeps the public provider ID and the public aliases of transferred
accounts. Providers, Subscriptions and Installed resolve that same owner.
The ordinary sign-in form carries the owner and package revisions. Selected
reauthorization and sign-out also carry the account generation. SDK switches
carry the configuration version. Signing out a removed subscription checks
the owner revision and each selected account's generation. A view from before
the transfer or rollback cannot change the current owner's credentials or
switches. CLI sign-in and sign-out for transferred subscriptions use the
managed `plugin sdk auth` commands so continuations and publication remain
bound to the current owner.
Other declared plugin pages and panels remain available.

`magpie plugin sdk auth rollback <public-provider> <owner-revision>` restores
the community projection after checking its original saved accounts and the
SDK credential generations recorded at transfer. A dispatched or uncertain
refresh, a changed account set or generation, or a dispatched SDK sign-in or
sign-out prevents automatic rollback. A refresh with a durable `not-started`
outcome still permits rollback. The host records the rollback constraint
before dispatching authentication and before acknowledging an accepted tool
that may replace the selected credentials. A credential-bearing call,
including model discovery, usage and component calls, also holds shared
subscription admission from dispatch through handler and callback completion.
An owner switch while a call is queued invalidates the call before dispatch.
Canceling a dispatched call leaves the admission in place until a confirmed
terminal or worker process exit. A native transport lost without a terminal
keeps the subscription busy until the hosting process exits. A Bun worker
declaring `provider-saved-signin` emits a terminal after its handler returns,
including on cancellation. Switching owners
and claiming credential work share the catalog lock across processes. Repeating a
completed rollback returns its receipt. The SDK package stays installed.
Removing the community package while the SDK owns its subscription keeps
the public provider's settings and saved sign-ins.
Restore the community package and enable it before rolling back. Transfer and
rollback hold the installed-plugin list lock from the source check through
publication. Waiting for the account settings and installed-plugin list uses
the command's deadline. Transfer checks the current discovery setting when
reconciling account settings. Import, activation, transfer and rollback
check it after waiting for saved credentials and before publishing each new
ticket, credential generation or owner revision. Unreadable discovery
settings, an unreadable current list or a disabled source stop the change.
Transfer and rollback check the public provider ID and custom-provider
collisions against the same current `providers.json` snapshot. They repeat
the check after waiting for the saved credentials and before each owner
revision is written. A missing file is an empty list. An unreadable
current file stops the transition even when a readable backup exists.
A transfer interrupted after it records a pending owner returns that
revision for recovery. Pending and rolled-back records suppress the SDK
subscription projection. A prepared ticket alone leaves both the community
subscription and the existing SDK provider ID unchanged.

`ListedAPIs` uses declared model protocols. The provider exposes the union
of models available to its enabled accounts; `Account.Lists` retains each
account's own restriction. `Account.fetch` invokes `provider.models` and
publishes a validated account-specific snapshot. A failed refresh keeps the
previous snapshot. Cache namespaces use `Manager.Namespace` and distinguish
store roots, package revisions, configuration versions, accounts and
credential generations. The validated SDK catalog is also persisted in the
existing private Store, so a fresh CLI can select dynamically discovered
models without starting another worker or vendor request. A measured empty
catalog remains distinct from a list that has never been fetched.
The in-process cache keeps up to 64 snapshots and 32 MiB of their encoded
catalog size. A cache hit checks the durable root's version and content.
An evicted snapshot can be reopened from the private Store.
Account eligibility uses the result and its known/unknown state from the
same read, including when another caller evicts the snapshot.

Catalog bytes are split into bounded content-addressed chunks. The root
record is published after all chunks and validation succeed. Publication
checks the selected package, configuration, enablement, owner and account
under the catalog and Store locks before comparing the root version read
before discovery. Account-bound publication checks the active settings
file at admission before reading the account. An unreadable or invalid
file blocks publication even when the settings editor can recover a
backup. A missing file uses the default. An identical result also
checks the root version and leaves it untouched. If another refresh already
changed the root, the caller reads that published catalog. Cancellation
before the write retains the earlier root. A write that completes during
cancellation still invalidates the provider cache. Cold reads
verify every chunk and the complete digest, then
apply the same SDK model validation as live discovery; unreadable data is
unknown rather than an empty or unrestricted entitlement list. Account or
configuration changes cannot reuse another selection's cache key. Each read
pins its account generation and cache key before lookup. Both warm and cold
reads recheck the account, package, configuration and enablement before
returning models. A deleted or replaced selection fails closed. A late cold
read cannot publish an earlier generation's catalog under a newer key.
Static models check the same selection and take their list from the manifest.

Models carry optional prices in USD per million tokens and explicit core
effort levels. The adapter maps these to `catalog.Model.Price` and
`Efforts`; the provider's declared price takes precedence over catalog
fallbacks, while core user overrides and rate policy still apply. Omitting
the SDK price leaves catalog fallbacks available; an explicit zero price is
free. Refreshed models are validated before publishing their account/store
snapshot.

`PriceAt` uses one lazy model projection per quote on an SDK account.
The free check and list pricing share it, including lookups by the vendor's
ID and the user's alias. Dispatch still checks the current account and
package selection at admission.

Codex's model tags track the exact saved catalog roots for projected
SDK providers with model discovery, including roots that have yet to be
published. A kept tag checks their file identity and metadata. A changed
root drops the held catalog before rebuilding the tag. Ordinary replies
carrying `X-Models-Etag` can therefore see a publication from another
process on the next request. The steady path checks only those files,
with no catalog discovery or worker call. Model reads still validate the
selected account and the root and chunk contents.

The extension catalog file is also part of `Hold`'s root checks and
`StampNow`. A catalog installed, enabled, disabled, upgraded or removed by
another CLI invalidates provider and model snapshots on the next request,
including GUI snapshots keyed by the stamp. Local manager changes notify
`catalog.Touched` so an active hold is invalidated too. These file checks
use the pure `extension.CatalogPath` helper and never acquire or replace the
default manager under the provider cache lock.

Private SDK account and root module-configuration metadata share a source
stamp. Each new hold, including one overlapping an existing request, observes
a scan that starts after the hold arrives. Holds already waiting for the next
scan share its revision. A hold arriving during a scan waits for a later
scan. This lets a new hold detect another process's account
replacement/deletion or configuration CAS before reusing provider/model
snapshots. A configuration transition rebuilds the
selected model catalog under its new version; already selected old bindings
remain fenced. Worker selection and the execute generation fence also compare
effective configuration values. This applies at dispatch after a worker slot
opens for SDK 1.1 and later. SDK 1.0 has no runtime configuration.
Replacing the configuration at the same CAS version retires workers
configured with different values. Reordering JSON
object fields leaves their configuration current. Unrelated private state and
account-local state do not invalidate it. `extension.AccountStamp` reads
directory/file metadata and handle-derived portable file identities without
reading credential contents, acquiring store locks or initializing a manager.
Identity distinguishes an atomic replacement
even when size, mode and mtime are preserved. Observations are serialized outside
the global provider cache mutex and published with monotonically increasing
revisions; a delayed old observation cannot replace a newer published one.
Unavailable metadata advances the invalidation revision for the whole batch.
Those holds can rebuild under the new revision. Each subsequent batch still
observes a fresh scan.
Quota-only account-file publication conservatively invalidates it too;
unrelated scoped state does not.
The stamp remains an optimistic cache hint: an in-place rewrite preserving
identity and metadata need not change it. Authoritative generation checks at
signing/preflight still reject the old binding and invalidate discovery, so
the following request reads the new account even in that case.

A request that already captured a revoked account generation stays fenced:
`RefreshedAccount` may follow only a proven refresh successor, never a manual
replacement. Signing/preflight records a terminal selection failure on the
existing `ExtensionAttempt`, so a stale local generation is returned as a
non-retryable conflict rather than entering generic upstream outage patience.
This is a not-started policy rejection, not a fabricated accepted/uncertain
write receipt, and it cannot overwrite an earlier durable terminal outcome.
The next request discovers a new binding and uses its current `Frame.Auth`.

The outbound adapter uses the selected account from `Sign` once in `Do`.
Direct calls, repeated attempts and credentials due for refresh run
preflight again. For SDK 1.9 and later, the selection pins the package,
configuration and enablement through the installed worker's dispatch
admission. The host checks the worker generation and the account under the
same catalog lock after any subscription admission. Sign-out removes
credentials in the same catalog-before-Store lock order.
After the worker queue opens, every supported SDK version checks
the account generation and discovery policy. Outbound provider requests also
check the current on/off policy for the selected public account, including a
transferred alias. Older inbound calls retain their original lease and
report the generation of the worker that answered.

`StartExtensionAuth`, `FinishExtensionAuth`, `RefreshExtensionAuth` and
`LogoutExtensionAuth` use the installed authentication contract. Completed
sign-ins save credentials and expiry through generation CAS; returned
helper results omit credentials. The manager supplies credentials through
`Frame.Auth`, rather than copying them into execute/models/usage parameters.

The ordinary Providers editor offers **Sign out** for each SDK account.
Its confirmation submits the SDK account selection through `/api/login/forget`.
A stale generation is refused before invoking
`auth.logout`. Once that hook succeeds, the store removes the selected
credentials by generation CAS. The account list and allowance cards drop
the removed account, and the provider keeps serving through its remaining
accounts. The subscription remains available for a new sign-in.

Refresh uses a host-owned [transaction](../adr/0007-transactional-account-refresh.md):
one cross-process owner rereads the selected generation, records dispatch,
and atomically publishes credentials, expiry, the next generation and a
sanitized completion receipt. An orphaned dispatch is uncertain and cannot
spend the same rotating token again. Caller cancellation after dispatch does
not abandon the bounded owner. Manager shutdown cancels and joins that work.
Publication stays in the originally selected Manager/Store.

Stateful authentication continuation uses the module's scoped private state.
API 1.4 permits that CAS for declared authentication lifecycle calls while
read-only actions and recall remain unable to mutate state. This does not
grant an accepted business-write receipt, usage invalidation or direct access
to the host-owned root configuration namespace.

Before execution, model refresh or usage, an account expiring within one
minute is renewed through the same owner. A local binding copy adopts only
the exact next generation and an expiry more than thirty seconds away.
Unknown expiry leaves current behavior unchanged. Network holds and manually
superseded generations fail closed. This is request-driven preflight,
separate from periodic scheduling and the module's vendor-specific exchange.

Typed `unauthenticated` errors from `provider.usage` also publish sign-in
state through the existing login metadata. Ordinary login rows, sign-in alerts
and idle renewal see the same mark. The adapter reserves a read token under the
cross-process login lock before dispatch; only that read may finish publishing.
A newer read therefore supersedes an older completion without a wall-clock
comparison. Failed, deferred, malformed or untyped reads do not clear a known
refusal. Only a validated fresh, clean usage result clears its matching mark.
Historical snapshots and unknown or failed windows preserve that refusal.
If the cross-process login lock is unavailable, token reservation and
publication defer without writing; reservation failure also prevents dispatch.
Paid task admission compares account policy without these transient read
markers. Its live check separately revalidates the selected generation's
authentication status, so a fresh usage read preserves task authority while
a selected-account authentication refusal still revokes it.
The mark is bound to the Store namespace, installed package, configuration,
enablement and exact credential generation. A replacement selection cannot
inherit it, even if a process switches the module off and back on after the
refusal's final selection check. The initial enablement retains its existing
mark identity. Idle renewal checks the same binding and discards a queued
selection if the enablement has changed.
Malformed mark metadata retains an existing refusal. No credential or vendor
error prose is copied into login metadata, and a plain HTTP status/error string
does not establish an authentication transition.

Selected account tools apply that same binding-aware authentication check in
[`PrepareExtensionAccountScope`](../../internal/provider/extension_tool_account.go),
before any renewal or tool dispatch and through its retained live check at
dispatch, host callbacks, acceptance and result publication. A typed usage
refusal for the selected credential generation blocks business sends and
refresh; public tools retain their unbound behavior. A mark for another Store,
package, configuration, enablement or credential generation cannot lapse the
current selection. The preliminary tagged-mark policy remains provisional so a genuine
replacement credential generation can recover. A tool's authorized account
replacement uses the existing publication-scope proof when checking its final
generation; it grants no renewed callback or replay authority.
`TestExtensionToolAuthPolicyUsesSelectedBinding`,
`TestExtensionToolUsageAuthRefusalRechecksPreparedDispatch` and
`TestExtensionToolUsageAuthRefusalBeforeHostCallback` cover admission,
stale-binding recovery and refusal arriving after preflight.

API 1.8 providers with `account-auth` may opt into idle authentication renewal
with `keepAlive: {leadMs: 60000}`. An absent/null declaration is off; an empty
object defaults to one minute. An explicit lead is an integer from one minute
through one hour. This renews authentication only, without model execution,
paid cache warming or inference accounting. Public and complete manifest
clones retain independent policy; private service manifests gain no account
renewal authority.

[`KeepExtensionsSignedIn`](../../internal/provider/extension_keepalive.go)
starts after the ordinary gateway listener binds and acquires the config
folder's serving lock. It uses that listener's SDK manager and skips the
renewal loop when the listener has no manager. Shutdown joins the renewal
worker before releasing that lock, while the listener drains separately.
Catalog inspection starts no worker or Bun, including with empty catalogs.
Only enabled providers and ordinarily listed, enabled, visible, nonpaused,
nonlapsed accounts with known expiry are eligible. Renewal, account tasks,
account-bound tools and check-ins check discovery in the active settings file
before admitting a call. An unreadable active file defers the work even when
the settings display can recover from a backup. The timer uses the selected account's
network hold/proxy policy and pins its package, configuration and generation
before calling the existing leased `auth.refresh` transaction. It verifies an
actual refresh-successor receipt, rechecks policy and binding, and never
adopts a manual credential replacement. Catalog and private account reads
follow the renewal owner's cancellation. Startup is cancellable; an already
dispatched rotating-token exchange finishes under the bounded refresh owner
before shutdown returns. Failures use the existing exponential backoff per
binding, capped at thirty minutes, without marking the account lapsed.

Auth, model discovery and usage calls apply the selected provider/account's
`Via` policy while preserving their parent request context. A network hold
prevents invoking the module; a held usage read remains deferred. The
`network.proxy` host service resolves a target URL under that same parent
context through `netproxy.Func`, including named and global proxies or a
direct route. `Options.proxy` is the selected choice, not necessarily a
resolved proxy URL.

## Declared provider switches (API 1.9)

Providers may declare bounded boolean `options` with defaults, localized labels,
descriptions and enable confirmations. These keys belong to the root module's
configuration; legacy plugin switches retain their existing settings owner.
Listing options reads the catalog and configuration without starting a worker.
Only declared values, the package revision and the configuration version reach
the provider editor.

The editor captures that owner when it opens. A redraw after an external
package or configuration change cannot transfer an existing edit or
acknowledgement to the replacement. Enabling a confirmed option asks even when
its text is available only in another locale. Save uses one checked
configuration CAS and returns only its version, preserving unrelated private
configuration in the Store. A successful CAS updates the editor's baseline
before the ordinary provider save; if that later save fails, retry does not
resubmit the committed options. Edits made while CAS is pending remain pending
against the new baseline and keep the editor open for another save. A failed
or missing CAS receipt makes the state unknown and requires reopening the
editor before another provider save, even if a later click removes its delta.
Cached metadata retains that refusal; ordinary reopen reads fresh metadata. The first
save of a fresh editor validates its complete declared switch snapshot through
CAS, even when unchanged, fencing a delayed write with an older version.
Further saves in that same editor can reuse its acknowledged unchanged
snapshot. New edits or a different editor opened while the ordinary provider
save is pending remain open when that save completes.

The shared lazy renderer retains legacy placement, explicit false values,
confirmation and delayed save behavior. Actual-source VM regressions cover
these state transitions; they do not establish browser layout or native
dialog behavior.

## Paid selected-account tasks (API 1.9)

An authenticated executor with `module-rpc` and `usage-reporter` may declare
`accountTasks: [{"id":"cache-keepalive","action":"cache.ping"}]`. Each task
names a declared write action on the primary or its private service.
[`account_tasks.go`](../../pkg/extension/account_tasks.go) defines the bounded
wire; [`AccountTasks`](../../internal/extension/account_tasks.go) owns the
gateway lifetime. Empty catalogs and inspection start no task worker or Bun.
This is separate from idle authentication renewal. The module owns explicit
paid opt-in, vendor templates, cadence, cache evidence, stop rules and limits.

While a selected foreground stream is live, the module first calls
`accountTasks.cancel` with its task ID and opaque conversation `key`, before
acceptance or vendor dispatch. This binds the current conversation claim,
cancels and joins its previous task owners, and fences late scheduling by an
older foreground. Other keys and host-derived callers remain independent.
After successful measured execution, `accountTasks.schedule` carries only
`task`, `key`, an opaque `job` and `dueAt` in Unix milliseconds. The module
keeps credential-free templates itself; registration supplies no account,
model, auth or vendor payload. The host waits for successful foreground
completion. Overdue work runs once immediately, without a catch-up loop.
There are at most 32 outstanding tasks per caller/account and 256 per owner.

Each tick acquires a new lease and uses the existing selected-account,
refresh, model, proxy, quota, routing-pool, rest, caller-budget and concurrency
owners. It does not choose a replacement account. Package/configuration,
credential generation and caller policy are rechecked before dispatch and
while active. Dirty usage must be reconciled through the existing revision
acknowledgment; a deferred or failed read cannot admit another foreground.
The module may register one successor from a successful live tick. No
stream, auth, vendor client or original request context survives a timer.

For a provider with model discovery, tick admission fetches and validates
the selected account's current catalog under its network hold and proxy.
A hot root-configuration CAS therefore requires no cold restart or explicit
catalog refresh before a new task can be admitted. An empty, failed or
malformed discovery cannot borrow an older catalog to admit the task.
Concurrent package, configuration and account changes still fence the read
and dispatch. Ordinary metadata-only catalog reads remain inert.

The private durable intent and ordinary operation journal prevent the same
tick from paying twice, including after an uncertain outcome or a new host
process. Tasks are not replayed after restart. An accepted action may report
cumulative `OwnCall` snapshots through `accountTask.usage`
(`ReportAccountTaskUsage` / `reportAccountTaskUsage`). The host retains the
latest validated snapshot; progress does not append ledger rows.
`usage.append` and the terminal result's optional `call` share the same
one-call gate. Termination, including cancellation, flushes admitted evidence
once through normal accounting, its observers and the original caller's
budget. The Usage row keeps the public provider and user selected
at dispatch, including when a subscription transfer ends the task. The
gateway supplies the existing `budget.Append` sink, which calls the
ordinary ledger append without creating another row. Missing evidence never
becomes invented zero usage. Canceled module callbacks cannot regain
authority. Joining the host owner does not prove that a vendor stopped work,
and ordinary accounting remains best effort rather than crash-durable
exactly-once delivery.

`ServiceManifest` delegates `component.accountTaskActions` only for the
service's write actions named by the installed root's `accountTasks`.
These services negotiate API 1.9 with `module-rpc` alone. Their task action
may emit validated progress or return terminal usage without acquiring
`usage-reporter`, `provider.usage`, outside-history, normal `usage.append`
or account-auth authority. Ordinary services retain their API 1.1 projection
unless another explicit declaration requires a newer version. The task
owner reads the pinned complete root descriptor; the public primary
handshake deliberately omits private actions and their task descriptors.
The host still requires its exact scheduled context and accepted receipt.

`internal/gateway/extension_account_tasks_test.go` exercises the real
`Server.Front`, a separately built synthetic module and a local paid endpoint:
ordinary lifecycle, conversation/caller isolation, current routing and
policy, refresh receipts, cancellation snapshots, normal accounting and
successor/no-replay behavior. `internal/extension/account_tasks_test.go`
checks durable intents and refusal of ordinary RPC authority; Go and JS task
tests check descriptor and wire parity.
`internal/provider/extension_account_tasks_catalog_test.go` checks hot
configuration admission with an actual model-discovery worker, current
account/proxy selection, failed and empty refreshes, holds, cancellation and
package/configuration/generation changes during discovery.
`internal/gateway/extension_account_tasks_service_test.go` runs separate Go
and JS private workers through the ordinary gateway, cancels after two
acknowledged snapshots, and checks the last facts, one normal observer row,
the original caller's spent budget and no second vendor call.

## Selected-account check-in (API 1.8)

An authenticated provider may declare
`checkin: {"action": "daily.claim", "recheck": false}` under `module-rpc`.
The action must be a declared write, on the primary or its private service.
[`extension_checkin.go`](../../internal/provider/extension_checkin.go)
appends exact account IDs to the ordinary check-in runner, Settings and
quota cards. Automatic execution defaults to off. Provider/account Off,
account exclusion, manual policy, the ordinary owner's day and retry gates
keep their existing owners. Identical display names never merge accounts.
Discovery uses the check-in caller's deadline for catalog and private
account reads. The runner rechecks the account's current On and visibility
before refresh and after primary or private service startup, before a write
can dispatch. A manual press remains available while the provider is Off.

[`Manager.CheckinAccount`](../../internal/extension/account_checkin.go)
pins package/configuration and selected account generation, rechecks after
lazy service startup and applies the selected refresh, proxy and hold
policy. Its unary request carries `day` and `proxy`; credentials stay in
`Frame.Auth`. The finite result preserves absent and explicit zero credit,
and accepts a bounded message and strict RFC3339 boundary.

The host-private [`checkin_intent.go`](../../internal/extension/checkin_intent.go)
reserves a durable operation before dispatch. The caller's context covers
the Store lock wait and the reservation up to the atomic rename. Once the
reservation commits, the host records the dispatch outcome durably even
when the caller cancels. Lost, invalid, failed or
uncertain completion refuses another write across manual presses, manager
restart, credential generation changes and loss of the display record.
Known not-started and ordinary completed non-claim rechecks get distinct
intents. The saved result retains the exact operation scope and terminal
state. The declared account check-in action accepts only a reserved intent
from this owner.

API 1.20's `rate-limited` result represents a completed rate-limit decision
with a required `until` boundary. The same completed receipt is returned
before that boundary, including after a manual press or a day change. A
successor gets a new operation ID when the boundary is reached. The ordinary
scheduler keeps the exact boundary in `ValidUntil` with its existing
`failed` display state. An SDK `failed` result still requires
reconciliation, even when it includes `until`.

Credit grants do not create an inference ledger row. See
[ADR 0024](../adr/0024-sdk-account-checkin-gateway-activation.md) for the
wire, reconciliation boundary and scoped evidence.

## Declared operations and model facts

API 1.3 adds a finite `ProviderDescriptor.operations` registry separate from
conversational protocols. The
[operation contract](../adr/0008-provider-operation-contracts.md) defines exact
endpoints, raw binary/multipart handling and the compatibility floor. An
omitted list retains conversational execution only. The actual leased
manifest and both author SDKs reject undeclared operations before dispatch.
`ExecuteRequest.operation` names the operation; `Options` preserves method,
path, escapedPath, query and selected proxy. Host credentials remain separate.

API 1.7 adds `systemone` with `ExecuteRequest.protocol == ""`. A model of
kind `decision` declares `operations: ["systemone"]`; its absent, null or
empty protocols mean no conversational API. Other model kinds keep their
existing protocol defaults, and pre-1.7 media operations keep the chat
envelope without acquiring a per-model operation requirement. Provider
admission and the actual leased execute both enforce the negotiated minor.
System One requires model discovery. Static decision entries are hints;
unknown, stale and successfully empty account catalogs grant no decision
operation. A cold empty catalog stays empty. The provider-wide display union
can resolve a group or classifier, but the selected account's current
catalog authorizes execution. No model is inferred from its name.

The adapter enters ordinary `/v1/systemone`, `/v1/decisions`, decision groups
and classifier routing. Each execution has its own acceptance state, usage
snapshot and operation receipt. Calibration and main classification remain
distinct executions, including under one native request ID. Accepted,
uncertain or completed-but-failed work stops account/group fallback and
the original classifier turn before previous-intent, default or nested
conversational dispatch. Errors retain their typed cause through deadlines
and caches. A cached/shared failure preserves the terminal fence but cannot
give another request the original operation receipt.

The decision owner consumes normalized usage once after reading and closing
the response: a present final snapshot overrides cumulative events, including
zero; an absent final snapshot retains the latest event. Only absent
normalized usage falls back to raw System One token counts. Measured terminal
failures reach the ledger, observers and recent-call/route diagnostics.
Decisions translation validates the answer before the SDK execution is
recorded as successful and appends no second billing row. This slice uses
normalized token/cache totals; SDK cost/currency is not a new monetary
conversion policy.

Kinds distinguish images, video, embeddings, rerank, speech and transcription
from chat. Modalities retain unknown versus explicitly empty values.
Maximum/default context, release, temperature, service tiers, subscription
rates and reasoning defaults reach the ordinary catalog. Invalid refreshes
retain the last validated catalog.

Ordinary image/video selection requires supported operations. Video IDs bind
continuations to the original module/provider/account, including after
credential rotation. Pausing that account cannot transfer its video to
another account that became the provider's default.

## Quota and execution

Execution accounting uses the ordinary ledger, routing history, Recent calls,
observers and budgets. The selected execution owns a deeply copied cumulative
snapshot. A present final snapshot replaces the latest event, including zero;
an absent final snapshot retains it. API 1.7's optional sparse `Usage.facts`
also preserves cache TTL, reasoning, deferred counters, vendor identifiers,
service tier, credits and reported monetary currency. An explicit USD amount
uses the existing USD cost override; another currency is retained as
`reported_cost` without conversion. Media closes the response before reading
terminal status and never appends an error payload to already streamed audio.
See [the accounting decision](../adr/0023-sdk-execution-observability.md) and
[`extension_usage.go`](../../internal/gateway/extension_usage.go).

An executor can report the model identifier it sent to its vendor with
`HeadersEvent.sentModel`, or `ExecuteResult.sentModel` if no headers event
was emitted. The gateway keeps a distinct identifier in the routing attempt
and Usage row as `wire`. Missing identifiers fall back to the configured
upstream model name. The host ignores identifiers longer than 200 bytes or
containing whitespace or control characters. It also strips
`X-Magpie-Sent-Model` from SDK responses so a vendor's response header cannot
supply this metadata. Older modules can omit the field.

When an accepted conversational execution returns an HTTP refusal, the
gateway's terminal fence still returns client409 and `X-Should-Retry:false`.

When a conversation stream reaches the gateway's idle limit,
`holdWriter.watch` cancels the selected attempt. The routing owner waits
for the watcher and reads the final execution receipt before it ends the
reply. An accepted or uncertain execution keeps its 409 outcome and the
no-replay message in the stream. Without a terminal execution receipt,
the idle limit ends the attempt with 504 before final-refusal retry
policy can select another account or schedule a wait. Watch and the
usage ledger keep the chosen terminal status and any usage already read.
The separate `upstream_failure` value retains the observed adapter status
and explicit bounded machine code/type before format repair or error-shape
translation, at the existing upstream response callback. Only the selected
provider's error body is observed as its consumer reads it: no eager read,
drain or success-body wrapper. A complete body of at most 64KiB supplies
explicit code/type; incomplete, unreadable or larger bodies retain status
alone. Close and read errors pass through. A later response supersedes the
snapshot; a local error with no response does not erase it. For SDK execution
this is the adapter-normalized status, not a vendor status hidden by that
adapter (for example a native error inside vendor HTTP200). Recent
calls, the route/try, the single ordinary ledger row, observers and failed-call
diagnostics receive the same value. It contains no message or headers and
grants no retry, refresh or fallback authority. Missing/malformed/oversized
machine fields remain absent; a failure with no observed HTTP refusal does
not invent upstream facts. For a held terminal refusal, body-enabled attempt
observers receive the final client fence response, not the discarded private
error body. No extra ledger append or journal write is introduced. This is a
host-private observation, separate from negotiated request-policy APIs.
Here observers means the ordinary in-process `usage.Observer` callbacks.
The public SDK ledger-observer allowlist and OTLP attribute schema do not
gain new fields in this change.

API 1.8 separates accepted module-owned calls from outside account history.
`usage.append` sends one measured call per live accepted module/tool write
intent through the ordinary `Append` and its observers. The host supplies
identity and refuses read/inference origins, revoked generations and replaced
package/configuration bindings. A provider's explicit `calls: true` opt-in
adds `provider.calls`, a bounded unary paged reader used by the existing
`ReadOutsideCalls` loop and `IngestReported`. History has reported-RID
deduplication and retains its watermark on failure; it has no live observer
effects. See [the own-call/history contract](../adr/0025-sdk-own-calls-and-outside-history.md)
for exact authority, limits, failure and best-effort delivery semantics.

`diagnostics.emit` is a scoped API 1.7 host callback. Modules provide bounded
codes/phases and numeric facts; the host supplies identity and applies a
sixty-entry per-module minute budget before the existing application logger.
Debug and info can use the first fifty entries. Warnings can use the first
fifty-five, and errors can use all sixty. Accounts and components share
these limits.
Free-text, request bodies and credentials are not accepted. Worker stderr is
still drained without logging. Diagnostic records do not mutate an account or
constitute an acceptance receipt.

API 1.8 `catalogAccess` declares `selected` or `inventory` authority for
`catalog.read`. The provider owner projects enabled, listed models, per-account
restrictions and cached quota windows with model applicability and timestamps.
The caller receives opaque account references, not account names, keys, endpoints
or headers. A missing or failed reading is unknown. Reading this projection does
not refresh vendor data. The host checks package, configuration, cancellation and
selected account generation before and after the owner read; private services
retain only explicitly granted root permission.

The separate API 1.8 `ledger-observer` capability and `ledger` descriptor use the
ordinary `usage.Observer` entry point. Only the serving gateway consumes the
bounded queue, with an allow-listed numeric/metadata projection. Bodies, sessions,
request IDs, account names, key labels and raw error text are omitted. Enqueuing
does not read a catalog or wait for a module. Oversized or malformed text
labels are omitted from the SDK projection. An invalid agent label becomes
`other`. Valid accounting fields and the saved ledger row retain their values.
Each queued delivery pins the
package/configuration target observed when the row was appended; replacement or
disable cannot redirect old rows to a new target. Calls are unary and read-only,
including delegated host callbacks. The host validates acknowledgements from
every runtime. Overflow and failed delivery are counted and
reported through the application log. Stopping the observer cancels and joins its
processor. Imported outside-account history retains its deliberate observer
bypass. This sink is not request capture or guaranteed durable export.

`provider.usage` returns validated quota windows through the existing
subscription quota path. Routing and cached card reads bind quota windows
to the account and usage revisions, package, configuration, enablement
generation and public subscription owner. An off/on cycle invalidates its
earlier allowances even when package and configuration stay the same. The
first ownership transfer also invalidates a reading made before it. A
fresh background reading can replace an older Usage-page card for the
same account. SDK accounts require their scoped reading after transfer,
including when legacy quota snapshots used the same public ID. Catalog
projection and the cached cards endpoint use the caller's context to
validate quota evidence.

The scheduled Usage refresh preserves the previous snapshot and its age
if account discovery runs out of time. It releases the pending read so a
subsequent request can retry. Completed discovery with zero accounts
replaces the previous snapshot with an empty one. Explicit refresh uses
its caller's context for the final SDK proof check before recording
history or replacing cached cards.

API 1.9 provider descriptors may declare `maxConcurrency` per account and
`maxAccounts` for active account fanout. Omission preserves the existing
default. Values are integers from zero through 1000; zero means unlimited.
The ordinary provider and gateway lane owners apply these defaults after
explicit provider/account settings, including a user's zero. The selected
package, configuration and account generation fence the declaration.
Queued requests recheck that selection before dispatch; cancellation and
response EOF release the existing lane slots. These defaults add no second
semaphore or account scheduler.

API 1.9 usage windows may carry optional `amount`, `balance`, `display`,
`unit` and `span` metadata. Amount is a nonnegative count; balance is a
signed reported wallet figure. Exact zero and an absent count remain
distinct through the native cards and JSON quota report. Numeric magnitude
is at most 2^53-1; display/unit are bounded text and span is at most
9223372036 integer seconds. Span and the vendor reset reach the ordinary
pace presentation. A historical reading retains its vendor display and
timestamp. This metadata supplies no missing `used` share, freshness,
financial balance authority, execution currency or measured cost.

API 1.4 adds exact model inclusion/exclusion sets and informational `aside`
windows through the existing quota matcher. A selected account also has a
durable usage revision independent of its credential generation. Only an
accepted declared write can call `usage.invalidate`, with no account selector
in its parameters. Dirty usage blocks inference, including the atomic
execution-journal admission boundary.

Usage reads capture and echo their revision. Only a current, fresh ordinary
reading can acknowledge it. The SDK validates the result and window `asOf`
and window `resetsAt` as Unix milliseconds through
`9999-12-31T23:59:59.999Z` before acknowledgment. Omission and zero denote
an absent timestamp. Explicit `null` is invalid. Unknown, deferred or historical results cannot reopen the
account. Store-backed proofs fence cached reads, late waiters,
routing and display across processes. Disk history alone restores no routing
authority after restart. See [ADR 0013](../adr/0013-usage-revisions-and-business-cancel.md)
for schema, durability and vendor mutation-ordering responsibilities.

The Usage page and quota exports both check the current subscription owner
before showing cached local readings. A legacy card kept under a public ID
that has transferred to SDK ownership is deferred until that owner produces
a current reading. Deferred cards retain display identity. Allowances,
balances, reset credits and plan renewal fields are withdrawn together.
An unreadable ownership record defers proofless local readings until
ownership can be checked.

The account editor uses its request deadline for account discovery, quota
proofs, history publication and check-in metadata. Expired reads skip
history publication. An account's shared quota read keeps its own deadline
so other callers can still receive the result.

Business `operation.cancel` at API 1.4 uses a new journaled intent while
retaining a separate target operation/reference. It leaves the target's
receipt unchanged. Transport cancellation and legacy API 1.3 control
cancellation keep their existing semantics.

Native-only conversations can require the original inbound module executor.
The host enforces that binding before signing and dispatch, including
aliases, groups and fallback. The module still validates its account/model
receipts; see [ADR 0011](../adr/0011-native-executor-constraints.md).

An explicit numeric `used: 0` is a real reading. Missing or null `used`,
`unknown: true`, or a window error produces an unknown text window instead.
Its public error text is bounded; the window keeps its
error and optional `asOf`. A known cached window also stays text with its
timestamp, rather than becoming a fresh percentage. Global `error`, `asOf`
and `readDeferred` remain distinct. Deferred data is not stamped as a new
read, entered as fresh history or used for new alerts. Fresh known windows
remain available beside unknown ones, but an incomplete account-wide subset
does not become an authoritative routing allowance.

The gateway selects and authorizes the model/account and applies its normal
budgets and concurrency limits before `Account.transport` calls
`provider.execute`. The request uses the selected canonical Chat, Responses,
Messages or Gemini protocol. The module translates to its vendor protocol
and returns the canonical response. Request options carry method, path,
query and selected inference proxy. Gateway credential headers are removed.
Streaming data uses bounded chunks and synchronous pipe writes, preserving
consumer backpressure. Usage events provide normalized token totals to the
existing ledger.

The gateway uses the SDK account catalog and declared operations for
embedding and rerank discovery. `/v1/models?kind=embedding,rerank`,
the Providers inventory and inferred retrieval groups share the same SDK
model-kind check. The gateway listing applies the usual disabled-model and
caller-key restrictions. Speech and transcription pipeline checks use the
same operation-aware catalog.

Translated readers confirm completion through
[`ExtensionResponseProtocolComplete`](../../internal/provider/extension_transport.go)
only after their real decoder accepted an explicit protocol terminal. Whole,
streaming continuation and search readers all participate. Their shared
decode state records actual typed failures from the Chat, Responses,
Anthropic and Gemini/Code Assist decoders; a later valid event cannot clear
that rejection. Observing this state preserves the decoders' existing
tolerance of unknown events. EOF alone, malformed events, a stream error
or an early caller close do not grant this signal.
The selected SDK body is recovered through private response-request context,
so response-body wrappers do not erase that ownership.

After this signal, body `Close` gives the remaining SDK data and authoritative
`ExecuteResult` one shared two-second grace, allowing `MaxChunkBytes` of
unread tail and one additional byte to detect overflow. The two seconds deliberately match the existing worker
[`retireCanceled`](../../internal/extension/client.go) cancellation grace:
the protocol reply has already ended, so this is a bounded local receipt
handoff, not additional vendor inference time. It is a host policy choice,
not a vendor service guarantee. Expiry or excessive tail retains uncertainty
even if a completed receipt arrives while cancellation joins the call.
Without the signal, `Close` cancels immediately and never drains an unfinished
reply. All concurrent `Close` calls and caller cancellation join the same owner.

A validated completed `ExecuteResult` remains authoritative when the client
protocol was rejected or closed early; protocol failure still uses its error
status and `ExtensionAttempt.Outcome(true)` prevents another execution. No
receipt is synthesized from a decoded terminal, drain EOF or cancellation.
Typed not-started failures retain their existing policy. The two-second grace
bounds receipt waiting and tail draining before cancellation, not every join:
the existing process worker may need its own cancellation grace, and an
in-process native worker that ignores cancellation indefinitely can still
block its owner. This transport change does not introduce worker termination.

Every SDK execution projects `Options.callerId` from the host-owned
`extension.CallerIDFrom(ctx)`, including ordinary canonical requests and
local-owner requests. Body fields, headers and native metadata cannot supply
or override it. Native metadata is included under `Options.native` only when
its module matches the selected executor. Only that matching native request
ID is carried as `Options.requestId`. The operation ID hashes
module, provider, account, caller identity, the effective session and native
request ID, so equal request IDs in different callers or sessions remain
independent. Without a matching request ID, a call receives a new
random operation ID. Authors must give intentional regeneration a new native
request ID or session. Credential generation remains in the journal scope
for fencing, but does not change a logical native operation's identity:
reauthentication must not execute an already recorded request again.

Same-protocol Chat execution bypasses the legacy `toolPairsOut` repair for
SDK providers. Consecutive assistant fragments, reasoning aliases, distinct
cache boundaries and missing tool results reach the module without that
repair joining or manufacturing them. The module owns validation and any
vendor-specific normalization. Ordinary non-SDK Chat relays retain their
existing repair. Other gateway policies and protocol translations are
unchanged.

Same-protocol Messages execution also leaves SDK-owned normalization to
the module. After restoring gateway-salted tool IDs, `advisorTools` still
enforces the caller key's model policy, maps allowed same-provider advisor
IDs to upstream names and removes unavailable advisors and their references.
Both Messages execution and `count_tokens` leave tool pairing to the selected
SDK provider, preserving incomplete exchanges, signatures and cache boundaries.
Other providers retain core tool pairing. SDK execution also leaves thinking
controls and billing lines as sent. Model selection and trusted execution
identity still use the ordinary host owners.

The manager owns durable operation acceptance and replay rejection. The
ordinary gateway inference path shares an `ExtensionAttempt` across its
attempts. Accepted, uncertain or already completed execution cannot trigger
an automatic retry or another member/account. Terminal refusals use HTTP 409
or the current stream's error event, with retry disabled and an operation
reference. An explicit not-started outcome leaves normal router fallback
available. This guard does not authorize replay or reconcile a vendor's
operation; status/cancel remain explicit operation-control calls.

## Verification

`extension_provider_test.go` in the provider package exercises account and
generation policy, model discovery, quota conversion, credential separation,
streaming, native metadata, stable operation IDs and store isolation.
The separate `extension_canonical_test.go` files in provider and gateway
verify trusted caller projection, private metadata ownership, exact Chat
tool histories at the installed SDK execution boundary and the non-SDK
repair control. Gateway cases enter through `Front`.
`extension_messages_body_test.go` covers omitted and explicit Messages
thinking controls, signed split tools, cache boundaries, token counting and
the non-SDK thinking-default control. `account_stamp_config_test.go` and
`extension_config_catalog_test.go` cover root configuration CAS, metadata
identity, unreadable sources, public/authenticated held catalogs and
unrelated-state stability.
`extension_models_test.go` checks custom-model prices and effort levels,
unknown versus zero prices, core overrides and rates, one price projection
per quote across two accounts, and rejection of invalid refreshes without
replacing the last validated snapshot.
`extension_models_cache_test.go` checks cold discovery without another worker
call, measured empty catalogs, configuration isolation, late refresh rejection,
chunk integrity and held-read account/configuration revocation. Its generation
barrier checks that a cold read cannot publish an earlier account's models
under a later generation.
`extension_network_test.go` checks selected account/provider/global proxy
resolution, named proxy authentication, direct routes, parent context and
held calls without dialing any target. `extension_replay_test.go` checks
that reauthentication retains a native operation's identity and original
journal scope without executing it again.
`extension_keepalive_test.go` and `extension_keepalive_policy_test.go` cover
idle opt-in/eligibility, unknown reads, package/config/account revocation,
selected proxy, backoff and a single real exec refresh shared with foreground
renewal. The gateway test of the same name goes through `ListenAndServe`,
checks that `New`, `Front` and failed bind stay inert, joins an in-flight
refresh on shutdown and serves empty catalogs without Bun. Public Go/JS/type
tests cover bounds, API compatibility and independent manifest snapshots.
`extension_decisions_test.go` covers decision projection and selected-account
warm/cold entitlements. The gateway test of the same name installs actual
exec and C-shared native fixtures, then exercises both public decision APIs,
group and original classifier replay fences, separate calibration accounting,
headers and receipt isolation. The leased-operation admission test checks
that unnegotiated work never reaches the durable journal.
`extension_hold_test.go` covers catalog writes without local callbacks,
including install, enable/disable, upgrade and removal, and local changes
inside an active hold; its cache lifetimes are held long to exclude TTL
expiry as the cause of invalidation.
`extension_generation_test.go` adds independent-store manual replacement with
long cache lifetimes, overlapping holds, revoked selection before dispatch
and preservation of accepted, uncertain, completed and replay outcomes and their
original durable operations. It also blocks an account observation to verify
the global cache lock stays available and a delayed older observation cannot
discard a newer catalog. `account_stamp_test.go` checks read-only metadata stamps,
atomic replacement with preserved metadata and unavailable namespace boundaries.

The gateway regression builds a small external Go worker from
`testdata/extensionworker`, installs it into a temporary configuration and
uses the real manager and `Server.Front`. It exercises worker death after
acceptance, a completed accepted failure, an ordinary successful completion,
completed-operation conflicts on repeated native request IDs and safe
fallback after an explicit not-started outcome. It does not rebuild the host
as part of installation or contact a vendor. Run the focused packages
under temporary HOME/XDG with pinned caches and offline dependency lookup:

```sh
go test -race -tags nogui ./internal/provider ./internal/gateway -run '^TestExtension' -count=3
```

An in-process cold binding is not proof of cross-process persistence. For
that check, let a real `magpie serve` discover a fixture-only dynamic model,
stop it, then run separate `magpie models`, `magpie model spec` and
`magpie group add ... models=...` processes against the same temporary
configuration. The fixture's invocation journal must stay unchanged during
those reads and selections. Replacing module configuration or credentials
must remove that model until discovery runs again; deleting the account
must remove it as well.

These fixtures establish the provider boundary and synthetic idle renewal,
not live vendor parity or complete Go/Bun runtime conformance.

SDK API 1.12 providers can return an explicit final `refusal` on a proved
not-started execution. The [shared request-policy owner](request-policy.md)
classifies that actual response, preserves selected account/model window
applicability, bounds short waits and applies the existing routing order to
eligible siblings. Intermediate callback diagnostics never authorize fallback.
The accepted/uncertain/output fence remains prior to this decision.
