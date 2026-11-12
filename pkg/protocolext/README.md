# Experimental protocol SDK

This package defines an in-process Go codec contract. It imports only the
standard library. API major zero is experimental and is independent of the
application version. There is no external executable, Bun host, IPC handshake,
plugin loader, OAuth service, agent wiring or vendor implementation here.

Construct `New(adapters...)` before serving. It either returns an immutable
`Registry` or rejects the entire set. Each adapter supplies a unique id and one
literal, clean, exact POST path. Registration rejects nil adapters, duplicate
ids or paths, incompatible versions, empty/unknown/duplicate capabilities and
adapters without `inbound-codec`. No prefix, subtree, path wildcard or escaped
path is registered. Ids, paths and negotiated capabilities are frozen at
registration; `Contract()` and `Paths()` return independent snapshots.

The host implements API 0.2, accepting the older 0.1 inbound contract as well.
`InboundContract()` requires `inbound-codec`; `CatalogContract()` also requires
`model-discovery`. The other named capabilities describe possible future
contracts and are currently refused. A contract declares requirements; it does
not grant arbitrary host access or implement a process runtime.

`Inbound.Decode` receives the original cancellation context, a private copy of
the native HTTP headers, and a bounded body after the gateway's caller guards.
Its `Request` selects one existing host operation:

| Field | Meaning |
| --- | --- |
| `Protocol: Chat` | `Body` is canonical Chat Completions JSON, with no native credentials or transport metadata. |
| `Protocol: Models` | `Body` must be empty and the registered contract must include `model-discovery`. The gateway projects its authorized catalog. |
| `Session` | Optional native conversation id, never a credential or account id. The gateway bounds it and uses it only when existing session headers are absent. Existing caller scoping still applies. |
| `Timeout` | Zero means no additional deadline. A positive duration starts at bridge entry, including body reading and decoding. A negative value is refused. A codec must reject a native deadline that is invalid, expired or explicitly zero when its protocol requires a positive one. |

`Inbound.Reply` must return a non-nil `Response` for each request. The canonical
handler writes to it synchronously, with the client's backpressure; adapters
must not accumulate an unbounded reply. Implement `http.Flusher` when needed
for streaming. `Finish` is called exactly once, including on decoding or
operation refusal, with the request cancellation/deadline error or nil on
normal return. It is responsible for detecting missing terminal events and
encoding its protocol's final result. Its errors are logged without their
text. There is no host guarantee that a particular codec is lossless.

Adapters must be safe to share across concurrent requests. Per-request state
belongs to `Response`, not a mutable adapter declaration. Implementations are
trusted linked Go code; immutable registration prevents accidental contract
changes and route escalation, and is not an isolation boundary for arbitrary
Go code.

An `Error` is a public decoding refusal. `Message` must be safe for the caller
and contain no request bytes, secrets or raw upstream diagnostics. The gateway
accepts an error status in the HTTP 400–599 range and otherwise uses 400.
Other decode errors are answered with a generic message.
