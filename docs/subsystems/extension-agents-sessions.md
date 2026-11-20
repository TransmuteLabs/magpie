# Executable extension agents and sessions

This reference describes the implemented host paths for native/exec/Bun SDK agent
wiring and session conversion. The host owns discovery, scope, approved paths,
file preconditions, execution and recovery. The extension owns the agent's
config schema, inspection, format-aware restoration and session serialization.
An executable extension is a trusted local program, not a filesystem sandbox.

`magpie sessions sources --json` reports SDK source preparation, errors and
native bindings through the same owner as `GET /api/sessions/sources`.
Explicit native takeover uses `magpie sessions sources bind <module> <source>
--expected <package-revision>`; release uses `sources unbind <agent> --expected
<binding-identity>`. Inspection returns identities without filesystem roots.
Bind and release retain the owner's revision and identity checks, including
release after the module is disabled. Listing and help never grant read consent.

## Sources of truth

| Responsibility | Source and symbols |
| --- | --- |
| Public descriptors and RPC payloads | [`pkg/extension/contract.go`](../../pkg/extension/contract.go), `AgentDescriptor`, `AgentField`, `SessionDescriptor`; [`types.go`](../../pkg/extension/types.go), `AgentInspectRequest`, `AgentInspectResult`, `AgentPlanRequest`, `AgentPlan`, `SessionRequest`, `SessionResult` |
| Descriptor validation and required methods | [`manifest.go`](../../pkg/extension/manifest.go), `Manifest.Validate`, `RequiredMethods`; [`wire.go`](../../pkg/extension/wire.go), `MethodCapability` |
| Ordinary agent discovery | [`agents.go`](../../internal/agent/agents.go), `All`, `local`; [`extension_actions.go`](../../internal/agent/extension_actions.go), `ExtensionAgents`, `extensionAgents`, `extensionAgent` |
| Agent lifecycle and preview | [`applied.go`](../../internal/agent/applied.go), `ConnectHow`, `Apply`, `Pick`, `Wired`, `Disconnect`; [`native.go`](../../internal/agent/native.go), `NativeConnection`, `DisconnectPlan.Revision`; [`preview.go`](../../internal/agent/preview.go), `DisconnectPreview` |
| Session routes and native source reads | [`carry.go`](../../internal/sessions/carry.go), `carries`; [`carry_convert.go`](../../internal/sessions/carry_convert.go), `CarryOver`; [`extension_actions.go`](../../internal/sessions/extension_actions.go), `SessionExtensions`, `extensionSessionRoutes`, `carryExtension`, `ConvertExtension` |
| Shared Pi/OMP native ownership | [`extension_omp_owner.go`](../../internal/sessions/extension_omp_owner.go), `extensionOMPFileOwners`, `extensionOMPSharedFiles`; small hooks in `piFilesIn` and `ompFilesUnder` |
| Native target binding and checked reuse | [`extension_target_store.go`](../../internal/sessions/extension_target_store.go), `resolveSessionTargetStore`, `sessionTargetStore.check`; [`extension_reuse.go`](../../internal/sessions/extension_reuse.go), `acquireSessionMigration`, `sessionMigrationReceiptValid` |
| Selected-store journal/blob planning | [`extension_source_inputs.go`](../../internal/sessions/extension_source_inputs.go), `sessionSourceRuntime`, `collectSessionSourceInputs`, `readSessionInputs`, `sessionInputSnapshot.check` |
| File plan preparation and execution | [`actions.go`](../../internal/extension/actions.go), `Actions.AgentInspect`, `AgentPlan`, `SessionConvert`, `Execute`, `ActionPolicy`, `ActionPlan` |
| Module lifecycle and scoped state | [`manager.go`](../../internal/extension/manager.go), `Default`, `Manifest`, `Invoke`, `Store`; [`store.go`](../../internal/extension/store.go), `Store.GetAccount` |

The gateway resolves SDK client IDs, declared aliases and User-Agent prefixes
from the same enabled agent discovery used by ordinary agent commands.
[`extension_clients.go`](../../internal/agent/extension_clients.go) caches that
projection against the catalog file identity. Install, replacement, disable
and removal affect the next request after the catalog changes. Saved
disconnection records remain available to agent commands for recovery.

## API 1.6 JSONL session sources

The separate API 1.8 rooted storage authoring and adapter seam is described in
[Rooted executable storage sources](extension-storage-sources.md). Its
coordinated capability admission and ordinary owner acceptance remain
separate from the accepted JSONL reader contract.

`session-source` is independent of `session-migration`. A converter does not
register a reader. `sessionSources` descriptors declare a codec, agent, exact
store slots and one finite JSONL layout. The host extracts the selected ID;
`session.source.read` must confirm it in a typed summary or transcript.
The public DTOs and validation live in
[`session_source.go`](../../pkg/extension/session_source.go).

Ordinary Claude Code and Codex transcript, search and call-content reads use
the linked [Claude](../../internal/extension/builtin/claude/transcript.go) and
[Codex](../../internal/extension/builtin/codex/transcript.go) record decoders.
They emit the same `SessionSourcePart` type as an installed source.
The host selects the history owner, opens the file, limits the read and checks
consent before returning content. The Codex decoder observes turn metadata
before applying the host's candidate-line filter, so a matched response keeps
the model named by its turn. Call previews use native byte offsets. If the
native owner changes, the host drops the preview.

Pi, OMP, Raft and Cindy use the linked
[`SessionTreeCodec`](../../pkg/extension/session_tree.go) for transcript and
said-index records. The [Pi family module](../../internal/extension/builtin/pisession/tree.go)
projects node identity and decodes content. The host keeps branch selection,
seek offsets and the read permission checks. Compressed input keeps decoded
records until the active branch is known and renders only that branch's
parts.

[`extension_source_registry.go`](../../internal/sessions/extension_source_registry.go)
resolves current native stores or frozen HOME-relative declarations.
Native readers stay default until an explicit checked host binding delegates
their ownership. Disable/removal/revocation hides delegated data; it does not
silently fall through to another parser. Unknown SDK formats never use Claude's
default decoder.

Native trash and restore hold the same file locks as source bindings and
storage consent while moving files. They check current ownership under those
locks. If ownership has transferred or cannot be determined, the original
session or its trash entry stays in place. Gateway-only conversation trash
continues to use the gateway store.

