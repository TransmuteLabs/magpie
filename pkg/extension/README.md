# Go executable extension SDK

Implement a `Module` with a validated `Manifest` and handlers for every
advertised capability. `Serve(ctx, input, output, module)` runs newline-delimited
JSON RPC on the supplied transport. An executable normally passes `os.Stdin`
and `os.Stdout`. Stdout belongs exclusively to RPC; write diagnostics to stderr.
No installation, process supervision or filesystem writes are performed by
this library.

`Frame` is the shared Go/Bun wire contract. JSON envelopes are at most 24 MiB.
Binary `body` fields and `data` events are base64 strings, with data chunks at
most 64 KiB. Other streaming events are bounded at 64 KiB of JSON.
Host-service params/results allow 1 MiB plus 64 KiB of JSON overhead
(`MaxHostBytes`), preserving support for 1 MiB state values.
A server admits at most 32 live calls. Duplicate live IDs,
malformed frames, truly unknown acknowledgement sequences and admission overflow
terminate the connection and cancel its calls; they are not silently retried.
Unknown methods return `method_not_found` without invoking another handler.

The built-in `sdk.describe` returns the public manifest, omitting runtime,
entry and args. The host can fill those packaging fields from its installation
record before validation, then compare `Public()` with installation authority.
`Public` clears only packaging, retaining identity, version, descriptors and
negotiated capabilities. Manifest and handler declarations are copied before
traffic. `FullContract()` is API 1.0; the original codec host contract remains
API 0.3 with the 0.1 inbound and 0.2 catalog contracts still supported.

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

`Stream.HostCall(ctx, method, params, out)` supports the scoped `state.get` and
`state.cas`, `network.proxy` and `account.replace` services. It serializes callbacks within a parent, sends a unique
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
Ordinary token refresh continues to use the existing auth/state lifecycle.

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
