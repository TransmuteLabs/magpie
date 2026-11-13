# Internal extension runtime comparison

Date: November 13, 2026.

Native Go C ABI loading, the preserved exec runtime, SDK Bun and composite
modules are implemented. Native Go runs inside Magpie's PID; exec Go runs
in a child, like SDK Bun. Keeping JSON frames at the native C boundary does
not make it an external worker.

This comparison is for modules we write and maintain ourselves. The goal is
to keep our provider/account behavior outside frequently merged upstream
code with little author boilerplate. Private GitHub release packages using
developer authentication are the normal distribution path. There is no
marketplace, new registry or arbitrary-language framework implied.

Implementation has scoped native/composite and gateway/tool proof;
[1.1 validation](../adr/0005-native-composite-validation.md) and
[1.2 validation](../adr/0006-gateway-hooks-validation.md) identify the actual
trees and later fixes. This is not a whole-repository green or vendor-parity claim.

## Compared implementations

| Column | Actual implementation and evidence boundary |
| --- | --- |
| Legacy TS | Magpie provider/OpenCode/Pi host: shared Bun child. Inspected source; no live vendor account used. Separate legacy `agent.js` and gateway middleware run through moejs inside Magpie, outside this column. |
| ProxyPilot C | CLIProxyAPI-family C-shared implementation in the local ProxyPilot checkout identified below. Source comparison only; no ProxyPilot binary/vendor flow run. |
| SDK exec Go | Installed executable using the public Go SDK, common host/lease/journal and process transport. Real compiled-worker/CLI/gateway fixture evidence exists. |
| SDK native Go | Installed Go C-shared library using the public SDK adapter and common host owners. Actual host-PID, stream/callback/lifecycle and composite fixture proof on macOS and Linux. |
| SDK Bun TS | Installed JS/TS primary or service using the JS SDK and managed Bun. Real Go-host/Bun API 1.1/1.2 fixtures, plus 102 Node/102 Bun tests and strict types. |

“Native protocol” in an older gateway test means a provider request format,
not library loading. Actual PID checks now separately establish native
execution. The [SDK validation record](../adr/0003-external-extension-runtime-validation.md)
covers earlier gates; it is not a substitute for the native/composite proof.

## Placement, transport and copying

| Parameter | Legacy TS | ProxyPilot C | SDK exec Go | SDK native Go | SDK Bun TS |
| --- | --- | --- | --- | --- | --- |
| Load | Bun imports OpenCode modules or adapts Pi providers. | Dynamic library; resolves `cliproxy_plugin_init`. | Starts module executable. | Loads library; resolves versioned open/send/close exports. | Starts Bun with module entry. |
| PID / address space | Bun child shared by provider plugins. | Host PID/address space. | Child per serving component instance. | Magpie PID/address space; no primary child. | Bun child per serving component instance. |
| Go runtime / heap | Magpie Go and Bun are separate. | Go C-shared image carries runtime support; no shared Go interfaces. | Independent child Go runtime, heap and GC. | C-shared runtime support inside host PID; host/plugin Go heaps/interfaces are not interchangeable. | Magpie Go and Bun heaps are separate. |
| Call transport | Legacy JSON over stdin/stdout. | C function tables carrying JSON method bytes. | SDK NDJSON over OS pipes. | Same SDK frames over C spans/callbacks and in-memory adapters. | Same SDK NDJSON over OS pipes. |
| Serialization | JSON envelopes and encoded stream bytes. | JSON DTOs/envelopes over C ABI v1/schema v1. | JSON; binary fields use base64. | Same JSON/base64; no process IPC. | Same JSON/base64. |
| Copying | JS/Go encoding, pipe and stream buffers. | Unix uses `C.CBytes`/`C.GoBytes`, then plugin free; Windows copies returned buffers too. | Encoding/base64, pipes and application buffers. | Ownership-safe copies of borrowed spans, C buffers and JSON/base64; not zero-copy. | Encoding/base64, pipes and JS buffers. |
| Streaming | Provider fetch body/chunks. | Executor callbacks and host HTTP/model stream read/close APIs. | SDK events with ordered sequence/ACK. | Same events/ACK through synchronous C callbacks. | Same events/ACK. |
| Backpressure | 512 KiB stream credit, 64 MiB stream budget and RPC reserves. | Bounded stream channels; callback emit can block on consumer. | One unacknowledged event per call; execution/response/control admission credits. | Same common credits; bounded synchronous C emit preserves backpressure. | Same SDK credits as exec. |
| Bounds | Legacy host-specific limits. | Host/plugin ABI and stream-specific limits. | 24 MiB frame, 16 MiB protocol request body, 64 KiB stream chunk. | Same SDK limits; borrowed buffers valid only until callback return. | Same SDK limits. |

