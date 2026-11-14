# Consolidated SDK 1.6 host verification

Date: November 14, 2026.

Status: scoped host gates pass; repository-wide acceptance remains incomplete.
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
implementation in core. The signing regressions above exercise policy,
staging and dispatch with fixtures. Real Keychain access, application
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
Several vendor fixtures observed two unexpected `GET /` calls; their common
origin is under investigation. No routing assertion or vendor call count was
relaxed to call this run green.

This broad result remains red until its cause is fixed and the affected and
repository-wide gates complete on a new frozen source. The unsafe installed
Claude launcher test remains explicitly excluded.

Independent Source/native-account-operation union review is also separate
from the author gates. Domain modules retain their own installed-host,
provenance and vendor-format acceptance records. Generic SDK owner adapters
still listed as remaining in
[Extension surface coverage](../subsystems/extension-surface-coverage.md) are
not completed by this verification.
