# ADR 0004: Native Go extensions use a stable C ABI

Date: 2026-11-13

Status: implemented with scoped macOS native and composite integration proof.
Final immutable acceptance gates remain pending the integration owner; no
whole-repository green result is claimed. This supersedes ADR 0003's
external-executable-only Go installation decision.

The scope is our internal modules, maintained by the same developers as the
fork. Contracts keep those modules separate from upstream-owned Magpie code;
this is not a public plugin marketplace.

## Problem and correction

Our integrations need both Go work inside Magpie and optional external
services. The original executable Go SDK used the same process boundary as
the Bun SDK. Native loading adds the requested C-shared option while
preserving that executable format.

## Decision

Support `runtime: "native"` for `.dylib`, `.so` and `.dll` distributions.
Load libraries into Magpie with the versioned C contract in
[`pkg/extension/native/abi.h`](../../pkg/extension/native/abi.h), not
`plugin.Open` or shared Go interfaces.
The public Go SDK owns the exports, handles and memory rules. An author
registers an ordinary `extension.Module`; building with `-buildmode=c-shared`
provides the native entrypoints without handwritten C/RPC glue. Registration
runs from `init`, because loading a library does not run `main`.
The SDK snapshots the full manifest, handlers, actions, startup hook and
inbound registrations; implementations must support concurrent sessions.

Keep the original nine primary capability contracts, scoped host services,
canonical data, stream acknowledgement and operation journal shared across
native and process modules. SDK API 1.1 adds unary ModuleRPC and component
lifecycle without changing the primary codec/provider lease. Explicit SDK
1.0 manifests retain their original behavior.

The C ABI exports `magpie_extension_open_v1`, `magpie_extension_send_v1` and
`magpie_extension_close_v1`. It checks ABI version and descriptor size and
uses opaque integer handles and bounded borrowed byte spans.
No Go pointers, interfaces or allocator-owned buffers cross the boundary.
The sender keeps a span alive until the call returns; a receiver copies it
before retaining it. The adapter copies bytes and retains JSON/base64;
native transport does not mean zero serialization or zero copying.
The format of an SDK frame is independent of whether it crosses a process
boundary. SDK frame/event limits and acknowledgement backpressure apply
through synchronous C callbacks as well as through process pipes.

Native code shares the host's address space, environment and failure domain.
It must not change process-wide state such as the working directory. Scoped
host-service checks remain meaningful API boundaries, not a security sandbox
against our own native code. A host cannot forcibly kill a stuck native function or
recover from arbitrary memory corruption. Libraries with Go runtimes remain
mapped until process exit; closing a session is not `dlclose`.

Logical close revokes the session, cancels contexts and unblocks send/emit
without joining arbitrary author code. A callback reserved before close may
finish later; the host rejects its revoked opaque handle and keeps the
borrowed buffer valid until return. Normal `component.stop` first offers
cooperative startup/disposer cleanup. The existing executable runtime remains
available when a separate failure domain and forceful process retirement are
useful.

## Composite module requirement

A module is the installation/version/configuration/lifecycle unit, not a
synonym for a single runtime. The user explicitly requires combinations of
in-process Go work, external Go services and a TS interface. The native
loader in this ADR is one execution primitive, not the whole module model.

That composition is implemented by [ADR 0005](0005-composite-module-contract.md).
The native primary keeps the nine existing capability paths; declared unary
actions may target private exec/Bun services. One root lease pins package,
config, selected account and operation. Window metadata and named bindings
reuse the existing pages/panels/service bridge. Adjacent assets alone do not
register a UI, and services are not separate installations.

## Implementation and verification

| Source | Responsibility |
| --- | --- |
| [`native.go`](../../pkg/extension/native/native.go), [`exports.go`](../../pkg/extension/native/exports.go) | `native.Register`, SDK server sessions, exports and ownership-safe byte spans |
| [`native_cgo.go`](../../internal/extension/native_cgo.go), [`native.go`](../../internal/extension/native.go) | Platform loader, frame bridge, callback handles and bounded transport |
| [`module_transport.go`](../../internal/extension/module_transport.go), [`manager.go`](../../internal/extension/manager.go) | Common native/process transport and root leases |
| [`native_unsupported.go`](../../internal/extension/native_unsupported.go) | Clear refusal on unsupported/no-cgo hosts |
| [`echo-native`](../../examples/extensions/echo-native/README.md), [`hybrid-go`](../../examples/extensions/hybrid-go/README.md) | Real author sources and build scripts; no author C glue |

`TestNativeSharedLibraryLifecycle` builds and loads a real C-shared library
through Manager, checks host PID and exercises callbacks, ACK/cancel and
replacement. An actual SDK CLI native tool returned PID 76438 from CLI host
PID 76438 with Bun unavailable. `TestCompositeHybridBundle` further checks a
native primary in the host and an external Go helper, including old lazy
v1/config1 execution after v2/config2 publication. These go beyond the earlier
export-in-dependency probe; that probe alone was never Manager proof.

Supported loader source branches are Darwin, Linux, FreeBSD and Windows,
with cgo enabled. Current runtime evidence is macOS. Remote Linux native
execution is being prepared with no result yet; Windows native execution
has not been performed. No-cgo tests/vet and cross-target checks are scoped
build/refusal evidence, not runtime evidence for those systems.

Native and exec packages use the same full-package catalog installation,
hash checks, pins and updates. Private GitHub release authentication is
implemented by the installer using developer credentials; it does not become
module/provider configuration. See
[installation](../subsystems/executable-extensions.md#installation) for
credential resolution and update semantics.

No benchmark or CLIProxyAPI hook-parity claim follows from using a C ABI.
The [runtime comparison](../subsystems/extension-runtime-comparison.md) lists
the costs, failure boundaries and missing hooks.
