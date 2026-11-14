# Consolidated SDK 1.6 host verification

Date: November 14, 2026.

Status: scoped host gates and the explicit split safe repository gates pass.
The unsafe installed-app launcher and existing fixture opt-ins remain
declared exclusions; native macOS GUI, deployment and vendor acceptance
remain separate.
This record separates the first deployment of Magpie with an empty external
plugin catalog from later installation of our domain modules. No application
was installed, restarted, released or pushed during these checks.

## Source and preservation

The consolidated executable source is commit
`d07078c7926daf6f33af09c40297c941bd39da92`, tree
`ed28dfd65eca68d15e9400b56660bbbf5e7bc319`: 10,420 inventoried files.
Its source archive SHA-256 is
`e4a52c8ebf86beaaecbe538f48cff72fb1af1e0b93d5e0bac89e934ccf727969`.
Evidence is retained at
`usbox:/tmp/magpie-sdk-consolidated-2yztvlcf`.

The Source and native-account-operation implementations share this one host.
Model catalog persistence, canonical Library target identity and agent Env
config expansion are already included. No domain-specific built-in Devin
registry or experimental protocol selector was restored.

The separate recovery audit verified 252 archived file versions and
reconstructed four original Git indexes, including 108 staged versions.
Recovered implementations were compared with current behavior. Necessary
changes are already integrated or have newer equivalents; the archived
prototype codec belongs in the standalone Devin module. Backups and original
patches remain preserved. This preservation audit is not vendor acceptance.

## Environment and scoped gates

All execution took place on `usbox`, using isolated HOME/XDG directories,
pinned existing Go caches and offline dependency resolution. No live vendor,
account, Keychain or installed application participated. Source files remained
separate from generated native libraries and the verification host binary.

| Gate on the frozen source | Terminal result |
| --- | --- |
| Public SDK, protocol SDK and extension runtime, race | 1,177 pass events; five explicit native fixture opt-in skips |
| Ordinary SDK host owners, race | 339 pass events; 48 explicit fixture opt-in skips |
| Complete Sessions package, race | 976 pass events; 54 explicit opt-in skips |
| Model catalog and generation regressions, race, 20 repetitions | 700 pass events; no failures, skips or races |
| Update source, signing and update dispatch, race, 20 repetitions | 340 pass events; no failures, skips or races |
| Complete JS SDK | Node 139 and Bun 139 passes; no failures or skips |
| Strict TypeScript and Go formatting | Exit zero |
| Linux amd64 CGO-enabled `nogui` vet and build | Both exit zero |
| Darwin arm64 and Windows amd64 CGO-disabled `nogui` vet and build | Four exits zero |
| Native Source ordinary owners, three fresh processes | Each process: 43 pass events; no failures, skips or races |
| Nonloopback local Claude callbacks, race, 20 repetitions | 40 pass events; no failures, skips or races |

Counts include subtests and repetitions, not only distinct test functions.
Cross-compilation does not prove native macOS GUI or Windows execution.

## Magpie without external modules

The actual built CLI and gateway passed `empty-core-smoke.py` with empty SDK
and legacy catalogs and an unavailable Bun executable. The result is retained
as `empty-core-smoke-result.json`. No module installation or vendor migration
is required for that ordinary host path.

The first rollout keeps the fork update feed and local macOS update-signing
implementation in core. The signing regressions above exercise the GUI/CLI
control surfaces and update dispatch with mocks. Darwin-specific signing
implementation and tests were cross-compiled by vet; they were not executed
on macOS in this run. Real Keychain access, signed staging, application
replacement and macOS permission continuity were not exercised; their
contract remains in [App updates](../subsystems/app-updates.md).

## Preserved failures and corrected verification setup

The first wrapper matched `core-build` and `source-build` as cross-platform
mode names and failed before invoking Go. Those launcher errors remain in
`harness-original-errors.json`. The corrected wrapper built both the host
and native fixture successfully; they were then used for actual checks.

Repeating the entire native Source matrix three times in one Go process
exhausted the intentional 64-image capacity during fixture installation.
Native Go libraries cannot be unloaded, and these tests create fresh authority
namespaces. This is not evidence of a Source publication race. The failed
`source-native-result.json` and log remain intact. Three separate test
processes, each with `-count=1`, then passed without changing runtime source,
image authority or the capacity limit. Their results are
`source-native-repeat{1,2,3}-result.json`.

The original network guard permitted only loopback and blocked two existing
callback fixtures addressing the server's own assigned nonloopback IP.
The corrected verification guard additionally permits exact locally assigned
addresses. Every run proves that its own nonloopback listener is reachable
and an external destination is refused. It does not grant access to a LAN
range or disable the guard. The actual callback tests passed 20 repetitions.
The guard is for trusted test fixtures, not adversarial socket-address memory.

## Repository-wide result and remaining work

The original safe broad command completed with exit one after 1,368.16 seconds:

```sh
go test -json -mod=readonly -tags nogui -p=6 -timeout=45m \
  -skip '^TestScriptFindsClaudeMoved$' ./...
```

