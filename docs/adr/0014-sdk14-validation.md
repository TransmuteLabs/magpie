# SDK 1.4 validation

Date: 2026-11-13
Status: SDK 1.4 safe application snapshot accepted in the recorded environment;
later SDK 1.5+ changes require separate acceptance.

The upstream integration is already recorded in
[SDK 1.3 validation](0009-sdk13-validation.md). This record concerns the
additional [executor](0011-native-executor-constraints.md),
[routing](0012-sdk-routing-strategies.md) and
[usage/cancellation](0013-usage-revisions-and-business-cancel.md) contracts.
It does not declare all extension surfaces or vendor implementations complete.

## Environment and scope

Execution uses `usbox`, temporary HOME/XDG, pinned Go caches and offline Go
dependency resolution. Tests use synthetic accounts, local HTTP servers,
real executable/native SDK hosts and checked module packages. The installed
app, live credentials, vendor accounts and user sessions are untouched.

## Scoped evidence before final aggregate

These are distinct immutable sources and overlays, not one combined run:

| Scope | Terminal evidence |
| --- | --- |
| Usage/cancellation after strict private metadata fix | Complete SDK/runtime race: 950 pass events; focused race ×20: 580; zero failures/skips on that source. Five behavioral negative overlays. Source chain recorded in `/tmp/magpie-sdk14-usage-lane-report.md`. |
| Native executor after nested-lease/wire fixes | Unit/codec/runtime race ×20: 300; ordinary gateway/bridge race ×3: 96; zero failures/skips. `usbox:/tmp/magpie-executor-v4-5h9oy987`, based on the recorded v1/v2/v3 archive/overlay chain. |
| Independent executor/usage closure | Unchanged independent probes plus wire-value regression, race ×20: 160; vet exit 0. `usbox:/tmp/magpie-sdk14-independent-ZABJvu/fixed-closure/`. |
| Routing with private-service projection | Scoped race ×20 across five packages, Node 115/Bun 115, five strict TS consumers, scoped vet and Linux nogui build exit 0. Source archive `578030c9bd90495e83653546f5e55611836ec37a6c66af3e1c4184625bd5ff76`. |

The executor's single-function guard-bypass overlay sends native-only
payloads to a plain provider and produces two named assertion failures.
Independent old-source probes reproduce the nested empty-policy lease
substitution and incomplete schema-3 usage metadata findings; both were
closed by unchanged probes against the fixed overlay.

The hybrid routing fixture first fails public-manifest validation and actual
installation when a private service's strategy outlives its projected action.
The fixed projection retains the full root contribution while restricting
the worker description to owned actions and their strategy metadata.

Stateful authentication lifecycle CAS has an additional actual-native
regression and independent closure: race ×20 has 60 pass events, with vet
exit 0. Disabling only the auth exception fails the continuation probes
while read-denial checks still pass. Private-service routing has an
independent race ×3 rerun with 12 pass events, focused Node/Bun checks and
vet exit 0. These preserve read-action, recall, root-config and
accepted-write invalidation restrictions.

## Combined SDK checkpoint

The combined source is tree
`592c401a94c7f62a9d0c9ba1aa28b2506d206062`, archive SHA256
`90e9ef30df9fde24b169a0943eea28c256f1a6931591507b8c4b579b14a70cff`,
based on `b83708af3`. Evidence is
`usbox:/tmp/magpie-sdk14-combined-nQJ7a3`; its `source.json` inventories
10,186 files and `post-source-audit.json` confirms unchanged source after
the gates. The temporary index did not mutate the live index.

| Gate | Terminal result |
| --- | --- |
| Complete Go SDK/protocol/runtime, race ×1 | 975 test/subtest pass events, 330 roots, six packages; zero failures/skips; exit 0 |
| Complete JS SDK | Node 115/Bun 115, zero failures/skips; exit 0 |
| Five strict TypeScript consumers | Exit 0 |
| Changed Go formatting | 68 files, empty `gofmt -l`; exit 0 |
| Whole-tree Linux cgo nogui vet/build | Both exit 0 |
| Whole-tree Darwin/Windows no-cgo nogui vet/build | All four exit 0 |

Terminal commands, environment and durations are in the adjacent
`*-result.json` files. These are SDK checkpoint gates, not whole-repository
tests. A later receiver-variable rename in
`internal/provider/extension_executor.go` is recorded separately in
`receiver-check-result.json`; documentation changes are separate from
executable-source verification.

## Pending application acceptance

The later combined application snapshot includes the corrected native
Pi/OMP owner and per-account generation fixture:

- HEAD at capture: `0dc09a544`.
- Tree: `33b2bc95f7fd3530b6d14db7c170da66fef186f9`.
- Archive SHA256:
  `7f7d95808e1c1c0a3defd4f2bdb0e2438ca3eb078e31c893e3166e0e3d263278`.
- Evidence: `usbox:/tmp/magpie-sdk14-final-5sWPZp`.
- All 10,188 source hashes and six native fixture inputs remained unchanged
  through the recorded gates.

