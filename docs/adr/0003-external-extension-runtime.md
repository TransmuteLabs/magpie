# ADR 0003: Executable extension contracts

Date: 2026-11-12

Status: implemented as SDK v1. Local conformance, review closure and repository
gate limitations are recorded in
[the validation record](0003-external-extension-runtime-validation.md).

## Decision and scope

Extend the constructor-only codec SDK into an installable external module
system. Public Go contracts live in `pkg/extension`; the corresponding Bun
library lives in `sdk/js`. Core installation/runtime owners live in
`internal/extension`. Existing `pkg/protocolext` codecs remain compatible.
Vendor modules and their source trees are separate packages.

Use supervised executable workers rather than Go `plugin.Open`. The same
bounded stdio RPC works on macOS, Linux and Windows and for both Go and Bun.
Keep authentication policy, catalog projection, account selection, budgets,
final retries and durable operation ownership in the host.

## Package contract

A package contains `magpie-plugin.json` and its runtime files. The manifest
has `schema: 1`, `id`, `version`, `runtime` (`exec` or `bun`), relative `entry`,
optional `args`, and `contract: {version: {major: 1, minor: 0}, capabilities: []}`.
Descriptors are `inbounds` (id/path), `providers` (id/name/protocols/models),
`agents`, `sessions` and `tools`. Exact route and identity declarations are
frozen at installation and checked again against the worker's handshake.
Runtime paths and arguments are packaging details, not handshake authority.

Capabilities use the existing vocabulary plus `session-migration`:
`inbound-codec`, `outbound-executor`, `agent-wiring`, `account-auth`,
`model-discovery`, `usage-reporter`, `operation-control`, `tool-provider`.
Declarations do not grant methods from another capability.

Installation copies a bounded package, rejects symlinks/traversal,
records file hashes and publishes the catalog atomically under a lock.
Uninstall/disable revokes new work; old generations drain. Private state and
old package generations are retained for recovery rather than deleted while
another host process can still be using them.

The November 13 distribution addition follows the useful boundary in the
neighboring CLIProxyAPI plugin store: platform-specific release assets and
SHA-256 before activation. It keeps executable workers rather than adopting
the donor's C-shared runtime. GitHub release ZIPs stage through the existing
catalog installer and worker handshake. The catalog records source provenance;
explicit updates follow unpinned sources, preserve user pins and private
state, reject module-identity changes and use a source-generation precondition
to prevent stale updates from resurrecting removed modules. See the
[owning reference](../subsystems/executable-extensions.md#installation) for
the distribution naming and update behavior.

## RPC contract

Transport is newline-delimited JSON over stdin/stdout. Stdout is exclusively
RPC. Stderr is drained with bounded retained diagnostics. Binary fields are
base64 JSON strings; all response/data chunks are at most 64 KiB. An envelope
is bounded at 24 MiB (including a base64-encoded native body up to 16 MiB).
Concurrent calls are bounded.

Frames have `kind`, `id`, optional `method`, `params`, `result`, `error`,
`seq`, `event`, `data`, `scope`, `auth`, `parent` and `deadline`
(Unix milliseconds). Account credentials are injected only into their selected
call's `auth`, not into host callbacks or process environment.

| kind | Direction | Meaning |
| --- | --- | --- |
| `call` | host → worker | Invoke a negotiated method with scoped authority |
| `event` | worker → host | One numbered streaming event |
| `ack` | host → worker | The consumer processed that event; one outstanding event per call |
| `result` | worker → host | Exactly one terminal result/error |
| `cancel` | host → worker | Cancel that call's context |
| `host.call` | worker → host | A scoped host-service request belonging to the parent call |
| `host.result` | host → worker | Its terminal result/error |

The SDK's Go API uses `Module{Manifest, Handlers}` and
`Handler(context.Context, Call, *Stream) (any, error)`.
`Stream.Emit` waits for acknowledgement. `Stream.HostCall` uses a child
request ID and the parent call's authority. The Bun API exposes the same
module/handler/event semantics with AbortSignal cancellation.

Ordinary execution has 14 credits, native response encoding 16 and operation
control two, within 32 live RPCs. The host holds at most 16 logical native
responses per worker. Inference can therefore enter its own codec without
exhausting the credits needed to encode its output.

Every worker implements `sdk.describe`, returning its manifest. Methods:

| Capability | Required methods |
| --- | --- |
| inbound-codec | `inbound.decode`, `inbound.response` |
| outbound-executor | `provider.execute` |
| model-discovery | `provider.models` when providers are declared; inbound Models projection otherwise |
| account-auth | `auth.start`, `auth.finish`, `auth.refresh`, `auth.logout` |
| usage-reporter | `provider.usage` |
| operation-control | `operation.status`, `operation.cancel` |
| tool-provider | `tool.call` (tool descriptors in the manifest) |
| agent-wiring | `agent.plan` |
| session-migration | `session.convert` |

`inbound.decode` maps a native request to a canonical operation and optional
session/deadline, stable request ID and bounded namespaced metadata.
Same-module provider execution and encoding use the same generation lease.
Negotiated native tool requests enter existing MCP permission/budget owners.
`inbound.response` maps each canonical response event
(`start`, `write`, `flush`, `finish`, `cancel`) to native streamed events.
The host supplies a stable response ID to isolate per-request codec state.
The Go library adapts existing `protocolext.Inbound` implementations.

`provider.execute` receives canonical protocol, body, headers, model,
account scope and an operation ID. It emits `headers`, `data`, `flush`,
`usage` and `accepted` events, followed by its result. Credential-bearing
native requests never become generic routing metadata.

The common scope is `{plugin, provider, account, generation, session,
operation, attempt}`. It comes from the host; a worker cannot select another
account or operation by changing its params.

## State, effects and conversions

Host services initially provide `state.get` and `state.cas`, scoped to the
plugin and account generation of the parent call. Versioned values are
bounded JSON. `network.proxy` resolves the effective parent transport policy.
`account.replace` is limited to an accepted tool action with account-auth
authority and replaces only its selected account. The host owns credential
storage and account-generation CAS.

Before acknowledging `accepted`, the host journals that operation durably.
A dispatched worker's unexplained death is uncertain; it is never silently
retried. A later independent request may start a new worker, but does not
replay a previous operation. Module-specific status/cancel can reconcile it.

Agent wiring returns a declarative file plan; session conversion returns
bounded documents in an explicitly selected format. The host performs path,
precondition, dry-run, backup, atomic-write and rollback handling. Plugins
never receive blanket authority to apply arbitrary host file writes.

Executable plugins are trusted local programs, not a filesystem sandbox.
Core validates its own services and does not hand workers unrelated account
secrets, environment credentials or unrestricted core API objects.

## Implementation gates

The completion and regression matrices are in the SDK work plan. This ADR
does not mark any capability complete before its host path and Go/Bun
conformance fixture pass.
