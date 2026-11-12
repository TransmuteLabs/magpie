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
store roots, package revisions, accounts and credential generations.

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

`StartExtensionAuth`, `FinishExtensionAuth`, `RefreshExtensionAuth` and
`LogoutExtensionAuth` use the installed authentication contract. Completed
sign-ins and refreshes save credentials through generation CAS; returned
helper results omit credentials. The manager supplies credentials through
`Frame.Auth`, rather than copying them into execute/models/usage parameters.
Renewal timing and native authentication logic remain separate from these
provider helpers.

Auth, model discovery and usage calls apply the selected provider/account's
`Via` policy while preserving their parent request context. A network hold
prevents invoking the module; a held usage read remains deferred. The
`network.proxy` host service resolves a target URL under that same parent
context through `netproxy.Func`, including named and global proxies or a
direct route. `Options.proxy` is the selected choice, not necessarily a
resolved proxy URL.

## Quota and execution

`provider.usage` returns validated quota windows through the existing
subscription quota path. Both the reading cache and routing freshness are
fenced by current account generations. Late reads cannot publish routing
state for a replaced account. External quotas do not reuse the legacy
name-only disk fallback across generations.

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

Native metadata is included under `Options.native` only when its module
matches the selected executor. A matching native request ID is also carried
as `Options.requestId`, with the host's opaque caller identity in
`Options.callerId` when present. Caller identity comes from trusted request
metadata, separately from the module's raw metadata. The operation ID hashes
module, provider, account, caller identity, the effective session and native
request ID, so equal request IDs in different callers or sessions remain
independent. Without a matching request ID, a call receives a new
random operation ID. Authors must give intentional regeneration a new native
request ID or session. Credential generation remains in the journal scope
for fencing, but does not change a logical native operation's identity:
reauthentication must not execute an already recorded request again.

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
`extension_models_test.go` checks custom-model prices and effort levels,
unknown versus zero prices, core overrides and rates, and rejection of
invalid refreshes without replacing the last validated snapshot.
`extension_network_test.go` checks selected account/provider/global proxy
resolution, named proxy authentication, direct routes, parent context and
held calls without dialing any target. `extension_replay_test.go` checks
that reauthentication retains a native operation's identity and original
journal scope without executing it again.
`extension_hold_test.go` covers catalog writes without local callbacks,
including install, enable/disable, upgrade and removal, and local changes
inside an active hold; its cache lifetimes are held long to exclude TTL
expiry as the cause of invalidation.

The gateway regression builds a small external Go worker from
`testdata/extensionworker`, installs it into a temporary configuration and
uses the real manager and `Server.Front`. It exercises worker death after
acceptance, a completed accepted failure, an ordinary successful completion,
completed-operation conflicts on repeated native request IDs and safe
fallback after an explicit not-started outcome. It does not rebuild the host
as part of installation or contact a vendor. Run the focused packages
under temporary HOME/XDG with pinned caches and offline dependency lookup:

```sh
go test -race -tags nogui ./internal/provider ./internal/gateway -run '^TestExtension(Provider|Auth)' -count=3
```

These fixtures establish the provider boundary, not native vendor parity,
background refresh scheduling, or complete Go/Bun runtime conformance.