JSON framing and process placement are independent decisions. The native
transport removes OS-pipe crossings and the primary child, while retaining
C calls, marshaling, base64 and copies. No benchmarks were run; there is no
fixed latency, RSS or throughput ranking.

## Errors, cancellation and lifetime

| Parameter | Legacy TS | ProxyPilot C | SDK exec Go | SDK native Go | SDK Bun TS |
| --- | --- | --- | --- | --- | --- |
| Ordinary error | Hook exceptions become legacy errors. | JSON error envelopes and adapter errors. | Typed public SDK errors; plain errors/panics sanitized. | Same server error contract; cannot contain arbitrary native faults. | `SDKError` public fields; other exceptions sanitized. |
| Crash boundary | Can lose plugins sharing that Bun host; Magpie is separate. | Native fault can terminate/corrupt host. | Worker generation fails; host survives. | Native fault can terminate/corrupt Magpie. | Component child fails; host survives. |
| Cancellation | Abort/control and host retirement. | Context checked before synchronous C call; no forced interruption. | Cancel frame/context, then process retirement after grace if needed. | Cooperative contexts/session revocation; stuck C/Go code cannot be killed in isolation. | AbortSignal/cancel, then child retirement if needed. |
| Uncooperative code | Child-tree termination can affect shared plugins. | Active-call guard can wait indefinitely for stuck call. | Host can terminate child tree. | Logical close unblocks transport; arbitrary stuck module code may remain. | Host can terminate child tree. |
| Accepted effects | Legacy provider-specific rules. | Separate adapter/retry rules; not Magpie's journal. | Accepted/uncertain/completed journal; no automatic replay of uncertain dispatched work. | Same root journal/account owners. | Same root journal/account owners. |
| Replacement / drain | Shared-host replacement; source permits up to ten minutes. | Config/snapshot replacement; explicit unload waits for active calls. | New root generation handles new work; old leases drain up to 30 seconds. | Same logical generation drain; code remains mapped. | Same root-generation drain as exec. |
| Stop / disposal | Legacy owner cleanup. | Shutdown/free/unload after guarded calls. | `component.stop` joins cooperative startup/disposer; hard close remains available. | Same SDK stop; session close revokes without waiting for arbitrary code. | Stop aborts calls/waits and joins cooperative startup/disposer; child can be killed. |
| Physical unload | Ends child host. | Unix `dlclose` / Windows DLL release. | Exits child. | Libraries remain mapped until Magpie exits; close is not `dlclose`. | Exits child. |
| Retry after failure | Legacy-specific. | Host-specific. | Later independent call may start a new runtime; effects are not silently retried. | Same common policy; reopening is not unloading a Go image. | Same common policy. |

## Account correctness, configuration and host authority

