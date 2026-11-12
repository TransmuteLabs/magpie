# ADR 0003 validation: executable extension SDK

Evidence checkpoint: November 12, 2026, updated at 23:11 +03:00.
Readiness: **SDK v1 implemented; scoped gates green on tree `2c9056f…`;
full acceptance NON-GREEN.** All five independent-review findings are closed.
The completed older broad run exited 1. The subsequent `Default()` raw-config
cache change passed the final current-tree regressions and native/Linux/Windows
runtime vet. It changes no SDK contract.

This is the validation companion to
[ADR 0003](0003-external-extension-runtime.md). It records evidence rather
than changing public contracts or the ADR's implementation status.
The SDK source and this record form the local implementation change set.

## Checkpoint, plans and verification tree

The continuation source is `.omx/notepad.md`. The scoped requirements and test
plan are `.omx/plans/prd-extension-sdk-v1.md` and
`.omx/plans/test-spec-extension-sdk-v1.md`. Later terminal logs and the leader's
final evidence update supersede stale “running” notes in that checkpoint.

The evidence root is `/tmp/magpie-sdk-v1-checks/`.
`snapshot.json` identifies the older broad run; `final-snapshot.json`
identifies the later reviewed code used for final scoped gates:

| Item | Recorded value |
| --- | --- |
| Initial SDK checkpoint | `000de62dc` |
| Checkout HEAD used to create the snapshot | `89fe929d6f4cd05e28d2ae823e4d5fa87165c3cb` |
| Older broad verification tree | `56827f89e95211f03e925c71e2a664f33834e863` |
| Older snapshot commit | `7b82432a317517b083401204561b2456a068c63f` |
| Older verification worktree | `/tmp/magpie-sdk-v1-verification` |
| Final scoped verification tree | `2c9056f085bb08b09687a4ba0deb4eeb0da5651b` |
| Final snapshot commit | `7376c186f7a3fd94764d8fe5dd3535c3d05681f3` |
| Final verification worktree | `/tmp/magpie-sdk-v1-final-verification` |
| Logs, runner and JSON outcomes | `/tmp/magpie-sdk-v1-checks/` |

Tree hashes distinguish the older failing broad run from final scoped
verification. The leader attributes the final SDK and GUI/OTLP runs to
`2c9056f…`; six final vet/build results are in
`final-gates/go-results.json`. Individual earlier logs do not all embed a
tree hash and retain their original scope. The later `Default()` fast-cache
change is outside this final snapshot and has its own passing evidence:
`final-current-regressions.log` and `default-cache-vet-{darwin,linux,windows}.log`.

## What each proof establishes