[`extension_source_adapter.go`](../../internal/sessions/extension_source_adapter.go)
prepares summaries outside the session cache mutex, then exposes immutable
records through ordinary discovery, `List`, `Find`, management and stats.
`allFilesWith` and `parserFor` do not call workers. SDK summaries are not written
into the persistent native path cache. Failed reads under unchanged authority
retain detached last-good metadata as `source_status: stale`; revoked authority
hides it. Source preparation errors are exposed at `/api/sessions/sources`.
Persisted grant checks also run while another decoder is preparing. After
locked listing/management/statistics reads, publication rechecks grants,
consent, epoch and the immutable view outside the cache mutex. A changed view
rebuilds the result, including cached unchanged replies; an update from a
separate CLI cannot retain revoked rows through the old in-process epoch.

[`history_publication.go`](../../internal/sessions/history_publication.go)
captures effective read consent from checked settings bytes, durable native
delegations, the view and its epoch on both sides of publication. A warm
settings memo, unchanged listing response or absent SDK catalog cannot stand
in for that check. Native enumeration shares one authority snapshot across its
registered readers, including WSL groups. A checked, active JSONL binding
selects the history reader while permitted native call accounting still
supplies measured requests. Accounting checks the exact package and config
generation without starting the decoder. Rooted storage claims exclude native
accounting too, since read-only SQLite can create sidecars. An unresolved
binding or unreadable claim cannot grant another reader.
Direct `DesktopTitles` and `PIDesktopTitles` use the same publication snapshot
without preparing workers or taking the session-cache mutex, so gateway title
lookups and session assembly have the same ownership boundary.

The checked input reader is shared with conversion. Optional `session.inputs.v1`
planning and source decoding use the same package/config lease and approved
store. Journal/blob snapshots include physical identities, permissions and
content digests. Bodies are released after summary decoding and reacquired for
transcripts. Both the 16 MiB/128-document binary budget and the 24 MiB encoded
frame budget apply.

API 1.9 sources may opt into `descendants.kind: journal-stem-jsonl`.
[`extension_source_descendants.go`](../../internal/sessions/extension_source_descendants.go)
enumerates JSONL only below the selected journal filename without its `.jsonl`
suffix. A descriptor may exclude at most 16 directory basenames; it cannot
name another root or grant module-authored paths. Traversal is bounded to
10,000 entries and 16 directory levels, with the same aggregate file/byte
budgets as planned inputs. Symlinks fail closed. The snapshot retains checked
directory membership and identity, including an absent tree, so new/removed
children invalidate an otherwise unchanged journal.

The host marks captured child paths in `SessionSourceReadRequest.Descendants`.
A summary must return exactly one `{path, sessionId}` in `Subs` for each
admitted child, with unique identities distinct from the main session.
Unadmitted attachments grant no child identity. Colliding main/descendant
identities suppress ambiguous records. The adapter projects validated IDs
through ordinary `Session.Subs`, `GatewayKeys`, native-ID and statistics
deduplication. Children contribute usage and IDs only; the module keeps its
main transcript separate. Storage sources do not inherit this JSONL authority.
The older API 1.6 descriptor and no-descendant behavior remain supported.

Read consent remains `Reads(agent)` and `OwnOff`. Project/folder filters grant
no extra authority. Calls cannot borrow account credentials; source methods are
unary and read-only. Appends/rewrites are fully decoded initially; incomplete
trailing entries are excluded. Existing pricing computes costs from normalized
nonnegative usage, and host-native recipes construct resume commands. Sources
with no supported launcher have no resume action.

The synthetic native fixture under `examples/extensions/session-source-jsonl`
tests ordinary discovery/read/search/stats/conversion and opaque attachments.
It is not evidence of vendor format support. SQLite, compressed journals,
remote/backend history, call/trace registration and deletion are outside this
first cut. The decision and lifecycle bounds are in
[`0019-jsonl-session-sources.md`](../adr/0019-jsonl-session-sources.md).
Historical preview verification and independent review are recorded in
[`0020-session-source-preview-validation.md`](../adr/0020-session-source-preview-validation.md).
The merged SDK 1.6 host and its current acceptance boundary are recorded in
[`0021-consolidated-sdk16-validation.md`](../adr/0021-consolidated-sdk16-validation.md).

## API 1.10 source-backed agent resume

JSONL and rooted storage sources may declare `resume: "agent"` with an
[`agentResume` recipe](../../pkg/extension/session_resume.go). The recipe names
a launch-capable agent from the same package and at most 32 literal argv
elements, with exactly one whole `{sessionId}` argument. The declared agent
must own the source's agent ID or explicitly profile that native target.
Source data does not supply a command, executable path or environment.
Existing `none` and `native` resume declarations keep their behavior.

The [linked resume inventory](../../internal/extension/builtin/sessionresume/recipes.go)
uses the same `SessionResumeRecipe` type for native commands. The host uses
the recipe's agent to check executable availability for listed sessions.
Pi also uses its recipe for SDK sources that declare native Pi resume.
OMP resolves the profile from the selected store for resume and from the
chosen target for CarryOver. The host validates the profile before asking
the module for arguments. The native command renderer rejects flag-like or
overlong session IDs and admits only shell-safe atoms. The host retains the
working-directory and WSL wrappers, Cursor's directory requirement and
agy's local launch overlay.
Opening a Codex session checks its saved provider at launch time. When
the provider still exists and differs from the current choice, the Codex
module supplies the structured override recipe. The host keeps ownership
of the checked move when the saved provider no longer exists.

Listings render a quoted command through the current Magpie executable:
`magpie sessions resume <source-agent> <session-id> --expected <identity>`.
[`extension_source_launch.go`](../../internal/sessions/extension_source_launch.go)
binds the displayed identity to the current package/configuration and physical
root grant, selected journal snapshot and locator or rooted storage
locator/family, recipe and working directory. The command reacquires that
selection under current source consent. A stale token, replaced root,
disabled/removed package or changed configuration fails closed.

[`PrepareExtensionResume`](../../internal/agent/extension_resume.go) uses the
ordinary `Starter` and the recipe's exact agent ID. It requires that package's
connected SDK binding, retaining its configuration files, model, launch
arguments and environment. It checks current package/configuration authority,
binding revision, applied file captures, shell blocks, gateway/key and the
captured launch. A disconnected profile, a recipe argument that disables the
profile, an unavailable executable or a nonempty relative working directory is
refused. There is no plain or unrelated launcher fallback.

