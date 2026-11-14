# External provider adapters

The executable module and RPC owners are `internal/extension`. Provider
projection, authentication helpers and normalized transport are owned by
`internal/provider/extension_*.go`. Existing OpenCode plugins retain their
current owner and transport.

## Discovery and account policy

`ExtensionProviders` reads enabled installed manifests with
`outbound-executor`. `ExtensionProviderID` gives each module/provider pair a
stable core namespace; a module cannot replace a built-in by naming its
provider after one. `Accounts` projects the first account in use, with the
others available through `AlsoOn`. An entirely paused provider remains
visible, while `OwnPaused` prevents routing through it.

Authenticated accounts come from `Manager.Store`, with their exact ID and
credential generation. Core remembers selection, order and on/off state in
its existing account metadata, without copying credentials there. Account
discovery off excludes authenticated extensions before their store is read.
Modules without `account-auth` are public providers and remain available.
Account IDs must remain distinct under the core's existing case-insensitive
account policy; an ambiguous account set is refused.

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

Catalog bytes are split into bounded content-addressed chunks. The root
record is published only after all chunks, validation and the selected
package/config checks succeed. Interrupted or failed refreshes retain the
earlier root. Cold reads verify every chunk and the complete digest, then
apply the same SDK model validation as live discovery; unreadable data is
unknown rather than an empty or unrestricted entitlement list. Account or
configuration changes cannot reuse another selection's cache key. Each read
pins its account generation and cache key before lookup. Both warm and cold
reads recheck the account and selected package/configuration before returning
models; a deleted or replaced selection fails closed, and a late cold read
cannot publish an earlier generation's catalog under a newer key.

Models carry optional prices in USD per million tokens and explicit core
effort levels. The adapter maps these to `catalog.Model.Price` and
`Efforts`; the provider's declared price takes precedence over catalog
fallbacks, while core user overrides and rate policy still apply. Omitting
the SDK price leaves catalog fallbacks available; an explicit zero price is
free. Refreshed models are validated before publishing their account/store
snapshot.

The extension catalog file is also part of `Hold`'s root checks and
`StampNow`. A catalog installed, enabled, disabled, upgraded or removed by
another CLI invalidates provider and model snapshots on the next request,
including GUI snapshots keyed by the stamp. Local manager changes notify
`catalog.Touched` so an active hold is invalidated too. These file checks
use the pure `extension.CatalogPath` helper and never acquire or replace the
default manager under the provider cache lock.

Private SDK account and root module-configuration metadata share a source
stamp. Each new hold, including one overlapping an existing request, detects
another process's account replacement/deletion or configuration CAS before
reusing provider/model snapshots. A configuration transition rebuilds the
selected model catalog under its new version; already selected old bindings
remain fenced. Unrelated private state and account-local state do not
invalidate it. `extension.AccountStamp` reads directory/file metadata and handle-derived
portable file identities without reading credential contents, acquiring store
locks or initializing a manager. Identity distinguishes an atomic replacement
even when size, mode and mtime are preserved. Observations are serialized outside
the global provider cache mutex and published with monotonically increasing
revisions; a delayed old observation cannot replace a newer published one.
Unavailable metadata invalidates the snapshot rather than comparing as unchanged.
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

`StartExtensionAuth`, `FinishExtensionAuth`, `RefreshExtensionAuth` and
`LogoutExtensionAuth` use the installed authentication contract. Completed
sign-ins save credentials and expiry through generation CAS; returned
helper results omit credentials. The manager supplies credentials through
`Frame.Auth`, rather than copying them into execute/models/usage parameters.
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

API 1.8 providers with `account-auth` may opt into idle authentication renewal
with `keepAlive: {leadMs: 60000}`. An absent/null declaration is off; an empty
object defaults to one minute. An explicit lead is an integer from one minute
through one hour. This renews authentication only, without model execution,
paid cache warming or inference accounting. Public and complete manifest
clones retain independent policy; private service manifests gain no account
renewal authority.

