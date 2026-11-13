# Magpie executable extension SDK

`@magpie/extension-sdk` has no package dependencies and runs on Node 22+
and Bun with the same newline-delimited JSON protocol as `pkg/extension`.
The root exports the server authoring API and TypeScript declarations.
`@magpie/extension-sdk/ui` is a separate browser-only author entry.
Nothing is installed or published by the examples or tests.
SDK API 1.1 adds `module-rpc`, actions and component lifecycle; explicit
SDK 1.0 manifests retain their original nine-capability behavior without
requiring component configuration.
Package 1.2.0 adds the negotiated `gateway-hooks` capability and tool exposure;
`CONTRACT` now advertises SDK API 1.2 and eleven capabilities. Explicit
1.0 and 1.1 manifests retain their earlier behavior.

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

## SDK 1.1 modules and components

SDK 1.1 introduced ten capabilities and component groups. Keep the manifest's
explicit minor version at 0 for an existing SDK 1.0 worker. A 1.1 module
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
The adapter preserves bridge errors. It does not add a per-call AbortSignal,
streaming RPC or another transport: frame disable/update/unmount cancellation
is owned by the existing page bridge. Config DTOs use `{version,values}`;
CAS takes `(expectedVersion, values)`.

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
handlers. Supported contract versions are 1.0, 1.1 and 1.2. Handlers outside the
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
| model-discovery | provider.models when provider descriptors exist; otherwise requires inbound-codec |
| account-auth | auth.start, auth.finish, auth.refresh, auth.logout |
| usage-reporter | provider.usage |
| operation-control | operation.status, operation.cancel |
| tool-provider | tool.call |
| agent-wiring | agent.plan; agent.inspect for rich descriptors |
| session-migration | session.convert |
| module-rpc (SDK 1.1) | module.call (auto-dispatched from module.actions) |
| gateway-hooks (SDK 1.2) | gateway.request / gateway.response / gateway.signin, only as enabled by hooks |

## Gateway hooks and tool exposure (API 1.2)

Declare `hooks: {request, response, signIn, timeoutMs, origins}` with the
`gateway-hooks` capability and contract minor 2. At least one flag must be
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

Tools may declare `exposure: "local-owner"` or `"shared"` only in API 1.2.
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
