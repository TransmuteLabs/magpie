# Executable extension agents and sessions

This reference describes the implemented host paths for executable SDK agent
wiring and session conversion. The host owns discovery, scope, approved paths,
file preconditions, execution and recovery. The extension owns the agent's
config schema, inspection, format-aware restoration and session serialization.
An executable extension is a trusted local program, not a filesystem sandbox.

## Sources of truth

| Responsibility | Source and symbols |
| --- | --- |
| Public descriptors and RPC payloads | [`pkg/extension/contract.go`](../../pkg/extension/contract.go), `AgentDescriptor`, `AgentField`, `SessionDescriptor`; [`types.go`](../../pkg/extension/types.go), `AgentInspectRequest`, `AgentInspectResult`, `AgentPlanRequest`, `AgentPlan`, `SessionRequest`, `SessionResult` |
| Descriptor validation and required methods | [`manifest.go`](../../pkg/extension/manifest.go), `Manifest.Validate`, `RequiredMethods`; [`wire.go`](../../pkg/extension/wire.go), `MethodCapability` |
| Ordinary agent discovery | [`agents.go`](../../internal/agent/agents.go), `All`, `local`; [`extension_actions.go`](../../internal/agent/extension_actions.go), `ExtensionAgents`, `extensionAgents`, `extensionAgent` |
| Agent lifecycle and preview | [`applied.go`](../../internal/agent/applied.go), `ConnectHow`, `Apply`, `Pick`, `Wired`, `Disconnect`; [`native.go`](../../internal/agent/native.go), `NativeConnection`, `DisconnectPlan.Revision`; [`preview.go`](../../internal/agent/preview.go), `DisconnectPreview` |
| Session routes and native source reads | [`carry.go`](../../internal/sessions/carry.go), `carries`; [`carry_convert.go`](../../internal/sessions/carry_convert.go), `CarryOver`; [`extension_actions.go`](../../internal/sessions/extension_actions.go), `SessionExtensions`, `extensionSessionRoutes`, `carryExtension`, `ConvertExtension` |
| File plan preparation and execution | [`actions.go`](../../internal/extension/actions.go), `Actions.AgentInspect`, `AgentPlan`, `SessionConvert`, `Execute`, `ActionPolicy`, `ActionPlan` |
| Module lifecycle and scoped state | [`manager.go`](../../internal/extension/manager.go), `Default`, `Manifest`, `Invoke`, `Store`; [`store.go`](../../internal/extension/store.go), `Store.GetAccount` |

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

`All()` builds the native/built-in and existing JS agent list, then appends
`extensionAgents`. `local()` does the same without WSL agents.
`ExtensionAgents` reads the enabled installed manifests without calling a
worker. A descriptor without config files has the explicit action API but
does not register an ordinary agent. Rich descriptors negotiate inspection;
ordinary wiring uses descriptors with config files.

`extensionAgents` reserves existing IDs and aliases, other known clients, and
`magpie`, then rejects collisions. Duplicate SDK agent IDs also fail discovery.
It does not replace a native or JS agent. `PluginErrors()` reports discovery
and ownership conflicts. A module disabled or removed from the enabled
catalog is absent from the next ordinary discovery.

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
Ordinary routing selects enabled descriptors by source and target, suppresses
ambiguous SDK routes, retains built-in conversion precedence, and does not
duplicate a target already offered by a native carry operation. Target
availability uses the existing availability check or executable lookup;
`resumeCommand` must already support that target.

```text
native session assembly -> carries -> enabled descriptor route -> Carry
CarryOver -> read consent + offered Convert route
          -> extensionSessionRoutes -> carryExtension
          -> convertExtensionSource(selected Session)
          -> Actions.SessionConvert -> ActionPlan
          -> validate native ID/resume command -> Actions.Execute
          -> CarryResult {agent, id, path, command}
```

Listing routes does not invoke the converter. Dispatch refuses a route that
has disappeared instead of falling through to an unrelated native converter.
The ordinary conversion context is bounded at 30 seconds.

Public `ConvertExtension` checks read consent and resolves the native session
once by agent/ID, then calls `convertExtensionSource`. Ordinary
`carryExtension` passes its already selected `Session` directly to that
helper; it does not re-resolve the ID and substitute another store's path.
The selected path and working directory therefore stay together even when
ordinary lookup now returns another legitimate session with the same ID.

The helper requires consent, nonempty agent/ID/working directory, an absolute
source path, local writable provenance, and a matching parent session scope.
It reads a bounded regular file, verifies the opened identity and pathname,
and checks size/mtime across the read. The resulting snapshot retains the
selected path, physical file identity and SHA-256 content hash.
After module planning, and again immediately before ordinary execution, the
host rechecks that identity and hash. Same-size edits with restored mtime and
same-content file replacement both fail. `options.cwd` and native resume use
the selected session's working directory.

The module receives one document containing `filepath.Base(source.Path)`,
source format and original bytes. This does not capture all files of an
aggregated session or infer the working directory from vendor document
contents. It is a file-source path, not a generic database export or complete
session-graph exporter. The source check is local to the session adapter:
Actions has no source-read guard hook. A public explicit caller retaining a
prepared plan does not gain a deferred source guard on its later `Execute`,
and ordinary conversion does not lock another application's source across
the final check and output application.

The converter returns documents in the requested target format and an
optional `SessionResult.id`. Ordinary conversion requires a nonempty ID and
a safe host-generated resume command before publishing files. Explicit
conversion may omit an ID. The host neither accepts a worker-supplied shell
command nor validates the target's complete vendor session schema.

Ordinary output uses HOME plus a session-only `CreateUnder=outputDir`
policy. Returned relative document paths are rebased below that prefix and
must be new files; existing files, traversal and collisions appearing after
preview are rejected. The source is not a planned output and is not changed
by host conversion. The returned `CarryResult.path` is the first path in the
action's lexicographically sorted preview, so the adapter has no separate
primary-document designation for multi-document output.

Native resume uses the existing `resumeCommand` with the returned ID,
working directory and selected output path. It returns the command to the
caller; this adapter does not execute a shell. Targets with no existing
native resume implementation, unknown source formats and explicitly selected
file sets use manual conversion workflows. The SDK path returns before the
native converter's `carried` cache and adds no equivalent reuse cache.

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
