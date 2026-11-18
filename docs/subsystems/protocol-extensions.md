# Go protocol codec embedding

This page covers the explicitly linked Go codec interface. Installation,
native Go and external process behavior are documented in
[Executable extensions](executable-extensions.md).

The standard-library-only `pkg/protocolext` package defines `Inbound`,
`Request`, `Response`, immutable exact-route registries and capability checks.
An embedding composition root builds `protocolext.New(adapters...)`, then
calls `gateway.NewWithProtocols(registry)`. Both constructors return errors
for invalid declarations. Explicit construction loads only that registry.
Ordinary `gateway.New()` additionally discovers installed executable modules.

`gateway` remains internal. An external package can implement public SDK
interfaces, while a composition root permitted by Go's internal import rules
must link it. Installed native, executable and Bun modules use the public
extension SDK and require no private host import.

Registry snapshots freeze identity, paths, versions and capabilities.
Per-request `Bind(context.Context)` is optional; binding retains those frozen
declarations. Mutable request state belongs in the bound codec or response.
Implementations must be safe for concurrent use.

The gateway rejects its existing API namespaces, `/api`, `/_magpie`,
agent-wrapper and WSL paths. Caller guards precede decode; normalized requests
then pass through the normal canonical policy/accounting path. The codec
cannot choose an administrative destination or caller identity.

Ordinary built-in Chat directly uses the public `protocolext.Request` DTO
and the Decode-compatible method in
[`chat_normalization.go`](../../internal/gateway/chat_normalization.go).
After existing key-tier handling, that private decoder applies effort,
cache-key, message-tool and tool-shape normalization in their original order.
Both `/v1/chat/completions` and `/chat/completions` share this owner. It is
not a registered or bound `Inbound` and adds no `Reply`/`Finish` lifecycle.
The built-in reader, headers, writer, routing, policy and accounting retain
their existing owners; the native bridge's limits and header normalization
do not apply to this direct call.

`Request` selects Chat, Responses, Messages/Anthropic, Gemini, authorized
Models discovery or negotiated own-module tools. Body JSON and metadata are
bounded. Session headers take precedence over a decoded session. A positive
timeout starts at bridge entry, including reading/decoding; negative values
are refused. Models needs its discovery capability; tools need tool authority
on both the module and codec and an installed owner.

Dictionary offers and `dcz` inputs use the route's decoded request limit before
caching or acknowledging a body. Native routes share the 16 MiB cap, or a
smaller configured Server limit, with `protocolEntry`. An oversized body is
refused before native Decode and is neither kept nor answered as a dictionary
miss inviting replay. Ordinary and Codex routes retain their Server/Codex
limits and read deadlines through the same bounded reader.
Native dictionary pre-reading also establishes a private request-local timeout
origin. The codec's later positive timeout includes that reading and Decode
work. Plain native requests retain their original start boundary; a shorter
parent deadline still wins and zero declares no additional deadline. A timeout
declared after Decode cannot itself interrupt an earlier read; the existing
read deadline and caller cancellation govern that stage.

`Reply` returns a non-nil synchronous `http.ResponseWriter` with `Finish`.
Implement `http.Flusher` for streaming. `Finish` runs once, including refusal,
and receives cancellation/deadline information. Codecs own framing and must
reject features they cannot represent. Explicit public `Error` messages must
be safe for callers; other decode errors remain private.

Installed codecs preserve that rejection contract across SDK RPC.
`extension.NewHTTPError` encodes public rejection statuses 400–599 using the
existing bounded error code field; 400 retains `invalid_params`, while other
statuses use `http.NNN`. `Error.HTTPStatus()` decodes only recognized bounded
codes, never message text. Invalid statuses fall back to 400 and unsafe
messages become a sanitized internal error. No native ABI, error JSON shape
or minimum negotiated capability changes.

The installed host restores the public status, preserving the existing 400
fallback for other public codes. Old hosts retain their old fallback;
precise status needs the corrected host and rebuilt codec SDK helper.
Caller cancellation/deadline identity survives startup/decode, and response
cleanup forwards bounded native terminal frames with the correct
`canceled` or `deadline_exceeded` cause. Framed protocols still own their
wire status: a Connect response may remain HTTP 200 while its terminal
frame carries the canonical rejection. Raw private wrapper causes are
never included in that public message.

The legacy codec version is separate from process-manifest v1. Catalog
adapters using minor 2 remain valid; tool adapters negotiate minor 3.
Minor 4 adds `ExecutorContract()` and `Request.Executor = SameModuleExecutor`.
Only an installed SDK 1.4 module with inbound and outbound capabilities can
use that conversation constraint. The host derives its owner from the live,
original inbound lease, filters ordinary candidates and checks the same
lease again before signing and dispatch. Aliases, groups, fallback and nested
empty policies cannot change that lease. Null or unknown wire policies fail
decoding; discovery, tools and linked-only codecs cannot require this
installed-module executor. Native receipt/signature validation still belongs
to the module. See [ADR 0011](../adr/0011-native-executor-constraints.md).
The host still refuses unrelated outbound/auth/wiring authority through this
inbound registry.

SDK 1.6 and codec 0.5 add a separate finite native account-operation lane,
described in [the account-operation ADR](../adr/native-account-operations.md).
It resolves an installed declaration against its current owned SDK agent
binding and same-module provider/account, then uses the existing module.call
effect/receipt path. The original inbound lease owns its reply. It accepts
neither a client-selected account nor an arbitrary vendor destination.
Initial admission is local-owner only; group routes and delegated restricted
keys are refused. Accepted SDK 1.5 declarations retain their old behavior.

Regression suites are in `pkg/protocolext`, `internal/protocolext` and the
gateway's `TestProtocolBridge*`/`TestClientPath*` tests. Use temporary HOME with
pinned caches as specified in the [code standards](../code-standards.md).
