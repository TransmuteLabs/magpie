# ADR 0005: A module describes its components and compatibility

Date: 2026-11-13

Status: implemented with scoped native/Go-service/window and Go-host/Bun
proof. Final immutable acceptance gates remain pending the integration
owner; this decision does not claim a green whole-repository run.

## Decision

Our plugin is one installed module, maintained by us, with one version,
configuration and enable/update lifecycle. It may combine a native Go
controller, external Go services and a TypeScript interface. The module
describes those parts and their contracts. Core interprets the description;
vendor-specific behavior remains in the module.

Preserve the existing nine capability paths on one primary controller.
Use the explicitly negotiated `module-rpc` capability in SDK API 1.1 for
declared actions. Only those actions may target private services. This keeps
the current inbound request lease and its primary response encoder intact.
It avoids a general rewrite routing every existing method between components.

An example of the new additions to `magpie-plugin.json`:

```json
{
  "schema": 1,
  "id": "our-module",
  "version": "1.0.0",
  "runtime": "native",
  "entry": "lib/primary.dylib",
  "contract": {
    "version": {"major": 1, "minor": 1},
    "capabilities": ["module-rpc"]
  },
  "compatibility": {
    "hostAPI": "^1.1",
    "nativeABI": 1,
    "platforms": ["darwin/arm64"]
  },
  "actions": [
    {"id": "refresh", "inputSchema": {"type": "object"}, "effect": "read"},
    {
      "id": "backend.run", "component": "backend",
      "inputSchema": {"type": "object"}, "effect": "write"
    }
  ],
  "services": [{
    "id": "backend",
    "runtime": "exec",
    "entry": "bin/backend",
    "contract": {"id": "our-backend", "version": 1},
    "methods": ["backend.run"],
    "start": "lazy",
    "dependsOn": [],
    "ready": {"timeoutMs": 10000, "httpEndpoints": ["ui"]}
  }],
  "window": {"metadata": "package.json"},
  "bindings": [
    {"id": "page.refresh", "from": "window:page", "action": "refresh"},
    {"id": "controller.run", "from": "primary", "action": "backend.run"},
    {
      "id": "page.service", "from": "window:page",
      "service": "backend", "endpoint": "ui"
    }
  ]
}
```

These are current public JSON names in
[`composite.go`](../../pkg/extension/composite.go). The example is an
illustration with placeholder payload paths; the built, exercised distribution
is [`hybrid-go`](../../examples/extensions/hybrid-go/magpie-plugin.json).
Its package separates `lib/primary`, `bin/backend` and `ui/` assets; runtime
entry paths are relative to the package root.

## Compatibility and legacy selection

No `magpie-plugin.json` means the existing legacy JS/TS loader for ordinary
plugin installation. File extensions do not select a new SDK runtime.
A valid explicit SDK 1.0 manifest without composite fields keeps its
single-controller behavior, including calls without component configuration.
An explicitly described new module that is malformed or incompatible is an
error, never a reason to silently retry it as a legacy TypeScript plugin.

The descriptor declares its SDK contract, composite-host API, native ABI and
supported platforms. These name API compatibility rather than requiring
our integrations to track every upstream application release tag.
`compatibility.hostAPI` accepts `"^1"` or `"^1.1"`; `nativeABI`, when set,
must match ABI 1; `platforms` optionally restricts `GOOS/GOARCH`.
These are separate from semantic module versions. Platform/ABI compatibility
is checked before executing the component. An installed CLI must itself
contain this SDK; module installation cannot add commands to an older app.

## Contracts and startup

The primary is the root `runtime`/`entry`. A service is a private `exec` or
`bun` component, with an exact named contract and version. `dependsOn` forms
an acyclic service graph; `start` is `eager` or `lazy`. There are at most 16
service descriptors, including multiple entries sharing one binary. The
aggregate actions/services/bindings budget remains 1024. Readiness has a bounded
timeout and describes the actual component/contract/instance/configuration
version and exactly its declared HTTP endpoints. Startup defaults to ten
seconds; `ready.timeoutMs` can override a service timeout up to 300000 ms.
Dependencies start first, then eager services, then primary configuration.
A helper's configure snapshot contains its declared dependencies; the primary
gets ready eager services. Lazy services start when an action/binding needs them.

