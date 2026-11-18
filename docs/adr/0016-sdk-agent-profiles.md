# SDK 1.5 agent launch and explicit profile ownership

Status: implemented and verified as a scoped AgentWiring checkpoint. The
combined SDK 1.5 application checkpoint remains a separate acceptance gate.

## Problem

SDK agents with exact config files already participate in ordinary agent
discovery, model pickers and connect/disconnect. An agent that takes routing
only from its launch environment cannot use that adapter without inventing a
config file. Reusing an existing agent also needs an explicit ownership
handoff: a second tile must not race the original agent's writers.

The host must keep these mechanics independent of vendor protocols and of
the module's native, executable or Bun runtime.

## Decision

Extend `AgentDescriptor` at API 1.5 with aliases, user-agent prefixes,
structured launch metadata, an optional explicit `profile.target`, and
typed Library locations. Version negotiation rejects this metadata below
1.5. Descriptors using the older fields continue using the previous config
adapter. An ID/name-only descriptor remains an explicit action surface.

Launch metadata reuses the existing `launchSpec`, ordinary `Starting`,
`Starter`, CLI `run`/`env`, and shell-function owner. No legacy plugin VM,
synthetic config file, vendor schema, or process-specific launch protocol is
created. `Starting.Active` permits an environment-only connection without
requiring a model flag. Existing launchers retain their previous behavior.

Example of a launch-only agent:

```json
{
  "id": "example-native",
  "name": "Example native",
  "bin": "example",
  "aliases": ["example-cli"],
  "ua": ["example-agent/"],
  "launch": {
    "env": {"EXAMPLE_URL": "{url}", "EXAMPLE_KEY": "{key}"},
    "args": {"--mode": "proxy"},
    "plain": ["login"]
  }
}
```

An explicit profile adds `"profile":{"target":"existing-agent"}`. Its ID
must remain distinct. The target is resolved from the existing local agent
owners; profiles cannot chain through another SDK profile. Existing ID,
alias and launch-command collision refusal remains the default. A profile
does not suppress its base before an explicit connect/set action succeeds.

After adoption, ordinary discovery suppresses the base row and resolves its
aliases to the profile. The base's Library identity is reused. An inactive
profile does not become another Library writer. A standalone SDK agent can
declare instructions, MCP format/location and skills locations; the normal
Library writers remain responsible for their contents.

## Binding contract and authority

Inspection and planning receive a host-derived binding:

```json
{
  "target": "existing-agent",
  "revision": "7",
  "connected": true,
  "values": {"model": "magpie/example/model"}
}
```

`agent.plan` returns an explicit `binding` change containing `revision`,
`connected` and declared field `values`, beside its existing file plan.
The module echoes the canonical decimal revision; it cannot select another
target, module, home or revision. The host replaces caller-supplied binding
snapshots, validates fields and limits, and binds both snapshots to the
confirmation hash. Go and JS validate the same DTOs. A binding-only action is
allowed only for a declared launch with no config files and an actual host
snapshot.

`Actions.Execute` approves and applies the checked file plan. The ordinary
agent controller owns persistence of the binding; the explicit action API
alone does not acquire ordinary agent ownership.

## State and lifecycle

The private `sdk-agent-bindings.json` ledger stores the module/agent/home,
descriptor, package/config identity, canonical revision, field values,
original and applied file snapshots, and owned shell blocks.

The transition is:

```text
read current record and package/config
  -> prepare module plan on a captured lease, without ownership locks
  -> exclusive target lock + global ledger lock
  -> recheck revision, descriptor, package/config and local preimages
  -> durable pending record
  -> checked file transaction + checked shell transaction
  -> recheck selected runtime
  -> durable owned/disconnected record
```

The target lock fences ordinary base mutations, including held objects,
background sync, direct field/native callbacks and whole ordinary mutation
entrypoints. Shared OS locks permit nested base operations; an exclusive
handoff cannot split a multi-field ordinary mutation. Local atomic agent
owners participate as well. WSL agents are not profile targets in this cut.
Separate older Magpie binaries that do not implement this contract are not
coordinated by these locks.

Ordinary file-agent disconnect previews run on a copy of the configuration.
That copy retains the durable `sdk-agent-bindings.json` file, but does not
link `sdk-agent-locks`: the latter contains only empty OS-lock sentinels,
not ownership records or user data. The preview creates its own private
lock directory. Other existing read-only directory links are unchanged;
there is no general symlink-ignore rule.

The action owner revalidates selected module/config and account authority
after waiting for its file lock, before each file, and at completion. The
controller also checks the captured runtime around shell effects. Replaced
authority rolls back known writes. File and shell rollback never overwrite
unexpected user changes.

