# Magpie and SDK acceptance

## Scope

The first rollout starts Magpie with empty SDK and legacy plugin catalogs.
Each companion module has its own release and client acceptance.
The SDK distribution uses the application release version. Wire API
`1.12`, native ABI `1` and native account codec `0.5` describe independent
compatibility contracts.

## Rollout candidate

The current source includes upstream `48fffb60c581dcc3e2116835a36b8600b8aae188`.
Packet `upstream-48fffb60-safari.1DrhaQ7G` records the SDK projection
`d6a8500b08d6cf826ec94efb03920dbcc7565822` with 11,352 files and the fork
projection `28c410ea1e777225f16c561451c704afa78b4907` with 11,367 files.
The browser inventory has 1,618 files. The update feed and local macOS
signing policy are in the fork projection.

The provider editor waits for its Advanced headings before drawing.
After an exhausted load it still opens, and a later opening retries.
About leaves both update-source rows in place and creates the picker in the
lazy update module. The uninstall module resolves the current button before
confirmation, including when Settings redraws during the import. Settings
keeps notification and search warnings visible using an explicit row
class compatible with Safari 15. The script inventory recognizes a lazy
import with a fixed module path and a cache-busting query. Astra 6.2
approved these changes on source review. Both projections passed all
99 targeted Chromium and WebKit checks and the core script budget under
the race runner.

Vet and build passed for both projections on macOS arm64, Linux amd64
and Windows amd64. The restart and updater checks passed 40 race test
events on the SDK projection and 82 on the fork. The Library checks
passed 96 race test events on each projection.

The preceding packet `upstream-48fffb60-signing.xyaS99Q2` retains the
Masking warning visibility failure and the local signing receipts. Its
full updater race run passed 262 test events, with one skip. With local
signing enabled, the update feed must use verified HTTPS throughout its
redirect chain. The chosen proxy still applies. The earlier broad Go
failure in the search heartbeat test did not reproduce in twelve
isolated repetitions. The broad successor run remains required.

The current candidate still requires packaging, empty-catalog, broad Go
and full browser receipts. A live update using the selected Keychain
identity remains a separate check.

### Previous tray candidate

Upstream `de321cc92e72c28e6ec1c95859d2bd30ced768ab` is recorded in packet
`upstream-de321cc9.6M24ucLU`, with SDK projection
`1246e208765e1163fa368d6598a5988b5f688b19` to 11,340 files and fork
`302657694817b8e5bf1d17b315c40a9666d458ee` to 11,355 files. The full
browser inventory had 1,612 files.

The integrations preserve the upstream platform-specific tray labels,
the refresh rule for on-demand allowance reads, and Settings search.
The tray window picker retains the SDK account-specific choices, including
choices temporarily absent from the current account. The GitHub token
rejection fixture now exercises a distinct token for each repetition.

The preceding restart regression run on upstream `98355866` recorded
40 race-enabled pass events in the SDK projection and 78 in the fork.
The focused browser runs for restart, routing and provider updates also
exited successfully there. Independent review approved the restart guard
and the tray integration on upstream `fefab53d`. The focused Go tray
run recorded 10 pass events per projection at `fefab53d`.

### Previous Gemini decision candidate

That source includes upstream `32a1818681bb0112bd63a7011c11c6bf32814381`.
Packet `upstream-32a18186.BFUhQakx` binds the SDK projection
`6ca8b9df451f56e07bc83976be0ba127270f714d` and the fork
`e4b85b4a1e81278267fe66943eb84e199ddce6e6` to file manifests.
Gemini Decisions uses the provider's decision signing protocol. The
gateway retains its bounded response read, close error handling, SDK
usage and terminal outcome handling. The decision and account regression
run recorded 275 race-enabled pass events on the SDK projection.
Independent review approved the conflict resolution.

The broad Go run on `0beb5dea` passed 112 packages, with 33,838 pass
events and 219 skips. The current projection adds the Alma documentation,
the two SSE regressions, the quota display change and Gemini Decisions.
The quota change passed 50 focused browser tests plus the Go script
budget and asset typing checks. The full browser run binds all 1,606
files to the current projection's manifest.

### Previous packaged candidate

The previous packaged candidate includes upstream `2c748531a68167c14b1a08c24edd83660e0627b4`.
Packet `upstream-2c748531.q8LxPQNJ` holds the SDK projection
`e2334a3c0dd447a0d85c97a6da9a07d2a3c83c4a` and fork tree
`e96e23ea7584db4192b651b34dbc732b483dbe03`. The two additional Go test files
in `idle-final.j4vIno84` produce projection
`888e2392f59beae7c6bbe09a2a26bcb86f1aec` and fork tree
`ee8711d563478972d7bf988ac78cc15f86584bfd`.

The earlier PR platform and browser checks used projection
`74d99cf363498819d3efcef6e07dd0cb6d3cbe77` in `upstream-0beb5dea.NZzCcqF7`.
Its difference from `e2334a3c` is one paragraph about Alma's local skill
precedence in `docs/subsystems/skill-router.md`. Independent review verified
that the paragraph leaves the Go and GUI inputs intact. The PR projection
passed the platform matrix and 49 focused browser tests in Chromium and
WebKit.

