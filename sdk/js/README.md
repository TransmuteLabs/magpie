# Magpie executable extension SDK

`@magpie/extension-sdk` is the dependency-free Node 22+/Bun author entry for
installed executable modules. It uses the same newline-delimited JSON contract
as the [Go SDK](../../pkg/extension/README.md). The root exports server
authoring functions and TypeScript declarations; `@magpie/extension-sdk/ui`
is the separate browser-only entry over the existing page bridge.

The SDK is alpha and shares Magpie's application release identity. Its checked-in
package version is the development placeholder `0.0.0-alpha.0`. From the same
checkout used to build Magpie, run `make sdk-js-pack VERSION=v0.1.2 SDK_JS_DIST=/path/to/output`
with that build's actual `VERSION`. The target accepts SemVer, removes a leading
`v`, and stamps a temporary package before ordinary offline `npm pack`; source
metadata stays unchanged and existing output artifacts are never overwritten.
It requires Node 22+ and npm. Untagged Git descriptions such as a bare commit or
`dev` require an explicit SemVer version. The release owner pairs this tarball
with the application built from the same checkout and version; this target
does not publish or run as part of ordinary Go builds.

Package release versions are separate from the wire contract revision API 1.15
and native ABI 1. Module manifest contract minima and capability gates keep
those protocol numbers, irrespective of the package release version.
Declare the minimum contract and capabilities your module needs. `CONTRACT` describes available
support; it does not upgrade a manifest or grant every capability. Explicit
SDK 1.0 manifests retain their nine-capability, single-primary behavior
without component configuration; the example below deliberately stays at
minor 0. No external module is required for Magpie startup.

