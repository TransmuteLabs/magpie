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

A separate public SDK repository is optional. For the current increment,
retain the public SDK packages in the public Magpie fork. Plugin authors can
depend on a tagged or pinned revision of that module; installing a compiled
plugin remains a separate distribution path.

The current canonical Go module is `github.com/yetone/magpie`, retained for
upstream compatibility. Authors importing `pkg/extension`,
`pkg/extension/native` and `pkg/protocolext` must therefore select our fork
explicitly with a consumer-side Go module replacement. Using the fork's
GitHub owner as an import prefix without changing its module declaration
does not select the intended canonical module.

Pin a published fork revision instead of a moving branch. Local development
can replace the module with an adjacent checkout; this is not proof that an
unpublished local commit can be downloaded from GitHub. Importing public
SDK packages does not require compiling the app's GUI or importing its
private packages. The SDK architecture tests keep that source boundary
explicit.

An independent SDK module/repository can follow when a separate release
cycle is useful. Such extraction must include the public codec contracts
and native C header, update import identity consistently, preserve host
compatibility checks and test an external consumer. It must not merely move
files while leaving an accidental dependency on the entire host.

The [plugin-first delivery plan](../plans/plugin-first-delivery.md) schedules
concrete module slices in parallel. The following are remaining SDK areas,
not sequential prerequisites for starting each plugin:

1. Provider operation dispatch and model metadata, with binary/stream
   bounds, original method/path/content type and task/account affinity.
2. Library targets/resources and session source/read/resume ownership.
3. Middleware, observation, routing, budget, masking and search adapters.
4. Full MCP contributions, generated settings, background activation and
   remaining launch/browser-author facades.

Each boundary needs its own tests before migrating our OpenCode, ZCode,
OMP-session and Devin implementations. Their archive remains a donor,
not an SDK dependency or a reason to restore vendor switches in core.

## Acceptance rule

Record contract coverage, actual ordinary-host adapter coverage and domain
migration evidence separately. For every claimed adapter, prove discovery
and execution, off/update/config replacement, failure/cancellation, scope
and side effects through that ordinary host path.

Run new verification on `usbox`; `usbox-adm` is administration only. Use
the declared Go toolchain, isolated configuration and offline fixtures.
Historical checkpoints, feature-off cross-builds, unexecuted declarations
and skipped live tests keep their original limits. Do not call the SDK
complete while the source inventory still identifies required adapters as
remaining.