[`session_resume_cli.go`](../../internal/cli/session_resume_cli.go) stops SDK
helpers before exec. The independent ten-second teardown timeout only bounds
helper cleanup; authority checks continue to use the caller's context.
[`startRunChecked`](../../internal/cli/start_cli.go) shares ordinary `startRun`
PATH lookup and argv/environment construction, then rechecks source and agent
authority immediately before exec, after selecting the working directory.
Those final checks read current captures and owners without acquiring workers
or calling a background-context manifest accessor. If exec returns, the CLI
restores its previous working directory.

The regressions are in
[`extension_source_launch_test.go`](../../internal/sessions/extension_source_launch_test.go),
[`extension_resume_test.go`](../../internal/agent/extension_resume_test.go),
[`session_resume_cli_test.go`](../../internal/cli/session_resume_cli_test.go)
and [`start_checked_test.go`](../../internal/cli/start_checked_test.go).
The native CLI tests require an explicitly built synthetic SDK package and
fixture executable through `MAGPIE_RESUME_TEST_DIST` and
`MAGPIE_RESUME_TEST_AGENT`. They cover both source descriptor families,
config/env/argv/cwd preservation, helper disposal and revocation/cancellation
during final PATH lookup. Synthetic fixtures do not establish compatibility
with an installed vendor client.

## Agent discovery and ordinary call graph

An enabled module declaring `agent-wiring` may describe an agent as:

```json
{
  "id": "external-editor",
  "name": "External editor",
  "bin": "external-editor",
  "configFiles": [".external-editor/settings.json"],
  "fields": [{"key": "model", "label": "model"}]
}
```

`configFiles` contains exact HOME-relative paths, without environment or tilde
expansion, absolute paths, traversal or globs. `bin` is an optional executable
basename. Declared fields become ordinary model picker fields: their options
contain default, the current value, and gateway model options. The module
translates the host's `magpie/<reference>` values into its own schema.

`All()` builds the native/built-in and existing JS agent list, then adds
`extensionAgents` and applies explicit SDK profile ownership. `local()` does
the same without WSL agents.
`ExtensionAgents` reads the enabled installed manifests without calling a
worker. An ID/name-only descriptor has the explicit action API. A descriptor
with config files, or a declared SDK 1.5 launch, registers an ordinary agent.
Rich descriptors negotiate inspection.

`extensionAgents` reserves existing IDs and aliases, other known clients, and
`magpie`, then rejects collisions. Duplicate SDK agent IDs also fail discovery.
Ordinary descriptors do not replace a native or JS agent. SDK 1.5 profiles
may explicitly adopt an existing local target under the ownership rules
below. `PluginErrors()` reports discovery and ownership conflicts. A disabled
or removed module is absent unless its durable binding still needs recovery.

### SDK 1.5 launch, profile and Library metadata

[`agent_metadata.go`](../../pkg/extension/agent_metadata.go) adds `aliases`,
`ua`, structured `launch` (`env`, flag/value `args`, model flag and `plain`
commands), `profile.target`, and typed Library locations. All require API
1.5; earlier agent declarations keep their existing adapter and behavior.
A model flag requires a declared model field. A launch with no config files
uses a host binding rather than a manufactured editable file.
SDK launch Env and Args resolve `{config}` to the first declared configuration
file, or the existing base agent's current path for a profile. The structural
Go/JS validators require a declared file or profile when the placeholder is
used; a profile whose base has no configuration path is refused before
adoption or launch. SDK Env expands gateway/key/config placeholders in one
pass, preserving spaces and literal placeholder-looking text inside the
resolved path. Discovery, source-backed resume and owned-process launch share
controller construction and local profile-base resolution, including legacy
plugin targets. A process inherits the same config path as ordinary Start;
its foreground recipe, binding/lease checks and spawn lifetime remain separate.
Legacy agentplug Env keeps its existing placeholder behavior.

Ordinary `magpie <agent> run` rechecks the selected SDK launch after
executable lookup, using the same launch admission as session resume. A
changed binding, package or active configuration stops the launch. The
final check reads host state without starting the module worker.

`magpie claude run` and `claude-magpie` resolve the existing Claude target's
SDK binding before preparing a launch. An installed owner uses the same
binding and launch checks as resume. An unavailable binding or a bound
profile without a launch recipe stops the launch. With a free target, the
linked Claude module supplies the `AgentLaunch` overlay. The host resolves
the settings path using the shared launch compiler and fences the overlay
write and the model restoration against changes in target ownership.

Built-in Antigravity CLI supplies its `AgentLaunch` overlay for `magpie agy
run`, `env` and the command shown on the agent row. The recipe declares the
settings flag, model flag, gateway variables and remote-control arguments.
The host decides when the overlay applies and couples it to the user's
current model. Shell profiles in a WSL distro still read its settings file
locally when they run.