| Author task | Minimum contract and owner |
| --- | --- |
| Frame handlers, scoped callbacks and binary/stream limits | Explicit API1.0 example below and [transport contract](../../pkg/extension/README.md#shared-contract-and-transport) |
| Private components, actions and instance lifecycle | [API1.1 components](#sdk-11-modules-and-components); [host lifecycle](../../docs/subsystems/executable-extensions.md#transport-lifetime-and-replacement) |
| Gateway recall/capture/sign-in hooks and private tools | [API1.2 hook and exposure limits](#gateway-hooks-and-tool-exposure-api-12) |
| Finite provider operations, model metadata and rotating credentials | [API1.3 provider contract](#provider-operations-models-and-refresh-api-13) |
| Routing order and candidate facts | [API1.4 routing](#routing-strategies-api-14) |
| Selected-account window actions and polling sign-in | [API1.5 actions](#selected-account-window-actions-api-15), [auth broker](#polllink-authentication-from-a-window-api-15) and [browser entry](#browser-author-entry) |
| Agent launch, profiles and Library targets | [API1.5 agent owner](../../docs/subsystems/extension-agents-sessions.md) |
| JSONL sessions and native account operations | [API1.6 session sources](#session-sources-in-api-16) and [native account operations](#native-account-operations-in-api-16) |
| Decision models and selected-account discovery | [API1.7 decision declarations](#capability-specific-declarations) |
| Check-in, activation, own calls, history, search, storage, catalog and ledger | [API1.8 check-in/activation](#check-in-and-gateway-activation-api-18), [provider owner](../../docs/subsystems/extension-providers.md) and [surface census](../../docs/subsystems/extension-surface-coverage.md) |
| Provider switches, paid tasks and effective agent/model facts | [API1.9 declarations](#capability-specific-declarations) and [provider owner](../../docs/subsystems/extension-providers.md) |
| Owned agent processes, agent-backed resume and related migration inputs | [API1.10 process contract](../../docs/subsystems/extension-processes.md) |
| Account-only providers without inference | [API1.11 account-only declarations](#capability-specific-declarations) |
| Bounded response recovery through existing host retry owners | [API1.12 request policy](../../docs/subsystems/request-policy.md) |
| Selected budget policy | [API1.13 budget policy](../../docs/subsystems/plugin-runtime.md#executable-budget-policy) |
| Scoped search of built-in Memory from local tools | [API1.14 tool recall](../../docs/subsystems/memory.md#sdk-tool-recall) |

The [manifest validator](manifest.js) and capability-specific validators own
author admission. Ordinary host adapters own selected accounts, permissions,
publication and lifecycle. Descriptor validation alone does not make a tool
private or prove a host access path. See the
[installation/owner reference](../../docs/subsystems/executable-extensions.md)
for package authority and the
[ADR evidence index](../../docs/adr/README.md) for historical decisions and
source-bound validation. Source availability is not an npm publication,
vendor migration or whole-application acceptance claim. Examples and tests
install and publish nothing.

```js
import { serve, SDKError } from "@magpie/extension-sdk";

await serve({
  manifest: {
    schema: 1, id: "example", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 0 }, capabilities: ["tool-provider"] },
    tools: [{ name: "echo", inputSchema: { type: "object" } }],
  },
  handlers: {
    async "tool.call"(ctx, call, stream) {
      ctx.signal.throwIfAborted();
      const state = await stream.hostCall("state.get", { key: "counter" });
      if (!state.found) throw new SDKError("missing", "Counter is not configured");
      return { content: call.params.arguments };
    },
  },
});
```

## Capability-specific declarations

Decision models require API1.7 and use `kind: "decision"`,
`operations: ["systemone"]` and no conversational protocols; missing, null
and empty decision protocols all mean none. The provider declares the
operation and model discovery. The host uses the selected account's
discovered model association, rather than static hints or another account's
catalog. Execute uses `protocol: ""`; admission and dispatch require the
module's own negotiated minor 7. Explicit older manifests retain their
earlier negotiated behavior.

Own-call reporting, account history, search sources, native storage sources
and catalog projection require their API1.8 declarations. `catalogAccess`
grants either an inherited selected-account or inventory read; `catalog.read`
returns visible models and cached quota evidence without credentials or
vendor refresh. The independent `ledger-observer` capability with `ledger: {}`
receives `ledger.observe`. This is a bounded, sanitized, read-only
numeric/metadata sink, separate from body capture and memory hooks. The host
retains original package and configuration authority, counts delivery drops
and never waits for it while writing a ledger row.

API 1.13 primaries can declare `budget-policy` and
`budgetPolicies: [{ id: "policy" }]`. After owner selection, the host calls
`budget.reserve` and `budget.spent` with bounded numeric policy facts.
Use `validateBudgetPolicyCall` and return `{ restrictions: [] }` or one-based
scope references and bounded retry intervals. The host retains native
reservation and accounting and grants the handler no host callbacks.

API 1.14 tools can declare `exposure: "local-owner"` and `memorySearch: true`.
From the declared tool's `tool.call` handler, use
`await stream.hostCall("memory.search", { query, limit: 5 })` to receive
`{ hits: [{ kind, text }] }` within the caller's scope.

An API 1.15 primary can declare `masking-rules` and
`maskingRuleSets: [{ id: "team", rules: [{ kind: "SDK_TICKET", regex: "ticket-[0-9]{8}" }] }]`.
After explicit selection, the gateway applies the declarations through
its existing masking engine without calling a worker method.

A primary can use API 1.12's separate `request-observer` capability with
`requestObserver: {}` to receive `request.observe`. Validate `call.params` with
`validateRequestObservation` and acknowledge with `{}` or no return value.
The process-local `requestId` groups start, numbered attempts and end.
The bounded, best-effort feed carries routing metadata without bodies,
raw headers, session IDs or credentials. The worker has no host callbacks.

Provider switches, paid selected-account tasks, effective agent model
metadata, provider defaults, rich quota facts and descendant session sources
use API1.9 typed declarations and validators shared by Go and JS. Ordinary
configuration, usage, budget and Sessions owners retain publication and
policy. Paid tasks require an explicit descriptor and an admitted parent
call; the source version does not schedule them.

Declared owned agent processes, agent-backed session resume and related
inputs for session migration require API1.10. The host retains launch,
session and file authority; declarations grant no unrestricted process or
file access. See the [process owner](../../docs/subsystems/extension-processes.md).

Account-only services require API1.11: a provider with `account-auth` and
empty protocols, models and operations. Search, tools, usage and declared
account actions reuse the selected-account lifecycle without inventing an
inference endpoint. In a mixed module, inference capabilities belong only to
its inference providers; service accounts cannot discover or execute models.
Older contract minors retain their existing admission rules.

## SDK 1.1 modules and components

Composition and `module-rpc` require API1.1. Keep the manifest's explicit
minor version at 0 for an existing SDK 1.0 worker. A 1.1 module
receives SDK-owned `component.configure`, `component.ready` and
`component.stop`; ordinary calls require successful configuration.
`sdk.describe` stays available before configuration and after stop.

Register declared action handlers in `module.actions`, not
`handlers["module.call"]`. The SDK dispatches `module.call {action,input}`
to that component's handler, exposes `call.action`, and replaces
`call.params` with the input. Action owner, service methods, acyclic
dependencies, compatibility, bindings and package paths are validated
against the manifest.

ModuleRPC is unary. A write action may emit and await `accepted` before its
effect; every other event is rejected, including read-action acceptance.
A forbidden event also fails the terminal result if author code catches the
emit error. Existing non-ModuleRPC capability streams retain their protocol.
Root `module.call` input uses the bounded frame budget; scoped host callbacks
retain `MAX_HOST_BYTES`. A large session journal does not make ModuleRPC
streaming or enlarge a service callback's authority.
`stream.hostCall("component.call", {binding,input})` carries the unchanged
parent scope and does not forward `call.auth` in the callback.

An optional `module.start(ctx, snapshot)` returns
`{endpoints?: Record<string,string>, dispose?: () => void | Promise<void>}`.
Its context survives a successful configure call and ends on component stop
or transport shutdown. Configuration is immutable for that instance:
`{component,contract,instance,config:{version,values},services}`. Version is
a canonical uint64 decimal string. Endpoint URLs are loopback HTTP(S) with
an explicit port; no endpoint is dialed by the SDK.

Business handlers receive the frozen per-call `ctx.module`. Use
`moduleContextFrom(ctx)` for an independent mutable clone of config,
dependencies and endpoint maps. Stop aborts active handlers/ACK/host-call
waits and joins the disposer and cooperative startup cleanup once.
Private startup/disposal diagnostics are not serialized. Author code must
cooperate with cancellation; shutdown waiting remains bounded by
`shutdownTimeoutMs`.

Startup may also return `state`, an opaque instance-local value. Business
handlers receive the same reference at `ctx.module.state` and through
`moduleContextFrom(ctx).state`; it is deliberately not JSON-cloned, frozen,
or included in readiness/wire messages. Config/services remain independent
snapshots. Use this for per-instance controller state rather than globals
shared by multiple `serve` instances.

`moduleHTTPHeaders(moduleContext)` returns a fresh plain object with
`X-Magpie-Component-Instance`, `X-Magpie-Component` and
`X-Magpie-Component-Config`, from the instance, component and config version.
Stamp these on the component's HTTP responses. The helper reads no opaque
state, config values or account auth and adds no HTTP server/client framework.
The host proxy owns proof validation and removal before frontend delivery.

`publicManifest` publishes only the current component's actions and clears
runtime packaging, services, window, bindings and compatibility.
`serviceManifest(root, id)` derives that service's runtime/entry, root
identity/version, SDK 1.1 ModuleRPC contract, component identity and owned
actions. `checkCompatibility` additionally checks declared native ABI and
host platform; plain validation does not load native code.

## Browser author entry

The `ui` entry imports no Node/Bun module and delegates the installed
`window.magpie` page bridge:

```js
import { ui } from "@magpie/extension-sdk/ui";

await ui.ready;
const snapshot = await ui.config.get();
await ui.config.cas(snapshot.version, { mode: "selected" });
const answer = await ui.backend.call("page.start", { prompt: "hello" });
```

`ui.service` is the existing bridge's absolute token-proxied service URL.
`ui.theme`, `ui.context` and `ui.visible` expose the current frame state;
`onTheme`, `onVisibility` and `onAction` subscribe to its existing events.
`ui.read(name, params)` uses declared page permissions. PNG operations
`ui.image.copy(blob)` and `ui.image.save(blob, name?)` use the same parent
bridge. They do not grant additional permissions.
The adapter preserves bridge errors. It does not add a per-call AbortSignal,
streaming RPC or another transport: frame disable/update/unmount cancellation
is owned by the existing page bridge. Config DTOs use `{version,values}`;
CAS takes `(expectedVersion, values)`, where values is a JSON object.

Handlers receive `ctx: {signal: AbortSignal}`, an immutable call and a
stream. `call.auth` is the host-selected account's JSON authentication
value, separate from `call.scope`. Scope includes `plugin`, `provider`,
`account`, `generation`, `session`, `operation` and `attempt`. Unsigned
64-bit generation/attempt values are numbers when safe and `bigint`
otherwise; outgoing scope is serialized as JSON numbers without rounding.
Never convert a large generation to `Number`.

`stream.emit(event, data)` waits until the host acknowledges that event's
sequence number. Only one event per call may await an ACK; a simultaneous
second emit rejects. Handlers must await emits and host calls before returning.
`stream.hostCall("state.get" | "state.cas" | "network.proxy" | "account.replace", params)` creates a distinct
child request with `parent` and the original scope. It never forwards
`auth`. Authority is inherited from the parent, not chosen by request params.
The host remains responsible for account-generation checks and CAS.

To resolve a request's effective proxy, call
`await stream.hostCall("network.proxy", {url: targetURL})`. The result is
`{url: effectiveProxyOrEmpty}`: an empty string means direct. The host
resolves account/provider proxy settings and global bypass policy from
the parent context. This service only resolves configuration; it never
dials the destination. Pass the returned proxy to the module's own
transport rather than copying global proxy environment variables.
`NetworkProxyRequest`, `NetworkProxyResult` and `HostMethod` describe
the public JSON contract.

`stream.hostCall("account.replace", {credentials})` replaces the account
selected by the parent scope and returns `{id, generation}`. The host
requires a durably accepted operation: emit `accepted` with the operation
ID and await its ACK before calling this service. The SDK preserves the
parent scope and leaves all authorization/generation checks with the host.
A refusal is propagated without retry; no replacement can select another
account by supplying a parameter. `AccountReplaceRequest/Result` describe
the DTOs, with uint64 `generation` represented as a safe number or bigint.

The transport accepts `call`, `cancel`, `ack` and `host.result`, and emits
`event`, `result` and `host.call`. `sdk.describe` is built in and returns a
detached manifest without runtime/entry/args, matching Go `Manifest.Public`.
The wire uses `deadline` in Unix milliseconds for Go compatibility;
`deadlineUnixMs` is also accepted on incoming calls and exposed on `Call`.
When both spellings appear they must agree. Cancellation or a deadline
aborts the signal, rejects pending ACK/host-call waits, and produces one
terminal result. EOF cancels every active call and waits up to
`shutdownTimeoutMs` (default 1000, maximum 30000) for handlers to clean up.
Author code must stop its own timers/processes when aborted.

An aborted call can return its terminal error without waiting for an ACK.
The worker remembers at most 1024 outstanding closed event credits and 1024
sent child requests canceled with their parent. An exact late ACK or a
matching child result is consumed once; unknown, duplicate or evicted
replies are protocol errors. A call ID cannot be reused while its canceled
credit is still remembered, matching the Go SDK. Once that ACK is consumed
or evicted, the ID may be used again. Hosts should allocate fresh call IDs:
an old and a new ACK with identical ID/sequence are indistinguishable.

There are at most 32 pending handlers and 32 pending host calls. Concurrent
host calls from one parent are serialized, with at most 32 queued per parent.
Handlers
that ignore cancellation retain a slot until they settle. Each JSON line
is at most 24 MiB excluding its newline. Native request bodies may be up
to 16 MiB. The `data` event carries a base64 string decoding to at most
64 KiB; other event JSON is also at most 64 KiB. Host params and host
results use `MAX_HOST_BYTES`, 1 MiB plus 64 KiB (1,114,112 UTF-8 bytes),
so a full 1 MiB state value fits with its service metadata.
`encodeBytes(Uint8Array, maxBytes?)` and `decodeBytes(base64, maxBytes?)`
validate canonical base64 and byte limits. They return base64 strings and
`Uint8Array` respectively; use `MAX_CHUNK_BYTES` for streaming bodies.

Stdout belongs exclusively to RPC. The SDK prints no diagnostics or raw
exceptions. Ordinary thrown errors become plain `internal` errors; only
an explicit `SDKError(code, message, outcome?)` exposes its message.
Do not put secrets in public errors, stdout or module diagnostics.
The SDK uses only the supplied input/output streams and does not change
global logging functions. Authors remain responsible for their own code.

Manifest validation checks schema/version, clean package paths,
capability/descriptors, duplicate IDs/paths/models/protocols and all required
handlers. Supported contract versions are 1.0 through 1.8. Handlers outside the
advertised capabilities are rejected. Runtime is `exec`, `bun` or `native`; protocol
names match Go: `chat`, `responses`, `messages`, `anthropic`, `gemini`.
The manifest does not grant filesystem or network sandboxing.

Packaging metadata also accepts `runtime: "native"` for the Go C-shared
libraries described by ADR 0004. A native package has the same clean
relative `entry` and negotiated SDK contract, with `args` absent or empty:
libraries have no process argv. The public handshake omits runtime,
entry and args for every runtime. JS/Bun `serve` remains a stdio worker;
native library loading and the C ABI belong to the Go host/SDK.
This manifest parity does not load or test a native library from JavaScript.

An inbound decoder may also return `DecodedRequest.protocol: "models"`
or `"tool"`. A tool decode requires both `inbound-codec` and
`tool-provider` on that module and uses a base64 JSON `ToolRequest`
body (`{id, arguments}`). The host resolves the module's own frozen tool
descriptor through the existing MCP JSON-RPC key and budget policies.
Native decode itself performs no vendor operation. `"tool"` is an inbound
decoded operation, never a provider protocol or an additional capability.

| Capability | Required handlers |
| --- | --- |
| inbound-codec | inbound.decode, inbound.response |
| outbound-executor | provider.execute |
| model-discovery | provider.models when inference-provider descriptors exist; otherwise requires inbound-codec |
| account-auth | auth.start, auth.finish, auth.refresh, auth.logout |
| usage-reporter | provider.usage |
| operation-control | operation.status, operation.cancel |
| tool-provider | tool.call |
| agent-wiring | agent.plan; agent.inspect for rich descriptors |
| session-migration | session.convert |
| module-rpc (SDK 1.1) | module.call (auto-dispatched from module.actions) |
| gateway-hooks (SDK 1.2) | gateway.request / gateway.response / gateway.signin, only as enabled by hooks |

## Selected-account window actions (API 1.5)

A window action binding may declare `provider`, fixed to a provider of the
same module with `account-auth`. Primary bindings and service endpoints cannot
grant accounts. Existing root bindings and two-argument calls stay compatible.

`ui.backend.accounts(binding)` returns only
`AccountSummary{id, generation, expiresAt?}`. The user selects an entry;
`ui.backend.call(binding, input, {account: {id, expectedGeneration}})` sends
that selection outside business input. No first-account fallback is made.
Use strings for generation, including values above 2^53. The backend author
entry exports `validateAccountSelection`; the browser entry keeps its own
Node-free types and delegates through the existing parent bridge.

The host owns ref/part/provider, verifies exact current generation and fixes
the backend scope/auth on one lease. Neither input fields nor a module
callback choose credentials. The primary and private helper inherit the same
scope; auth is never put into summaries or frontend configuration. Handlers
must not return auth in their own domain results.

Selected writes require a stable parent request intent. Host persistence
binds that intent to the selection, binding and business-input fingerprint;
changed values conflict before a new effect, including across Store reopen.
The action still emits ordinary accepted receipts. The host rechecks
window admission after lazy startup and before acceptance/callbacks.
It does not silently renew credentials or adopt a replacement generation.

The optional hybrid bundle `--account-bindings` mode contains
[a typed account picker](../../examples/extensions/hybrid-go/ui/account-app.ts).
The [GUI owner fixture](../../internal/gui/extension_window_accounts_integration_test.go)
checks native/helper selection and lifetime; its presence is not a passing
result on another source. Window action bindings grant no account scope to
ordinary MCP calls: those use the separate [tool owner](../../docs/subsystems/plugin-tools.md).
The [ADR evidence index](../../docs/adr/README.md) preserves the selected-window
decision and historical validation without transferring acceptance.

## Poll/link authentication from a window (API 1.5)

Declare root `authBindings` with `id`, exact `from`, same-root `provider` and
`methods: [{id: "device", mode: "poll"}]`, plus optional `reauthorize`.
The root needs `account-auth`. This grant is separate from action bindings:
an existing `binding.provider` does not grant sign-in.

For the standard Providers → Add → Subscriptions form, declare
`provider-signin` alongside `account-auth` and set the binding's `from` to
`provider:signin`. Each polling method supplies a `label` and can declare
`text`, `secret` or `select` fields. The host passes the collected values
to `auth.start` and uses the same polling broker as the window flow.
`window:page` bindings continue to open their plugin page.

```ts
const started = await ui.auth.start("signin", {method: "device"});
const progress = await ui.auth.finish("signin", started.flow);
```

Schedule another finish using `retryAfterMs` while the state is waiting or
pending; core enforces the next-poll deadline itself. `ui.auth.cancel` takes
the same binding and opaque flow. The return type is `WindowAuthFlowView`,
with display/progress fields and a safe account summary on completion.
The host retains module continuations and credentials; it does not return
raw authentication results/errors to the iframe.

The page sandbox permits scripts but blocks approval popups. Use
`createAuthLinkPresentation({container, input, select})` from the `ui` entry
with a labeled input and a "Select URL to copy" button. Pass the latest
`WindowAuthFlowView` to `update`; only waiting/pending HTTPS addresses without
embedded credentials are shown. The helper makes the input read-only and
selects its text on click for ordinary keyboard/context-menu copying. Tell
the user to copy the address into a browser tab, then check sign-in. Terminal
flows, invalid URLs and `null` clear the field; call `dispose()` on pagehide.
This is browser presentation only: it adds no opener, clipboard permission,
bridge method or host API requirement. Bundle this SDK helper with the page.

When `reauthorize` is declared, pass `account: {id, expectedGeneration}`
outside `input`. A completed sign-in or an existing granted account picker
provides a safe summary. Exact generation is checked; no successor is adopted.
The first admitted flow upgrades that module's account store to schema 4, so
co-running older CLI/gateway Store readers must be upgraded. No such upgrade
occurs during metadata/configuration discovery.

`poll` is a transport mode, independent of a vendor method's name. The broker
owns start/finish timing, selection, continuation and credential publication.
It does not provide a callback receiver. A module can own that receiver in
its [component lifecycle](#sdk-11-modules-and-components), retaining private
instance State and releasing resources in its disposer. Polling alone does
not implement a loopback/PKCE callback or select an arbitrary auth method.
Key/snapshot import requires its own declared entry path.
See the [window auth owner](../../docs/subsystems/executable-extensions.md#window-integration)
for current authority and the [ADR evidence index](../../docs/adr/README.md)
for the broker decision and historical verification.

## Provider operations, models and refresh (API 1.3)

A provider's `operations` is a finite list, separate from conversational
`protocols`: `embeddings`, `rerank`, `images.generate`, `images.edit`,
`audio.transcribe`, `audio.translate`, `audio.speech`,
`videos.create`, `videos.get`, `videos.content`, `videos.delete`,
`videos.remix`. They require `outbound-executor`, contract minor 3 and the
OpenAI-compatible `chat` family. `ExecuteRequest.operation` selects one;
ordinary conversation and native token-counting calls keep it empty.

The author server and actual leased Go-host descriptor both validate
permission before dispatch. Declaring metadata or a service does not grant
an operation or register arbitrary URLs/public routes. The typed
`ExecuteOptions` has method, path, query, proxy choice and optional
caller/request/native metadata. The host includes `escapedPath` only for
API 1.3; API 1.1/1.2 retain their earlier option shape. Use `network.proxy`
for effective network policy rather than treating the proxy-choice field
as a dial URL.

API 1.3 model fields add `kind`, `inputModalities`, `outputModalities`,
`maxContextWindow`, `released`, `temperature`, `serviceTiers`, `free`,
`rate`, `rateWas`, `alwaysThinks`, `dynamicTools`, `defaultEffort`.
Omitted/null modalities mean unknown; `[]` explicitly means none and survives
public snapshots. Temperature omission differs from false. Ordinary host
catalog projection preserves the declared facts and user overrides.
Decision metadata has its own API1.7 operation/model validation; provider
switches, paid tasks, defaults and rich quota facts use their API1.9
declarations. These grants do not follow from the API1.3 model fields.
See [capability-specific declarations](#capability-specific-declarations)
and the [provider owner](../../docs/subsystems/extension-providers.md).

Require `compatibility.hostAPI: "^1.3"` for a module that relies on rotating
`auth.refresh`. The handler still makes one unary exchange using the
host-selected `call.auth` and returns replacement credentials plus expiry.
The Go host owns strict per-account cross-process locking, durable dispatch
fences and atomic credentials/expiry/generation/receipt publication.
The public caller receives only a sanitized receipt. A module mutex or
`state.cas` cannot substitute for this owner.

`AuthResult.expiresAt` is Unix milliseconds; zero is unknown. Ordinary
provider execute/model-fetch/usage preflight can renew known near-expiry
credentials and adopt only a durably proven refresh successor. Manual
replacement/deletion still revokes stale selections. The module must not
silently rotate/retry inside execution. Canceling a dispatched caller ends
its wait while the bounded host owner publishes or fences the result;
an unseen rotated token lost during a crash still requires reauthorization.
See [ADR 0007](../../docs/adr/0007-transactional-account-refresh.md).

API 1.3 attachment-aware session modules may declare the read-only
`session.inputs.v1` ModuleRPC action. The host validates the input plan,
reads approved paths/digests from the selected native store and pins
planner/converter to one lease. It does not grant the module arbitrary host
filesystem access or generic session reader/resume registration.

## Check-in and gateway activation (API 1.8)

An authenticated provider can declare
`checkin: {action: "daily.claim", recheck: false}`. Require minor 8,
`account-auth`, `module-rpc` and the corresponding declared write action.
The ordinary host check-in owner retains Settings, the default-off daily
switch, account exclusion, manual policy, retry gates and quota cards.
The action may belong to the primary or a private service; public
handshakes omit check-in metadata for actions owned by another component.

Use `validateAccountCheckinRequest(call.params)` for the owner's canonical
`day` and selected `proxy`. Credentials stay in `call.auth`. Await
`stream.emit("accepted", {operationId: call.scope.operation})` before the
write and return `validateAccountCheckinResult(result)`. Outcomes are
`claimed`, `done`, `inactive`, `ineligible`, `failed`, `captcha` and
`own-app`; optional `credit` and `streak` preserve absent and explicit zero.
`message` is at most 4096 UTF-8 bytes. `until` is a strict RFC3339 boundary
with at most nine fractional digits.

The host durably reserves the intent and retains its terminal operation
reference in the ordinary saved result. Unknown or failed completion blocks
another write across manual/daily calls, process restart and credential
generation changes. Known not-started and ordinary completed non-claim
rechecks use new intents; credit grants create no inference billing row.

A root `activation: ["gateway"]` requests a lease after successful listener
bind. The ordinary gateway stays cold during `New`/`Front`, metadata reads
and failed binds. Disable, removal, configuration replacement and shutdown
release and retire that generation through the bounded component disposer.
Startup failure leaves ordinary HTTP serving. Private services inherit no
activation. See [ADR 0024](../../docs/adr/0024-sdk-account-checkin-gateway-activation.md)
for the host owners and scoped verification.

## Routing strategies (API 1.4)

Declare root `routingStrategies` with `id`, `name`, `action`, optional
`flags` and `timeoutMs`, and declare that action with `effect: "read"` under
`module-rpc`. Require contract minor 4. The
[tested descriptor fragment](../../docs/adr/0012-sdk-routing-strategies.md#author-contract)
matches the installed Go worker fixture; no additional runtime capability is
required.

The ordinary provider/group `routing` setting is `moduleID/strategyID`.
Enabled declarations appear in the existing selector, with no worker startup
from discovery. The action receives `RoutingRequest`; use exported
`validateRoutingRequest`, and return one unary `RoutingResult` validated by
`validateRoutingResult(result, request.seats.length)`. Its `order` is a full
permutation of seat indexes. Public
[TypeScript usage](test/routing-types.ts) is checked alongside the other
consumer fixtures.

The action may be implemented by a private service. `publicManifest` filters
each component's routing descriptors together with its owned action list;
the complete root still registers the private strategy and the host lazily
dispatches it to its declared backend. Public handshake metadata does not
replace the root's installed contribution authority.

Flags are `exact`, `byUse`, `byFree`, `byPlan`, `bySpeed`, `byPrice`, retaining
the existing Strategy semantics. Metrics and opaque seat/wallet identities
carry no auth, selected-account binding or conversation. Bounds are 64
descriptors, 256 seats, 32 renewals per seat and 1 MiB JSON input/output,
with finite measurements and safe integer timestamps. API 1.4 read authority
refuses persistent `state.cas` writes.

The ordering context defaults to 500 ms; explicit `timeoutMs` is 25–2000 ms
and covers cold acquisition plus RPC. The upstream Strategy signature has no
HTTP caller context. Errors, streaming/invalid results, deadlines and revoked
package/configuration results preserve listed order and all core guards.
The [ADR evidence index](../../docs/adr/README.md) links routing decisions and
source-bound historical checks. Those records do not assert a pass on the
current application or on a different module.

## Gateway hooks and tool exposure (API 1.2)

Declare `hooks: {request, response, signIn, timeoutMs, origins}` with the
`gateway-hooks` capability and contract minor at least 2. At least one flag must be
true. Register ordinary functions in `module.handlers` under
`"gateway.request"`, `"gateway.response"` and `"gateway.signin"` for exactly
the flags declared. The SDK owns validation, cancellation and wire dispatch;
authors implement no additional RPC adapter. See the
[real process fixture](test/fixtures/gateway-worker.js).

`GatewayRequest` carries an object `body` and `GatewayContext`: origin,
protocol, model, stream, path, agent, session, cwd, query and kind.
The request handler returns `GatewayRequestResult` with optional replacement
object `body` and JSON `state`; null/undefined means an empty result.
`GatewayResponse` carries request, reply, success, state and the same context.
`GatewaySignIn` carries `{event: GatewaySignInEvent}` with provider,
providerName, account label, state (`signed-out` or `ending`) and optional
`endsAt` as RFC3339 text, never epoch milliseconds. These labels do not grant
account authority.

Request hooks are read; response/sign-in hooks are write. All are unary.
Only a write may emit an acknowledged `accepted` receipt; every streaming
data/progress event is rejected, and catching the Emit error still fails the
terminal result. A response/sign-in result may contain domain fields such
as `outcome`; it is not an inference executor outcome. Gateway handlers
receive no selected credential auth, provider/account or nonzero account
generation. Populated credential scope is refused before invoking the handler.

State is bounded to `MAX_GATEWAY_STATE_BYTES` (64 KiB) of UTF-8 JSON, both
when returned by request and received by response. Origins default to
`local` and `magpie-key`; explicit entries may be `local`, `magpie-key`,
`gateway-key`, `remote` or `browser`, without duplicates. Browser is an
explicit SDK opt-in: a loopback web page is not granted the default local
memory recall/capture access, even when it uses the default key. The legacy
request-hook origin model remains unchanged. A sign-in hook requires local
permission. `timeoutMs` is 0/default or an integer from 100 through 30000;
the host owns that execution budget.

Tools may declare `exposure: "local-owner"` or `"shared"` with API 1.2 or later.
Omission keeps the legacy shared default. The host adapter owns discovery and
invocation filtering; SDK descriptor validation alone does not make a tool
private. API 1.2 accepts compatibility `hostAPI: "^1.2"`.

Our memory module can combine these hooks/tools with its own private store,
service and UI. That is independent of access to Magpie's built-in shared
Memory owner, which is not supplied by these DTOs. See the
[surface coverage matrix](../../docs/subsystems/extension-surface-coverage.md).

An agent descriptor with only `id` and `name` remains a manual action.
Rich descriptors declare any nonempty `bin` (an executable basename),
`configFiles` or `fields: [{key, label}]`; they require `agent.inspect`
as well as `agent.plan`. These metadata fields are independently optional:
bin-only and fields-only agents are valid without config files. There are
at most 128 config files and 128 fields per agent.
Config paths are case-insensitively unique. They are exact clean paths
below HOME, at most 1024 UTF-8 bytes, with no root/traversal, wildcards,
Windows drive/backslash or `.magpie-extension` location. No files are read
by the authoring SDK during descriptor validation.

Session descriptors may remain plain `id`/`name` actions, or declare a
complete `from`/`to`/`outputDir` route. Formats must be distinct IDs and
the output directory follows the same HOME-relative path rules.
`AgentInspectRequest/Result` and the optional `SessionResult.id` mirror
the public Go DTOs. A capability needs its descriptors: executor/auth/usage
need providers, agent wiring needs agents and session migration needs sessions.

Models may declare `price?: ModelPrice | null` and `efforts?: Effort[]`.
An absent or null price means unknown; an object, including `{}` or an
object of zero rates, means an explicit price and can be free. Price rates
are finite nonnegative numbers per one million tokens: `input`, `output`,
`cacheRead`, `cacheWrite`, `cacheWrite1h`. Omitted rates are zero; currency
is USD when omitted or explicitly `"USD"`. The SDK preserves unknown
versus free and does not look up or borrow another model's price.
Efforts are at most seven unique SDK levels, preserved in order:
`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`.
`ultra` and aliases such as `light`, `extra_high` and `auto` are not
advertised levels in this SDK contract.

For embedding, `serve(module, {input, output, shutdownTimeoutMs})` accepts
Node/Bun readable and writable streams and resolves on clean EOF. Invalid
framing or transport failure rejects with a sanitized `SDKError`.
`examples/echo.js` is a directly runnable byte-echo tool worker.

Run checks without downloading anything:

```sh
node --test sdk/js/test/*.test.js
node sdk/js/test/conformance.js
tsc --noEmit --strict --target es2022 --module nodenext --lib es2023,dom sdk/js/test/types.ts sdk/js/test/composite-types.ts sdk/js/test/gateway-hooks-types.ts
```

The tests use in-memory `PassThrough` streams plus real Node/Bun children.
SDK 1.0 conformance covers the original nine capabilities; SDK 1.1 process
tests cover configure/ready/stop, auto actions and component callbacks.
API 1.2 tests use real Node/Bun child processes for hooks, unary acceptance,
host callbacks, private errors and cancellation; their host state fixture
does not claim ordinary gateway/tool-owner integration.
Conformance also checks scoped host calls, full uint64
generations, streamed acknowledgements, cancellation, public/private errors,
and stdout purity. Bun is used only if already present (`BUN_BIN` may name
an existing binary). No vendor endpoint, live config or package registry
is contacted.

Terminal IDs retire before result bytes become visible, while canceled
handlers retain independent admission capacity until they finish.
The [ADR evidence index](../../docs/adr/README.md#historical-validation-records)
preserves exact snapshots, commands, original reds and later corrections.
Public/server conformance, ordinary host integration, native platform
execution and live vendor acceptance remain separate checks; this guide
claims no new passing result.

## Selected-account UI workflow

`createAccountWorkflow` composes the existing SDK 1.5 window grants without
adding a host capability:

```ts
import { createAccountWorkflow } from "@magpie/extension-sdk/ui";

const accountsUI = createAccountWorkflow("account.inspect", "signin");
await accountsUI.refresh();
async function inspectChosenRow(chosenRowID: string) {
  accountsUI.select(chosenRowID);
  return accountsUI.call({ operation: "info" });
}
// In a separate sign-in handler, after explicit row selection:
await accountsUI.start({ method: "device" }, true);
```

Render `snapshot.accounts` and let the user choose the row. IDs and canonical
generation strings come only from host discovery. Refresh never auto-selects;
an observed generation change clears the selection. A refused call clears
the cached list and selection, sets `snapshot.stale`, and is never retried.
Refresh and obtain another explicit choice before attempting a new action.

For new sign-in, omit the second `start` argument. Render the safe
`snapshot.flow` URL/code, wait its `retryAfterMs`, and call `finish()` to poll.
Use `cancel()` to request host cancellation. Completion refreshes discovery
without selecting its returned account. Identical in-flight starts are
coalesced; a failed start is not silently exchanged again. The helper owns
no timer, direct HTTP, credential storage or authority: the parent bridge and
host retain those responsibilities. SDK 1.4 root bindings keep their existing
`ui.backend.call(binding, input)` interface.

See [the workflow record](../../docs/adr/0018-window-account-workflow.md) for
the concurrency, failure and verification boundaries.

## Session sources in API 1.6

Capability `session-source` negotiates exactly `session.source.read`.
The `SessionSourceDescriptor`, `SessionSourceReadRequest` and
`SessionSourceReadResult` declarations describe a finite local JSONL reader.
Manifest validation and worker dispatch enforce source ownership, selected
session scope, ID confirmation, unary results and binary/frame budgets.

Read only the captured `documents` passed by the host. Return a typed summary
or transcript within the supplied limits; do not return filesystem roots,
absolute paths, shell commands or costs. Omitted usage is unknown, while an
empty usage array is a measured zero. Native takeover requires an explicit
host binding and existing read consent still applies.

`session-migration` declares conversion only. The API1.6 JSONL source grant
does not provide SQLite, compressed-journal or remote/backend readers.
[Rooted storage sources](../../docs/subsystems/extension-storage-sources.md)
use separate API1.8 declarations and native host checks; merely declaring
conversion grants neither reader. The [agent/session owner](../../docs/subsystems/extension-agents-sessions.md)
defines ordinary binding and resume authority. Historical composition and
validation stay in the [ADR evidence index](../../docs/adr/README.md).

## Native account operations in API 1.6

Capability `native-account-operations` requires finite
`accountOperations: [{id, agent, provider, action}]` declarations and
`inbound-codec`, `outbound-executor`, `account-auth`, `agent-wiring` and
`module-rpc`. Each operation refers to a same-module provider, a declared
model-bound agent launch and a primary action with effect `read` or `write`.
Compatibility validation does not grant arbitrary routes or account access.

An inbound decode selects `protocol: "account-operation"`, `accountOperation`
and `executor: "same-module"`. Writes also provide a stable `requestId`.
The host resolves the current owned agent's explicit visible model and exact
account generation. The action receives `{model, body, callerId}`, with the
selected scope and auth kept separately on the call. `body` is base64 on the
shared wire and bounded by `MAX_HOST_BYTES`.

Return `{status, headers, body}`. Only content type and retry-after headers
are admitted. The host retains the original inbound lease, local-owner
policy, read/write restrictions and durable acceptance journal, then checks
selection again before replying. Groups, forwarded/restricted callers and
remote credential delegation are outside this adapter. The module still
validates vendor wire and receipts. See the
[operation decision](../../docs/adr/native-account-operations.md) and its
separate module-startup/replay acceptance boundary.

## Request policy API 1.12

API 1.12 supplies the optional `request-policy` capability: shared response
classification and bounded transport retries, plus explicit final refusals for
host-owned account/model disposition. It preserves accepted/uncertain/output
fences and does not advertise reactive authentication recovery. The
[request-policy reference](../../docs/subsystems/request-policy.md) defines timing, windows,
settings, Go/JS helpers and remaining boundaries.

Executors that declare `request-handoff` can return `handoffExecution` after
local admission is exhausted before effects. Return it before emitting any
execution event, with usage omitted. Magpie can try the next eligible candidate
immediately within the request budget. The optional retry hint is returned when
no candidate can serve.

`decideRequest` validates granted retries: the returned `step` must match the
exact valid named request, `action` must be `wait`, and the request must carry
complete response facts for `provider.execute`, `tool.call` or `module.call`
plus `read-only` or `rejected-before-effect` evidence. Status must be 200–599,
excluding redirects and known acceptance/partial statuses 201, 202 and 206.
Known denial reasons (including unsafe, revoked, canceled and exhausted
budgets) cannot grant a retry. Auth, request/content refusal, unknown
classification or scope, and request-scoped quota cannot grant one either.
Unknown reason strings remain compatible; validation does not require a fixed
list of successful reason values. Counts and attempt must be
nonnegative safe integers, with provider retries no greater than the root
count. Invalid grants reject; non-retry terminal diagnostics remain available
without safe evidence. Abort or callback failure never returns a permit.

`requestWithPolicy` applies the same validation before sending another clone
of the request. Read a response clone inside the classifier: consuming or
locking the original response body fails the call. Manual redirects still
reach the classifier for bounded response capture, then return without a
recovery callback; opaque redirects return directly. Invalid or detectably
forwarded responses fail before classification. Retry/error paths cancel
response bodies; denied retries leave the preserved body with the caller.
These checks add no scheduler, counters, account selection or credentials.
Durable acceptance, output and revocation remain host-owned barriers,
including across a wait and between named steps.
