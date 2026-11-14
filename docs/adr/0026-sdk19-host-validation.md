# SDK 1.9 combined host checkpoint

Date: November 14, 2026.
Status: combined synthetic host acceptance completed; application installation,
actual platform signing and domain-module acceptance remain separate.

## Source and deployment boundary

The tested implementation is commit
`46756a48c9c9f204a8369638dc63cbcb83d02031`, tree
`db40ddbf8944b54f9f689d444644af4c3bc76881`, including upstream
`e6e0585bd3d92a79dab08f0955789a5801382b71`.
Commit `8e4140523` saved every corrective tracked and untracked source file
before that merge. One host branch and worktree remain; the recovery stash
was neither applied nor deleted.

The first deployment is Magpie with the SDK and **no external plugins**.
This record does not authorize or claim an installed-app replacement, a
release publication or an account migration. The existing fork update feed,
workflow and [local macOS signing](../subsystems/app-updates.md) remain owned
by their ordinary host paths.

The physical verification copy is
`usbox:/tmp/magpie-sdk-final-base-eg014lri/source`.
Its archive SHA256 is
`e188abd5d115c7da3167a6296e7d07f9bd31766a738ea4e6326ff093e4a74a61`.
All 10,751 inventoried source hashes and normalized file modes matched after
verification. The Linux host binary SHA256 is
`32fbdeabd20ed3059f61a91f7614a51a2ac9b908b12dd637c204a82918a28256`.
Subsequent documentation changes do not alter this implementation identity.

## Verification environment

All builds and tests ran on `usbox`, using Go 1.26.8 and existing pinned
dependency caches with `GOPROXY=off`, `GOSUMDB=off`, `-mod=readonly` and
temporary HOME/XDG directories. A descendant network guard rejects external
connections while allowing synthetic local peers. Its external-denial and
local-address calibration ran before the gates.

The broad suite and accounting runs additionally use isolated network/user
namespaces. Native positive ownership tests instead retain observable host
ownership. The hidden-ownership negative runs inside its required namespace.
No real account, credential, vendor request or installed agent configuration
was used.

PASS counts below are Go test events, including parent tests and subtests;
they are not counts of unique test functions.

| Gate | Terminal result |
| --- | --- |
| Safe broad `go test -tags nogui ./...` | Exit 0; 31,277 PASS events, 109 passing packages, 1,522.83 seconds |
| SDK, codecs, runtime, routing and window metadata with `-race` | Exit 0; 1,404 PASS events; no race warnings |
| Ordinary provider/gateway/agent/Library/GUI/CLI/session SDK owners with `-race` | Exit 0; 589 PASS events; no race warnings |
| Complete `internal/usage`, `plugins/middleware` and `plugins/otel` packages with `-race` | Exit 0; 790 PASS events, no skips or race warnings |
| Update/feed/mirror/signing owners with `-race -count=20` | Exit 0; 820 PASS events, no skips or race warnings |
| Empty-catalog routing/startup with `-race -count=20` | Exit 0; 320 PASS events, no skips or race warnings |
| Actual native/composite/window/auth-owner lifecycle | Exit 0; 10 PASS events, no skips or race warnings |
| Native image generation, dependency, capacity and repeated-reinstallation opt-ins | Exit 0; 4 PASS events, no skips or race warnings |
| Ordinary exec/native System One/Decisions owner, three repetitions | Exit 0; 75 PASS events, no skips or race warnings |
| Hidden native dependency ownership and separate worker-auth fixture | Exit 0; 1 and 2 PASS events respectively, no skips or race warnings |
| Native JSONL Source lifecycle/revocation | Three fresh processes, each 60 PASS events, no skips or race warnings |
| Public JS SDK on Node and Bun | Both exit 0; 203 tests per engine, no failures or skips |
| Strict TypeScript and actual-source GUI function tests on both engines | Exit 0; 16 GUI function tests per engine |
| Repository Go formatting | Exit 0, no unformatted files |
| Linux CGO and Darwin/Windows no-CGO `go vet` and `go build`, `-tags nogui ./...` | All six gates exit 0 |

The broad suite explicitly excludes `TestScriptFindsClaudeMoved`, which can
launch an installed real application, and six native ownership/lifecycle
fixtures run separately above. Its 216 skip events also include platform,
opt-in and no-test-package cases. The owner suite's optional module-specific
native migrations retain their separate domain-package acceptance; a skip is
not a pass.

The complete receipts and logs remain under
`usbox:/tmp/magpie-sdk-final-base-eg014lri`.
`broad-network-result.json` SHA256:
`cf39f2f6f88ecbdd1029416ca5f2075e914821a1ac539960e9458f3a515c6a63`.
`final-runtime-gates.json` SHA256:
`31335bc72306db8b7fea02e30931cbcddd50b68f56ff5f5155f88904c28ffee2`.

## Empty-catalog executable

The actual compiled Linux binary, with Bun unavailable and both extension
catalogs empty, passed CLI version/help and both plugin-list commands.
Its ordinary HTTP page, settings, plugin metadata, update and signing-status
routes returned 200. No external child worker or native module image appeared.
The permanent gateway regression additionally exercises model discovery and
one ordinary execution against a configured synthetic HTTP provider.

The fixture reproduces a queued request to a retired listener before reusing
the address and identifies its own execution requests explicitly. Unmarked
GETs and POSTs still fail; there is no blanket background-request exemption.
The original historical intermittent sender remains unattributed.

## Behavioral negatives and review

An independent bounded review approved the root-config metadata stamp,
Anthropic SDK/core advisor-policy boundary and display-only agent sync
authority on this exact physical tree. Its unchanged regressions passed
45 race-enabled events. Restoring prior behavior produced named failures:

- External root configuration CAS retained stale held provider catalogs.
- Legacy Anthropic shaping changed SDK caller thinking/tool/cache input.
- Display-only model renaming unnecessarily renewed native ownership.

A separate deliberate mutation placing the SDK break before advisor policy
failed five authorization/mapping cases while its allowed-bare control passed.
The paid-task component-binding bypass was independently closed with a
different unchanged behavioral probe. Exact public import-boundary mutations
also fail when native-only OS imports escape their declared two-file scope.

The combined boundary review packet is
`usbox:/tmp/magpie-final-boundary-review-20261114.y6ovIZ4m`;
its `result.json` SHA256 is
`b6c8f56f04a50d5e2f6bc9079363c32f493ce16f601921f5532fd46a0e6c8cd7`.
Earlier red runs and evidence setup failures remain recorded separately.

One Source stress invocation used `-count=3` in a single process. Its third
iteration exhausted the intentional 64-image process-lifetime limit after
177 PASS events and failed two late fixture setups before their assertions.
Three separate processes preserve all 60 original lifecycle/revocation
events per run and pass. Neither production limits nor ownership checks were
changed to accommodate the harness.

## Limits

Darwin/Windows checks above are no-CGO cross-builds and vet, not native runtime
or GUI acceptance. Real macOS Keychain/codesign, permission continuity and an
installed-app update remain for the live test phase. The Linux update tests
protect policy and ordinary route registration; they do not execute the
Darwin-only signing implementation.

Rendered browser coverage and the five domain packages have their own
acceptance records. In particular, the Devin hot-configuration paid-task
fixture still requires a separate cause and closure; its passing cold-start
control does not make that failed case green.

API 1.9 is the implemented finite contract set described in the
[surface census](../subsystems/extension-surface-coverage.md). This host
checkpoint does not turn the census's remaining optional owner integrations
into implemented SDK features or establish live vendor compatibility.