| Evidence | Observed result | Boundary actually exercised |
| --- | --- | --- |
| `runtime-and-sdk.log` | Terminal package passes: public Go SDK 47 distinct roots / 141 test-pass events; compatibility SDK 12 roots / 12 events; runtime 58 roots / 188 events; no failures/skips | Public contracts, wire/state/lifecycle fixtures, and runtime tests. Events include subtests; these numbers are not three repeated root runs. |
| `runtime-race.log` | Earlier runtime package pass: 51 roots / 177 test-pass events, no failures/skips | Earlier scoped race checkpoint; not an additional independent proof of all later patches. |
| `worker-first.log` | `TestWorkerInstalledAndMultiplexed` passed | Actual installed executable worker and stdio RPC; the worker implements fixture behavior. |
| `runtime-and-sdk.log`, `installed-rerun.log` | `TestBunWorkerInstalledThroughGoHost` passed in both runs, with no skip | An actual Bun child installed and invoked by the Go Manager. This is stronger than a mocked host interface, but is not a real vendor integration. |
| `.omx/notepad.md` | Reports JavaScript library runs with 60 Node and 60 Bun tests, no skips | Checkpoint-reported library/conformance evidence. Separate JS runner logs are not present among the log files inspected here; do not promote this to an independently re-read final-patch result. |
| `installed-rerun.log`, `native-e2e-final.log` | `TestInstalledGoSDKUsesOrdinaryGateway` passed; final plain log records package success | A compiled public Go example is installed, discovered by ordinary `gateway.New()`, and called through local HTTP. Native JSON/SSE, authorized Models, completed-request 409, disable, and rejection before worker startup are exercised. |
| `cli-blackbox.json` | 13 actual CLI invocations matched the recorded expected behavior | The compiled Magpie CLI exercised add/list/call/off/on/reload/inspect/rm and `plugin add` autodetection. The disabled call correctly exited 1; this is not “13 zero exits.” |
| `lifecycle-review-regressions.log` | Seven distinct runtime roots repeated three times: 21 passes, no failures/skips, terminal package pass | Focused lifecycle/host-service regressions, including cancellation of unpublished installation and wedged stdin cancellation. Review closure is established separately below. |
| [Agent/session verification record](../subsystems/extension-agents-sessions.md#scoped-verification-record-and-limits) | Original integration: 31 roots ×3 = 93 passes. Follow-up: 16 agent/session roots ×3 = 48 passes, package vet exit 0 | Executor-recorded scoped evidence, including installed ordinary flows, dotted-ID ownership isolation, selected-source identity/hash guards and existing Carry siblings. The reference records commands and limits; a separate log for this lane is not claimed here. |
| `review-delta-final.log` | All five selected packages pass under race: plugin, Library, gateway/MCP, provider/quota and CLI; no failures/skips | Final review-delta host-policy regressions, including tool replay identity, network contexts, rich hub results and the quota span fixture. |
| `bun-account-replace-host.log` | Three real Bun/Go-host runs pass under race | Accepted account replacement commits generation 2 and replacement auth; this exercises the actual Bun callback rather than only a mocked scope. |
| `final-sdk-complete.log` | All four SDK packages pass under race: `internal/protocolext`, `pkg/protocolext`, `pkg/extension`, `internal/extension`; no failures/skips | Final snapshot contract/runtime coverage: 47 public SDK roots and 62 runtime roots, plus compatibility/host tests. |
| `synctest-regressions.log` | Selected extension/OTLP/GUI regressions pass under race, count 3 | Lazy metadata-only Manager lifecycle fixes the newly discovered synctest bubble ownership fatal. |
| `gui-otlp-complete-final.log` | Both complete GUI and OTLP packages pass under race; six GUI skips are recorded | Final snapshot package coverage after the synctest fix; not a claim that every platform-dependent GUI fixture ran. |

The HTTP tests use loopback servers and synthetic vendors. Provider-policy,
account-generation, rich tool scope and CLI unit fixtures may replace
`Call`/`Invoke` or host interfaces. Such tests establish the asserted host
boundary, not a vendor's OAuth behavior, real billing, native application
acceptance, or every Go/Bun callback combination.

The historical `gateway-first.log` and `gateway-installed.log` contain
failures. They remain diagnostic evidence, not green gates. The installed
gateway test has subsequent passing evidence in `installed-rerun.log` and
`native-e2e-final.log`; do not count the initial failed run as a success or
classify it as an unrelated baseline.

## Nine-capability readiness matrix

All nine host paths are implemented and the five bounded review findings are
closed. The matrix records scoped proof and limits; it does not claim vendor
parity or a green broad suite. F1–F5 refer to the closed-finding ledger below.

| Capability | Actual production entrypoint and call path | Fixture/process proof | Remaining qualification |
| --- | --- | --- | --- |
| `inbound-codec` | Ordinary `gateway.New()` and [`Server.protocolEntry`](../../internal/gateway/protocol_extensions.go) discover [`Manager.Protocols`](../../internal/extension/inbound.go), then `remoteCall.Decode` / `remoteResponse` call `inbound.decode` / `inbound.response`. | Installed Go JSON/SSE/catalog/disable/409 roundtrip, compatibility tests and real Bun Manager fixture; F3 rich tool path regressions pass in the final delta. | Synthetic echo protocol; no real vendor codec parity claim. |
| `outbound-executor` | Provider discovery exposes [`ExtensionProviders`](../../internal/provider/extension_provider.go); ordinary routing reaches [`extensionAccount.execute`](../../internal/provider/extension_transport.go) and Manager `provider.execute`. | Installed native→provider→native Go roundtrip; executable outcome/fallback fixture; F1/F2 final identity/network regressions pass. | Real worker dispatch with synthetic providers; no billable vendor call verified. |
| `account-auth` | SDK CLI [`sdkCLIAuthCommand`](../../internal/cli/sdk_cli.go) calls [`StartExtensionAuth`, `FinishExtensionAuth`, `RefreshExtensionAuth`, `LogoutExtensionAuth`](../../internal/provider/extension_auth.go); results use account-generation CAS. Accepted tool callbacks reach host account replacement. | Go CAS/scope tests and three real Bun/Go-host replacement passes commit generation 2/new auth; F2/F4 closed. | CLI13 did not perform real login; no OAuth/vendor account parity or live credentials claim. |
| `model-discovery` | Ordinary catalog discovery uses `ExtensionProviders`, `extensionAccount.models` / `fetchModels`, `provider.models`; inbound Models projects the authorized host catalog. | Installed Go native Models request; account/model/price/effort tests; final auxiliary network-policy regressions pass, F2 closed. | Static and synthetic dynamic catalogs, not vendor discovery parity. |
| `usage-reporter` | Ordinary allowance reads reach [`extensionQuota`](../../internal/provider/extension_provider.go) and `provider.usage`; execution usage events reach the provider outcome owner. | Scoped usage/unknown-window/invalid-number tests; final delta includes the corrected `TestQuotaSpanCarriesEveryWindowField`; F2 closed. | Structured fake usage and policy assertions, not reconciliation against real vendor counters. |
| `operation-control` | Production CLI [`sdkCLICall`](../../internal/cli/sdk_cli.go) → `Manager.Call` dispatches `operation.status` / `operation.cancel`; the runtime journal records operation state. | Real Go/Bun control calls, lease/cancel/journal fixtures, native 409 and final tool replay regressions; F1 closed. | No automatic vendor status reconciliation claim; protocol cancellation does not prove successful vendor cancellation. |
| `tool-provider` | Ordinary discovery → [`RunExtensionTool`](../../internal/plugin/extension_tools.go); [`tools_mcp.go`](../../internal/gateway/tools_mcp.go) and [`extensionToolContext`](../../internal/gateway/extension_tools.go) apply caller/account guards. Hub preparation reaches the same host owners. | Real CLI tool RPC; final MCP/Library/hub/CLI policy delta passes; real Bun account callback passes; F1–F4 closed. | Many policy assertions use fake runtime interfaces. No live proxy/vendor or credential-delegation claim beyond the tested boundaries. |
| `agent-wiring` | [`All` / `local`](../../internal/agent/agents.go) → [`extensionAgents`](../../internal/agent/extension_actions.go) → ordinary `Agent` / `NativeConnection` → `agent.inspect` / `agent.plan` / checked Actions. | Installed lifecycle/manual/native-ID tests, independent ownership/preimages, and closed Go/Node/Bun descriptor parity review, F5. | Synthetic config schema; filesystem/post-apply stash limits remain documented, with no real editor parity claim. |
| `session-migration` | Native [`carries`](../../internal/sessions/carry.go) → [`CarryOver`](../../internal/sessions/carry_convert.go) → [`carryExtension` / `convertExtensionSource`](../../internal/sessions/extension_actions.go) → Actions `session.convert`; host `resumeCommand` supplies the native command. Explicit CLI conversion also uses the action service. | Installed executable converter reached through a normal native session; selected A remains paired with A's Cwd after same-ID lookup changes to B. Identity/hash, consent, create-only output, ambiguity/disable and source preservation are tested. | Only supported native resume targets and a bounded file source are automatic. Unknown formats use explicit files. The output is a fixture, not real OMP or other vendor-format parity; no real user session was migrated. |

The agent/session details and intentionally limited source guard are specified
in [the owning reference](../subsystems/extension-agents-sessions.md).
Capabilities are negotiated permissions; a module declaring one does not
receive another capability's host authority.

## Snapshot gates and non-green broad acceptance

The runner is `/tmp/magpie-sdk-v1-checks/run_go_checks.py`, operating on the
immutable verification worktree with temporary HOME/config/cache and pinned
Go caches, `GOPROXY=off`, `GOSUMDB=off`, and `GOTOOLCHAIN=local`.
`go-results.json` now records the completed older run;
`final-gates/go-results.json` records six successful final-tree vet/build gates.

| Gate | Environment/command family | Older `56827f…` exit | Final `2c9056f…` exit |
| --- | --- | ---: | ---: |
| `vet-darwin` | Native macOS `go vet ./...` | 0 | 0 |
| `vet-linux` | Linux/amd64, CGO off, `go vet -tags nogui ./...` | 0 | 0 |
| `vet-windows` | Windows/amd64, CGO off, `go vet ./...` | 0 | 0 |
| `build-darwin` | Native `go build` | 0 | 0 |
| `build-linux` | Linux/amd64, CGO off, nogui cross-build | 0 | 0 |
| `build-windows` | Windows/amd64, CGO off, cross-build | 0 | 0 |
| `test-nogui` | Full safe subset of `./...`, command below | **1; 2197.36 s** | No new whole-repository run claimed |

The macOS logs contain deprecation/linker/SDK deployment-target warnings
despite exit 0. Builds and vet do not establish execution on every supported
operating-system version.

The completed older full test command was:

```sh
go test -tags nogui -timeout 30m -p 4 \
  -skip '^TestScriptFindsClaudeMoved$' -json ./...
```

The skip is explicit because that fixture starts the installed
`/Applications/Claude.app`. It was not rerun for this document and must not be
silently reinstated. The explicit skip is unchanged; the completed safe
subset is non-green.

| Older broad failure | Classification and final evidence |
| --- | --- |
| `TestAlmaProvidersSeenDecodeOnce`, `TestPrimeAgent`, `TestPluginSOCKSProxy`, `TestMarshalRawIsEncodingJSON`, `TestMarshalSizedIsJSONMarshal` | Baseline failures identified by the leader's base comparisons; not silently fixed in this SDK lane. |
| CLI `TestEveryRouteHasACommandOrIsNamed`: `POST /api/settings/codex-open` | Also reproduced on base `89fe929d6` in `baseline-route-parity.log`; baseline route-parity failure. |
| GUI/OTLP fatal “close of synctest channel from outside bubble” | **New SDK regression**, fixed by lazy cold Manager lifecycle. `synctest-regressions.log` passes under race/count 3; complete GUI and OTLP packages pass in `gui-otlp-complete-final.log` on the final snapshot. |
| `TestQuotaSpanCarriesEveryWindowField` | **New SDK integration fixture failure**, fixed by updating the expected fields fixture. The quota delta passes in `review-delta-final.log`. |
| Gateway package alarm after 30 minutes | Suite-wide timeout, not proof that an individual test hung. At the alarm, `TestKeptAliveChatErrorNamesTheModel` had run about 1 second and its current subtest 0 seconds. |

The global alarm belongs to the gateway test binary's total suite budget.
It must not be attributed as a 30-minute hang of its last active test.
Scoped final successes close the identified SDK regressions; they do not
turn the older exit 1 into a green full-suite result. Full acceptance remains
**NON-GREEN**.

## Five independent-review findings: closed

Avicenna closed all five findings and returned **APPROVE for the bounded
scope**: reviewer-reported 16 Go, 4 Node and 4 Bun checks, with gopls and
type checks clean. Boole, Hegel and Harvey's fixes/proof handoffs are frozen.
Host and CLI network-policy work is grouped as F2 below.

| ID | Finding / owner | Closure evidence | Status |
| --- | --- | --- | --- |
| F1 | Tool operation idempotency / replay identity — Boole | Reviewer closure plus passing `TestExtensionToolNativeRequestReplayAfterReauth` and `TestExtensionToolNativeIdentityIsolation` in `review-delta-final.log`. | **CLOSED** |
| F2 | Effective host/tool and CLI network policy — Boole + Hegel | Reviewer closure; final delta passes account network context, held-network refusal, direct-call policy, `TestSDKCLICallAppliesSelectedNetworkPolicy` and `TestSDKCLIHeldNetworkPolicyDoesNotInvokeModule`. These assert the host policy boundary, not live vendor proxy egress. | **CLOSED** |
| F3 | Rich native/hub tool policy and authority — Boole | Reviewer closure; final MCP/Library/hub delta passes `TestExtensionToolsHubPreservesRichResultAndPolicies`, `TestExtensionToolsHubRichPathKeepsScopeAndGuards` and legacy-result compatibility. | **CLOSED** |
| F4 | Bun accepted account-replacement callback parity — Harvey | Reviewer closure plus `bun-account-replace-host.log`: three actual Bun/Go-host passes under race; the fixture asserts generation 2 and newly stored auth after accepted replacement. | **CLOSED** |
| F5 | Agent descriptor validation/negotiation parity — Harvey | Reviewer Go/Node/Bun parity and gopls/type approval; final SDK package run passes `TestRichActionMetadataValidatesActualPathsFieldsAndSessionRoutes` and immutable metadata tests. | **CLOSED** |

Closed findings retain the fixture/vendor limits in the capability matrix.

## CLI artifact and user build requirement

The black-box CLI artifact is
`/tmp/magpie-sdk-v1-checks/magpie-sdk`, built from SDK-containing source.
It is a compiled test artifact, not the installed Magpie application.
An older installed binary is not made SDK-capable by installing an extension.
Users must run a build or release containing this SDK.

For example, using that compiled artifact:

```sh
/tmp/magpie-sdk-v1-checks/magpie-sdk plugin sdk list --json
/tmp/magpie-sdk-v1-checks/magpie-sdk plugin sdk add /absolute/path/to/module
/tmp/magpie-sdk-v1-checks/magpie-sdk plugin sdk call sdk-echo tool.call \
  '{"id":"echo","arguments":{"text":"CLI worker"}}'
```

The call example assumes the echo module was installed from its distribution.
The thirteen recorded CLI calls prove local management and a tool RPC; they
do not prove all CLI auth/action/network combinations. This work did not
install, restart or replace a user's application, contact real vendor accounts,
publish a release, or perform a real account/session migration. The ZCode
migration hold remains in force.

## Final integration record

| Final gate | Outcome |
| --- | --- |
| Reviewed code snapshot after F1–F5 | Tree `2c9056f085bb08b09687a4ba0deb4eeb0da5651b`, `final-snapshot.json` |
| Review closure and targeted deltas | All five CLOSED; final review delta, real Bun callback, four-package SDK, synctest and complete GUI/OTLP logs listed above pass |
| Older complete safe-subset `test-nogui` | Finished exit 1 after 2197.36 s; failures and gateway suite alarm classified above |
| Final snapshot cross-platform vet/build | All six exit 0 in `final-gates/go-results.json` |
| Later working-copy `Default()` raw-config fast cache | `final-current-regressions.log`: 63 passing test/subtest events under `-race`, including installed native Go tools, worker lifecycle, real Bun callback, provider replay and quota fields. Runtime vet on macOS/Linux/Windows exits 0. |
| SDK readiness / full acceptance | SDK v1 implemented with green scoped gates on the identified snapshot; full repository acceptance NON-GREEN |
| Local source disposition | Implementation and evidence committed together; no push, release or installed-app replacement |

This document was prepared by reading the checkpoint, source entrypoints and
existing artifacts. No broad test, build, installed application or vendor
operation was executed to produce it.