An incomplete rollback keeps the pending record. Clearing it requires proof
that both file and shell transition preimages were restored; restoring only
the agent config is insufficient. Partial application, failed rollback and
user changes remain diagnosable and require an explicit checked disconnect.

An off/removed/redefined module with an owned record retains a recovery row.
Starting it fails closed rather than silently launching the agent on its
own account. Recovery uses the stored descriptor and exact host snapshots;
it does not execute disabled module code. The old target remains reserved
until recovery completes. A held object for an old target cannot disconnect
a subsequently adopted replacement target.

Offline whole-file restoration is refused after a successful online action
rebases unrelated user edits. The module's schema-aware disconnect is then
needed to preserve those edits. Outside text in shell profiles remains owned
by the user throughout.

## Compatibility with previous SDK ownership

An earlier config adapter keeps its original snapshots in the legacy stash.
A new launch/profile controller must not capture the already-wired files as
their original state. If that exact module/agent has legacy ownership, the
new controller refuses adoption and names the prior owner. Reinstalling the
previous descriptor and using its ordinary disconnect preserves user fields;
fresh 1.5 adoption can then capture the actual disconnected preimage.

This is an explicit compatibility boundary, not automatic ownership
migration. Refusal does not edit the legacy config, stash or durable backups.
No live app, account or agent needs changing to verify this path: executable
fixtures exercise the entire transition.

## Sources and acceptance

- Public metadata and DTOs: `pkg/extension/agent_metadata.go`,
  `agent_binding.go`; JS parity: `sdk/js/agent-metadata.js`.
- Ordinary controller and discovery: `internal/agent/extension_controller.go`,
  `extension_actions.go`, `extension_metadata.go`.
- Durable ownership and fences: `extension_binding_store.go`,
  `extension_fence.go`, platform lock helpers and `extension_legacy_owner.go`.
- Existing owners: `internal/extension/actions.go`, `actions_restore.go`,
  `internal/update/shellfn_plan.go`, `internal/library/extension_agent.go`,
  `internal/cli/start_cli.go`.
- Fixtures cover ordinary All/Find/Starter, real executable argv/env,
  CLI run/env, Library writes, explicit profile handoff, held writers,
  concurrent stale plans, cancellation, package/config replacement, off
  recovery, drift, partial shell rollback and legacy ownership refusal.

Verification must include Linux race runs, Go/JS/TS contract parity,
unchanged independent probes, causal negatives for the authority/rollback
guards, and platform build/vet evidence. An unrun or platform-skipped check
must be recorded as such.

### Checkpoint evidence, 2026-11-13

The Agent-only source, based on `44cc0152d`, contains the separately
verified Codex and Pi baseline fixes but excludes the AuthBroker changes.
The Linux checks used Go 1.26.8, isolated HOME/XDG and pinned offline caches.

- Full SDK race: 958 test passes, no failures or skips.
- Controller, shell, CLI and authority race count20: 460 passes, no skips.
- Ordinary disconnect-preview race count20: 140 passes, no skips. Removing
  only the ephemeral-lock exclusion makes both new preview assertions fail.
- Node and Bun: 122 passes each; all seven strict TypeScript fixtures pass.
- Linux CGO1, Darwin arm64 CGO0 and Windows amd64 CGO0: all six nogui
  build/vet gates pass. Cross-platform compilation is not runtime evidence.

The independent review closes the two reported HIGH findings and the
captured-target boundary: 260 race count20 passes and six compiling
production-guard negatives with actual assertion failures. All six core
ownership/authority source files remain identical to that reviewed source.
This is bounded review closure, not approval of every SDK surface.

Composing the Agent-only changes onto AuthBroker commit `e320b3919`
preserves its 30 Auth-only files byte-for-byte, all Auth additions in the
five shared files, the account-generation fix and the supported TypeScript
contract minors 0 through 5. That composed source passes the full SDK race
with 1,022 test passes, the broader owner selection with 136 passes,
Node/Bun with 125 passes each, all nine strict TypeScript fixtures, and
formatting checks on all 39 changed Go files.

The full Agent/Library/update race run is **not green**: 3,460 passes and two
failures, `TestDownloadRetriesAndReports` and
`TestWSLFirewalledFindsAnotherWay`. The former also fails its unchanged
count20 run once; the latter reports a fixture cleanup/probe race. Both
baseline corrections and the subsequent combined application acceptance
are tracked separately. The 31 SDK-agent cases in that broad run pass.
The unsafe installed-Claude discovery fixture is explicitly excluded;
platform-specific skips remain skips. No real vendor, live agent config,
installed app, native macOS/Windows runtime or browser UI was exercised by
this Agent checkpoint.