SDK-owned `component.configure` and `component.ready` control calls initialize
one runtime instance from its frozen context. A module may supply startup,
readiness and disposal hooks; it does not implement the ABI or control RPC
plumbing. Hooks use the module lifetime rather than the temporary describe
request's deadline. Return a disposer even after a partial startup failure;
the SDK invokes it on that error. Resources created before a hook panics
without returning its disposer remain the author's cleanup responsibility.

Go `Module.Start` returns `(ModuleReady, func(), error)`; JS `module.start`
returns `{endpoints, state, dispose}`. SDK-owned `component.stop` cancels
business calls, pending emits/host callbacks and startup, joins cooperative
startup cleanup and invokes the disposer once. Group cleanup stops clients
in reverse start order with bounded control waits. Exec/Bun can be terminated;
native cancellation and disposal remain cooperative. Logical native close
revokes transport without unloading the library or waiting for stuck code.

`ModuleReady.State` and `ModuleContext.State` retain one opaque local
reference per runtime instance; JS exposes `state` through `ctx.module`.
The value is never JSON-cloned or serialized. Config/dependencies/endpoints
are independent copies; the author synchronizes State and closes its resources
in the disposer. Separate instances do not share startup State.

One group belongs to the root package/configuration generation. Nested helper
calls use that group even if the installed catalog changes during a request.
Services are not independently installed, enabled, updated or given account
identities. Disabling the root rejects new work and withdraws its window parts;
existing root leases drain before owned resources are disposed.

## Actions, scope and effects

An action names its owner, input schema and `read`/`write` effect. The SDK
provides `Module.Actions` and dispatches `module.call {action, input}`.
A binding names who may invoke which action or service endpoint.

The browser API is `ui.backend.call(binding,input)` from
`@magpie/extension-sdk/ui`, delegating to `window.magpie.backend.call`.
The parent window supplies its own opaque reference, bound to module,
package revision, config version and part. The UI does not supply an
arbitrary component URL, account scope or credentials.

The controller uses `Stream.HostCall` with `MethodComponentCall` and
`ComponentCallRequest{Binding,Input}`, or JS
`stream.hostCall("component.call",{binding,input})`. Core derives the target
from the frozen binding and the parent lease. It inherits the selected
account generation, cancellation and operation, without another lookup of
the current installed module.

There is one root operation journal for a write. The primary must have
acknowledged durable acceptance before delegating a write. A private helper
call does not create another `dispatched` record for that operation.
Accepted effects are not replayed automatically when a helper fails. A read parent cannot
escalate to a write helper binding.

ModuleRPC 1.1 is unary: terminal JSON plus an `accepted` receipt for a write.
Streaming data/progress on this method is rejected explicitly, and a read
action cannot emit acceptance. Catching the Emit error does not rescue the
terminal result. ModuleRPC is separate from MCP/inference retry semantics.
Existing capability streams remain available on the primary, and declared
HTTP/SSE service endpoints use the existing page-service streaming proxy.

## Configuration and window ownership

Configuration has a fixed module namespace, `Scope{Plugin: rootID}` and
`module.config`, with a CAS version. Its version is a canonical decimal uint64
string; values are a bounded, unambiguous JSON object, replaced whole through
CAS. The default is `{"version":"0","values":{}}`. The interface and every
component see the same snapshot. A configuration change creates a new runtime
generation; an in-flight lease retains its old configuration and helpers.