Agent action settings snapshots use `sdk.ActionSettings` in Go and
`ActionSettings` in TypeScript. The request's `settings` field keeps its
existing JSON representation. Each file has a relative path, existence and
mode, raw bytes encoded as base64, and a `beforeHash`. Linked Codex profile,
global provider, saved provider/catalog/effort restoration, auxiliary
models, effort and background compaction edits pass the same typed snapshot in process and
return an `AgentPlan`. Claude effort and routing-related environment flags
use that contract on their ordinary configuration paths. Connect, model
changes and Sync also include the picker and capabilities in the checked
settings plan. Disconnect restores the route and removes the owned picker
in one plan. A fingerprint of the written picker preserves a subsequent
user edit even when its rows still say `via magpie`.
OMP uses it to update its provider catalog and set a Magpie model on a
role. Picking that model for the default role also updates `enabledModels`
in the same plan. The host resolves the profile's actual YAML paths,
checks the catalog and config snapshots, and remembers its API only after
publication. The plan preserves a configured provider API and user agent
header and can read a legacy `models.json` when creating the YAML catalog.
Catalog refresh and removal use the same checked publication.
Pi and its forks use a linked settings plan to publish the selected default
model and its `enabledModels` entry in one write. Reset checks both
`settings.json` and `models.json` before clearing the default, the explicit
Magpie scope entries and Magpie's provider. Concurrent edits to either
file survive a rejected or rolled-back reset. The thinking-level picker
uses the same settings plan without rebuilding the catalog.
Scope refresh reads the catalog and settings in one checked action.
Rename edits the explicit scope entries with the same settings contract.
The host records excluded and restored scope entries after publication.
The Pi and OMP Protocol fields pass the selected API into the checked
catalog action. The host stores the preference and catalog metadata after
the action succeeds.
StepCode Sync plans its provider catalog and selected model against the same
`models.json` and `config.toml` snapshots. Model-reference rename uses the
checked TOML publisher. Saved scope entries change after publication
succeeds, so a rejected write leaves them available for the next Sync.
OpenCode v2 refreshes its provider from the same snapshot as the primary
model selection. Sync updates provider format, catalog and discovery
configuration in one checked write. Output limit history is published
after the configuration accepts the plan. The discovery sidecar is prepared
separately when a model selection needs it.
The ordinary Codex subagent model and effort fields use the auxiliary model
plan. A model change checks the saved effort against the new model and clears
an incompatible effort in the same checked `config.toml` write. The gateway's
subagent model override still belongs to Magpie settings. Saving that override
and clearing Codex's native override are separate file operations.
Codex compaction combines feature, threshold and provider capability changes
in one config write. The host records compaction ownership after the write
succeeds. Codex catalog refresh uses the same catalog merge as profile
configuration. It retains user fields from the host's snapshot, skips an
unchanged catalog and leaves a concurrent edit for the next sync.
Changing the Codex memories model updates extraction and consolidation
together. Disconnect removes the routed memory and subagent picks in the
same checked config write that restores the top-level route settings and
managed provider URLs. The plan preserves independent model picks and URLs
edited since Magpie set them. The host releases saved URL values after the
plan succeeds.
The Claude effort plan combines environment, per-model and top-level changes
into one settings-file edit. On Connect, model picks and Sync repairs, the
route settings plan publishes the gateway address and key, main model, tiers,
subagent model and managed output limit together. It removes the old
`ANTHROPIC_MODEL` override in the same write. The host authorizes caller keys
against the settings snapshot and records field fingerprints after publication.
The plan also removes enabled cloud switches that would bypass the gateway
and tier labels that no longer describe the selected model. Sync applies
the same rules to the current file, and restoration metadata comes from
the snapshot used for the checked write.
Disconnect compares those fingerprints against its own snapshot. It restores
the original route and removes its effort override in one plan. It preserves
hand-edited values and deletions. When the current endpoint is confirmed as
Magpie, it also clears tiers still naming a Magpie model, including tiers
selected by hand against that endpoint. The same plan applies a native or default
model when the user picks one. Originals stay in the stash if publication
conflicts. Settings from earlier versions use the recognized gateway, model
and key when no fingerprint record exists. The route flags plan publishes
Tool Search, Auto Mode Review and Desktop Explore together.
[`ApplyLinkedAgentPlan`](../../internal/extension/linked_actions.go) checks
the declared destinations, input hashes and current files before publishing.
Linked file errors name the logical input. The host reports the matching
local path and preserves the original cause.
The host follows native config links, rejects plans whose files alias one
another, and keeps the existing malformed-catalog backup paths. The host
rechecks the declared inputs at each publication, including retries after
a Windows sharing failure. For linked Codex Desktop publication, the
catalog is checked again before the configuration can reference it. On
failure the plan rolls back only files it published, using its own
snapshots and comparing its publication to the current file. The outer
agent operation leaves those plan inputs alone during rollback, including
user edits made before the plan started. A process that edits a file independently can still
race the interval between a final comparison and the filesystem operation.

[`extension_controller.go`](../../internal/agent/extension_controller.go)
adapts these declarations to ordinary All/Find/Starter, NativeConnection and
CLI run/env. Environment-only launches use `Starting.Active`. SDK shell
functions fail closed when Magpie is unavailable. A bound module that is
off, replaced or awaiting recovery cannot silently launch its agent on an
own account.

Named setup profiles preserve SDK field keys containing dots. New snapshots
keep ordinary `agent.field` keys when neither component contains a dot; otherwise
`@field:` followed by a JSON `[agent, field]` pair identifies the field exactly.
Legacy flat keys remain readable against declared agent fields, including dotted
agent IDs and WSL distro names. If more than one pair matches, restore refuses
before changing fields or model lists, InUse is false, and Details displays the
unresolved key rather than assigning it to one agent. A declared but undetected
agent still participates in legacy ambiguity checks, without becoming a write
target. Only legacy keys with multiple dots require InUse to discover those
additional declarations. Conflicting explicit and
legacy aliases are also refused. Missing agents remain skipped/displayed as
before. Snapshot, restore and details use the shared owner in
[`profile/fields.go`](../../internal/profile/fields.go).

An explicit profile keeps a distinct ID and resolves an existing local
built-in/legacy target. Before adoption, the base remains unchanged. A
successful connect/set reserves the target, suppresses its ordinary row,
and resolves its aliases to the profile. A target cannot be another SDK
profile or WSL agent. ID/alias/launch-command collision refusal otherwise
remains the default.

The host supplies `binding:{target,revision,connected,values}` to inspect
and plan. A plan returns `binding:{revision,connected,values}` beside its
files. Revisions are canonical decimal uint64 strings. Scope selectors,
undeclared values, null values and stale revisions are refused. The
confirmation hash includes both binding snapshots. `Actions.Execute`
approves/applies files; the ordinary controller commits agent ownership.

The private `sdk-agent-bindings.json` ledger records pending/owned/
disconnected states, original/applied files, shell blocks and selected
package/config. Strict target and ledger locks coordinate cross-process
ownership. Held base objects, ordinary mutation entrypoints and local atomic
owners are fenced. Actions recheck selected authority after waiting for
their write lock, before every file and at completion.

An effect-free `sync` keeps the existing ownership revision. This requires an
owned, recovery-safe record with identical package/config, descriptor,
executable, selected values, applied file snapshots and shell targets/bodies,
and no declared file changes. The controller still executes the one-use checked
action and rechecks current authority under its normal locks; it simply leaves
the ledger untouched. Native account-operation receipts and admitted reads
therefore survive a catalog refresh that changes no owned state. Explicit
reconnects, field changes, package/config replacement, drift and recovery retain
their normal transition and fencing behavior. The regressions are in
[`extension_controller_sync_test.go`](../../internal/agent/extension_controller_sync_test.go).