| Parameter | Legacy TS | ProxyPilot C | SDK exec Go | SDK native Go | SDK Bun TS |
| --- | --- | --- | --- | --- | --- |
| Authority | Existing plugin/account/request context. | Broader executor/auth/frontend/interceptor host APIs. | Host fixes plugin/provider/account/generation/session/operation; callback cannot substitute scope. | Same checks at SDK API; native code itself shares host privileges. | Same fixed parent scope. |
| Credentials | Async request scope selects `getAuth`, loader and `auth.set`. | Auth-index/file callbacks; not Magpie generation CAS. | Selected auth in call, not global env; `account.replace` requires accepted selected tool scope and CAS. | Same call-local auth/CAS; do not use process globals for account selection. | Same auth/CAS; actual Bun replacement fixture proves generation-2 auth. |
| Private state | Legacy host/provider stores and plugin conventions. | Host callbacks plus plugin-owned state/config. | `state.get/cas` in parent namespace/account generation. | Same callbacks through C bridge. | Same callbacks through stdio. |
| Root module config | Existing options conventions. | Host config snapshots/plugin config. | API 1.1 fixed `module.config`, root-only namespace, whole-object decimal-version CAS. | Same config; old lease retains old snapshot. | Same config and lifecycle DTOs. |
| Local instance state | Legacy module/host objects. | Plugin-owned objects. | `ModuleReady.State` / `ModuleContext.State`, opaque and local. | Same per-session opaque State; never serialized through ABI. | Startup `state` / `ctx.module.state`, same local reference. |
| Environment | Legacy Bun environment/proxy setup. | Host process environment. | Minimal child env and module HOME/config/cache; unrelated provider credentials omitted. | Magpie process environment; cannot receive a separate child HOME. | Same minimal child policy as exec. |
| Filesystem / cwd | Host settings; child commands use existing plugin-work policy. | Host filesystem permissions/global state. | Normal child filesystem access; checked host Actions own approved agent/session writes. | Host filesystem permissions; avoid process cwd/env mutations. Checked Actions remain the file-plan owner. | Same child/Actions split as exec. |
| Network | Existing contextual fetch/proxy adaptations. | Host HTTP/model execution callbacks. | `network.proxy` resolves parent policy; module performs networking. | Same per-call resolution; not a new host HTTP client. | Same resolution, JS performs networking. |

These are correctness boundaries for our own code, not OS sandbox claims.
Root package/config generation and selected account generation are different
identities; both stay pinned when a primary calls its helper.

## Authoring, builds, compatibility and distribution

| Parameter | Legacy TS | ProxyPilot C | SDK exec Go | SDK native Go | SDK Bun TS |
| --- | --- | --- | --- | --- | --- |
| Author entry / burden | Existing OpenCode export or Pi provider. | Reviewed Go example writes C declarations and init/call/free/shutdown exports. | Ordinary Go `main` calls `Serve` with a `Module`. | `native.Register(Module)` in `init`, empty `main`; SDK supplies all C exports and wire glue. | JS/TS `serve(module)`; SDK supplies wire/control dispatch. |
| Actions / startup | Existing hooks. | API-specific registration/lifecycle. | `Module.Actions` auto-dispatch; optional `Module.Start`/disposer. | Same Go APIs; no author C shim or global instance registry. | `module.actions`, `module.start`/dispose and typed context. |
| Artifact | Existing package/file workflow. | Platform shared library. | Executable plus manifest/payloads. | Shared library plus manifest/payloads; no native argv. | Script/package plus manifest/payloads. |
| Bun dependency | Yes for provider host; separate moejs paths differ. | None for native loader. | None unless a declared component uses Bun. | None for native + Go helpers + prebuilt TS UI. | Managed Bun required for execution; Node is a conformance runner. |
| cgo / C toolchain | Not for JS source. | Go C-shared build and Unix loader need C toolchain/cgo; separate Windows loader. | Pure Go possible, dependencies permitting. | C-shared author build and supported cgo host required; no-cgo refuses native clearly. | Not for JS source itself. |
| OS / execution proof | Bun/platform dependencies. | Unix/Windows loaders inspected, not executed here. | Platform executable; pure Go cross-build possible. | Darwin/Linux/FreeBSD/Windows loader branches; macOS executed. Linux test preparing, no result yet; Windows unexecuted. | Bun-supported platform and dependencies. |
| ABI / version coupling | Legacy OpenCode/Pi/host APIs. | C ABI 1 + JSON schema 1. | SDK version/capabilities/JSON; no shared Go package build IDs. | C ABI 1 + SDK contract; no `plugin.Open` Go type identity. Compiler/runtime combinations still need platform proof. | Same SDK contract plus Bun/JS compatibility. |
| SDK selection | Absent SDK descriptor retains legacy loading. | Its own library discovery. | Explicit valid SDK manifest; runtime `exec`. | Explicit valid SDK manifest; runtime `native`. | Explicit valid SDK manifest; runtime `bun`. |
| Backward compatibility | Existing plugins/accounts retain owners. | Different ABI/registration/accounts; not Magpie binary-compatible. | API 1.0 single-primary format retained. | Supports same contracts; not an automatic port of legacy or ProxyPilot libraries. | API 1.0 nine-capability behavior retained; invalid new descriptors error. |
| Private release auth | Existing legacy distribution remains. | Not a Magpie installer contract. | Common GitHub installer: env token, `gh`, HTTPS Git credential fallback; scoped/lazy. | Same installer; credentials never become module/provider config. | Same installer. |
| Update / pins / integrity | Existing legacy updater. | Separate reference implementation. | Full-package checksums/hashes, catalog CAS, unpinned update, explicit tag pins. | Same package catalog including helpers/UI. | Same package catalog including helpers/UI. |
| Upstream merge footprint | Existing compatibility adapters stay. | Separate host; copying its hook stack would be a larger port. | Generic seams in core, our behavior in modules. | Adds loader/transport/adapter at the same seams; no vendor switch. | Reuses common seams and JS author library. |