It recorded 30,497 pass events, 206 skips and no race warnings. Besides the
two guard-blocked callbacks described above, failures occurred in
`TestAgentGoneInTheRetryPause`, `TestCappedSubscriptionGivesWayInGroup`,
`TestCodexSubagentOnAnotherProviderPutOnTheModelSet`,
`TestNestedGroupPatienceInnerOff`, `TestLastOneLeftIsTriedAgain`,
`TestClaudeWarmedReadAgain` and `TestSignInMovesSaysWhatSwitchDoes`.
Several vendor fixtures observed two unexpected `GET /` calls. A calibrated
diagnostic overlay reproduced that shape in the provider fixtures and traced
both sender sockets, through `/proc/net/tcp` and process file descriptors, to
an external `moshi-hook` process. The test binary's client recorder saw no
outgoing root request. The identical scoped tests passed 40/40 in an isolated
network namespace with no root requests. The original broad run did not
capture sender identities, so this establishes the reproduced cause, not
retrospective attribution of every broad failure. No external process was
stopped and no routing assertion or vendor call count was relaxed.

This original broad result remains red in the retained evidence. The affected
and repository-wide follow-up gates on a new frozen source are recorded below.
The unsafe installed Claude launcher test remains explicitly excluded.

Independent Source/native-account-operation union review approved the bounded
composition and authority paths on the same frozen source. The corrected
fixture input passed 918 Go race events and 21 tests each in Node and Bun.
The original lifecycle probe named `review.union` while its reused helper
sent scope `fixture`; the runtime correctly rejected the mismatch. Unchanged
probe bytes with a matching external fixture identity pass. Historical
validator and missing-capability negatives retain named assertion failures.
The full report is
`/tmp/magpie-source6-independent-kbw52oxm/final-d070/REVIEW.md`; its evidence
archive SHA-256 is
`ce138eabf98baac911d80a1e16df816dcd3f90ec8e35a85743a7cbe75af309fb`.

This bounded review is separate from whole-application acceptance.
Domain modules retain their own installed-host,
provenance and vendor-format acceptance records. Generic SDK owner adapters
still listed as remaining in
[Extension surface coverage](../subsystems/extension-surface-coverage.md) are
not completed by this verification.

## Follow-up on the shared base

Frozen tree `f536d8202e40aae4b0fc36630aa3c1eb496d58d5` contains the same
production implementation, the two provider fixture lifecycle corrections,
and a permanent empty-catalog gateway regression. Its 10,421-file archive has
SHA-256 `388fb72a3644c68363832e0b83a7a2346ad896f5afe5b648fafefc07a7448d42`.
Evidence is retained at `usbox:/tmp/magpie-sdk-final-base-6bb0rob6`.

The formerly failing scoped tests passed 20 repetitions in a private network
namespace: 1,240 pass events, no failures, skips or race warnings, exit zero.
Linux, Darwin and Windows vet and formatting also exited zero. The actual
CLI/web smoke was repeated with the newly built host: empty SDK and legacy
catalogs, no Bun, no native module image and no executable plugin worker.

The first broad run in that namespace cannot serve as native-positive
acceptance. Its UID map exposes only user 1000 and hides the root ownership
needed to prove system dependency equivalence. The first hybrid native
instance worked; later independent image authorities correctly returned
`native_restart_required`. That run remains interrupted/red with exit -15;
its log, partial result and exact owned-process stop record are preserved.
Neither production loader checks nor fixture assertions were relaxed.

The independently reviewed replacement uses this explicit coverage union:

| Gate | Coverage and completed result |
| --- | --- |
| `broad-network` | Ordinary `./...` in the private network namespace: exit zero, 107 passing packages, 30,501 pass events, 205 explicit skips, no failures or race warnings; 1,283.2 seconds |
| `native-owners-race` | Four native-positive roots in the initial user namespace: ten pass events, no skips or races |
| `native-hidden-isolated` | Hidden-ownership refusal alone in a fresh user namespace: one pass, no skip or race |
| `window-auth-worker-isolated` | Authentication worker subcase in the private network namespace: two passes, no skips or races |

The broad command excludes exactly `TestCompositeHybridBundle`,
`TestNativeSharedLibraryLifecycle`, `TestExtensionWindowHybridRuntime`,
`TestWindowAuthOwnerPollProjectionThrottleAndPublication` and
`TestNativeImageHiddenSystemOwnershipFailsClosed`; these are covered by the
other rows. `TestScriptFindsClaudeMoved` remains the separate unsafe
installed-application exclusion. Other opt-in skips retain their existing
meaning. Native-positive results record the actual initial UID mapping
`0 0 4294967295` and native/helper process identities, not only a runner label.

The independent coverage report is
`/tmp/magpie-empty-catalog-review.rHKbv1/ACCEPTANCE-SPLIT.md`.
Its empty-catalog review also confirmed 80 scoped race pass events and a
behavioral mutant which makes the unchanged regression fail with HTTP 503
on ordinary model discovery. A subsequent test-only cleanup uses the existing
plugin-settle barrier and stops the server's idle timer. That exact final
test file has SHA-256
`0611c081eb371d3b573dc6598b6aec1b75651d5634f98bada37ac0e8a37a4d66`.
Its overlay on this immutable source passed the four scoped gateway tests
20 times: 80 pass events, no failures, skips or races; gateway vet and
formatting also passed. Evidence is retained at
`usbox:/tmp/magpie-empty-core-cleanup.kwYzYFHn`.
Independent source closure is
`/tmp/magpie-empty-catalog-review.rHKbv1/CLOSURE.md`.
No production implementation changed in this follow-up; its test-only
verification is separate from the frozen broad run.

The final frozen-source audit checked all 10,421 files with no changes.
The later delivery-plan/authoring-guide updates and validation text are
documentation-only; the final gateway cleanup has the separate scoped proof
above. This closes the SDK-alone Linux `nogui` acceptance slice. It does not
publish a release, install or restart the macOS application, execute the real
Keychain, make incomplete vendor modules ready, or supply the remaining
optional SDK owner adapters.