Failed transitions clear pending ownership only after both file and shell
preimages are proven restored. Otherwise a recovery row retains the known
snapshots. Disabled/redefined modules are recovered without invoking their
code. Unknown user changes are never overwritten. A successful online action
that rebases user edits disables offline whole-file recovery; the original
module's schema-aware disconnect is then required. Different target
generations cannot be recovered through a held old object.

The copied configuration used by ordinary file-agent disconnect previews
omits only the ephemeral `sdk-agent-locks` directory link, so the child
creates private locks for its sandbox. The durable binding ledger is
copied unchanged. Legacy plugin directory links retain their normal
behavior; no ownership guard or general symlink check is bypassed.

Legacy config ownership is not automatically adopted into the new
controller. It names the exact previous module/descriptor and refuses new
ownership until that descriptor's normal disconnect releases it. Config,
stash and durable backup bytes remain intact on refusal.

Typed Library metadata reuses existing instructions/MCP/skills owners. An
active profile inherits its base's target identity by default; an inactive
profile adds no duplicate Library target. See
[`0016-sdk-agent-profiles.md`](../adr/0016-sdk-agent-profiles.md) for the
contract and acceptance cases.

OpenCode discovery (`/v1/models?format=opencode` and `opencode2`) uses the
caller's own model catalog when its identity names an owned SDK profile of
OpenCode and the installed module is enabled at the owning package and
configuration revisions.
On the ordinary discovery URL, the profile ID comes from the agent-scoped
token or the User-Agent product name, before usage's prefix matching.
Both formats use that same profile for model visibility, ordering, limits and
the gateway key's model restrictions. Other callers receive the built-in
OpenCode catalog through the same key checks.

`extensionAgent` constructs `Agent`, its fields and `NativeConnection`:

```text
All / local
  -> extensionAgents -> extensionAgent -> Agent
Find -> All -> FindIn
ConnectHow -> Native.Connect -> agent.plan(action=connect) -> Actions.Execute
Pick -> Spell -> Apply -> Native.Apply -> agent.plan(action=set) -> Execute
Wired -> Native.Read -> agent.inspect + host ownership
Disconnect -> Native.Disconnect -> agent.plan(action=disconnect)
           -> Native.Execute -> Actions.Execute
SyncCatalog -> All -> owned, wired ListOnly agent -> Sync
            -> agent.plan(action=sync) -> Actions.Execute
```

Detection checks the declared files or executable. Discovery itself does not
inspect config through RPC. Inspection, plan preparation and execution each
use bounded 15-second contexts in the ordinary adapter.

## Inspection, settings and plans

`agent.inspect` accepts `{id, options, settings}` and returns
`{connected, values: {field: value}, detail?}`. `Actions.AgentInspect` supplies
its own snapshots even if an explicit caller supplied `settings`.
The ordinary adapter also uses host snapshots for `agent.plan`.
An explicit `AgentPlan` caller may supply settings, but that does not change
the host's captured file preconditions.

The settings envelope is:

```json
{
  "files": [{
    "path": ".external-editor/settings.json",
    "body": "e30=",
    "beforeHash": "<sha256>",
    "mode": 384,
    "exists": true
  }]
}
```

Bodies are base64 bytes on the wire. Hashes cover file contents. A missing
file has `exists: false` and `beforeHash: "missing"`; an existing empty file
has its own SHA-256 hash. Permissions and existence are separate preconditions.

Ordinary `options` contains `gateway` (the gateway base URL), `key` (the
agent-scoped credential), `models` (ordinary model options), and, for a set,
`field` and `value`. `previous` contains the snapshots recorded before host
adoption. The extension uses these originals for schema-aware disconnect,
preserving unrelated user changes.

API 1.9 agent model options additionally carry `maxOutput`, `efforts` and
`defaultEffort`. Both individual models and routing groups use effective
catalog entries: `Entry.PublishedOutput()` applies output overrides and
context clamping; effective effort overrides and reasonless settings apply
before projection. An unknown effort list is `null`, an explicitly empty
list is `[]`, and a default is present only when it belongs to the effective
list. Caller copies do not share mutable effort slices with the picker cache.
[`AgentModelMetadata`](../../pkg/extension/agent_model_metadata.go) and the
[JS helper](../../sdk/js/agent-model-metadata.js) validate these bounded facts
under API 1.9 `agent-wiring`. Modules negotiating an earlier minor receive
none of these new option fields.

API 1.9 also supplies a separate `options.decisionModels` array to both
ordinary agent adapters, for inspection and planning. Its
[`AgentDecisionModel`](../../pkg/extension/agent_decision_models.go) DTO has
`id`, `name`, either `provider` or `group: true`, and relative decision
`endpoints`. It contains no credentials or account records. The projection
reuses [`pluggedDecisionModels`](../../internal/agent/plugged_deciders.go),
the upstream agent-plugin connect owner backed by `OfferedDeciders`, including
its labels, provider picks and visible enabled decision groups. No decision
models are added to conversational `options.models`. No available decision
models means `[]`, never `null`; API 1.8 and earlier omit the new key.
The [JS DTO and validator](../../sdk/js/agent-decision-models.js) use the same
API 1.9 `agent-wiring` floor. This supplies discovery facts, not an invented
vendor agent configuration schema.

Controlled inspection receives the same gateway, key and model options as
planning. The existing private binding ledger retains the last requested
catalog as `modelOptions`; the next options envelope supplies it as
`priorModels` only for API 1.9. This is previous catalog evidence, while
`previous` remains the original settings preimage. A module can distinguish
its generated limits from preserved user edits across multiple syncs without
adopting those edits as generated values. A zero-effect sync retains its binding
revision and admitted native reads when checked configuration, launch state and
model authority remain unchanged. Display labels, notes, icons, picker sections
and catalog ordering alone do not republish that authority. Model references,
capabilities, effective context/output/efforts and unknown future option fields
still fence the old binding. The unchanged path consumes its checked plan and
rechecks files, shell state and runtime; a module's declared file changes still
use the ordinary durable transition.