The [native example](../../examples/extensions/echo-native/README.md) and
[hybrid example](../../examples/extensions/hybrid-go/README.md) contain the
exercised author workflow. No handwritten C is required. Native registration
runs in `init`, because loading a library does not execute `main`.

Release installation reads developer credentials only after the repository's
API challenges an anonymous request. SSH-only Git auth does not authenticate
the Releases API. Whole-package pins/provenance/checksums/update rules are in
[installation](executable-extensions.md#installation). Offline credential and
HTTP fixtures are not a claim of a live private-repository login.

## Hybrid composition is a module axis

Runtime belongs to a component. The module is the installation/version/config/
lifecycle unit. [ADR 0005](../adr/0005-composite-module-contract.md) implements
primary controller + declared sidecars; the original nine capabilities remain
on the primary, and only declared unary ModuleRPC actions route by owner.

| Parameter | Legacy TS | ProxyPilot C | SDK exec Go | SDK native Go | SDK Bun TS |
| --- | --- | --- | --- | --- | --- |
| Host-owned private services | Existing service conventions, not SDK root groups. | No equivalent composite contract established by this source comparison. | Up to 16 exec/Bun services with named contract/method owners. | Same; actual native primary + external Go helper proved. | Same descriptor/lifecycle contract. |
| Dependencies / readiness | Legacy service metadata. | Host-specific lifecycle. | DAG, eager/lazy startup, exact instance/config/endpoint readiness. | Same root group; old lazy helper stays on old lease after update. | Same Go-host control wire and JS startup API. |
| Helper calls | Legacy plugin-specific wiring. | Broader host callback APIs. | Primary `component.call` binding; inherits account/operation; no second dispatched journal. | Same parent lease across in-process primary and external helper. | Same call contract; read cannot escalate to write. |
| UI integration | Existing package metadata/page kernel. | Management/resource hooks; not Magpie's page bridge. | Declared window + existing `magpie.page/panels/actions/permissions` + named bindings. | Same TS UI independent of native runtime; does not require Bun. | Same existing page kernel; browser author entry imports no Node/Bun. |
| UI freshness / HTTP identity | Existing legacy ref/service owner. | No matching Magpie proof contract asserted. | Opaque package-revision + config refs; three instance/component/config response headers checked before body. | Same; HTTP/SSE response holds root lease, proof stripped before frontend. | Same SDK UI owner/proof. |
| Streaming boundary | Legacy provider/service streams. | Executor/host stream APIs. | Existing primary capability streams preserved; ModuleRPC unary only; HTTP/SSE via service proxy. | Same; native ABI supports primary streams, not ModuleRPC progress. | Same unary restriction and existing streams. |

Adjacent UI files alone do not integrate a page. `window.metadata` must
resolve valid package metadata, and named bindings grant only that part's
actions/endpoints. SDK packages do not enter legacy `plugins.json` or start a
Bun provider host just to expose a page. Root config CAS retires old refs;
pending work is canceled on frame disposal/off/update.

## Capability coverage and missing ProxyPilot hooks

The SDK has eleven negotiated capabilities. The first nine stay on the primary:
inbound codecs, outbound execution, agent wiring, account auth, model discovery,
usage, operation control, tools and session migration. ModuleRPC is the tenth;
API 1.2 gateway-hooks is the eleventh. Gateway hooks and local-owner tools
allow independent memory-module composition; they are not shared-Memory,
stream-middleware, router or global-observer adapters. See the
[coverage census](extension-surface-coverage.md).
All three SDK transports use common production owners. These rows describe
implemented surfaces; the scoped fixtures below do not exercise every vendor
or every capability through every transport.

| Surface | Legacy TS | ProxyPilot C | SDK exec Go | SDK native Go | SDK Bun TS |
| --- | --- | --- | --- | --- | --- |
| Incoming protocol | Core formats; separate middleware owner. | Translation/normalization hooks. | Declared codecs enter ordinary guards and canonical dispatch. | Same primary codec contract and host owners. | Same primary codec contract and host owners. |
| Provider execution | OpenCode loader/fetch; adapted Pi provider runtime. | Executor/streaming executor. | `provider.execute`, ordinary routing/account selection. | Same executor/stream contract. | Same executor/stream contract. |
| Account auth | OAuth/key and Pi sign-in/refresh hooks. | Parse/login/poll/refresh plus frontend auth. | `auth.*`, selected auth/generation CAS. | Same account owner and scoped callbacks. | Same; generation-2 replacement fixture exists. |
| Models | Legacy provider/Pi catalog. | Static/per-auth registration and discovery. | Manifest/dynamic models, prices/efforts, authorized catalog. | Same model contract. | Same model contract. |
| Usage | Provider usage; separate global middleware observer. | Completed usage observer. | Provider usage/allowances and execution events. | Same; no new global observer. | Same; no new global observer. |
| Operations | Legacy call/abort behavior. | Executor/context/stream lifecycle. | Status/cancel and durable accepted/uncertain no-replay journal. | Same journal; cancellation cooperative. | Same journal. |
| Tools | MCP/hub tools; Pi-specific tools not exposed by provider adapter. | No dedicated Magpie-style MCP capability in reviewed API. | `tool.call` through ordinary MCP/hub and key/budget owners. | Same primary tool contract. | Same primary tool contract. |
| Agent wiring | Separate `agent.js`/moejs adapter. | No dedicated Magpie-style agent-plan capability in reviewed API. | Rich descriptors enter ordinary `Agent.All`/inspect/connect/apply/disconnect via checked plans. | Same agent-plan owner. | Same agent-plan owner. |
| Sessions | Pi provider adapter does not import session tools/commands/renderers. | No dedicated session-convert capability in reviewed API. | Routed converters reach normal CarryOver; explicit conversion also available. | Same converter owner. | Same converter owner. |
| Module actions | Existing plugin-specific hooks. | Different registration/callback contract. | Declared owner dispatch, inherited helper scope; unary only. | Same action/group ownership. | Same action/group ownership. |

The actual ProxyPilot API includes hooks not provided as equivalent SDK
surfaces, or not verified at the same execution point:

- Frontend auth replacement, including exclusive frontend auth.
- Auth-candidate scheduler and model-router callbacks before account selection.
- Ordered request interception before/after credentials; response normalization
  before/after translation; response and stream-chunk interception on other
  providers' traffic.
- Global thinking applier, completed-usage observer, executor token-count and
  raw executor HTTP-request hooks.
- Plugin-defined CLI flags, Management API routes and resource registration.
- Host HTTP/model stream APIs and auth-file inventory/runtime/save callbacks.

Related core or legacy behavior does not establish the same SDK hook.
`network.proxy` resolves policy without dialing; it is not `host.http.do`.
Native loading alone does not add these hooks. No cross-host vendor-parity
test or full CLIProxyAPI parity claim is made.

## Scoped proof and remaining gates

[`TestNativeSharedLibraryLifecycle`](../../internal/extension/native_integration_test.go)
and the actual native CLI roundtrip establish real library loading and host-PID
execution. [`TestCompositeHybridBundle`](../../internal/extension/composite_integration_test.go)
establishes primary PID = host/helper PID differs, scope/readiness/cleanup and
held old lazy v1/config1 after v2/config2 update.
[`TestExtensionWindowHybridRuntime`](../../internal/gui/extension_window_integration_test.go)
uses the real backend/status/SSE and checks cookies, off/update, response
identity and response lease hold. Go-host/Bun API 1.1 passed race count 3
without skips. JS has 87/87 Node and 87/87 Bun plus strict types.

These scoped checks are complete. Final immutable gates remain pending the
integration owner. Remote Linux native has no execution result yet; Windows
native is unexecuted. Cross-build/refusal tests do not close those runtime
gaps. No benchmark or live vendor migration was performed.

## Source references

Magpie links are relative to this repository:

- Legacy: [`host.go`](../../internal/plugin/host.go),
  [`host.js`](../../internal/plugin/host.js), [`pi.js`](../../internal/plugin/pi.js);
  separate [`agentplug.go`](../../internal/agentplug/agentplug.go) and
  [gateway middleware](gateway-middleware.md).
- Transport/lease: [`client.go`](../../internal/extension/client.go),
  [`module_transport.go`](../../internal/extension/module_transport.go),
  [`manager.go`](../../internal/extension/manager.go),
  [`native_cgo.go`](../../internal/extension/native_cgo.go).
- Author/ABI: [`abi.h`](../../pkg/extension/native/abi.h),
  [`native.go`](../../pkg/extension/native/native.go),
  [`exports.go`](../../pkg/extension/native/exports.go),
  [Go SDK](../../pkg/extension/README.md), [JS SDK](../../sdk/js/README.md).
- Composite/UI/config: [`composite.go`](../../pkg/extension/composite.go),
  [`components.go`](../../internal/extension/components.go),
  [`module_config.go`](../../internal/extension/module_config.go),
  [`pluginpage.go`](../../internal/gui/pluginpage.go),
  [`extension_window_service.go`](../../internal/gui/extension_window_service.go).
- Distribution/CLI: [`release.go`](../../internal/extension/release.go),
  [`release_auth.go`](../../internal/extension/release_auth.go),
  [`sdk_cli.go`](../../internal/cli/sdk_cli.go).

External source origin is specifically the local checkout:

`/Users/maratkarimov/work/SIB/Proxy/CLIProxyAPI-family/ProxyPilot`

Observed HEAD: `7ff4825858bd3887772411faf0ab7eaaf7045691`. The following paths
are relative to that external root, not Magpie or an assumed upstream version:

| External source | Behavior compared |
| --- | --- |
| `internal/pluginhost/loader_unix.go`, `loader_windows.go` | Dynamic load, C function tables, buffer copies/free and physical unload |
| `internal/pluginhost/client_guard.go`, `host.go` | Active-call guard, unload wait, registration/config snapshots |
| `internal/pluginhost/rpc_client.go`, `sdk/pluginabi/types.go` | JSON over C ABI, separate ABI/schema versions |
| `internal/pluginhost/stream_bridge.go`, `host_callbacks.go`, `auth_callbacks.go` | Stream backpressure and HTTP/model/auth callbacks |
| `sdk/pluginapi/types.go` | Concrete executor/auth/scheduler/interceptor/thinking/CLI/Management interfaces |
| `examples/plugin/simple/go/main.go` | Author C declarations and exported ABI glue in that example |
