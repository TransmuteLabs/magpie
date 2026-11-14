# SDK API 1.6: bounded local session sources

Status: implemented. The isolated preview's scoped execution evidence,
preserved failures and limits are recorded in
[ADR 0020](0020-session-source-preview-validation.md). That historical record
does not cover subsequent integration with agent profiles or native account
operations. Native ABI remains version 1.

## Problem

Session migration declares a converter, not a reader. Native discovery,
summary parsing, transcripts, resume and read consent have separate owners.
Adding a converter therefore does not make its source format discoverable.
Unknown agent IDs must not accidentally select the default Claude parser.

## Decision

Add one capability, `session-source`, one method, `session.source.read`, and
`sessionSources` descriptors distinct from migration's `sessions`.
The public contract is in `pkg/extension/session_source.go` and
`sdk/js/session-source.js`. Go/JS use API minor 6. Existing module contracts
and the native C ABI remain compatible.

The host enumerates one of three finite layouts: `<id>.jsonl`,
`<bucket>/<prefix>_<id>.jsonl`, or `<id>/log.jsonl`. It extracts the ID and
captures bounded documents. The module confirms that ID and returns a typed
summary or transcript. It cannot return filesystem roots, absolute paths,
shell commands, agent identity, prices or costs.

Roots come from current host-owned native slots or exact HOME-relative
directories frozen in a trusted installed manifest. Runtime options cannot
change a root. These grants govern host-mediated reads; trusted native/exec/Bun
programs are not an OS filesystem sandbox.

Native readers retain default ownership. Native delegation requires
`BindSessionSourceNative` with an exact installed revision, persisted binding
identity and current config generation. Disable, removal or changed binding
authority hides delegated data and does not silently restore another reader.
`UnbindSessionSourceNative` explicitly restores native ownership using the
expected binding identity.

## Runtime

Preparation, module calls and stable reads occur before the session cache lock.
Only owned immutable records enter the normal file/summary path. No RPC occurs
in `allFilesWith` or `parserFor`. SDK states are omitted from the native
persistent path cache. Journal and blob bodies are released after summary
decoding; transcript reads acquire and verify them again.

The existing checked `os.Root` input reader and snapshot checks are reused.
Optional `session.inputs.v1` planning and decoding share one pinned lease and
the same approved root. Every attachment's digest, physical identity, modes and
parents are checked; binary input is limited to 128 files and 16 MiB, and the
encoded request independently fits the 24 MiB frame limit.

Read consent is the existing per-agent `Reads` gate, including gateway mode's
`OwnOff`. Folder filters do not grant permission. Source calls cannot borrow a
provider/account scope, are unary and inherit read-only host callbacks.
Epoch/config/package/root checks prevent a prepared result from publishing
after authority changes. Local changes cancel preparation; fresh metadata
checks also detect changes made by another process.

A failed decode under unchanged authority retains detached last-good metadata
with `source_status: stale` and disables transcript/resume/carry. Revocation
hides the data instead. A new state pointer invalidates the ordinary listing
cache on the stale transition. Source errors are available at
`GET /api/sessions/sources`; failures are not reported as an empty successful
read. Appends and rewrites are fully decoded in this first cut; incomplete
trailing JSONL entries are not decoded.

Existing pricing owns costs. Missing usage is unavailable, not measured zero.
Native resume is host-rendered using the selected ID/path/profile; unsupported
launchers have no resume action. Source selection and attachment snapshots
continue through ordinary checked migration.

## Bounds and follow-up

This cut implements regular uncompressed local JSONL only. SQLite, packed
journals, remote/backend history, arbitrary layouts, module shell commands,
native schema takeover by installation alone, call/trace reader registration
and deletion are not implied by source registration.

The synthetic native example demonstrates the contract and host ownership,
not a recovered vendor codec or a live vendor session. Platform builds are
distinct from execution on those platforms.