The fork passed vet and build for macOS arm64 and the Linux and Windows
amd64 cross targets. All 47 local-signing fixture tests passed. The Darwin
certificate reader uses `--extract-certificates=<prefix>`, the form
verified against the system `codesign`. Its read-only probe and failing old
invocation are recorded in the predecessor packet's `signature-regression`
directory. The corrected call passed ten race-enabled repetitions.

The macOS arm64 ZIP has SHA-256
`50c526d95f202b2b5d5ade2e497dd35e85ef4abb167e1dd01901585e0f9191f1`.
The extracted application passed strict ad-hoc signature verification and
started with empty SDK and legacy catalogs. It completed one loopback
inference request, returned 404 for an uninstalled native route, and exited
normally with both catalogs still empty. The smoke receipt binds the
binary hash and source manifest. The later fork tree adds only Go tests.
The update with an enrolled Keychain signing identity remains a live test.

The shared SSE timeout regression covers an accepted execution before and
after model output reaches the client. It checks for one terminal error,
one dispatch and matching 409 statuses in Watch and the usage ledger.
Both cases passed three race-enabled repetitions on the SDK projection.
Restoring the premature timeout error produced two terminal events in
the streamed case and failed the new test. The same packet retains the
passing and failing receipts.

## Earlier baseline

The earlier November 17, 2026 baseline uses upstream
`889991b15f9ed3af3ed4e7e783dc832ef9c3e929`. The upstream review projection
and the application distribution have separate source trees.

| Build | Tested Git tree | Manifest files |
| --- | --- | ---: |
| Upstream SDK projection | `47de090e5ecd937615d145b3a5dc7d7317be03c0` | 11,318 |
| Application with fork update policy | `ce05fb098520b8b6ac0dc65a0f6e34f1b9f44715` | 11,333 |

The release workflow, repository-specific settings and local update-signing
policy belong to the fork. The common SDK projection retains the public
contracts, generic host adapters and their tests.

## Host verification

| Check | Recorded outcome |
| --- | --- |
| Broad Go suite on the SDK projection | Go 1.26.8 on macOS arm64, `-tags nogui`, exit 0. 112 packages passed with 33,795 pass events and 219 skip events. |
| Broad Go suite on the fork | Same toolchain, flags and exclusions, exit 0. 112 packages passed with 33,850 pass events and 219 skip events. |
| Explicit broad-suite exclusions | `TestTreeRSS`, `TestCombinedOutputOwnAnswersForItself` and `TestScriptFindsClaudeMoved` passed 20 repetitions in their guarded fixtures. The Desktop fixture isolates installed-app discovery. |
| Platform checks | Both trees passed vet and build for macOS arm64, Linux amd64 and Windows amd64. Linux and Windows used cross-compilation with CGO disabled. |
| JavaScript SDK | Node reported 347 passes and Bun reported 282. Both conformance runs and strict TypeScript checking passed. |
| Targeted races | The SDK projection recorded 21 pass events and the fork recorded 68 for the changed provider, Settings and updater paths. |
| Rendered GUI | The focused Chromium and WebKit suite passed 96 tests on the SDK projection and 136 on the fork. The separate full 1,603-file browser gate is required for PR acceptance. |
| Fork publication gate | Offline fixtures verify that release eligibility requires a successful Test run for the exact release commit. |

After source freeze, the validation-link repair and the native OMP test
changed both trees. The OMP assertion now compares the canonical destination
path so the macOS `/var` alias resolves to the same file. Three repetitions
of the ordinary native OMP cases passed with that test update.

The first full browser attempt exposed a keyboard focus loss in sidebar
tabs. A regression test delays the icon module until About has focus and
presses Enter after the list redraws. Both engines reproduced the failure
before the fix and passed with focus restoration. A nearby animation
fixture now holds the opening at its midpoint through the redraw. Both
engines pass the fixture, and removing the row-size rule still fails its
selection geometry assertion. The two application trees above predate
the sidebar fix.

Behavioral controls cover selected-account callback admission, final refusal
and retry-budget decisions, credential redaction, settings interactions and
native session export. The retained logs contain both the original failures
and the successful successor runs.

## Upstream refresh

The successor includes upstream `e1c186cfee68e124a05defb621a4f435a457b1d7`.
Its SDK projection is `b800a6a01698b5c361e26a58c89135649e4a42ae`.
The corresponding fork tree is `78e84cd87ccf206abae16bbd4fca87ab84e9a8ef`.
The Codex catalog update preserves the separate model-list behavior for
SDK accounts. The provider and gateway checks recorded 298 race passes,
including twenty repetitions of the two repaired Codex fixtures. Vet also
passed for both packages.

The focused browser checks for Claude settings and sidebar navigation
passed all 46 tests across Chromium and WebKit. The provider-options VM
fixture passed all 14 tests, including the dirty-state check after the
rendered control changes. The receipts for this refresh are in
`upstream-e1c186cf.PC5Nb6s5`.

