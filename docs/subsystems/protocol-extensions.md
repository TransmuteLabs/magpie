# Experimental protocol extensions

The kernel can embed trusted, vendor-neutral Go inbound adapters through an
explicit per-server registry. No adapters are bundled or automatically enabled.
The scope and future runtime boundary are recorded in
[the protocol SDK ADR](../adr/0001-protocol-extensions.md).

## Responsibilities and sources of truth

| Owner | Responsibility | Source |
| --- | --- | --- |
| Public SDK | Version and capability checks, immutable exact endpoint registry, normalized request and synchronous response contracts | [`pkg/protocolext`](../../pkg/protocolext/README.md) |
| Host composition | Validate the embedding application's route ownership before accepting a registry; keep lookup and matching specific to that host | [`internal/protocolext/host.go`](../../internal/protocolext/host.go), `New` |
| Gateway bridge | Explicit server construction, request normalization inside caller guards, canonical model discovery or inference before budgets and concurrency | [`protocol_extensions.go`](../../internal/gateway/protocol_extensions.go), `NewWithProtocols`, `protocolEntry` |
| Path classification | Existing static client namespaces and an additional per-server exact matcher | [`oneport.go`](../../internal/gateway/oneport.go), `ClientPath`, `Server.ClientPath` |

## Construction and state

An application composition root constructs `protocolext.New(adapters...)` from
the public SDK, then calls `gateway.NewWithProtocols(registry)`. Both constructors
return an error on invalid registration. A nil or empty registry enables no
extension routes. Existing `gateway.New()` callers retain that behavior.
There is no mutable global registry, environment-selected vendor switch,
settings-file change or live registration operation.

`gateway` remains an internal package. A composition root must be allowed to
import it under Go's internal-package rules; external adapter packages can
implement the public SDK without importing the gateway. They still need
deliberate linkage by that composition root. The current CLI and web
constructors do not load adapters.

The gateway refuses registry endpoints in its existing client API namespaces,
the page's `/api` namespace, helper callbacks under `/_magpie`, and the agent
and WSL path wrappers. The refusal covers currently unknown subpaths of those
namespaces too, so a registration cannot replace an administrative or SDK
endpoint. Registry identity, paths and negotiated capabilities are snapshots;
the linked adapter itself must remain safe for concurrent use.

## Runtime path

1. `Server.Front` applies the existing LAN, sharing, proxy, mTLS and Host
   checks. `Handler` retains its Origin, authenticated caller, browser-key,
   read-only and remote-hop guards. They run before the codec is entered.
2. Inside `withCaller`, `protocolEntry` selects an exact registered endpoint.
   Unknown paths continue to normal dispatch. Registered calls require POST
   and an unescaped literal path.
3. The bridge reads through the existing bounded request reader and deadline
   logic, capped at 16 MiB or the server's smaller body limit. The codec gets
   cloned headers and the original request context. Private decoding errors
   are hidden; explicitly public SDK errors can describe a refusal.
4. Only canonical Chat JSON or an empty Models operation is accepted. Models
   additionally requires the frozen `model-discovery` capability. A codec
   cannot choose another host route, method, destination URL or identity.
   Positive timeouts run from bridge entry; negative timeouts are refused.
5. A cloned request retains the caller context, cancellation, Host, peer,
   gateway auth, forwarding provenance and page-port mark. Native transport
   headers, query, body replay function and trailers are discarded. Existing
   session headers take precedence over a bounded decoded conversation id.
   The session is also recorded in caller context for remote-magpie forwarding.
6. Gateway-key aliases resolve on normalized JSON before `keyLimited` and
   `keySlots`. Existing model/account restrictions, scoped budgets, provider
   routing, middleware and accounting handle inference. Models maps to GET
   `/v1/models`, projects the caller's authorized catalog and consumes no
   inference reservation or provider request.
7. The existing handler writes synchronously to the adapter's response.
   `Finish` receives the cancellation/deadline cause exactly once. The codec
   owns native framing, termination and representability checks.

No local HTTP round trip, credential store, session migration, operation
journal, external worker or reload lifecycle is introduced.

## One-port forwarding

`Server.ClientPath(path)` recognizes static gateway client routes plus that
server's exact registered paths. Package-level `gateway.ClientPath(path)`
keeps its upstream static behavior and knows no registrations.

The GUI `onePort` handler uses the current serving instance's matcher and
forwards registered paths through that instance's `Server.Front`, marked with
`FromPagePort`. A local proxy connection therefore remains subject to the
gateway's sharing and key checks. Unknown subpaths stay behind the page's own
handler.

The web proxy path to a different process has no registry
discovery or manifest exchange. Do not claim web forwarding from registration
in another process alone or publish a fixed list of vendor paths.

## Compatibility and runtime limits

This is an experimental in-process Go inbound bridge, not a complete external
plugin system. External Go executables, Bun/JS/TS loading, local IPC,
handshake transport, process supervision, draining and cross-language
conformance are not implemented. Naming future capabilities grants none of
them. No vendor codec, custom OAuth flow, provider implementation or agent
session migration is part of this port. Existing owners retain their behavior.

## Verification

Use a temporary HOME with `GOPATH`, `GOMODCACHE` and `GOCACHE` resolved before
changing HOME, and `GOPROXY=off`, as required by the
[code standards](../code-standards.md). Targeted offline checks:

```sh
go test -tags nogui ./pkg/protocolext ./internal/protocolext
go test -tags nogui ./internal/gateway -run 'TestProtocolBridge|TestClientPath|TestLazyCatManifest|TestPagePortMark'
go test -race -tags nogui ./pkg/protocolext ./internal/protocolext ./internal/gateway -run 'TestContract|TestRegistry|TestCatalogContract|TestSDKImports|TestHostRequires|TestProtocolBridge|TestClientPath|TestPagePortMark'
```

The fixtures use dummy adapters and local fake providers. They check immutable
contracts, reserved namespaces, real `Front` caller guards, native metadata
removal, cancellation, session/provenance preservation, authorized discovery
and aliases resolving before model-scoped budgets. The SDK architecture test
walks only its own source and imports no vendor implementation tree. These
checks establish the generic bridge, not compatibility with any native vendor
client or a future process runtime.
