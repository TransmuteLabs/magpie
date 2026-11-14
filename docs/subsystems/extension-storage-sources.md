# Rooted executable storage sources

This is the implemented API 1.8 storage authoring and adapter seam. Both Go
and JS registries admit `session-storage-source` at minor 8. It does not
change the API 1.6 JSONL source contract or establish vendor compatibility.
Global SDK versions, capability admission and published JS file lists are
coordinated with the other API 1.7/1.8 owners.

The generic host knows finite root slots, database/sidecar names, legacy/media
trees, opaque session locators and normalized summaries, transcript parts and
media. The module owns SQLite schema, transactions, WAL interpretation,
migration precedence, replay and media hydration. There is no SQL RPC, browser
filesystem route, account grant or parser fallback.

## Sources of truth

| Responsibility | Source and symbols |
| --- | --- |
| Descriptor, request and result validation | [`storage_source.go`](../../pkg/extension/storage_source.go), `ValidateStorageSources`, `DecodeStorageRequest`, `DecodeStorageResult` |
| Scoped callback DTOs and author helpers | [`storage_reader.go`](../../pkg/extension/storage_reader.go), `Stream.NativeStorage`, `StorageFiles`, `StorageBytes`, `CheckStorage` |
| Native rooted descriptor access | [`native/storage_reader.go`](../../pkg/extension/native/storage_reader.go), `OpenStorage`, `StorageRoot`; platform siblings |
| Private read lease and parent broker | [`storage_grant.go`](../../internal/extension/storage_grant.go), `NewStorageGrant`, `beginStorageCall`, `finishStorageCall`, `storageHostCall` |
| Consent, discovery and publication | [`extension_storage_source.go`](../../internal/sessions/extension_storage_source.go), `BindStorageSource`, `InspectStorageSources`, `prepareStorageSources`, `storageSourceRead`, `StorageSourceMedia` |
| Owner HTTP routes | [`extension_storage_sources.go`](../../internal/gui/extension_storage_sources.go), `extensionStorageSourceRoutes` |
| JS/TS authoring parity | [`storage-source.js`](../../sdk/js/storage-source.js), [`storage-source.d.ts`](../../sdk/js/storage-source.d.ts) |

## Authority and snapshots

`session-storage-source` requires API 1.8 and a native primary. A descriptor
declares at most sixteen roots and families, eight exact sidecars and sixteen
finite trees per family. A root has either one frozen HOME-relative path or a
known native root slot. The `opencode.data` slot resolves the existing native
OpenCode data owner; modules cannot supply an arbitrary runtime path.

Inspection exposes opaque root references and exact installed revisions.
Binding requires both values and persists the selected package/configuration,
root device/inode and source identity. Reads still require the existing
`Reads(agent)` consent and respect `OwnOff`. Revoked or disabled delegated
ownership hides the data instead of selecting the built-in parser.

Each invocation pins one exact package/configuration/runtime lease, root
directory and finite family. `session.storage.list` has source-only scope;
`session.storage.read` must match the selected session locator. Both are unary
and carry no provider, account, generation, attempt or credentials. Private
`storage.*` callbacks require that exact active parent and scope; a component
or another parent cannot borrow the grant.

Linux and Darwin native access atomically duplicate a private borrowed
directory descriptor with close-on-exec set. Every path component is opened
read-only with `Openat` and `O_NOFOLLOW`; devices, sockets, symlinks and
undeclared family members fail. The owned descriptor remains pinned after a
pathname replacement and is closed independently of the borrowed descriptor.
There is no proc-fd reopen or path fallback. The native SDK helper exposes
only declared family members. Legacy tree enumeration and byte snapshots
remain brokered, with opaque file references and snapshot cursors. Other
platforms refuse rooted native reads explicitly. Darwin arm64/amd64 CGO0
build and vet establish source support; actual Mac native loading, rooted
access and lock interoperability remain an isolated runtime acceptance gate.

Darwin POSIX record locks belong to the process: closing another descriptor
for that inode can release a live module reader's locks. The broker therefore
uses rooted metadata-only probes for identity checks, and compares device and
inode without requiring Go's private `os.FileInfo` representation. Darwin
storage grants serialize across native library instances. Once native access
starts, broker member descriptors are retained within a finite 256-descriptor
budget until a validated parent terminal frame. Cancellation closes the
grant to publication but does not release those descriptors or admit another
native lifetime before that terminal. Worker retirement alone is insufficient;
an uncertain handler that never terminates keeps admission closed. The module
still owns its SQLite transaction and all DB/SHM lock and close coordination.

The broker snapshots legacy bytes once, charges the complete physical
snapshot against the aggregate budget, and serves bounded chunks from those
bytes. Publication verifies identities, metadata, directory membership and
content hashes. Existing database/WAL/SHM members are pinned by physical
identity; absent members remain absent for that snapshot. Database content
consistency belongs to the module's real read-only transaction.

Before callback dispatch and result publication the broker rechecks root,
consent, package, configuration, enabled state, live worker, lease and
cancellation. The sessions adapter also rechecks the selected immutable view
before exposing summaries, transcripts or media. Default limits are 10,000
entries/parts, 64 MiB read bytes, 4 MiB text and 8 MiB media, with pages of at
most 256 entries and a ten-second ordinary host invocation.

## Owner surfaces and remaining acceptance

The authored owner routes are `GET /api/sessions/storage-sources`, checked
`POST .../bind` and `POST .../unbind`, and selected normalized
`GET /api/sessions/storage-media`. Media has no raw path selector and is
served as an attachment with no-store, nosniff and a sandbox policy.
The single-root ordinary `sessions sources bind`/`unbind` owner delegates to
the same storage consent functions.
`magpie sessions sources list [--json]` returns the same opaque roots and
consents as storage inspection. Explicit selection uses `sources bind
<module> <source> --root <slot> --root-ref <root-reference> --expected
<package-revision>` and retains the same root/reference/revision checks.
The existing single-root convenience binding remains available. Listing does
not grant ordinary history reads or root consent; disabled sources retain
their exact identity for checked release. Selected media bytes currently
have no CLI export command, recorded explicitly in the route parity guard.

The current scoped tests cover public validation, finite native access,
raw snapshot accounting, directory replacement, foreign parent/session,
consent/configuration/off/cancellation and publication hashes. The opt-in
`TestStorageNative` suite in `internal/sessions` additionally installs a full
native module and uses its actual SQLite reader with a separate synthetic
WAL writer. It covers ordered parts/media, migration deduplication, unchanged
refresh, donor-file hashes and immediate publication revocation. Enable it
only with fixture distribution/writer/host paths; no live store is read.

Delegated storage ownership suppresses the built-in database enumeration
before it opens files through `storageNativeFiles` in `allFilesWith`.
Late filtering alone can let a native SQLite parser write SHM.
Unchanged refresh retains selected records rather than invalidating held
session objects; changed authority still invalidates them.

Owner route tests retain ordinary web cookie/origin guards and reject unknown
or trailing JSON in checked binding requests. Cross-process HTTP evidence,
rendered consent controls, selected media display and actual OpenCode binary
acceptance are distinct gates. Native call/trace accounting is a separate
owner from the session source adapter.