[`KeepExtensionsSignedIn`](../../internal/provider/extension_keepalive.go)
starts only after the ordinary gateway listener binds and joins on shutdown.
Catalog inspection starts no worker or Bun, including with empty catalogs.
Only enabled providers and ordinarily listed, enabled, visible, nonpaused,
nonlapsed accounts with known expiry are eligible. Discovery off and failed
policy/store reads suppress renewal. The timer uses the selected account's
network hold/proxy policy and pins its package, configuration and generation
before calling the existing leased `auth.refresh` transaction. It verifies an
actual refresh-successor receipt, rechecks policy and binding, and never
adopts a manual credential replacement. Startup is cancellable; an already
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

The private durable intent and ordinary operation journal prevent the same
tick from paying twice, including after an uncertain outcome or a new host
process. Tasks are not replayed after restart. An accepted action may report
cumulative `OwnCall` snapshots through `accountTask.usage`
(`ReportAccountTaskUsage` / `reportAccountTaskUsage`). The host retains the
latest validated snapshot; progress does not append ledger rows.
`usage.append` and the terminal result's optional `call` share the same
one-call gate. Termination, including cancellation, flushes admitted evidence
once through normal accounting, its observers and the original caller's
budget. The gateway supplies the existing `budget.Append` sink; it calls the
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

[`Manager.CheckinAccount`](../../internal/extension/account_checkin.go)
pins package/configuration and selected account generation, rechecks after
lazy service startup and applies the selected refresh, proxy and hold
policy. Its unary request carries `day` and `proxy`; credentials stay in
`Frame.Auth`. The finite result preserves absent and explicit zero credit,
and accepts a bounded message and strict RFC3339 boundary.

The host-private [`checkin_intent.go`](../../internal/extension/checkin_intent.go)
reserves a durable operation before dispatch. Lost, invalid, failed or
uncertain completion refuses another write across manual presses, manager
restart, credential generation changes and loss of the display record.
Known not-started and ordinary completed non-claim rechecks get distinct
intents. The saved result retains the exact operation scope and terminal
state. Credit grants do not create an inference ledger row. See
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
does not read a catalog or wait for a module. Each queued delivery pins the
package/configuration target observed when the row was appended; replacement or
disable cannot redirect old rows to a new target. Calls are unary and read-only,
including delegated host callbacks. Overflow and failed delivery are counted and
reported through the application log. Stopping the observer cancels and joins its
processor. Imported outside-account history retains its deliberate observer
bypass. This sink is not request capture or guaranteed durable export.

`provider.usage` returns validated quota windows through the existing
subscription quota path. Both the reading cache and routing freshness are
fenced by current account generations. Late reads cannot publish routing
state for a replaced account. External quotas do not reuse the legacy
name-only disk fallback across generations.

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
reading can acknowledge it; unknown, deferred or historical results cannot
reopen the account. Store-backed proofs fence cached reads, late waiters,
routing and display across processes. Disk history alone restores no routing
authority after restart. See [ADR 0013](../adr/0013-usage-revisions-and-business-cancel.md)
for schema, durability and vendor mutation-ordering responsibilities.

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
Execution then bypasses legacy vendor thinking defaults and billing-line removal.
An omitted thinking control stays omitted, explicit thinking controls and
signed tool/cache history remain present. Ordinary non-SDK Messages relays
retain their existing shaping. Model selection and trusted execution
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
thinking controls, signed split tools/cache boundaries and the non-SDK
thinking-default control. `account_stamp_config_test.go` and
`extension_config_catalog_test.go` cover root configuration CAS, metadata
identity, unreadable sources, public/authenticated held catalogs and
unrelated-state stability.
`extension_models_test.go` checks custom-model prices and effort levels,
unknown versus zero prices, core overrides and rates, and rejection of
invalid refreshes without replacing the last validated snapshot.
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