`agent.plan` accepts only `connect`, `set`, `sync` and `disconnect` actions.
Its result is `AgentPlan{files: FileChange[]}`. Each change names an approved
relative path and its body, optional mode, removal flag and optional
`beforeHash`. The host fills an omitted hash from its own snapshot and rejects
a supplied mismatch. A removal cannot also supply a body or mode.

Preparation checks the enabled manifest, declared capability and descriptor
ownership. Explicit helpers preserve the caller's parent scope and reject
another plugin's scope. An account-bearing scope requires a provider and
current nonzero generation; the store generation is checked before dispatch
and while applying effects. Ordinary calls use agent-operation scopes and
let the action service fill the plugin identity.

## Approved files and permissions

The ordinary agent root is HOME; the exact file allowlist comes from its
installed descriptor. Explicit action callers supply their own trusted
absolute root and allowlist. The host canonicalizes the root and records its
identity. `os.Root` confines its operations; target components must not be
symlinks or non-directory parents. Existing targets must be bounded regular
files. Reserved `.magpie-extension*` components are unavailable as action
targets.

These are permissions for host-mediated actions. Registration reserves
agent IDs, not exclusive ownership of physical config paths across modules.
The host does not infer a vendor schema or prove that a declared path belongs
to the named agent.

| Effect | Implemented mode policy |
| --- | --- |
| New target with no requested mode | `0600` |
| Existing target with no requested mode | Preserve captured permission bits |
| Explicit target mode | Accept only ordinary `0000`–`0777` permission bits; zero means the default/preserve rule |
| New parent directories | `0700` |
| Atomic write temporary file | Created exclusively as `0600`, then changed to the selected mode |
| Private backup directory and files | New directories `0700`; originals, manifest and status files `0600` |

Windows precondition comparisons use the writable/read-only bit rather than
assuming POSIX mode equivalence. Directory fsync is performed on other
platforms; the Windows path omits directory fsync.

Actions allow at most 128 files and 16 MiB aggregate snapshot/output/input
bytes per applicable budget. RPC requests must also fit below the SDK frame
budget. The ordinary adapter puts backups under
`appdir.Config()/extensions/action-backups/<plugin>/<descriptor>/action-*`.
Writing a nonempty plan requires an explicit backup directory.

## Preview, execution and rollback

`AgentPlan` and `SessionConvert` prepare a host-owned `ActionPlan` without
writing target files or backup directories. `Preview()` and `Snapshots()`
return cloned data. The revision binds the plugin, parent scope, manifest,
method, descriptor, intent, root, backup destination, create-only prefix,
captured originals and proposed file effects. Execution requires that exact
revision and refuses a consumed plan.

Execution rechecks the enabled manifest, generation, root identity, original
contents, permissions and existence. A persistent `.magpie-extension.lock`
serializes cooperating writers at that root. Originals and recovery metadata
are durably backed up with status `prepared` before the batch writes targets.
Each target is checked again before its write; file replacement uses an
exclusive temporary file, file sync, rename and platform-appropriate directory
sync. Success records backup status `applied`.

Failure or cancellation rolls back applied effects in reverse order and
removes newly created empty directories. Rollback restores a file only when
it still matches the action's expected output; an independently changed file
is preserved and recovery reports the retained originals. A completed
rollback records `rolled_back`. This is a checked batch of per-file
replacements, not an atomic filesystem-wide transaction or protection against
every non-cooperating write between a check and a rename.

`DisconnectPreview` uses `Native.Disconnect` for SDK agents. The adapter maps
the action's file diff into `DisconnectPlan` and binds its revision to the
complete host action revision, including manifest, modes and existence.
`Native.Execute` accepts only the corresponding pending plan and still uses
Actions' precondition checks. Preview itself does not apply the disconnect.
Format-aware removal/restoration remains the module's responsibility.

## Manual wiring and ownership recovery

The adapter regards an agent as wired only when inspection reports a
connection and its host stash ownership record exists. A manually configured
gateway connection alone does not grant background sync or disconnect
ownership. With no record, sync returns without a plan and disconnect returns
an empty plan.

The ownership record is the stash entry
`sdk.agent.<plugin>:<agent>`, containing the first adoption snapshots.
SDK IDs cannot contain `:`, so dotted module and agent IDs remain distinct
components. This unreleased SDK does not migrate the old dotted keys.
After successful connect/set/sync file application the adapter saves that
record if absent and clears the disconnected mark. After successful
disconnect application it removes ownership and records the user's
disconnected choice. Recreating an `Agent` object reads the same stash state.

The stash update is after the Actions file transaction. A stash write failure
therefore reports that files were applied and names the backup location; it
does not roll the files back. A crash in this interval has no automatic
ownership reconciliation in this adapter. Recovery backups remain available.

## Converter routing, source documents and native resume

A normal conversion route declares, for example:

```json
{
  "id": "external-converter",
  "name": "External converter",
  "from": "source-agent",
  "to": "target-agent",
  "outputDir": ".target-agent/sessions"
}
```

`from` and `to` must be distinct IDs and `outputDir` a clean HOME-relative
directory. ID/Name-only converters remain usable by explicit file workflows.
Ordinary routing selects enabled descriptors by source and target and suppresses
ambiguous SDK routes. A native history offers one choice per target. An eligible
installed converter takes that choice when available, with the linked converter
as its default. The direct Pi-to-OMP fork retains precedence for native Pi
history. Delegated JSONL history uses an installed converter for the selected
journal, including when its descriptor disables transcript presentation. An
opaque storage locator offers no raw journal conversion. Target availability
uses the existing availability check or executable lookup. `resumeCommand`
must already support that target.

```text
session assembly -> eachCarry -> offered Carry choices
CarryOver -> selectSessionHistoryOwner -> eachCarry -> selected Convert route
  linked -> convertLinkedSession -> checked publication
  installed -> carryExtensionOwned -> checked source files
            -> optional session.inputs.v1 (shared Manager lease)
            -> Actions.SessionConvert -> ActionPlan
            -> validate native ID/resume command -> ExecuteCreateOnly
  result -> CarryResult {agent, id, path, command}
```

Listing routes does not invoke the converter. Dispatch refuses a route that
has disappeared instead of falling through to an unrelated native converter.
The ordinary conversion context is bounded at 30 seconds.

