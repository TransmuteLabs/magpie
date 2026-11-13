# SDK API 1.1 validation: native and composite modules

Evidence checkpoint: November 13, 2026.

Readiness: **native/composite scoped acceptance passed; whole-repository
acceptance remains non-green.** This record covers SDK API 1.1, independently
of the subsequent API 1.2 gateway-hook and tool-exposure work.

The decisions are [native ABI](0004-native-extension-abi.md) and
[composite ownership](0005-composite-module-contract.md). Scoped acceptance
does not establish support for every upstream extension point.

## Source identity and execution

The source snapshot is tree
`59bd6d1f74383849f328cbf4a0146f0294e674d6`, based on
`f2d98c309a389159284543d160da4f1a3e721529`.
It includes the cached-generation admission fix, configuration retirement,
primary-before-helper disposal, HTTPS service proof, and updater/test
isolation corrections present at that checkpoint.

The one subsequent SDK 1.1 production delta changes the process transport's
stored `command.Wait` method value into `func() error { return command.Wait() }`.
It preserves deferred reaping after stdout drain while making the existing
process-ownership AST guard recognize that ownership. Its separate overlay
verification is recorded below.

All verification started after the host correction used **`usbox`**, as a
regular user. `usbox-adm` is administration only. The authoritative Linux
checks use the repository's declared Go **1.26.8**, isolated HOME/XDG paths,
pinned caches, synthetic HTTP, and offline dependency resolution during
tests. Source snapshots and actual module payloads are separate from the
installed application and live accounts.

Local evidence:

- `/tmp/magpie-sdk-v11-verification-checks/usbox-snapshot.json`
- `/tmp/magpie-sdk-v11-verification-checks/usbox/`
- `/tmp/magpie-executor-final-evidence/`

Remote evidence:

- `usbox:/tmp/magpie-sdk-v11-usbox-niq306rt/`
- `usbox:/tmp/magpie-executor-review-spnyobob/source-59bd6d1`
- `usbox:/tmp/magpie-reap-wait.Uduu7SAe/`

## Scoped results

| Gate | Observed result | Boundary established |
| --- | --- | --- |
| Complete SDK race run | Exit 0; 753 test-pass events; seven tested packages | Public Go SDK, native adapter, compatibility codecs, runtime, metadata parser and hybrid package. Two example main packages have no tests; this is not two skipped test cases. |
| Cached-generation admission | `-race -count=20`, exit 0 | Package/config interleavings reject stale acquisition without incrementing the replacement generation's lease count. |
| Group/config review regressions | `-race -count=20`, exit 0 | Idle runtime retirement after CAS, cancellation after durable commit, active lease draining, metadata reads without lifecycle channels, and primary disposal while helpers still serve. |
| HTTPS/page-service/budget gate | Exit 0 under race | Real trusted TLS, rejection of untrusted certificates and wrong instance proof, redirect normalization, proof stripping, lease release and unchanged script budget. No production TLS verification bypass. |
| Linux native distribution | Build and real CLI installation exited 0 | C-shared primary, external Go backend and TS UI built on usbox. Native handler PID equals Magpie PID; backend PID differs. No Bun runtime is needed. |
| Config/nested write/replay CLI | Expected exits 0/1 | Config CAS is visible to both components, nested write runs, and repeating the same operation is refused. |
| Process reaping overlay | Guard red before; guard and process lifecycle race gates green after | Stored Wait remains deferred, every started command has an owner, cancellation/drain/unpublished-client cleanup remains intact. Guard allowlist unchanged. |
| Native ABI review | Independent approval, no remaining ABI findings | Bounded C spans, opaque handles, callback revocation and logical close. This is a source review, not a new platform runtime run. |
| Composite review | Four findings corrected with regressions | Cached-generation admission, immediate config retirement, disposal order and HTTPS endpoint parity. |

The scoped SDK command was:

```sh
go test -json -race -tags nogui -p 3 \
  ./pkg/extension/... ./pkg/protocolext ./internal/protocolext \
  ./internal/extension ./internal/windowmeta \
  ./examples/extensions/hybrid-go/... -count=1 -timeout=15m
```

Before the host correction, macOS native/GUI tests and six platform
vet/build gates passed on earlier immutable checkpoints. They remain
historical evidence with their original source identities; this record
does not relabel them as final-tree macOS runtime proof. Linux native
execution above is current to `59bd6d1…`. No Windows native library was
executed; no-cgo cross-builds establish feature-off compatibility only.

## Whole-repository result

The completed safe broad run on usbox used the same tree:

```sh
go test -json -tags nogui -p 3 -timeout 45m \
  -skip '^TestScriptFindsClaudeMoved$' ./...
```

It **exited 1** after 1,368.62 seconds, with 29,309 test-pass events.
The explicitly excluded script fixture can launch an installed desktop
application and was not run. Other runtime/platform fixture skips retain
their own reasons in `broad-safe-result.json`; they are not passing tests.

Failures included the downloaded-toolchain overlay location, the
version-dependent OpenCode assertion, the process-wait AST guard,
SOCKS HTTPS CONNECT expectations, and additional gateway/provider/OTLP
tests. Subsequent narrow fixes or environment corrections do not turn that
completed broad run green. In particular, copying the same declared Go
toolchain outside GOMODCACHE resolves its overlay restriction, but requires
a new run to establish that test's result.

The earlier macOS broad run also exited 1, on tree `1031fef…`, and is retained
separately. This local SDK checkpoint is not a published release and makes
no claim that the entire repository or every extension surface is ready.

## Explicit limits

- All request/account/state behavior uses fixtures; no live vendor account,
  private repository credential, native Devin client, ZCode migration or
  production memory deployment was exercised.
- ModuleRPC 1.1 is unary. Existing inference/tool streams and HTTP/SSE
  services retain their own contracts.
- Native modules share the host failure domain. Logical close cannot kill
  arbitrary stuck native code, and libraries remain mapped until exit.
- API 1.2 memory composition, browser provenance, gateway hooks and
  local-owner tool exposure need their own final snapshot and acceptance
  record. Their implementation is not evidence for this API 1.1 checkpoint.