The broad Go run for this refresh passed 112 packages with 33,684 passing
test events and 219 skip events. It used the same explicit exclusions as
the earlier run. The PR and fork passed vet and build on macOS arm64 and
the Linux and Windows amd64 cross targets.

The GUI controller stopped admitting batches after the upstream branch
advanced. Both admitted batches passed 747 tests across 40 files. The
source and process closure checks passed. `FINAL-RESULT.json` records
incomplete coverage in `final-fixtures.RcM19ZG5/gui-bound-6i9mqwnc`.

The next refresh includes upstream `69233d6bcfda248a17b613a199e0f2735b0a3e08`.
The packet `upstream-69233d6bc.DeDpbZAi` records SDK projection
`d7fc94ab7c028468254e4167c83806dbbbbfc004` with 11,321 files and fork
tree `45847e26d67295e28e58e1de5d6aaf6a2dd52808` with 11,336 files.
Both exports were checked against their Git inventories, blob hashes
and modes. The working fork differs from the frozen tree in this
validation record.

This update adds MCP server runtimes to the Docker image and a Library
diagnostic for a missing Chrome executable. The fork keeps its Docker
workflow restriction alongside the upstream workflow update. The GUI
inventory now has 1,604 files, including the MCP diagnostic test. The
Library suite and twenty race repetitions of the two browser diagnostic
Go tests passed on the new SDK projection.

The first focused JavaScript run found a missing navigation timeout in
`scroll-positions.test.cjs`. The fixture now gives page loads 60 seconds
and keeps the five-second interaction timeout. The successor differs
only in that fixture. Its PR tree is
`77b63eee16dacd3fce94536422170f6a0497a5e5` and its fork tree is
`717c3cf790459ce57de0d11610786d09e74bce95`. All 39 focused checks passed,
including Chromium, WebKit, translations and the navigation-timeout
guard. Independent review verified all 17 upstream paths in the PR and
the fork-only release and signing boundary. The receipts are in
`upstream-69233d6bc.DeDpbZAi/nav-fix.lNrNknf6`.

The next refresh brings in upstream
`a265632daedbed0df4a2087a0a09fd5393f64cf2`. It adds separate picker rows
for one model at different reasoning settings. All four upstream paths
apply cleanly. The test README retains both the upstream test description
and the SDK fixture documentation. Packet `upstream-a265632d.TgkK25T9`
holds fork tree `b4aa07b5aacf47595bc9971104b004c2f9e526aa` with 11,337 files.
Subdirectory `pr-docs.llDjltw0` holds PR tree
`237bd6e26c3b10ca9ecd5c20ca0edf4f0a7afb4d` with 11,322 files.
The PR export omits the fork-only local signing test description.
All 1,605 GUI test files retain their hashes and modes.
The PR boundary passed independent review. The three focused routing
test files recorded nine passes on `237bd6e2`. The preceding PR tree
`77b63eee` passed the Go suite with 112 passing packages, 33,704 passing
test events and 219 skip events. Both platform matrices on that preceding
candidate passed. A fresh run covers the corrected exports.

## Application and modules

The earlier macOS arm64 bundle was built with
`VERSION=0.0.0-sdk.alpha.20261117`. The ZIP containing the ad-hoc-signed
application has SHA256
`cbf1495c26da532d1d6a50f5dc381dc961a541cf9f23cf3e38be13ee54b9eb35`.
Its ordinary serve command advertised models, completed a request through
a loopback provider and exited normally. Both plugin catalogs stayed empty.
The fork's automatic migration checks also cover saved retired-provider
accounts and migration records. An absent community plugin is installed
only through an explicit action.

The refreshed macOS arm64 ZIP was built from fork tree
`fe6206a478b0461425751eaa392c92f28c9f9d06`. Its SHA-256 is
`882035e48f9a0cca3113b61899b75a35862fa01299813572dfba63960db34403`.
The extracted application served one loopback inference request, kept both
plugin catalogs empty and exited normally. Both listeners closed. The next
fork tree changed only the provider-options VM fixture.

All seven standalone distributions passed an install, off, on, reload and
remove cycle with a private configuration. After the reference refresh,
Devin and OpenCode passed a fresh build and the same cycle. Claude Desktop
passed accepted native-host checks for connection, model discovery, Messages
and streaming, request accounting and disconnect. OMP replay checks cover
Codex and Claude native exports while preserving archived history.

The rollout still requires the real macOS update with the selected Keychain
identity. Cross-platform native runtime, vendor sign-ins and real clients
have their own acceptance. Companion source locks record the exact versions
and checks for each module.

## Records

The retained verification packet `accepted-base-work.e4p7cKVM` contains
`TREES.json`, source manifests, commands, toolchain pins, terminal test
receipts and the artifact and empty-catalog smoke records. The earlier
validation revision is preserved in the frozen trees along with its
historical evidence index. The architectural and deployment references
describe the behavior and the rollout steps.

- [Subsystem references](../subsystems/README.md)
- [Extension contracts and boundaries](../subsystems/extension-surface-coverage.md)
- [First rollout](../plans/plugin-first-delivery.md)
