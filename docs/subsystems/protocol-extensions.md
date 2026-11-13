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

`Request` selects Chat, Responses, Messages/Anthropic, Gemini, authorized
Models discovery or negotiated own-module tools. Body JSON and metadata are
bounded. Session headers take precedence over a decoded session. A positive
timeout starts at bridge entry, including reading/decoding; negative values
are refused. Models needs its discovery capability; tools need tool authority
on both the module and codec and an installed owner.

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
The host still refuses unrelated outbound/auth/wiring authority through this
inbound registry.

Regression suites are in `pkg/protocolext`, `internal/protocolext` and the
gateway's `TestProtocolBridge*`/`TestClientPath*` tests. Use temporary HOME with
pinned caches as specified in the [code standards](../code-standards.md).
