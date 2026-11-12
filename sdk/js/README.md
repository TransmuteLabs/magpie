# Magpie executable extension SDK

`@magpie/extension-sdk` has no package dependencies and runs on Node 22+
and Bun with the same newline-delimited JSON protocol as `pkg/extension`.
The package exports its JavaScript entry and TypeScript declarations from
the root. Nothing is installed or published by the examples or tests.
With the state/proxy services, agent/session metadata, decoded tools and
model policy included, this authoring API is frozen; subsequent work is
limited to bug fixes.

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
handlers. Supported contract version is exactly 1.0. Handlers outside the
advertised capabilities are rejected. Runtime is `exec` or `bun`; protocol
names match Go: `chat`, `responses`, `messages`, `anthropic`, `gemini`.
The manifest does not grant filesystem or network sandboxing.

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
tsc --noEmit --strict --target es2022 --module nodenext --lib es2023,dom sdk/js/test/types.ts
```

The tests use in-memory `PassThrough` streams plus real Node/Bun children.
Conformance covers all nine capabilities, scoped host calls, full uint64
generations, streamed acknowledgements, cancellation, public/private errors,
and stdout purity. Bun is used only if already present (`BUN_BIN` may name
an existing binary). No vendor endpoint, live config or package registry
is contacted.