Keep the complete root descriptor in host authority. `Manifest.Public()`
is a primary handshake projection, not the permission source for the group.
It removes packaging, compatibility, service/window declarations, actions owned
elsewhere and all bindings. `ServiceManifest(root,id)` derives a manifest with
the root identity/version, that service's contract and allowed actions.

Reuse `package.json.magpie.page/panels/actions/permissions`, existing
`pluginpage.go`, `pagebridge.js` and the window renderer. SDK-installed
window parts do not enter `plugins.json` or start a Bun provider host.
Refs/assets bind both package revision and config version; the old frame is
never retargeted to a new package/configuration. Disabling/removing a module
withdraws its parts and cancels pending bridge work. Native webview and browser
use the same parent-mediated bridge. Metadata `permissions` govern existing
window reads, while named bindings separately restrict action/endpoint access.
The lazy [`extensionwindows.js`](../../internal/gui/assets/extensionwindows.js)
adapter keeps the existing window kernel and legacy owner behavior.

The separate browser entry has no Node/Bun imports. It exposes `ui.ready`,
`ui.service`, `ui.backend.call`, `ui.config.get` and `ui.config.cas`.
There is no per-call AbortSignal API; the frame owner aborts pending work on
disposal, revision/config change or off.

Declared endpoint names resolve through runtime readiness, never a frontend
port choice. GUI targets are numeric-loopback HTTP endpoints with an explicit
nonzero port, excluding the gateway port. The existing proxy streams HTTP/SSE,
strips cookies and keeps the root lease until the response finishes.
Each backend response must carry exactly:

| Header | Expected value |
| --- | --- |
| `X-Magpie-Component-Instance` | Readiness instance |
| `X-Magpie-Component` | Declared component ID |
| `X-Magpie-Component-Config` | Frozen config version |

Go `ModuleContext.HTTPHeaders()` and JS `moduleHTTPHeaders(context)` return
fresh header maps with only those fields. No State/config values/auth are
included. The proxy checks them before forwarding body bytes and removes
them from frontend headers/trailers. This prevents a reused loopback port
from serving another instance/configuration under an old readiness record.

## Implementation seams and scoped proof

| Source | Contract owner |
| --- | --- |
| [`composite.go`](../../pkg/extension/composite.go), [`lifecycle.go`](../../pkg/extension/lifecycle.go) | DTOs, validation/DAG, projections, configuration/start/stop, local State and HTTP headers |
| [`components.go`](../../internal/extension/components.go), [`manager.go`](../../internal/extension/manager.go) | Root group/lease, action-owner dispatch, inherited account/operation and disposal |
| [`module_config.go`](../../internal/extension/module_config.go) | Fixed root namespace and CAS |
| [`module_window.go`](../../internal/extension/module_window.go), [`catalog_window.go`](../../internal/extension/catalog_window.go) | Complete descriptor authority, metadata validation, revision/config refs and bindings |
| [`extension_window.go`](../../internal/gui/extension_window.go), [`extension_window_service.go`](../../internal/gui/extension_window_service.go) | Existing page/panel bridge adapter and proved HTTP peer |
| [`sdk/js`](../../sdk/js/README.md), [`ui.js`](../../sdk/js/ui.js) | Matching server/declarations and browser author API |

Completed fixture proof includes a real combined module: native handler PID
equals host PID; its Go service has a different PID. `TestCompositeHybridBundle`
covers scope/readiness/off-on, native streams/nested cancellation, startup
cleanup and a held lease that starts v1/config1's lazy helper after v2/config2
is installed. `TestExtensionWindowHybridRuntime` exercises the actual backend,
status/SSE, cookie removal, response proof, invalidation and held response lease.

Go-host/Bun API 1.1 passed race count 3 with no skips. The JS SDK has 87 Node
and 87 Bun passes plus strict declaration checks. These are scoped proofs;
they do not establish vendor parity, benchmark results, Windows native
execution or final immutable repository acceptance. Remote Linux native
execution is being prepared with no result yet. The integration owner
maintains final gates; this lane migrated no vendor implementation.