The SDK/runtime race gate passed 975 events with no skips. Ordinary
provider/gateway/GUI/session integrations passed 717 events across race ×3;
their 63 native-fixture skips are explicit. Three separate fresh-process
native session suites each passed 95 events with zero skips. Node/Bun
each passed 115 tests, five strict TypeScript fixtures passed, formatting
was empty, and all six whole-tree nogui vet/build gates passed.

The single safe whole-repository run on that same source finished
**exit 1** after 1,879.45 seconds: 29,932 test/subtest pass events,
13,307 root passes, 104 passing packages and 172 recorded test/package
skip events. Five roots failed in three packages:

- `TestVertexExpressEditor`.
- `TestGroupFirstTokenFailsOver`.
- `TestHandedOverGatewayFinishesItsStream`.
- `TestOTelRetriesOnlyRetryableResponses`.
- `TestSentTellsMagpieFromEarlierTries`.

These original failures remain historical failures. Later commits isolate
fixture-owned POST traffic without raising expected counts:
`029eddd2a` covers the Vertex siblings and `240aab8e9` covers the
first-token helper, handover and OTLP retries. Their controlled unrelated
GETs reproduce assertion failures when only the isolation guards are
removed. Targeted race ×20, nested-group neighbors and Sent timing checks
pass after the fixes; this does not turn the frozen broad run green.

Separately, full session race testing exposed mutable-local-time access
from a stale-sweep timer and live state aliased into asynchronous cache
encoding. `708035d72` makes scheduling monotonic; `606325223` queues owned
persistent snapshots. Both have old-source/negative evidence and scoped
race/static verification. The fresh full session race gate on committed
`606325223` completed **exit 0**: 954 test/subtest pass events, 709 roots,
zero failures or race warnings, 32 explicit skip events, 131.41 seconds.
Native opt-ins were unset; their separate native suite evidence above is
not relabeled as execution on this snapshot.

That committed source is tree
`7283e9c29d194422a4ae970ae4232ff8f35244f3`, archived independently of
then-uncommitted fixture edits at
`usbox:/tmp/magpie-sdk14-main606-nsBxfN`. The baseline regression race ×3
gate on the same source passed 180 events/144 roots with no failures or
skips in 40.28 seconds. It includes provider hold/kept/clock checks,
cache snapshots, original Claude `/cd` coverage and the Vertex fixtures.

One bounded diagnostic positively attributes an observed unrelated GET to
an external `moshi-hook` process, using the exact client socket inode and
PID distinct from the Go test. It records no headers, command arguments or
secrets and kills no process. This attributes that observation only; it is
not a claim that every historical request had the same sender.

## Committed SDK 1.4 safe application acceptance

The exact committed source `08d29765db9f7da69897a5b5fff58b3216c4b2e1`
completed the safe whole-repository command **exit 0**. Its tree is
`062e0d746411ceb77ca990d2d7d149efdce14c03`, archive SHA256
`5f6e5f17717a0022c278cad59f520b5fa08854329c41a9f124184838b3406623`.
There are no overlays or later SDK 1.5 changes in this source.

| Result | Recorded value |
| --- | --- |
| Successful tests/subtests | 29,942 events; 13,316 roots |
| Successful packages | 107 |
| Failures / unfinished tests | Zero |
| Full command duration | 1,785.46 seconds; wrapper exit 0 |
| Explicit skips | 149 tests and 25 package/no-test events |
| Source integrity | All 10,191 hashes unchanged before/after |

The command is the safe `go test -json -tags nogui -p 2 -timeout=45m
-skip '^TestScriptFindsClaudeMoved$' ./...`, run on usbox with pinned
Go 1.26.8, Linux amd64 CGO1, isolated HOME/XDG and offline Go lookup.
It uses a private user/network namespace: caller UID/EUID/GID/EGID stay
1000, loopback is enabled there, and all five capability masks are zero
after setup. A real chmod-000 read fails before tests begin. No sudo,
global settings, dependency installation or process termination is needed.

Network isolation prevents the unrelated host loopback probes observed
earlier. It also adds two non-loopback-address skips. The exact same source
subsequently runs those two callback roots on the host network with
race ×3: six passes, zero failures/skips. This is supplementary coverage,
not a relabeling of the canceled host whole-suite run.

The native opt-in, platform-specific, live/browser and helper skips remain
explicit; the unsafe installed-Claude test has zero run events. This is
not a whole-repository race run or native execution on every platform.

Remote evidence is
`usbox:/tmp/magpie-sdk14-app08d-netns-a90s5p9n`. The copied packet at
`/tmp/magpie-sdk14-app08d-netns-verification-evidence` contains the exact
command/results, full raw log, all skip outputs, callback gate, namespace
proof and source audits. The verified raw-log SHA256 is
`9b6d3135bce7fa83039b125ac20d9c897ffe67676ca154d65467c3472ad7ccfc`.

Historical results remain separate: completed 33b host run is FAIL;
08d host run is canceled PARTIAL_RED; the same 08d namespace run is PASS
within these bounds. Later SDK 1.5 UI/Agent/Auth and source-adapter changes
need their own coherent snapshot and acceptance.

The whole-repository run excludes `TestScriptFindsClaudeMoved`, which can
launch the real installed app. That exclusion is explicit rather than a
passing test. Native Windows/macOS execution, live vendor parity and the
full browser matrix remain separate evidence requirements.
