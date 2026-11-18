# Owned duplex process transport

This slice adds a host-owned transport for a finite foreground write action.
API 1.10 declares `agent-process` with `processes: [{id, agent, argv}]`.
The shared manifest/API version and JS SDK integration belong to the SDK
contract owner. A declaration grants no independent launch route.

Ordinary `Lease.Call(module.call)` selects a process recipe only when its ID
matches the selected primary write action. It requires an explicit operation
and attempt, refuses nested/read/component parents, and caps its lifetime at
thirty minutes. `agent` registers the one host-owned resolver during startup;
this caller uses the current profile home as cwd and ignores paths in action
input. A window request supplies its existing host-owned intent and account
selection. There is no automatic tool, service, background or recall grant.
Other module actions keep their existing dispatch.

The low-level host entry point is `Lease.CallWithProcess`. The checked adapter,
`agent.ResolveExtensionProcess`, resolves a declared agent against its ordinary
owned binding, package/configuration revision, file-plan contents, shell
snapshots and executable selection. It uses that binding's existing launch
arguments, model flag and environment, followed by the recipe's literal
arguments. Resume/source selection remains a separate owner.

## Authority and lifetime

`OwnedProcessLaunch` is internal host data. Callbacks supply a recipe ID or
opaque handle. Executable, argv, cwd, environment, profile revision, account
generation, route and parent scope come from the host. The caller supplies a
deadline no more than thirty minutes away and an explicit operation/attempt.
Only a selected write `module.call` or `tool.call` owns the transport. Private
nested component calls cannot inherit it. Ordinary module actions use the
installed declaration and profile home. A separate trusted host caller
selecting another directory must obtain that directory through its existing
approval policy; the low-level grant does not replace source-selection consent.

Executable identity includes original symlink resolution, file identity and
SHA-256; cwd retains its directory identity. Admission rechecks identities and
lease/account/profile after the profile admission lock. The agent adapter
holds shared target and ledger locks through spawn, matching controller lock
order. `proc.Command` and `proc.StartTree` launch one child, three OS pipes and
an explicit environment. Windows batch files are refused because the command
helper otherwise invokes an implicit shell.

A manager admits at most four process parents. Per-owner filesystem watchers
recheck package/configuration/account/profile/executable revocation. Parent,
lease or manager cancellation and changed ownership close pipes, stop the
tree and join readers, writers and pumps. A returning root call closes its
owned I/O before joining admitted host callbacks, so a blocked stdin write
does not hold shutdown until the parent deadline. Each call joins its admitted
host callbacks before return. A successful transport result joins the last
host reply before canceling callback contexts; consuming that reply can precede
the transport's write completion. Reply sends retain parent cancellation and
the existing fifteen-second worker write budget. Error and cancellation paths
cancel callbacks before shutting down I/O and joining. Explicit process close
joins and ends publication;
unread queued output need not survive close. Natural exit is output EOF and
never starts a replacement.

## Wire contract

`pkg/extension/process.go` owns independent DTOs and strict validation.
Installation data validated by the shared owner is
`processes: [{id, agent, argv}]`: at most 128 literal arguments and 64 KiB
aggregate UTF-8 bytes including separators. That owner validates uniqueness,
declared-agent ownership, clone/public projection and capability/version.

| Callback | Parameters | Result |
| --- | --- | --- |
| `process.open` | `{recipeId}` | `{handle,incarnation,limits:{maxChunkBytes:65536,queueChunks:2}}` |
| `process.read` | `{handle,pipe:"stdout"\|"stderr",seq,maxBytes}` | `{seq,bytes,eof}` |
| `process.write` | `{handle,writeId,seq,bytes}` | `{written,delivery:"written"\|"uncertain"}` |
| `process.stdinEOF` | `{handle}` | `{}` |
| `process.close` | `{handle}` | `{joined,exitCode?,reason}` |

Bytes use JSON base64. Sequences are canonical positive decimal strings,
independently starting at `"1"` for stdout, stderr and stdin. Zero, leading
zeros, uint64 overflow, duplicate keys, unknown fields and oversized chunks
are refused. EOF differs from fault/cancellation. Core never combines,
line-decodes, truncates or silently drops outputs. The module owns framing,
RPC correlation, reverse authorization, protocol bounds and reconciliation.

There are four independent serial callback lanes per parent: stdout, stderr,
stdin writes and lifecycle. The ordinary callback lane stays separate. Each
lane has a worker-wide bound of 32. Each output pump has two 64 KiB queue
entries and a bounded current read; pipe backpressure reaches the child.
Pending reads cannot hold stdin, lifecycle, usage or the other output lane.
JS must retain the same separation and cancel/join its pending callbacks.

## Receipts, accounting and observability

The private Store persists `spawn-intent` before spawn and `write-intent`
before delivery. Writes require the ordinary operation journal's `accepted`
state. Delivery records written/uncertain and close records joined, without
arguments or payload bytes. Duplicate admission, write ID and sequence are
refused. Intent survives owner loss. ACK, EOF, partial write or an unknown
command query never cause replay or automatic restart.

Accepted remains the ordinary durable operation event. Receipt or exit0
does not imply financial success. The root handler must return an explicit
authoritative `outcome:"completed"`; missing, accepted or unknown terminals
leave the ordinary operation uncertain. Not-started cannot erase accepted
uncertainty. Measured finance/usage uses existing `usage.append`, retaining
component, selected-account, generation, accepted-write and once-per-parent
checks. Core adds no parallel financial log or fabricated usage on loss.

Lifecycle facts use the existing rate-limited diagnostic sink: route,
operation/attempt/session/component, parent call and process incarnation;
accepted/completed/uncertain phases; elapsed milliseconds; separate
input/output/error byte totals; cancellation reason and joined state. The
host observer receives the same immutable facts. Stderr, credentials,
arguments, tool payloads and vendor errors never enter logs or receipts.
The trusted synchronous host observer must return promptly.

## Bounded verification

`pkg/extension/process_test.go` locks independent-lane progress, cancellation
and strict params; two transport tests have named old-SDK RED assertions.
`internal/extension/process_owner_test.go` exercises real synthetic child
pipes, large lossless bytes, no-replay, revoked publication, parent/lease/
module/config/account/profile/executable lifetime, admission barriers and
explicit outcomes. `internal/agent/extension_process_test.go` checks ordinary
launch resolution, literal argv, retained revisions and changed files.

`internal/extension/process_integration_test.go` installs synthetic exec,
native and Bun modules through the ordinary package owner, calls ordinary
`Lease.Call`, drives a real child, appends ordinary usage and checks terminal
journal/diagnostic facts. Neighboring actions cannot borrow the grant.
`internal/agent/extension_process_ordinary_test.go` uses the registered
production resolver, ordinary connection/binding and a real synthetic child;
disconnected profiles fail and action input cannot change cwd or literal argv.
`internal/extension/process_client_test.go` holds callback cleanup behind a
barrier and requires its canceled parent to join. A terminal-error regression
requires owner I/O shutdown before callback join, with a named causal RED.
A delivered-reply barrier requires successful completion to join the write and
retain the worker for the next independent call, also with a named causal RED.
The source-locked peer tests
exercise reverse replies, pending execution stop, separate stderr, fifteen
MiB physical JSON, split UTF-8 and a final unterminated frame.
The raw Bun peer tests host frames; shared JS SDK acceptance stays with its
owner. The gates use immutable Linux snapshots with isolated HOME/XDG,
pinned offline caches, GOMAXPROCS=2, -p=2 and a synthetic-network guard.
Platform runtime, vendor compatibility and whole-host acceptance remain
separate from this transport slice.
