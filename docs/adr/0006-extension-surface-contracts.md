# ADR 0006: Domain modules compose typed host contributions

Date: 2026-11-13

Status: architectural decision; gateway hooks and private tools are the API
1.2 implementation increment. The remaining adapters in the
[coverage reference](../subsystems/extension-surface-coverage.md) are not
implemented by this decision.

## Problem

A provider runtime is not the whole extension system. A memory extension
needs tools, recall, completion observation, persistence and possibly a
window or service. An agent extension may need configuration, launch,
Library resources and session discovery. A session converter alone does
not register a reader, and provider usage reporting is not a global
gateway observer.

Our SDK must account for those existing owners without moving every
subsystem into the plugin manager. A generic action can carry a call but
does not register a router, limiter, embedder, Library target or MCP server.
Native Go loading changes execution location, not those semantics.

## Decision

Treat a module as a composition of negotiated contracts and contributions.
Keep runtime selection, components, compatibility, configuration,
installation/update and generation ownership common. Give every host point
typed input/result contracts and a narrow adapter into its current owner.
Use a domain label such as memory for documentation and presentation, not
as a substitute for declaring its actual capabilities.

Separate these layers:

1. **Runtime:** native Go, executable Go or Bun; optional private services
   and TS window parts.
2. **Negotiated contract:** public descriptors, method versions, data
   ownership, limits, effects and failure behavior.
3. **Host adapter:** discovery, admission, normal dispatch, revocation and
   disposal in the subsystem that owns the operation.
4. **Domain implementation:** our actual provider, memory, agent or
   converter, with vendor/data migration evidence.

A package may provide several contributions. No single “plugin type”
selects one exclusive runtime or grants unrelated access.

## Memory increment

An independent memory module can compose:

- local-owner `tool-provider` tools for explicit search/save/forget;
- `gateway-hooks` for automatic recall and completed-exchange capture;
- private durable state or a declared service holding its own database;
- configuration and optional TS pages/panels.

This does not need a second host-owned memory store. Integration with
Magpie's existing Memory CRUD, digest, semantic backend or distillation is a
separate owner API and adapter. An SDK module's private state is not
silently merged into `memory.json`.

API 1.2 implements the hook and tool-exposure boundaries. Request recall
is read-only through host callbacks, including delegated helpers. Failed
enrichment does not persist a host state write. Capture/sign-in writes use
the existing operation journal and have no automatic replay. Leases bind
recall and capture to one package/config generation.

Private tool names, descriptions and execution use host-derived ownership.
SDK browser-origin requests require explicit hook opt-in; the legacy
origin contract remains unchanged. Agent/session/project metadata is not
an authenticated user identity.

## Coverage beyond memory

The source-based coverage reference is the inventory, including distinctions
within provider operations and model metadata. It separates:

- provider execution/auth/models/usage from check-ins, search, standalone
  media/embedding/rerank operations and dedicated decision metadata;
- agent configuration from launch/aliases/WSL, Library targets and session
  discovery/read/resume;
- MCP tools from full MCP server resources/prompts/notifications;
- request/completion hooks from stream/account/provider middleware,
  global observation, routing, budget reservation/release and masking;
- UI parts/config from generated settings, cross-module services and
  application-wide background activation;
- runtime modules from imported skills, instructions, bundles and other
  Library data sources.

Every new adapter must retain its owner's ordering, authority, error and
shutdown semantics. In particular, a budget reservation's release
obligation cannot be replaced by a fire-and-forget event, masking must
retain its reversible state, and video/task polling must retain account
affinity.

## Common registration direction

Future host contributions should share declaration and lifecycle machinery:
an ID, host-known point, contract version, activation rule and declared
handler/action. This is a design boundary, not an existing
`magpie-plugin.json` field. Unknown points or incompatible versions must
fail explicitly.

The point registry must own typed validation, scope/effect policy,
registration lifetime and its attach/detach operation. Registration must
produce an owned revocable handle; uninstallation must withdraw new
discovery/admission before disposing the implementation, while existing
leases retain their generation within the drain bound.

Do not implement this as unrestricted host RPC, arbitrary Go function
exports, automatic imports of private packages, or one new capability per
product feature. The current primary codec/provider contracts and their
streaming leases remain intact. A common envelope does not erase the
different contracts of routing, budgets, memory, middleware or resources.

## Migration and upstream boundaries

Add adapters beside the subsystem owner with the smallest practical
registration/dispatch seam. Keep vendor behavior and module packaging in
separate directories/repositories. Preserve legacy JS/TS loading when no
SDK descriptor exists; an explicit invalid descriptor never falls back.

## SDK source distribution

The public Go SDK packages use the canonical module
`github.com/yetone/magpie`. Import `pkg/extension`, `pkg/extension/native`
and `pkg/protocolext` under that module path. When an upstream revision
contains the required SDK contracts, depend directly on its release tag or
pin its revision.

Before those contracts are published upstream, a consumer using a fork can
select a published fork revision with a Go module `replace` and retain the
canonical import paths. Local development can point the replacement at a
checkout containing the SDK. The public packages build independently of the
GUI and private host packages. Compiled plugins have their own distribution
and host compatibility checks.

An independent SDK module/repository can follow when a separate release
cycle is useful. Such extraction must include the public codec contracts
and native C header, update import identity consistently, preserve host
compatibility checks and test an external consumer. It must not merely move
files while leaving an accidental dependency on the entire host.

Companion modules can advance independently against the contracts they use.
Adapter coverage and remaining boundaries are tracked in the
[extension surface reference](../subsystems/extension-surface-coverage.md).

## Acceptance rule

Record contract coverage, actual ordinary-host adapter coverage and domain
migration evidence separately. For every claimed adapter, prove discovery
and execution, off/update/config replacement, failure/cancellation, scope
and side effects through that ordinary host path.

Use the declared Go toolchain, isolated configuration and offline fixtures.
Historical checkpoints, feature-off cross-builds, unexecuted declarations
and skipped live tests keep their original limits. Do not call the SDK
complete while the source inventory still identifies required adapters as
remaining.