The linked Claude Code and Codex modules declare their routes with the same
`SessionDescriptor`. On Carry Over the host opens the selected native
history, decompresses packed rollouts, and calls
[`SessionConverter.Convert`](../../pkg/extension/session_conversion.go)
with a borrowed reader and the target settings. The converter returns the
new native ID, a path relative to the declared store, and a writer for the
document. The host validates the result, stages the writer's output and
publishes the file through the selected target root. It rechecks the
target profile and store identity before and after linking the file,
and reuses a cached result only for the same target selection and file
identity. Linked conversion has no IPC document size limit.
Installed modules continue to use the `SessionRequest` and `SessionResult`
wire forms of `session.convert`.

Public `ConvertExtension` checks read consent and resolves the session once
by agent/ID. Both explicit conversion and ordinary Carry Over hand the
selected owner to `convertHistorySource`. The selected path and working
directory stay together even when a new lookup returns another legitimate
session with the same ID.

`CarryOver` retains the selected `sessionHistoryOwner` through availability
checks, migration waits, cached receipt reuse, source capture and publication.
Linked and installed conversions recheck that owner and current consent before
writing and before returning output. Installed input planning carries the same
admission check; Actions revalidation rolls back a revocation during apply.
An ordinary SDK conversion retains checked recovery through final output
capture as well. The installed converter's action holds its output
directories open and verifies their location and the published files at
commit. Rollback uses those same directories if one moved. Previously
published cached files are kept on revocation;
only this operation's still-matching outputs are eligible for rollback.

Native carry writes a private temporary file, checks authority, and links it
into its new name without replacing a competing file. It holds the open
destination directory through publication and rechecks that it is still at
the selected path. On late revocation it rolls back in that same directory,
even if renamed, and removes the output only if its identity, metadata and
content still match the created file. Independent
edits/replacements are preserved and the operation returns an error. A
filesystem without hard-link support fails the conversion without publishing
a result. These checks do not make filesystem effects and external consent
changes atomic against non-cooperating writers.

The helper requires consent, nonempty agent/ID/working directory, an absolute
source path, local writable provenance, and a matching parent session scope.
It reads a bounded regular file, verifies the opened identity and pathname,
and checks size/mtime across the read. The resulting snapshot retains the
selected path, physical file identity and SHA-256 content hash.
After module planning, and again immediately before ordinary execution, the
host rechecks that identity and hash. Same-size edits with restored mtime and
same-content file replacement both fail. `options.cwd` and native resume use
the selected session's working directory. API 1.3 ordinary OMP targets also
receive host-canonical `canonicalCwd`, `home` and `temp`; Codex targets receive
the selected Codex store's provider choice. The module does not choose these
roots from document content.

Without an input planner, the module receives one document containing
`filepath.Base(source.Path)`, source format and journal bytes. Packed Codex
journals are decoded under the existing bounded source budget while the
snapshot still binds their stored file. The host does not automatically
export every aggregated journal, database or session graph.

### Optional source-input planning

A module may declare the read-only ModuleRPC action `session.inputs.v1`.
The host first supplies a `SessionRequest` with the selected journal and a
path relative to its approved native store. The result is a version-1 input
plan naming relative `path` and lowercase SHA-256 pairs. This is a read
action, not permission for module-authored filesystem access or output.

The host chooses the source store, rejects traversal, symlink components,
duplicate paths and malformed hashes, then reads each requested digest
without searching another profile or default store. The plan result is
bounded to 64 KiB. At most 127 additional files fit beside the selected
journal; aggregate input stays within the existing 128-file/16-MiB action
budget and the SDK frame budget. Root ModuleRPC input uses its frame-sized
budget; scoped host callbacks keep their smaller independent limit.

With the ordinary Manager, planning and conversion share one acquired
module lease. A changed manifest fails the plan instead of substituting a
different generation between the two calls. Conversion receives the journal
and verified extra documents together. Source snapshots bind journal/blob
contents, file identities and permissions, parent-directory identities and
permissions, and the selected store identity; they are checked after
conversion and before
ordinary output application.

The source check stays local to the session adapter: Actions has no deferred
source-read hook. A public explicit caller retaining a prepared `ActionPlan`
still validates its source before a later `Execute`. Ordinary conversion
does not lock another application's source across the final check and output
application.

The converter returns documents in the requested target format and an
optional `SessionResult.id`. Ordinary conversion requires a nonempty ID and
a safe host-generated resume command before publishing files. Explicit
conversion may omit an ID. The host neither accepts a worker-supplied shell
command nor validates the target's complete vendor session schema.

Ordinary output resolves a host-owned native target before preparing its
`CreateUnder` policy. Conventional `.claude/projects` and `.codex/sessions`
prefixes select the current Claude/Codex config roots; `.omp/agent/sessions`
selects OMP's configured root, absolute agent override, active profile or
migrated XDG store. Named profiles select XDG only when that profile's
own XDG directory exists. Other descriptor prefixes keep their literal
HOME-relative behavior. No module option selects an arbitrary output root.

Binding and preview create no target directories. The closest existing
canonical ancestor becomes the confined action root. Before apply, the
adapter rechecks destination selection, physical directory identities,
permissions, absent components and freshly parsed Codex provider config.
Unreadable config, a dangling config symlink, directory replacement,
retargeted root or changed profile/provider invalidates the plan. A relative
default OMP agent override is rejected rather than published into another
store; named profiles ignore that default override as OMP does.

Returned relative document paths are rebased below the host-selected prefix
and must be new files; existing files, traversal and collisions appearing after
preview are rejected. The source is not a planned output and is not changed
by host conversion. The returned `CarryResult.path` is the first path in the
action's lexicographically sorted preview, so the adapter has no separate
primary-document designation for multi-document output.

Native resume uses the existing `resumeCommand` with the returned ID,
working directory and selected output path. It returns the command to the
caller; this adapter does not execute a shell. Targets with no existing
native resume implementation, unknown source formats and explicitly selected
file sets use manual conversion workflows. Host discovery includes current
OMP config/agent roots and named XDG profile stores. Native resume identifies
the configured profile, including through a symlinked root.

Shared Pi/OMP stores use the recovered fixed 256-byte UTF-8 v1 OMP title slot
to assign ownership before path deduplication. The probe reads that prefix
only, with regular-file and identity checks. Genuine Pi remains Pi; unknown
or unreadable shared prefixes are never claimed as OMP. Pi's flat session
directory is covered too. All four combinations of the independent Pi/OMP
read-consent gates retain those owners. Nonshared legacy OMP keeps its prior
ownership. This is native session metadata discrimination, not conversion or
an extension-selected filesystem root.

