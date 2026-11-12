# Executable extension SDK

Magpie installs local Go executables and Bun JS/TS packages without rebuilding
the app. Existing OpenCode/Pi plugins keep their existing runtime.

The contract decision is [ADR 0003](../adr/0003-external-extension-runtime.md).
See [extension providers](extension-providers.md),
[agents and sessions](extension-agents-sessions.md), and
[legacy Go codec embedding](protocol-extensions.md) for their owners.

| Owner | Responsibility |
| --- | --- |
| [`pkg/extension`](../../pkg/extension/sdk.go) | Public Go contracts, worker server and codec adapter |
| [`sdk/js`](../../sdk/js/README.md) | Corresponding Bun/JS worker and declarations |
| [`catalog.go`](../../internal/extension/catalog.go) | Local package copies, generations, hashes and atomic catalog |
| [`manager.go`](../../internal/extension/manager.go), [`client.go`](../../internal/extension/client.go) | Processes, leases, bounded RPC, cancellation and replacement |
| [`store.go`](../../internal/extension/store.go) | Scoped state, account CAS and durable operation journal |
| [`inbound.go`](../../internal/extension/inbound.go) | Native codec calls and response encoding |
| [`protocol_extensions.go`](../../internal/gateway/protocol_extensions.go) | Gateway guards and canonical dispatch |
| [`sdk_cli.go`](../../internal/cli/sdk_cli.go) | Installation, inspection, auth and explicit file-plan application |

The public SDK imports no private Magpie package. Its manifest contract is
`{major: 1, minor: 0}`; the legacy Go codec interface has an independent version.

## Installation

A distribution contains `magpie-plugin.json` and its runtime files. It declares
`schema`, `id`, semantic `version`, `runtime` (`exec` or `bun`), relative
`entry`, optional `args`, `contract` and capability-specific descriptors.

```sh
magpie plugin sdk add ./distribution
magpie plugin sdk list --json
magpie plugin sdk inspect my-module
magpie plugin sdk off my-module
magpie plugin sdk on my-module
magpie plugin sdk reload my-module
magpie plugin sdk rm my-module
```

`magpie plugin add ./distribution` selects this installer when the directory
contains the manifest. Other plugin paths retain legacy behavior. The
[Go example](../../examples/extensions/echo-go/README.md) needs no vendor or key.

The installer rejects traversal, symlinks, non-regular payloads, oversized
packages and conflicting ownership. It copies a unique generation, hashes
every file, verifies the worker's public handshake and rechecks the payload
before publishing. A failed check keeps the old catalog. Concurrent writes
use compare-and-swap under a lock. Upgrade preserves a user's disabled setting.

Data lives under `appdir.Config()/extensions`. Removal revokes new work but
retains private state and payload generations for recovery and workers still
draining in another gateway process.

## Process and transport lifecycle

A Go-only package needs no Bun process. Bun packages use the app's existing
managed Bun owner. The child gets its own HOME and a small process environment;
unrelated provider credentials are not copied into environment variables.

`sdk.describe` must match the installed identity, version, capabilities and
descriptors. Paths/arguments are packaging details. File hashes are checked
before a process starts. A failed worker is restarted only for a later,
independent call.

Stdin/stdout carry bounded newline-delimited JSON; stdout is exclusively RPC.
Stderr is drained without retaining request data or blocking on long lines.
An envelope is at most 24 MiB, a native request body 16 MiB and a streamed
binary chunk 64 KiB. Host services separately accommodate a 1 MiB state value.
Binary JSON fields use base64.

One stream event is outstanding per call; the consumer acknowledges it after
finishing. Ordinary execution, native response encoding and operation control
have separate admission credits. A provider stream cannot consume the credits
needed to encode its own output.

Cancellation reaches the worker context. An unresponsive canceled call retires
the process after a grace period. Replacement starts new work in the new
generation while the old one drains for up to 30 seconds. Unix process groups
and the existing Windows child-tree owner stop only the module's processes.

## Native gateway path

Ordinary `gateway.New()` discovers installed endpoints. Explicit
`NewWithProtocols(registry)` retains deliberately linked Go construction and
does not load installed codecs. Core, page, callback, agent-wrapper and WSL
namespaces cannot be claimed by a module.

1. LAN, Host, proxy, caller, Origin, browser-key and read-only guards precede
   decoding. Gateway-key aliases apply only after canonical normalization.
2. The request binds a verified codec generation. Native bytes/headers are
   bounded and cloned; gateway authorization, cookies and transport headers
   are removed. `X-Magpie-Caller-ID` is an opaque host-generated identity for
   namespacing native caches, not additional authority.
3. The codec returns canonical Chat, Responses, Messages or Gemini JSON,
   authorized Models discovery, or a declared tool call. It cannot select an
   arbitrary administrative route or caller.
4. The normalized request retains peer, caller, session, cancellation and
   decoded deadline, then enters existing routing, account, budget,
   concurrency and accounting owners.
5. Same-module execution and response encoding retain the acquired process
   generation. `Finish` releases codec state after success, refusal or cancel.

Optional opaque metadata goes only to the same module's provider. A stable
native request ID identifies one logical request in its caller/session;
reusing a completed ID is refused rather than executing it again.

Models discovery projects the authorized catalog without an inference
reservation. Native controls resolve only to the same module's declared
tools and use the existing MCP permission/budget path. They respect the
Plugin tools toggle and cannot select another module's tool.

GUI one-port forwarding recognizes installed routes from the serving instance
or the local installed catalog when proxying to another process. Page-port
marking remains intact; a loopback proxy does not acquire local authority.

## Host services and effects

The host fixes plugin/provider/account/generation/session/operation scope.
A callback cannot substitute another scope.

| Service | Meaning |
| --- | --- |
| `state.get`, `state.cas` | Bounded JSON in the parent plugin/account-generation namespace |
| `network.proxy` | Effective proxy for an HTTP(S)/WS(S) target under the parent's transport policy, without dialing |
| `account.replace` | Replace the selected account after an accepted tool operation and advance its generation |

Proxy choice and its login remain per-call, not shared process environment.
Modules own vendor networking and must honor the resolution. Executables are
trusted local programs, not an OS sandbox.

Account replacement requires account-auth authority and an accepted receipt
for the exact selected account. It returns only ID/generation; the handler
then returns its terminal result, since the old generation is revoked.

The host persists acceptance before acknowledging it. Unexplained dispatched
worker death is uncertain. Accepted, uncertain and completed operations are
not automatically replayed or sent to a fallback. A known unsent call may be
reported as not started. Module-specific status/cancel can reconcile an
operation; the journal does not claim a vendor transaction was undone.

## Verification and scope

Run under temporary HOME/XDG with pinned Go caches and offline module
resolution. Suites cover `pkg/extension`, `pkg/protocolext`,
`internal/extension`, extension integration tests in
provider/gateway/agent/sessions/plugin/CLI, and
`node --test sdk/js/test/*.test.js`.

The Go-host/Bun fixture exercises the shared wire. The gateway fixture builds
an independently executed Go plugin from public imports and uses an ordinary
server. These are SDK conformance evidence, not vendor-client parity claims.

The SDK itself migrates no OpenCode, ZCode, OMP or Devin implementation, changes
no installed app and publishes no package. Those modules keep separate source
and migration ownership.
