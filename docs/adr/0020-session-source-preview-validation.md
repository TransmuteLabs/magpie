# Session Source 1.6-preview verification

This record covers the bounded local JSONL Source preview, based on
`c9dd41e129fc0d1dc6e7ca7fa10d299ef0a97cef`. It does not approve the complete SDK,
change the main branch's API 1.5 contract, or claim vendor codec support.
It is historical evidence for the isolated preview; later integration changes
require their own verification.

## Frozen source

The executable candidate is Git tree
`b98397198c9e83a8da2d726bf9ea1713a5fedbc8`: 62 changed paths. Its immutable
verification source and SHA-256 inventory are retained.
Subsequent changes for this handoff are documentation only.

| File | SHA-256 |
| --- | --- |
| `pkg/extension/session_source.go` | `2bc8473a641f083b1dda0d7add85405ad0f2fca3b9ff72b103c7a46fcd520dc5` |
| `sdk/js/session-source.js` | `4e4041ed0a9a7f35bca49eef34606d14ee1ff7b3ff0401ec355f9ec1f688477b` |
| `sdk/js/index.d.ts` | `62a2a94b6d46cfbe34ffad8ef210bbf71090a683a4bc0548c9907855cf024776` |
| `internal/sessions/extension_source_adapter.go` | `9ea0338ae522db119d21536557b1220a5c3823a74945714ee0c1ba92c0daa830` |
| `internal/sessions/extension_source_publication_test.go` | `fdc63d74be173007762627d3ef19c99b75c436dae571737c6969b27e2833cd8e` |

## Actual execution

All builds and tests ran on Linux, with temporary HOME/XDG directories,
pinned existing Go caches, offline dependency resolution and no live vendors,
accounts or agent session writes. No new dependency was added.

The synthetic fixture is a real Go C-shared native module built from the same
source as the host. Its PID assertion proves in-process execution. Production
code overlays were not used for positive tests. The author-built native
library's SHA-256 is
`1bc65e7070b567c1ac1f247e1ce729665454f8a39d33afe10463f677f306f022`.

| Gate | Terminal result |
| --- | --- |
| Complete `internal/sessions`, `-race -count=1` | 989 test pass events, 32 explicit skips, zero failures |
| Native ordinary Source matrix | 24 pass events, zero failures/skips |
| Actual GUI ordinary Source API | One pass, zero failures/skips |
| Public Go SDK, `-race` | 430 pass events, zero failures/skips |
| Actual Go host/Bun Source worker, `-race -count=3` | Three passes, zero failures/skips |
| Complete JS SDK | Node 124 and Bun 124 passes, zero failures/skips |
| Strict TypeScript | Eight consumer fixtures, exit zero |
| Scoped `go vet` | Exit zero |
| Linux amd64 CGO-enabled `nogui` vet/build | Both exit zero |
| Darwin arm64 and Windows amd64 CGO-disabled `nogui` vet/build | Four exits zero; native execution is not implied |
| Native lifecycle and cancel/cache-lock barrier, each `-race -count=20` | 20 passes each |
| Local revocation matrix, `-race -count=3` | 18 pass events |
| Checked input/layout/root guards, `-race -count=20` | 220 pass events |
| Native active-read foreign-CLI off | 20 passes |
| Ten publication-window owners, each `-race -count=20` in its own test process | All 200 case repetitions pass |

The complete session suite's 32 skips are 24 pre-existing OMP native fixture
opt-ins and eight existing race/pool or macOS-only checks. They were not run.
Neighbor-only regression coverage also passed 89 events with the same 24
OMP opt-in skips.

The ordinary fixture covers List/Find/Get, titles, management, native IDs,
transcripts, search, host-priced usage and checked CarryOver, including opaque
attachment bytes. It does not establish support for a vendor's unrecovered format.

## Independent probes and causal negatives

Independent review approved the bounded V4 Source candidate and retained
its report and run records.
All 10,228 original snapshot files and the 62 owned changed paths matched their
captured hashes after execution. The independent positive gates contain
1,116 Go pass events, zero failures/skips, plus five Node and five Bun Source
contract passes. Events include repetitions and subcases, not distinct tests.

The original `e419a35b238f9b98cb8ae2345495e3cf2cef11ad` result remains historical
red. A separate CLI could disable a module during native decoding without
changing the reader process's epoch, and List/Find still exposed its row.

The unchanged independent probe, SHA-256
`a707fe9a5cbf7e1ca96b8398151d4f686b4fdeab44ce1ca5b69d03138a4ef6bd`,
passes on V4 with the actual native fixture and separate host CLI. The epoch
still stays unchanged; persisted grant checks now hide the row. Independent
repeats in three fresh host test processes also pass. Separate native probes
cover usage rewrite/repeated-read stability, gateway-mode `OwnOff`, an
explicitly bound OMP resume followed by config change/rebinding/off, and
gateway-key deduplication for the same agent/ID without merging distinct agents
sharing an ID.

Independent raw payload corpus SHA-256
`ff320ccd5777a414aff6e9ee295e3dd1a63cc9c035f5d2f9a34e4a67a77e4a37`
was replayed unchanged on V3 and V4. All 11 results match across Go, Node and
Bun: two valid payloads preserve explicit-zero versus unknown usage; eight
missing required fields and February 31 are rejected. Author tests additionally
share a 29-case raw corpus across Go and JS.

Four author production-code mutants fail with named behavioral assertions,
not compile errors: restoring stale-summary pointer reuse, removing the encoded
frame guard, bypassing selected ID equality, and bypassing the publication
retry. The last mutant retains the real separate-CLI off operation and grant
pruning; cached results, including the unchanged-list response, still leak
without rebuilding the result. An independent mutant making
`refreshSourceAuthority` a no-op also restores the unchanged privacy probe's
named List/Find failures.

## Limits and preserved red evidence

A single process repeating the entire native publication matrix 20 times
eventually hit Linux `cannot allocate memory in static TLS block` while
mapping more than 200 distinct copied Go shared libraries. Native libraries
remain mapped until host exit by the existing ABI contract. This is preserved
as an unsuccessful stress run, not relabeled green. Each publication case
was then repeated 20 times with its own test process; all passed without
changing production code.

The independent reviewer's first native matrix run omitted the required
`MAGPIE_SOURCE_TEST_HOST` fixture variable. Its failed prerequisite assertions
remain recorded separately. The corrected environment passed all 35 events
against unchanged source.

Darwin/Windows native loading, live vendor sessions, SQLite/compressed/remote
history and call/trace source registration were not tested or implemented by
this preview. No remote LSP server was available; actual Go vet/build and strict
TypeScript were used for type diagnostics. No new full repository suite or
browser-rendering acceptance is claimed by this scoped record.
