# Go extension SDK

Our modules keep integration behavior outside upstream-owned Magpie code.
Implement a `Module` with a validated `Manifest` and handlers for each
advertised capability. Current source API 1.5 includes native/exec/Bun,
composite modules, gateway hooks/private tools, transactional refresh, finite provider
operations, rich model metadata, namespaced routing strategies and declared
selected-account window actions and poll/link window authentication. The
[combined validation record](../../docs/adr/0009-sdk13-validation.md) names
its frozen SDK snapshot; the [surface census](../../docs/subsystems/extension-surface-coverage.md)
retains remaining adapters and does not claim full SDK or repository acceptance.
The [SDK 1.4 checkpoint](../../docs/adr/0014-sdk14-validation.md) records
combined SDK gates; final application acceptance remains separate.
The [selected-account 1.5 record](../../docs/adr/0015-selected-account-window-bindings.md)
has scoped host/UI evidence; combined 1.5 acceptance remains separate.
These are the window-specific SDK 1.5 cuts. Agent launch/profile adoption remains a
separate in-progress adapter and is not implied by the source version.

| Runtime | Author entry | Result |
| --- | --- | --- |
| `native` | `native.Register(module)` in `init`, empty `main`; build with `-buildmode=c-shared` | Library in Magpie's PID; the SDK owns all C exports and RPC glue |
| `exec` | `Serve(ctx, input, output, module)`, normally `os.Stdin`/`os.Stdout` | Supervised executable with its own Go runtime |
| `bun` | Corresponding [JS SDK](../../sdk/js/README.md) `serve(module)` | Supervised Bun component using the same contract |

Use the actual [native example](../../examples/extensions/echo-native/README.md)
or [hybrid author sources](../../examples/extensions/hybrid-go/README.md)
for complete builds. C-shared authoring requires cgo and a target C compiler.
Native libraries have no process args. Their versioned
[`open/send/close` ABI](native/abi.h) carries bounded SDK JSON frames inside
the host process; no Go pointers/interfaces cross it. JSON/base64 and copies
remain. Native close revokes a session; the library stays mapped until host
exit. Cancellation is cooperative and a native fault shares Magpie's failure
domain. The exec option remains available.

`Serve` runs newline-delimited JSON on its supplied transport. Exec stdout
belongs exclusively to RPC; write diagnostics to stderr. This library
performs no installation, process supervision or host filesystem writes.