The SDK adapter keeps at most 128 in-memory checked migration receipts.
Their key binds host namespace, source selection and working directory,
converter descriptor, module package/config generation, target selection and
host options. A hit rechecks the source journal, every declared attachment,
published output identities/hashes/permissions and parent directories,
target binding, module generation and read consent. Changed output is
preserved and a fresh create-only migration is prepared; revoked authority
or invalid source fails.

Only an identical migration key waits for an in-flight conversion, and that
wait observes the caller's context. Other keys can progress independently.
The cache mutex protects maps only, never Manager calls, RPC or file I/O.
These are process-local receipts, not restart-persistent deduplication or a
transaction against another application's concurrent writes.

### Independent OMP module

`magpie-sdk-plugin-session-omp` imports only public SDK packages and requires
API 1.3/native ABI 1 with `session-migration` and `module-rpc`. Its explicit
`omp-conversation-v1` interchange remains module-owned and lossless; it is
not a new SDK-wide conversation schema.

The module now declares ordinary OMP→Claude, OMP→Codex, Claude→OMP and
Codex→OMP routes, using the host's existing native discovery/readers and safe
resume commands. All four native routes have ordinary Find/preview/
CarryOver/native-reader/resume fixtures, including attachments, packed Codex
input, configured target roots, OMP XDG/profile stores and checked reuse.
Large ordinary round trips retain more than 3 MiB of text and attachment
blocks; the explicit archive round trip separately crosses 8 MiB.
The module's existing byte budgets remain enforced, including Codex's
duplicated UI event records. Fixtures execute the native module and host,
not the vendor applications. Claude↔Codex remains upstream-owned. None of
these routes registers generic SDK session discovery, readers or resume
callbacks.

## Scoped verification record and limits

The executor's recorded November 12, 2026 integration-handoff run used a
temporary HOME/USERPROFILE and config/cache directories, with GOPATH,
GOMODCACHE and GOCACHE resolved and pinned before changing HOME.
`GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`, `GOTELEMETRY=off` and
`-mod=readonly` kept verification offline without dependency changes.

```sh
go test -mod=readonly -json -race -tags nogui -p 1 \
  ./pkg/extension ./internal/extension ./internal/agent ./internal/sessions \
  -run '^Test(ActionDescriptors|Actions|ExtensionAgent|SessionExtension|Carry)' \
  -count=3 -timeout=5m
```

| Package | Selected root tests | Root passes across three runs |
| --- | ---: | ---: |
| `pkg/extension` | 1 | 3 |
| `internal/extension` | 16 | 48 |
| `internal/agent` | 6 | 18 |
| `internal/sessions` | 8 | 24 |
| Total | 31 | 93 |

The observed exit was 0, with no failed or skipped tests. This is a recorded
scoped result, not a claim that subsequent shared-worktree edits or the full
suite have passed.

[`extension_ordinary_test.go`](../../internal/agent/extension_ordinary_test.go)
installs an actual executable fixture and follows `All`, `Find`, connect,
apply, rediscovery, read-only preview, refusal after a user edit, disconnect
and disable. It also covers manual wiring and a built-in ID collision.
[`extension_actions_test.go`](../../internal/agent/extension_actions_test.go)
covers discovery, explicit planning, failed/ambiguous discovery and
confirmation invalidation across a changed manifest.

[`actions_test.go`](../../internal/extension/actions_test.go) covers host
snapshots, scope/generation/descriptor bounds, intent-bound confirmation,
path/hash/mode limits, rollback and rollback conflicts, new/removal recovery,
create-only output collisions, symlinks and concurrent cooperating plans.
[`extension_actions_test.go`](../../internal/sessions/extension_actions_test.go)
installs an executable converter and reaches it through a native session's
ordinary `Carry`/`CarryOver`, checks bytes/context/source preservation, and
rejects ambiguous or disabled routes. Its selected-source regression retains
record A while lookup of the same agent/ID changes to record B with a
different path and working directory. `TestSessionExtensionSelectedSourceGuard`
covers content and identity changes during planning and before application,
including restored size/mtime. `TestExtensionAgentOwnershipSeparatesDottedIDs`
checks independent adoption and original-setting restoration for the valid
`a/b.c` and `a.b/c` module/agent pairs. The selected existing Carry tests
exercise neighboring built-in behavior.

The fixture's config and session serialization are synthetic. These results
prove host reachability and the tested boundaries, not parity with any
vendor's real agent or session format, native application acceptance,
cross-platform execution, crash recovery of ownership, or a global
integration/vet/full-suite pass. The 31-root result above predates these two
hardening regressions; it is retained as the original integration proof.

The same-day hardening follow-up first verified both defects on the old
implementation: `TestExtensionAgentOwnershipSeparatesDottedIDs` failed on
cross-module ownership/originals, and the strengthened
`TestSessionExtensionOrdinaryCarryOver` received B's document with A's working
directory after lookup changed. Both selected roots failed before the fixes.

After the fixes and the source identity/hash guard regression:

```sh
go test -mod=readonly -json -race -tags nogui -p 1 \
  ./internal/agent ./internal/sessions \
  -run '^Test(ExtensionAgent|SessionExtension|Carry)' -count=3 -timeout=5m
go vet -mod=readonly -tags nogui -p 1 ./internal/agent ./internal/sessions
```

The observed race run passed 7 agent roots and 9 session roots three times:
16 roots, 48 root passes, zero failures/skips, exit 0. Package vet also
finished with exit 0. It used the same isolated HOME and pinned offline
caches described above. This follow-up did not run the broad suite or change
public SDK contracts.

The API 1.3 follow-up adds
[`extension_source_inputs_test.go`](../../internal/sessions/extension_source_inputs_test.go)
and [`extension_omp_native_test.go`](../../internal/sessions/extension_omp_native_test.go).
They cover the read-only input plan, source/store/parent guards and the
ordinary native OMP path. Route-specific, large-document and reverse-route
results belong to the owning frozen-source record; the historical tables
above are not acceptance results for those later changes.
The [combined API 1.3 record](../adr/0009-sdk13-validation.md) covers the host
checkpoint separately from each standalone route's native/domain proof.