Distribute the complete module with `magpie-plugin.json` in a local folder,
or a platform-specific GitHub release ZIP with `checksums.txt`. Include helper
binaries and prebuilt UI assets in that same package. The host
supports `magpie plugin sdk add github:owner/repository[@tag]` and
`magpie plugin sdk update [module]`; explicit tags stay pinned. See the
[installation contract](../../docs/subsystems/executable-extensions.md#installation)
for archive layout, full-package hashes, provenance, updates and private
developer authentication through env tokens, `gh` or HTTPS Git credentials.
Installer credentials are never module/provider configuration.
The public fork retains Go module path `github.com/yetone/magpie`. Our four
adjacent plugin repositories use
`replace github.com/yetone/magpie => ../magpie` during development, as used
by the native fixtures. Public repository access permits source dependencies;
it does not publish local revisions. Release consumers pin an available
fork revision; a separate SDK repository is optional.

Ordinary `plugin add` uses the SDK for local directories with this descriptor.
Absence preserves legacy JS/TS behavior; a malformed or unsupported SDK
descriptor errors instead of falling back. File extensions do not infer the
SDK runtime. Use an application build containing this SDK.

## Shared contract and transport

`Frame` is the shared Go/Bun wire contract. JSON envelopes are at most 24 MiB.
Binary `body` fields and `data` events are base64 strings, with data chunks at
most 64 KiB. Other streaming events are bounded at 64 KiB of JSON.
Host-service params/results allow 1 MiB plus 64 KiB of JSON overhead
(`MaxHostBytes`), preserving support for 1 MiB state values.
A server admits at most 32 live calls. Duplicate live IDs,
malformed frames, truly unknown acknowledgement sequences and admission overflow
terminate the connection and cancel its calls; they are not silently retried.
Unknown methods return `method_not_found` without invoking another handler.

The built-in `sdk.describe` returns the component's public manifest, omitting
runtime, entry and args. API 1.1 also removes service/window declarations,
compatibility requirements, bindings and actions owned by other components.
The host compares this projection with the matching projection of its complete
installation authority; a handshake is not the group's permission source.
`Manifest.Clone()` retains a deep copy of the complete root descriptor.
Manifest and handler declarations are copied before traffic.
`FullContract()` is API 1.5 with eleven capabilities; explicit API 1.0 modules without composite fields
retain their original single-controller behavior. The separate codec host
contract is API 0.4, accepting the older 0.1 inbound, 0.2 catalog and 0.3 tool
contracts as well.

An API 1.4 paired inbound/outbound module can return
`DecodedRequest.Executor = SameModuleExecutor` to preserve native-only
conversation semantics. The Go codec helper also declares
`protocolext.ExecutorContract()` and sets `protocolext.Request.Executor`.
The host derives the owner from the original pinned request lease and
checks that exact lease on every attempt, including groups and aliases.
Empty preserves ordinary routing; null, unknown policies, discovery/tools,
legacy modules and another inbound lease cannot grant or relax this
constraint. The module must still validate native receipts against the
actual account, generation and model. See
[ADR 0011](../../docs/adr/0011-native-executor-constraints.md).

Handlers receive `Call{ID, Method, Scope, Params, Auth}`. Scope belongs to the
host: plugin/provider/account/session/operation are strings, and account
generation/attempt are uint64 counters. Account generation is distinct from an
installed package generation. `Auth` is selected-account JSON provided by the
host, separate from public scope. Raw params do not authorize changing scope.
Plain handler errors and panics become generic `internal` errors; use `Error`
only for explicitly public messages. Code and an optional outcome use
`[A-Za-z0-9_.-]{1,64}`. Messages must contain non-whitespace text, occupy at most
4096 UTF-8 bytes, and contain no C0 control except tab, LF and CR, nor DEL.
Whitespace trimming follows JavaScript's whitespace set. Context cancellation maps to `canceled`
and deadline expiration to `deadline`.

`Stream.Emit(ctx, event, data)` sends one numbered event and waits for its exact
acknowledgement. Even concurrent Emit calls share a single credit per parent.
An error terminal implicitly cancels outstanding credit. A canceled Emit cannot
be followed by more events before acknowledging that credit or ending the call.
The worker remembers up to 1024 canceled credits and accepts one exact late
ACK for each; other IDs or sequence numbers remain protocol errors. Call IDs
with unconsumed canceled credit cannot be reused until that credit is consumed
or evicted from the bounded history.
Other calls can continue while a consumer pauses. A `data` event takes `[]byte`;
`accepted` takes a JSON object such as `AcceptedEvent`. Operation durability and
the decision to acknowledge acceptance belong to the host.

`Stream.HostCall(ctx, method, params, out)` supports the scoped `state.get`,
`state.cas`, `network.proxy`, `account.replace` and `component.call` services.
It serializes callbacks within a parent, sends a unique
child ID plus the exact parent scope and deadline, and waits for `host.result`.
The result must echo both child ID and `parent`. Up to 1024 canceled child
callbacks are retained so one exact late result is safely discarded. A wrong
parent, truly unknown child ID or duplicate result is a protocol error.
Late results never revive a canceled callback. Call auth is not copied to
host-service frames. State versions are opaque decimal strings. The host owns
state persistence, generation checks and CAS.

`network.proxy` accepts `NetworkProxyRequest{URL}` and returns
`NetworkProxyResult{URL}`; an omitted result URL means direct access or bypass.
The host validates the destination scheme (HTTP, HTTPS, WS or WSS) and resolves
the effective proxy from the original parent context, including selected
provider/account settings, global settings and bypass rules. This callback
does not dial a destination. Proxy credentials stay in the selected RPC;
the SDK installs no process-global proxy environment variables.

`account.replace` sends only replacement credentials and returns a secret-free
account ID and generation. The host authorizes it only for an accepted
`tool.call`, after durable acceptance has been acknowledged, with `account-auth`
and a selected provider/account/generation. The host performs generation CAS
against its account store. The module must return its terminal result after
replacement: old scoped state and subsequent events are deliberately revoked.
Rotating refresh instead uses the API 1.3 host-owned `auth.refresh`
transaction below; private state CAS is not its publication mechanism.

An earlier caller-provided context can shorten an Emit/HostCall deadline.
The parent call's cancellation always interrupts them, even if a handler
passes `context.Background()`. Handlers must otherwise cooperate with their
context and join any goroutines they create. Serve waits for handlers before
returning. On connection close or cancellation it closes transports that
implement `io.Closer`, unblocking pipe reads/writes. A non-closeable blocking
reader or writer must provide its own cancellation mechanism; Go cannot
interrupt an arbitrary `io.Reader`/`io.Writer` operation.

For existing codecs, put trusted `protocolext.Inbound` values in
`Module.Inbounds` and matching id/path declarations in the manifest. The SDK
installs both inbound handlers. `NativeRequest` selects `id` and `responseId`;
the response ID isolates a bound codec, its Reply and its state across RPCs.
Reply is created before Decode, and `protocolext.Bind` retains frozen registry
identity/capabilities. Decode returns protocol/body/session/timeoutMs plus
opaque requestId and bounded JSON metadata. Protocols include Chat, Responses,
Messages/Anthropic, Gemini, authorized Models and explicitly negotiated Tool
operations. Tool body is `ToolRequest` JSON naming a tool in the same module's
frozen manifest, and requires `tool-provider` in both the module manifest and
the codec's registered contract. The host sends it through its guarded MCP
tool pipeline; it is not a provider LLM protocol. The host owns model policy
and any namespaced metadata forwarding.

`ResponseEvent` supports start/write/flush/finish/cancel, using the same adapter
and response IDs and scope each time. Write chunks are bounded. Native output
becomes acknowledged headers/data/flush events. Finish/cancel releases state;
connection closure finishes remaining responses. Idle responses with elapsed
deadlines are reclaimed on subsequent helper calls. A maximum of 32 response
instances is retained. Codecs remain responsible for framing, semantic
representability and detecting truncated replies.

Provider execution/auth/models/usage, operation control, tools, declarative
agent plans and session documents have public DTOs in `types.go`. Implementing
their handlers is module-specific. Advertising their capability without its
required handler is an error. A provider capability needs provider descriptors,
wiring needs agents, migration needs session formats, and tools need their
schemas. Generic operation control does not require a provider.

`Model.Price == nil` means an unknown price. A non-nil `ModelPrice`, including
an all-zero value, is an explicitly known price; all-zero means free.
Prices are finite, nonnegative USD amounts per million tokens. An omitted
currency means USD; other currencies are refused. JSON uses `input`, `output`,
`cacheRead`, `cacheWrite` and `cacheWrite1h`. Price pointers and effort slices
are copied into immutable manifest snapshots. `Model.Efforts` lists at most
seven distinct canonical levels: none, minimal, low, medium, high, xhigh, max.
Vendor aliases, auto and ultra are not SDK model effort levels.

## Provider API 1.3

API 1.3 providers declare finite `ProviderDescriptor.Operations` independently
of their conversational `Protocols`. `ExecuteRequest.Operation` selects the
operation on the OpenAI-compatible `chat` wire family; ordinary conversation
and native Anthropic/Gemini counting keep an empty operation.

| Operation identifiers | Selected upstream work |
| --- | --- |
| `embeddings`, `rerank` | Retrieval |
| `images.generate`, `images.edit` | Image generation/edit |
| `audio.transcribe`, `audio.translate`, `audio.speech` | Audio input/output |
| `videos.create`, `videos.get`, `videos.content`, `videos.delete`, `videos.remix` | Video lifecycle |

Both the actual leased host descriptor and the author server reject
undeclared operations before dispatch. Metadata is not permission, and
declarations add no arbitrary upstream URL or new public gateway route.
`ExecuteOptions` types method, path, query, proxy choice and optional
caller/request/native metadata; `EscapedPath` is sent only to API 1.3
modules. Use `network.proxy` for effective transport resolution. Preserve
selected raw/multipart/binary bytes and the host's accepted/uncertain
no-replay policy. Vendor execution remains module-owned.

API 1.3 `Model` adds `Kind`, `InputModalities`, `OutputModalities`,
`MaxContextWindow`, `Released`, `Temperature`, `ServiceTiers`, `Free`,
`Rate`, `RateWas`, `AlwaysThinks`, `DynamicTools` and `DefaultEffort`.
Kinds and modalities are validated finite sets. Nil modality slices mean
unknown; explicit empty slices mean none and survive public snapshots.
Temperature is a pointer so unknown differs from false. Ordinary catalog
projection preserves these facts and user overrides remain authoritative.
These fields do not add dedicated decision-model or provider-option adapters.

### Host-owned credential renewal

Require API 1.3 and `Compatibility.HostAPI: "^1.3"` when a module relies on
rotating refresh. Keep the existing unary `auth.refresh` handler and return
an `AuthResult` with the selected account's replacement credentials and
expiry. Credentials come from `Call.Auth`, never a caller's replacement
scope. The host owns strict cross-process locking, a durable dispatch fence,
atomic credential/expiry/generation publication and a secret-free completion
receipt. Repeated completed-generation requests do not spend the token again.

`AuthResult.ExpiresAt` and host `Account.ExpiresAt` use Unix milliseconds;
zero is unknown. Ordinary provider preflight can renew known expiry before
execution, model refresh or quota reads and adopt only a receipt-proven
successor. Manual account replacement remains stale. Do not implement an
implicit refresh/retry inside paid execution or substitute a module mutex
for the host transaction.

Caller cancellation after dispatch ends its wait while the bounded owner
publishes or fences the result; Manager shutdown owns that work. An unseen
token lost after remote rotation cannot be recovered: uncertainty requires
explicit reauthorization. Native ABI stays 1. Exact limits and upgrade
behavior are in [ADR 0007](../../docs/adr/0007-transactional-account-refresh.md).

### Attachment-aware session inputs

An API 1.3 session module may declare the read-only ModuleRPC action
`session.inputs.v1`. The ordinary session owner validates its versioned
path/SHA-256 plan, acquires bounded inputs from the selected native store
and retains planner/converter on one lease. The module receives verified
journal/blob documents; it does not select another profile or read host
paths by itself. Source and target-store guards remain host-owned. This is
not generic SDK session discovery/reader/resume registration; see the
[session reference](../../docs/subsystems/extension-agents-sessions.md).

## Routing strategies API 1.4

Declare `Manifest.RoutingStrategies` with `RoutingStrategyDescriptor` entries
and a `module-rpc` read action. API 1.4 is required. Each descriptor names
`ID`, display `Name`, `Action`, optional `RoutingFlags` and `TimeoutMs`.
Use the fixed `moduleID/strategyID` as a provider or group's `routing` value;
the existing selector discovers enabled declarations without starting a worker.

The read action may belong to a declared private backend. The full root
retains its registration and dispatches through that component's lazy client.
`Manifest.Public()` exposes only routing whose action belongs to the
described component, so a primary handshake does not contain dangling
references to private actions. This projection does not change root authority.

The action decodes `RoutingRequest`, calls its `Validate` method and returns
a unary `RoutingResult`. Its `Order` must contain every input seat index once;
`RoutingResult.Validate` checks this independently. The
[tested worker](../../internal/extension/testdata/routingworker/main.go)
shows the full handler and read-only callback behavior, and
[ADR 0012](../../docs/adr/0012-sdk-routing-strategies.md) has the tested
descriptor fragment and ordinary HTTP evidence.

Flags retain the existing Strategy meanings: `Exact`, `ByUse`, `ByFree`,
`ByPlan`, `BySpeed`, `ByPrice`. Input contains bounded metrics and opaque
seat/wallet identities, without auth or selected-account scope. Limits are
64 descriptors, 256 seats, 32 renewals per seat and 1 MiB input/output JSON.
The host validates finite measurements, timestamps and the full permutation.
Read actions cannot persist `state.cas` changes on API 1.4.

The ordering context defaults to 500 ms; explicit `TimeoutMs` is 25–2000 ms,
including cold acquisition and the RPC. Existing `Strategy.Order` has no HTTP
caller context; this seam uses a bounded background context. Errors,
invalid/streaming output, deadlines and off/update/config revocation fall back
to identity order. Core account/key/model guards and failover remain intact.
Scoped worker/race verification and the combined SDK checkpoint passed;
application acceptance remains separate.

## Composite API 1.1

[ADR 0005](../../docs/adr/0005-composite-module-contract.md) defines one root
module with optional private services and window contributions. The existing
nine capability paths stay on its primary controller. The `module-rpc`
capability declares `actions`, their component owner, JSON input schema and
`read` or `write` effect. Services declare their own contract identity/version,
the exact action IDs in `methods`, startup policy, dependencies and named HTTP
endpoints. Validation rejects ownership conflicts and cyclic dependencies.
At most `MaxModuleServices` (16) private service descriptors are accepted,
including multiple entries using one binary; actions/services/bindings
together retain the 1024-descriptor budget.
`CheckCompatibility(manifest)` checks the running platform and native ABI
before a host executes components.

Put handlers in `Module.Actions`; do not implement `module.call` yourself.
The SDK dispatches `ModuleCallRequest{Action, Input}` only to an action owned
by this component. A handler receives the input directly in `Call.Params`
and the declared ID in `Call.Action`; its result is terminal JSON. ModuleRPC
is unary. A write may emit an acknowledged `accepted` receipt; read actions
cannot emit events, and data/progress are rejected even if a handler ignores
the Emit error. Existing capability streams are unchanged.
Root `module.call` parameters use `MaxFrameBytes - 4096`; scoped
`component.call`/host callback envelopes retain `MaxHostBytes`. Large source
documents do not increase callback authority or make ModuleRPC streaming.

Use `ServiceManifest(root, id)` to derive a helper declaration with the same
root ID/version, its `ComponentIdentity`, only its owned actions, and no
delegated inference/account/agent/session capabilities. The host resolves
`ResolveAction` and `ResolveBinding` against its frozen full root. A controller
uses `Stream.HostCall` with `MethodComponentCall` and
`ComponentCallRequest{Binding, Input}`; the host derives the destination and
inherits the original parent lease, scope, cancellation and operation.

SDK-owned `component.configure` initializes API 1.1 runtimes before their
ordinary calls. `ComponentConfigureRequest` supplies the component, named
service contract, instance and `ConfigSnapshot`. Primary uses `"primary"` and
an empty `ServiceContract`; a helper uses its derived component ID/contract.
Config versions are lossless decimal uint64 strings, including `"0"`.
An instance accepts one frozen snapshot; identical configure calls share one
startup, while changed snapshots require a new runtime instance.
Configure also supplies `Services map[string]ComponentReadyResult`, containing
only the dependencies selected by the host from this frozen group. Their
identities, config versions and named loopback endpoints are validated and
copied into startup and per-call `ModuleContext.Services`; authors need no
static ports or startup RPC to find dependencies. The host owns membership,
since a helper handshake intentionally hides the complete root service graph.

Optional `Module.Start` has signature
`func(context.Context, ModuleContext) (ModuleReady, func(), error)`.
Its context belongs to the Serve lifetime, with cancellation while its
configure call is pending. Once started, a completed configure call does not
cancel that lifetime. Return actual endpoints in `ModuleReady` and a disposer
for resources this instance owns, including on partial failure. A late
startup completion after cancellation is disposed. A hook that panics before
returning a disposer must clean up its own partial resources.
`ModuleContextFrom(ctx)` gives action and primary capability handlers an
independent copy of their component/configuration snapshot. The root host
group supplies eager service readiness to the primary and declared dependency
readiness to helpers. It does not delegate arbitrary root capabilities to
private services. Entries resolve relative to the installed package root:
for example `lib/primary.dylib`, `bin/backend` and `ui/` in the hybrid example.

`ModuleReady.State` can hold an instance-owned Go value such as a database
pool or client. `ModuleContextFrom(ctx).State` retains that same opaque value
within the instance; other Serve lifetimes have their own startup state.
Config and dependency maps are still copied. State has `json:"-"` and is
absent from configure/ready DTOs, host callbacks and native ABI frames.
Authors synchronize access to shared state and release its resources in
their disposer. No global instance registry or factory API is needed:

```go
type ComponentState struct{ Calls atomic.Uint64 }

module.Start = func(context.Context, sdk.ModuleContext) (sdk.ModuleReady, func(), error) {
    return sdk.ModuleReady{State: &ComponentState{}}, nil, nil
}
module.Actions["count"] = func(ctx context.Context, call sdk.Call, stream *sdk.Stream) (any, error) {
    value, _ := sdk.ModuleContextFrom(ctx)
    state := value.State.(*ComponentState)
    return map[string]uint64{"calls": state.Calls.Add(1)}, nil
}
```

`component.ready` returns `ComponentReadyResult` only after configuration and
startup succeed: component, contract, instance, exact config version and actual
endpoints. Endpoints are named loopback HTTP(S) URLs with explicit nonzero
ports. Startup does not create an HTTP server automatically. The host checks
the returned names against the root service's declared HTTP endpoints.
On each local service HTTP response, set the fresh headers returned by
`moduleContext.HTTPHeaders()` before writing status or body:
`HeaderComponentInstance` (`X-Magpie-Component-Instance`),
`HeaderComponent` (`X-Magpie-Component`) and
`HeaderComponentConfig` (`X-Magpie-Component-Config`). They identify the exact
instance, component and config version even when a listening port is reused.
The helper includes no State, configuration values, dependency records or
credentials. These headers belong to local service responses, not vendor
requests. The host checks and removes them before forwarding a response to
the window.
The SDK supplies no UI renderer or process supervisor; those belong to the
host's module group and existing window bridge.

## Root configuration and window bindings

Host `Manager.ModuleConfig` and `CompareAndSwapModuleConfig` own the fixed
`Scope{Plugin: rootID}` / `module.config` namespace. The initial snapshot is
`{"version":"0","values":{}}`. Values are a bounded, unambiguous JSON object;
CAS replaces the whole object. It creates a new runtime group/config version,
while acquired leases keep their old package/configuration and can start an
old lazy helper after a newer generation is installed. This root configuration
version is separate from selected account generation.

The CLI exposes:

```sh
magpie plugin sdk config sdk-hybrid-go
magpie plugin sdk config sdk-hybrid-go '{"value":"example"}' --expected 0
```

The write assumes the prior read returned version `"0"`; use the exact
version read. Reading does not start a worker, writes require explicit
`--expected`, and an old application without this SDK lacks these commands.
No live module configuration was changed to verify this documentation.

`window.metadata` names package-relative `package.json` using existing
`magpie.page/panels/actions/permissions`. The host validates a real page/panel
and reuses its existing renderer, permissions and bridge; merely adjacent
assets are insufficient. SDK packages stay out of legacy `plugins.json`.
Opaque refs bind package revision, config version and exact page/panel.
Named `bindings` grant that part an action or service endpoint, without
frontend account scope, arbitrary state keys or backend URLs.

The separate browser entry `@magpie/extension-sdk/ui` exposes readiness,
the service URL, theme/context/visibility state and events, row actions,
permission-checked reads and PNG copy/save. Named backend calls and root
config get/CAS use the same entry; config values must be a JSON object.
It imports no Node/Bun API and delegates to `window.magpie`. The parent
bridge cancels pending work on frame disposal,
update/config change/off. The existing HTTP/SSE proxy holds a root lease for
the whole response, strips cookies and validates/removes the three proof
headers above before frontend body delivery.

### Selected-account window actions (API 1.5)

Set `BindingDescriptor.Provider` on a window action binding to a provider
declared by the same root. The root needs `AccountAuth`; the binding cannot
be a primary binding or HTTP endpoint. The page selects a sanitized
`AccountSummary` and submits `AccountSelection{ID, ExpectedGeneration}` in
`WindowCallOptions`, outside the action input. Generations are canonical
nonzero uint64 decimal strings; `AccountSelection.Validate` and the JS
validator preserve values above 2^53.

The host resolves exact current account scope and sends `Call.Auth` only to
backend handlers. Private services inherit the same selected scope and lease.
Never serialize auth into domain results. Business input cannot grant
provider/account authority, and ungranted root bindings reject selection.
The host does not silently refresh or switch a selected generation.

Selected writes use a stable parent intent plus a durable immutable
scope/binding/input fingerprint and the ordinary acceptance journal.
Off/config/update/cancellation are checked after lazy startup and before new
acceptance or callbacks. The
[native hybrid fixture](../../internal/gui/extension_window_accounts_integration_test.go)
exercises the actual GUI/native/helper owner path. Its optional
`build.py --account-bindings` mode includes a typed account picker; the
default example remains API 1.1. See
[ADR 0015](../../docs/adr/0015-selected-account-window-bindings.md) for scoped
gates and the separate account-bound tool adapter.

### Poll/link window authentication (API 1.5)

Declare `Manifest.AuthBindings` independently from action `Bindings`.
Each `WindowAuthBindingDescriptor` names an exact window part, a same-root
provider and explicit `WindowAuthMethod{ID, Mode: "poll"}` entries. The root
requires `AccountAuth`. `Reauthorize` additionally permits a caller-supplied
typed `AccountSelection`; it does not permit choosing scope in business input.

The page calls `ui.auth.start(binding, {method, input?, account?})`, followed
by `finish(binding, flow)` or `cancel(binding, flow)`. Only
`WindowAuthFlowView` crosses to the page: an opaque flow, finite state, optional
verification URL/code/poll interval and committed `AccountSummary`.
`AuthStartResult.Continuation` and `AuthResult.Credentials` are backend DTOs
and are not browser return values.

Core captures the immutable lease/Store, persists intent before dispatch,
throttles polls and publishes account plus receipt atomically. Reauthorization
preserves dirty usage barriers and earlier refresh receipts. The first
admitted flow upgrades that module's account namespace to schema 4;
co-running older CLI/gateway Store readers must be upgraded. Metadata and
ordinary existing-account reads do not migrate the namespace.

This cut covers device and verification-link polling, including a method
whose vendor ID is `browser`. Loopback/PKCE callbacks, key/snapshot import
and arbitrary auth-method invocation are separate work.
[ADR 0017](../../docs/adr/0017-window-auth-broker.md) records the public
shape, compatibility and corrected scoped evidence; combined acceptance
remains separate.

## Gateway hooks API 1.2

Negotiate `GatewayHooks` and `Manifest.Hooks` with at least one of `request`,
`response` or `signIn`. Register only declared `MethodGatewayRequest`,
`MethodGatewayResponse` and `MethodGatewaySignIn` handlers in `Module.Handlers`.
Normal `Handler(ctx, Call, *Stream)` retains scoped state/CAS/proxy/component
HostCall support. API 1.0 and 1.1 declarations cannot advertise these additions.
`GatewayMethodEffect` classifies request as read and response/sign-in as write.

Request params are `GatewayRequest{Body, Context}`; return
`GatewayRequestResult{Body?, State?}`. An absent body preserves the request,
and a provided body must be a JSON object. `GatewayResponse` contains the
accepted request, reply, success, state and context. Opaque per-request JSON
state is bounded by `MaxGatewayStateBytes` (64 KiB). `GatewaySignIn` wraps an
`Event` with provider, providerName, account label, state and optional RFC3339
endsAt string. No credential fields are accepted.

Gateway context contains only host-derived origin/protocol/model/stream/path,
agent/session/cwd/query/kind. Provider/account/generation scopes and any Auth
payload are rejected. The host must enforce origins before sending private
bytes: defaults are `local` and `magpie-key`; explicit origins may also name
`gateway-key`, `remote` or `browser`. Browser traffic requires explicit opt-in,
even on loopback or with the default sharing key. The host overrides only the
SDK context to `browser` for a page request; legacy classification is unchanged.
Unknown or duplicate origins are invalid. Sign-in
requires explicit opt-in and local permission. Timeout is 100–30000 ms, or
zero for the host default.

Hook RPC is unary. Request emits no events; write response/sign-in hooks may
emit only an acknowledged `accepted` receipt. Other events are errors even
when a handler ignores Emit's error. Write hook results remain domain JSON;
their `outcome` field has no provider-execution interpretation. The host owns
body rollback, protected model/stream/tool fields, generation leases, bounded
completion delivery, operation journaling and conversation-export opt-in.

`ToolDescriptor.Exposure` omission preserves legacy shared behavior. Explicit
`shared` or `local-owner` requires API 1.2; the host enforces it on discovery
and execution. Memory can compose these tools, hooks and existing services,
without implied access to the independent built-in memory store.

## Stop and verification boundaries

For a configured component with a healthy, idle control transport, the host
attempts SDK-owned `component.stop` with a bounded context before closing
the transport. Stop cancels the component lifetime
and waits for its registered disposer and canceled startup before replying.
Cleanup runs once, including concurrent or repeated Stop calls. Stop before
Configure prevents startup, and Configure/Ready after Stop return errors.
The wire stays alive for the reply and `sdk.describe`. A host still hard-closes
an uncooperative worker; native session Close revokes without waiting for
arbitrary module code.

Native registration retains the complete root snapshot, action map and startup
hook, separately from the public handshake. Actual scoped proof now includes
the CLI host-PID roundtrip and
[`TestCompositeHybridBundle`](../../internal/extension/composite_integration_test.go):
native root PID equals host PID, helper PID differs, and an old lease starts
lazy v1/config1 after v2/config2 publication. The
[GUI integration fixture](../../internal/gui/extension_window_integration_test.go)
checks real status/SSE, cookies, off/update, response proof and lease hold.
Go-host/Bun API 1.1 passed race count 3 without skips; its historical JS
record has 87 Node/87 Bun passes plus strict types. The combined API 1.3
[checkpoint](../../docs/adr/0009-sdk13-validation.md) records 898 Go SDK/runtime
pass events, 108 Node and 108 Bun tests and three strict TS fixtures, alongside
policy/lifecycle/platform gates. These checks do not establish vendor parity.

Native runtime proof includes earlier macOS fixtures and Linux composite and
standalone OpenCode fixtures, including ordinary renewal and all four packaged
model routes. Windows native execution remains unverified.
No-cgo/cross-build checks are build/refusal evidence. Domain modules live in
separate repositories; no complete vendor migration or benchmark is implied.
This README claims no green whole-repository result.
