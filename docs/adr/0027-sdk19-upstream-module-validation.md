# SDK 1.9 upstream and module validation checkpoint

Date: November 14, 2026.
Status: the consolidated host passed plugin-free acceptance after the explicit
upstream221 and pinned upstream442 merges. Module, browser and live-platform boundaries remain
separate below.
This extends [ADR0026](0026-sdk19-host-validation.md), preserving that historical record.

## Source identity and rollout

The historical shared implementation checkpoint is commit
`d5b098d8183e88ab9fee1da0170d1c90b9590822`, tree
`11c2b9dd3d514bcc76c338f9ae680552319494dc`.
Upstream `aa358555f` is included through merges `222570209` and `c11acbc2`.
The immutable verification copies were captured over base `c11acbc2`;
their physical trees, rather than that base commit, identify tested bytes.

| Physical source on usbox | Tree | Source paths |
| --- | --- | --- |
| `/tmp/magpie-sdk-final-base-tvn9tc_o/source` | `8fe78ab9df13d733d61462eacce19bff523c1e58` | 10,758 |
| `/tmp/magpie-sdk-final-base-23ttp2l1/source` | `11c2b9dd3d514bcc76c338f9ae680552319494dc` | 10,758 |

The functional-tree binary SHA256 is
`c018c44332ff9238779d1d01340dfd751209c2c694942fd0406e4d6a3c96cc86`.
The final-tree binary SHA256 is
`7a00b14d014197e4f542ff2dac399eea8bd5a3292c5ac1b3a7059f7354570102`.
Each binary is the sibling `magpie` of its source directory above.

The exact `8fe78ab9` → `11c2b9dd` diff contains only two test files:
`internal/cli/settings_parity_test.go` relocates an unchanged string literal
into the formatted map group; `internal/provider/extension_account_tasks_catalog_test.go`
routes POST `/models` before decoding selected-account scope.
All production, assets and public SDK bytes are identical. Broad results
remain attributed to `8fe78ab9`; final-tree gates below were separately run.
Both source inventories contain 10,758 paths; post-run audits preserve their
hash/mode identities. The final audit reports `changed: []`.

First rollout is the SDK-enabled host with **zero external plugins**.
The existing [startup migration owner](../../internal/provider/migrate.go)
maintains already-installed targets without installing absent community
packages automatically; explicit installation/manual Move and auth refresh
retain their ordinary owners. No live installation, account migration or
settings change, publication or push was performed.

## Functional-tree acceptance

PASS counts are test events, including subtests and repetitions.

| Gate on `8fe78ab9`, packet `tvn9tc_o` | Terminal result |
| --- | --- |
| Safe broad `go test -tags nogui ./...` | Exit0; 31,293 PASS, 109 passing packages, 216 skips; 1,991.56s |
| SDK/codecs/runtime/routing/window metadata, `-race` | Exit0; 1,404 PASS; five explicit native opt-in skips; no race warnings |
| Public JS SDK on Node and Bun | Each exit0; 203 tests, no failure/skip |
| Strict TypeScript and actual-source GUI-function fixtures on Node and Bun | Exit0 |

Safe broad excludes the installed-app launcher `TestScriptFindsClaudeMoved`
and named native/decision fixtures exercised separately below. Platform,
optional-module and no-test-package skips are retained, not counted as passes.

## Final-tree acceptance

The initially pending native/Source/empty-binary gates now have terminal
receipts in `23ttp2l1/final-runtime-delta-gates.json`; all 17 groups exit0.

| Gate on `11c2b9dd` | Terminal result |
| --- | --- |
| Ordinary provider/gateway/agent/Library/GUI/CLI/session SDK owners, `-race` | 602 PASS; 49 optional fixture skips |
| Complete `internal/usage`, `plugins/middleware`, `plugins/otel`, `-race` | 790 PASS; no skips |
| Update/feed/mirror/signing policy, `-race -count=20` | 820 PASS; no skips |
| Empty-catalog routing/startup, `-race -count=20` | 320 PASS; no skips |
| Upstream shutdown/bulk-removal/CLI/budget regressions, `-race -count=20` | 120 PASS; no skips |
| Native/composite/window/auth-owner lifecycle; native image opt-ins | 10 and 4 PASS; no skips |
| Ordinary native Decisions owner, three repetitions | 75 PASS; no skips |
| Hidden native ownership negative; isolated worker-auth fixture | 1 and 2 PASS; no skips |
| Native Source lifecycle/revocation, three fresh processes | Each 60 PASS; no skips |
| Actual empty-catalog compiled binary | Exit0; CLI/help/lists and HTTP/settings/update/signing routes pass |
| Repository formatting; Linux CGO and Darwin/Windows no-CGO `nogui` vet/build | Empty format output; all six platform gates exit0 |

All listed race gates have zero failures and race warnings. The empty binary
has no Bun, installed SDK/legacy plugin, external child or native module image.
Native positives retain initial-user-namespace system ownership; broad and
accounting use isolated network/user namespaces. Hidden-ownership negatives
use their required isolation. Source repeats do not relax the 64-image cap.
All execution used usbox, Go1.26.8, temporary HOME/XDG, pinned existing offline
caches, bounded timeouts and calibrated descendant network guards.

| Receipt, relative to its packet | SHA256 |
| --- | --- |
| `tvn9tc_o/broad-network-result.json` | `7ee3c4e0587a0de547a3dbb2631b18c3d573463cf105bba66314e0d9d618fbd9` |
| `23ttp2l1/final-runtime-delta-gates.json` | `210ef0614f17caa5bb75ce2cbf461b91d672e4ff72c8846648b1b0f68326365b` |
| `23ttp2l1/final-static-platform-gates.json` | `f8a9aa116f5c802789c45d0893c3a3135320bd9ae48a688914d046a17404956f` |
| `23ttp2l1/audit-result.json` | `14eea940612df09c5caef6568a0b77c1471fae75779658f99f4bdbef5fb5133d` |

## Corrective evidence and retained failures

Paid admission now fetches the exact selected account's current model catalog
under its network hold before membership checks; generation/configuration,
proxy/quota/budget and no-replay fencing remain owned by existing paths.
Godel approved the current production hash `9b04f613b6b0316fc5366a723b4be76777d576af072cb00ede259f0b8c2107ad`.
`usbox:/tmp/magpie-devin-hot-cas-fix-20261114.kjo_hnjn/FINAL-RECEIPTS.md`
retains the unchanged regression: old code gives ten leaf failures/two passing
controls; final `11c2b9dd` gives 240 leaf PASS across 20 repetitions, no skips/races.
Earlier timeout, old-format and foreign-GET fixture RED receipts remain intact
under their original packets. The HTTP fixture correction is test routing,
not a production admission exemption or relaxed vendor-call assertion.

## Separate domain-package acceptance

These are bounded synthetic native-owner/package proofs on **`8fe78ab9` /
`c018c443`**, not fresh real-client/vendor or final-tree binary acceptance.
Source donors are not runtime community-plugin dependencies.

| Module | Exact evidence and boundary |
| --- | --- |
| OMP `c3677c9`, default0.1.0/API1.9 | `usbox:/tmp/magpie-two-owner-repin-20261114.SK7PeIJJ/final`: package, fresh Source→Codex CarryOver and actual CLI GREEN |
| OpenCode `376fa22`, default0.6.0/API1.8 | Same packet: native full-owner fixture and stock default SQLite/WAL HTTP owner GREEN; libraries distinguished |
| ZCode `9bec47b`, default0.13.1/API1.9 | `usbox:/tmp/magpie-zcode-final-native-75dm4l0j`: same-library lifecycle GREEN; 93 owned requests, one verified foreign connection rejected, zero unknown |
| CommandCode `b0f5c5f`, default0.1.2/API1.9 | `usbox:/tmp/magpie-commandcode-final-repin.YJ06ocvO`: newly terminal native-agent20/SystemOne20/quota/package GREEN; binary install/list/inspect and exact-source overlays distinguished |
| Devin `3bb7420`, default0.8.0/API1.9 | Corrected strict hot-CAS fixture passes three repetitions on `c018c443`; its f7 replay is recorded separately below |

The original installed held-task 38-second RED remains unattributed.
`usbox:/tmp/magpie-hot-cas-closure-20261114.sQVusLEq/EVIDENCE.json`
records six later captures reaching paid/held cancellation and two measured
rows, then all exiting1 in the old sampler. Native temperature and top_p use
FIXED64/wire1; FIXED32/wire5 is separate structural-negative coverage, not the
native sampling representation. Five captures use diagnostic host builds; one
uses exact `c018c443`. These remain failed installed gates.

The corrected fixture reuses the bounded wire scanner and checks original
sampling/tool bytes. Nine fixture regressions pass, including the old-parser
negative. Three corrected installed runs on `c018c443` and one on f7 pass:
both paid arrivals, cancellation/join and exactly two measured ACU rows.
`magpie-sdk-plugin-protocol-devin/FINAL-PACKAGE-RECEIPT.json` records the
package SHA256 `f38a8d230ec0ac494c6217470e1d68bff4be4fb23b634e11f0735e6c1053393d`.
This fixture correction does not explain the earlier held timeout or reopen
the separately approved catalog fix.

## Explicit upstream221 merge acceptance

Commit `9b55245a4ce77819984b04ab43e893de914b2632` merges
`221c91fae3ef70328ad115c445034de1cea40aa1`, twelve upstream commits beyond
`aa358555f`, into the shared SDK base. Its tree is exactly the final immutable
acceptance source, `0bd9c4b3b33a792e0046b04ddc99b7656062cef7`, captured over
`98f47d5f14914d5ab8999bafbf6b47918df85e55`.

The packet is `usbox:/tmp/magpie-sdk-final-base-iwlhew6h`; source is its
`source` directory and the sibling `magpie` binary has SHA256
`ae10487987e7e5464f96d741dc9a0fea2adb3a34cb464c58e169861bb4311c29`.
All 10,773 source entries retain their hash/mode identity after verification.
Later documentation commits do not change this implementation identity.

| Final tree0bd gate | Terminal result |
| --- | --- |
| Safe broad `go test -tags nogui ./...` | Exit0; 31,320 PASS events, 109 passing packages, 216 explicit skips; 2,112.59s |
| Complete root/TUI preflight and ordinary route guards, `-race` | 72 and 13 PASS |
| Unchanged permission-word guard, `-race -count=20` | 20 PASS |
| Chat-page behavior/startup guard, `-race -count=20` | 380 PASS |
| Ordinary chat/page concurrency, cancellation, caller/read-only and path guards, `-race -count=20` | 700 PASS |
| Fresh upstream agent/profile/routing/update regressions, `-race -count=20` | 840 PASS |
| Update/feed/mirror/local-signing policy, `-race -count=20` | 820 PASS |
| Actual empty-catalog compiled binary | Exit0; ordinary CLI and HTTP/settings/update/signing routes, no Bun, native module image or external plugin worker |
| Public JS SDK, strict types and GUI function fixtures | Node203 and Bun203; all five groups exit0 |
| Formatting and Linux CGO / Darwin-Windows no-CGO `nogui` vet/build | All seven groups exit0 |
| Post-run inventory audit | 10,773 checked; `changed: []` |

All listed race groups have zero failures and race warnings. The safe broad
keeps the installed-app launcher and named native opt-in exclusions described
above; their separate evidence is not relabelled as this final-tree run.
Execution used usbox, temporary HOME/XDG, pinned existing offline caches and
the existing descendant network guards. No new dependency, live account,
installed-app or update-source change was made.

| Final packet receipt | SHA256 |
| --- | --- |
| `broad-network-result.json` | `3c8420d134cf60a11faf4d9f154eb3edbf490260563c36377cd947c35544d3f3` |
| `final-broad-gates.json` | `a3a3703316dca65c865f43623d8e043db8f66f84ba281e57548792fde370611c` |
| `final-runtime-final-gates.json` | `6ead7ebf2711ae67379f2c504d645ebd845fab320a6945528b71e1f839417bc3` |
| `final-static-gates.json` | `b69ef932a39c76d0ca4ba2e8935a6efa68d2d44ef7933a5b236c6474e1a402af` |
| `signing-race-result.json` | `36a96e585e401890e52bea9a76f43386df1034477c9a3812f24bc4466ff3f59a` |
| `audit-result.json` | `179453dad10236fb7470700de301b17a365f035bacd4618df96043464772501f` |

### Preserved merge failures and corrections

The first capture f7, tree
`f7f26b2d5ee5388a089ab07a72f7372ed0068971`, is under
`usbox:/tmp/magpie-sdk-final-base-p2tiu0uf`; its binary SHA256 is
`9ddc4e4a076dbcc119e71b5c3b140253e8c419f3278dfadc5d8cdadd59c20adb`.
Its ordinary owners602, usage/OTel790, signing820, empty320 and prior-upstream120
race gates passed, as did the native/Source owner gates and twelve static
groups. Its broad was superseded after the unchanged startup guard detected
four eager upstream regexps: 11,837 partial PASS, process-15/driver241.

The second capture q, tree
`256c885c22c1751ccf959f4962b57749657ea9d4`, is under
`usbox:/tmp/magpie-sdk-final-base-a1r2qrqh`. Its superseded broad retains
22,292 passing test events, three failing guards and process-15/driver241:
missing TUI `permission mode`, unclassified DeepSeek chat key-slot routes and
the missing LazyCat client prefix. Both stopped jobs have
`remaining_owned_pids: []`; cancellation is not acceptance.

Original empty-binary drivers on f7 and q exited2 before any app attempt
because the freezer omitted the helper. The accepted helper was restored
verbatim, SHA256
`a052774c7f3db7c4d377925871c589a227e942736790b99abd9889001ea3198b`.
Separate `empty-core-recheck` and unchanged inventory audits pass on both;
the original setup errors remain intact. The final verification packet
includes the helper outside product tree0bd and passes the ordinary
`empty-core` gate.

The corrections reuse `lazyre.New`, the TUI dictionary and existing key-slot
owners. Chat Messages holds a lazy slot across rounds; ASR/TTS take eager
slots; local attachment operations are free. These classifications agree
with upstream `cd030691a36a164f7d2493127e3b0d653d035bda`. Existing startup,
translation and route guards remain unchanged. Test-only overlays on old
production preserve two ordinary concurrency negatives: page/page requests
did not queue, and a page request bypassed an agent's held slot.

Exactly seven paths differ between f7 and final0bd: `docs/reference.md`,
the gateway-routing reference, chatpage implementation/tests, key-slot
implementation/tests and the TUI dictionary. Public SDK, domain adapters,
signing and GUI assets are byte-identical between those captures; native
module proofs below remain attributed to f7.

### Five installed-package smokes on f7

All five use one self-contained native package, synthetic endpoints,
temporary state and no community runtime dependency. These are bounded
ordinary-owner proofs, not actual vendor/client acceptance.

| Module revision; default version / minimum API | Exact f7 packet and result |
| --- | --- |
| OMP `c3677c9`; 0.1.0 / 1.9 | `/tmp/magpie-two-owner-smoke-f7f26b2.6rd18vp9`: installed checked-plan CLI exit0 |
| OpenCode `376fa22`; 0.6.0 / 1.8 | Same packet: stock native SQLite/WAL HTTP owner exit0 |
| ZCode `9bec47b`; 0.13.1 / 1.9 | `/tmp/magpie-zcode-merge221-native-lsmaE5UY`: native lifecycle exit0, 93 owned requests, one identified foreign connection denied, zero unknown |
| CommandCode `b0f5c5f`; 0.1.2 / 1.9 | `/tmp/magpie-commandcode-merge221-smoke.jKvKGC/ordinary-refresh`: auth/catalog/SystemOne/quota/ledger exit0 |
| Devin `3bb7420`; 0.8.0 / 1.9 | `/tmp/magpie-devin-final-fixture-20261114.3miL8Zfd/fresh-host`: strict paid/cancel replay exit0, two ACU rows, no external request |

## Pinned upstream442 follow-up

Merge `1daf37be822d854cad132d7e2b0f7cc13e7477bb` includes the fixed upstream
cut `442964d70c32c31ad078245954adae569dfb82e5`: 25 commits beyond upstream221.
Its tree `85b65e4b297bdaa6fb46a0468987733fedc12067` is the tested physical
source under `usbox:/tmp/magpie-sdk-final-base-fu2hgmm4/source`.
The sibling binary SHA256 is
`64e47d2ce6dc1ec30655cb669b992ff66c0cb465b7ba0017b49dc2d4d31bfbcc`.
All 10,807 source entries retain their post-run hash/mode identity.
No later upstream revision is included in this acceptance.

The merge changes common Library, proxy, catalog and gateway owners. Public
SDK/runtime, update/local-signing implementation and fork-release workflow
are unchanged. The 38 key-slot mappings agree with upstream. Resolution
retains the page/page and mixed page/agent regressions, removing only the
equivalent appended duplicate of `TestKeyConcurrencyChatPage`. The core
script budget is 1,270,110: upstream's 17-byte change with the SDK's existing
270-byte saving retained.

| Final tree85 gate | Terminal result |
| --- | --- |
| Safe broad `go test -tags nogui ./...` | Exit0; 31,414 PASS, 109 passing packages, 190 skipped test events; 1,431.84s |
| Ordinary SDK provider/gateway/agent/Library/GUI/CLI/session owners, `-race` | 602 PASS |
| Canonical provider/gateway routes, `-race -count=20` | 580 PASS |
| Usage, middleware and OTLP owners, `-race` | 790 PASS |
| Feed/mirror/local-signing policy, `-race -count=20` | 820 PASS |
| Empty catalogs and installed-only startup migration, `-race -count=20` | 320 PASS |
| Library commands/profile ownership and proxy precedence, `-race -count=20` | 1,540 PASS |
| Ordinary route guards | 13 PASS |
| Actual compiled empty-catalog CLI/web | Exit0; no installed external module or Bun worker |
| Six affected Chromium files, one worker | 57 PASS, no failure/cancellation/skip; 59.48s |
| Corrected Darwin no-CGO vet and formatting | Exit0 |
| Post-run source audit | 10,807 entries; `changed: []` |

All listed race groups have zero race warnings or failures. The broad receipt
has 216 skip entries: 190 tests and 26 packages with no tests. The unsafe
installed-app launcher and six named native opt-in exclusions remain
explicit; their earlier proofs are not relabelled as this run.

Initial tree `013b3ab2d4cb863c3150af3110a377718402f38b` at
`usbox:/tmp/magpie-sdk-final-base-k848ot99` retains three REDs: CLI
`providers/delete` was both supported and `notYet`; LazyCat's documented
prefix was duplicated; the new AppKit tray test lacked its implementation's
`darwin && cgo` constraint. The correction changes exactly those three
test/doc paths. Production, SDK, assets and workflows are identical across
the two captures. `source-reuse-013b-to-85b65e4.json` records that identity
and the original receipts reused for five unchanged platform vet/build
gates, startup/TUI, Node203/Bun203 and strict types. These are explicit
reused positives, not claimed new executions. Darwin vet, failed route
guards, core build, empty-catalog and all gates in the table ran on tree85.

| Final packet receipt | SHA256 |
| --- | --- |
| `broad-network-result.json` | `aee8f701ebaef7a2bba93839f4e83ab69b9570624bae23e3980daa337ef60176` |
| `upstream442-library-netproxy-race20-result.json` | `a01a527d93dcd6e964080ac635b9d647fae69f1ab32c3bf11e0017e0f4963d1f` |
| `signing-race-result.json` | `892260ff92324542c3eb62cab42b03af281844973f428ade066f4e797dd853aa` |
| `upstream442-chromium-result.json` | `33730e5020a70f5ec68b38de5c9312d48bcbb3aaa9d9abb61f571ea6692de045` |
| `audit-result.json` | `1d476e08566880ceceb43e30588dab81ad3295b9eae97bf13ac65e76b803e703` |

The commit-bound `checkpoint-85-1daf37b-evidence.tar.gz` preserves all 13
fresh terminal gates and 12 explicitly reused positive gates. Its SHA256 is
`0afd369a7ff5d732e07400d86187ea3a20d143b2cc686a1f2d4e5b9685026696`;
all 63 manifest entries were verified after copying the 4,421,149-byte
archive locally. The final owned-process census is empty. This archive
includes the initial capture's reused evidence; no original RED was relabelled.

### Browser fixture and module corrections

Host commit `6508c079` changes three tests only. It checks the existing
`index.html → pluginpages.js → extensionwindows.js` loader chain and gives
two SDK fixtures the existing 60-second navigation allowance; action
deadlines and all-script syntax/built-in scans remain unchanged. On private
tree0bd plus those three overlays, Chromium passes 20 cases in five
languages. Four Node/Bun missing-import/missing-HTML-link controls fail at
their precise new assertions. The complete static guards still fail on 25
navigation entries in 23 originally-upstream files and the upstream
`headerpresets.js` dynamic-loader omission. Those failures were not exempted.
The packet is `usbox:/tmp/magpie-gui-0bd-fixture-closure-85_n630w`; its
97-entry evidence archive has SHA256
`d0e97da29385989174b2dc733c00e67d22f106fce5d2fe152e9bf349269de186`.
It remains distinct from tree85's 57 rendered Chromium cases.

OpenCode commit `e2112da6fe0c601ee6523a972ebcb69cbaeef681` repairs the
consumer's static-model overwrite of host context and lost API1.9
output/effort metadata. Default package 0.6.1 now requires API1.9; explicit
API1.8 compatibility is a separate current 0.6.1 variant, not a repackaged
0.6.0 release. Prior generated values can update while independent user
output, variants and default effort survive synchronization. Six old-code
assertion failures, 500 agent race passes, 271 module race passes, five final
version checks and both distribution checks are recorded separately.
Ordinary installed-native API9/V1, API9/V2 and API8/V2 flows all exit0 on
the exact tree013b host binary, including connect, reapply and model edits.
These are synthetic config proofs, not live OpenCode/vendor acceptance.
The packet is `usbox:/tmp/magpie-opencode-metadata-repair.T6xHh5YL`;
its delivery archive SHA256 is
`3f9609e9762b254036d20df8f16443f5b4bb2b7596421b315cdf438fd62c7501`.

ZCode fixture commit `687af12d52871e4d9995f9548639b005e68104e7` withdraws
the unproven closed-socket admission experiment. Active ingress again
matches `9bec47b` exactly and fails closed. All unsaved experimental source,
reviews and RED observations are preserved in a tracked 113-member archive,
SHA256 `088e272aa6d8ad2727894fcacb897c54bb59bf87d6a8c48577ce023dd4b75269`.
Separate bounded process-cleanup tests pass five cases and fail two on old
behavior. The independent review approves withdrawal/preservation and that
cleanup scope, not browser QA. Runtime/package source is unchanged:
0.13.1/API1.9. The 17.02s `auth.start` browser timeout remains RED with
unknown cause, and the original three unknown connections remain unknown.

## Remaining limits

Rendered Chromium acceptance on f7's eleven touched test files is terminal:
142 PASS, no failure/skip, Chromium151/Playwright1.62.1, one worker, 225.078s.
Its packet is `/tmp/magpie-gui-pinned-browsers-9sjvuwee/snapshot-f7f26b2-touched-221`;
the copied evidence archive SHA256 is
`d0b63fee1b09132fe26c583181773732860476b6f2619925afa9eca347ee8ab3`.
Earlier467 positives and three causal negatives keep their original pin.
The separate old full222 Chromium diagnostic is terminal RED: 13,881 tests,
13,324 PASS, 557 FAIL across 110 files, no cancellation/skip, 8,084.314s.
Its source is `2225702` / tree `5f3295a3`, not f7 or final0bd; all 1,872
frozen inputs retain their hashes. The archive
`gui-2225702-full-clean-evidence.tar.gz` has SHA256
`780a21d60e418050ab3065ad5cfe38d9e41dd4dc6f85d64d7a645719b6a81023`;
all 1,600 manifest entries were verified locally under
`/tmp/magpie-gui-2225702-full-evidence`. Only named baseline comparisons
are established; the remaining failures are not collectively classified as
upstream-only. This is not an accepted full-GUI run.

An additional f7 ZCode packaged-iframe test completes synthetic sign-in and
account persistence/selection, but its aggregate remains RED on three
unattributed connections. A separate diagnostic reproduction attributes its
own three failures to Magpie-spawned Devin processes whose sockets closed
before `ss` inspected them. That establishes an attribution-fixture race for
the reproduction, not a ZCode production defect or the sender of the original
three connections. The later admission experiment was withdrawn and preserved
as described above; it has no browser acceptance. This does not change the
bounded native-package result above.

WebKit lacks `libgtk-4.so.1`, `libjpeg.so.8` and codec `libx264.so`; no rendered
WebKit/full-GUI-matrix pass or OS-dependency workaround is claimed.
Native macOS/Windows runtime, real Keychain/codesign, permission continuity,
installed-app updates and live vendor/account use were not checked.
The 820 signing events prove host policy/routes, not actual Darwin signing.
Domain history/client/UI/parity limits remain in each module's own evidence.
This checkpoint authorizes neither external-plugin rollout nor live deployment.

## November 15 pinned upstream follow-up

Merge `7bc5ae211510d0b3fe436da8fa819b7e812729e5` includes upstream
`b754cb0958925c1625acd752577fb8a73987ae28`, nineteen commits beyond the
previous pin. Its exact tree is
`d4fb3900feae3c18ed76fe0298b018a674117f77`. The two textual conflicts
preserve the SDK lazy bridge alongside upstream's mirror editor and retain
the SDK's existing 270-byte core-script saving. Public SDK, dependency,
fork-workflow and local-signing implementation files are unchanged.

| Immutable source on usbox | Tree | Result boundary |
| --- | --- | --- |
| `/tmp/magpie-sdk-final-base-9k1xhvh0/source` | `09cfa50eda8d2dbf4c5003bfeff6bf865319bc0a` | Safe broad and SDK/browser compatibility |
| `/tmp/magpie-sdk-final-base-7g_42bvk/source` | `d4fb3900feae3c18ed76fe0298b018a674117f77` | Final builds, affected updater gates and empty-plugin smoke |

Exactly four paths differ between those captures: `docs/subsystems/app-updates.md`,
`internal/gui/update.go`, `internal/gui/update_owned_refresh_test.go` and
`internal/update/download_test.go`. Both post-run source audits check
10,838 entries and report `changed: []`. Later documentation does not
change the tested implementation bytes.

| Gate | Terminal result |
| --- | --- |
| Safe broad on `09cfa50e`, with the previously named launcher/native exclusions | Exit0; 31,454 PASS events, 109 passing packages, 216 skips; 1,534.48s |
| Ordinary SDK provider/gateway/agent/Library/GUI/CLI/session owners on `09cfa50e`, `-race` | Exit0; 602 PASS; explicit native opt-in skips retained |
| Final updater regressions on `d4fb3900`, `-race -count=20` | Exit0; 2,000 PASS; 364.51s |
| Complete `internal/update` on `d4fb3900`, `-race` | Exit0; 206 PASS; 12.48s |
| Final formatting and Linux CGO / Darwin-Windows no-CGO `nogui` vet/build | Empty formatting output; all six platform gates exit0 |
| Final Linux GTK3 and Windows default GUI vet/build | All four gates exit0 |
| Final compiled empty-catalog CLI/web | Exit0; no external plugin or Bun worker |
| Public SDK on `09cfa50e`, Node/Bun and strict TypeScript | Node203, Bun203 and types exit0; bytes unchanged in final source |

All listed race gates have zero race warnings. Automated verification uses
usbox, temporary HOME/XDG, pinned offline caches and the existing descendant
network guard. Live Mac inspection below is read-only; usbox-adm is unused.

Bounded Chromium fixtures pass 105 touched-page cases and another 30 updater
cases. These use the earlier `db3a8743` capture, whose GUI assets and browser
tests are byte-identical to both captures above. Site search passes Node5,
Bun5 and Chromium24. Rendered WebKit cannot start with the available system
libraries. The static old-WebKit guard retains its two prior failures:
the missing lazy `headerpresets.js` classification and the pre-existing
`:has()` navigation selector. This is not a green full-GUI matrix.

### Updater regressions and preserved negatives

Notes-language checks now preserve their cancellation owner when promoted
to a release lookup/download. Metadata-only checks retain the staged payload
even if the selected channel changes meanwhile; ordinary explicit channel
checks still replace it with that channel's release. Queued follow-up work
is registered before launch and joined by its owner.

The original production gives two named failures for staged replacement and
cancellation. A further unchanged test fails against `09cfa50e` when a
metadata lookup races a Stable selection: the waiting every-version payload
is replaced by `0.1.11` with a second download. The final inner guard passes
the repeated regressions and the ordinary channel-switch control.
Independent bounded review approves those final semantics.

The earlier repeated updater gate remains RED in its own receipt: 1,978
passing events and two download fixtures reporting three arrivals instead
of one and seven instead of six. Their TCP-arrival cause is not established.
Only these fixtures change to virtual time and a controlled transport;
retry limits, stalled-error classification, live caller context, complete
verified bytes and progress steps remain checked. Removing only
`fileClient`'s total-timeout reset causes twenty real
`TestDownloadSlowButMovingOutlastsTheClientTimeout` failures, while the
stall control passes twenty times. Restoring production gives forty passes.

An additional platform replay gives 460 local-signing passes using the exact
Darwin implementation and existing mock Keychain/codesign fixtures. Three
common OS guards are explicitly adapted to Darwin for that Linux replay.
The staging test requiring macOS `ditto` is excluded. This is not native
macOS execution, real-key signing or installed-update permission continuity.

### Current installed application and release source

The initial live-log audit found four signature refusals on November 15,
01:06–01:07: downloaded empty-team/ad-hoc input did not match the installed
official team's `LY7MVTUDZG`. That describes the previous installation.
During verification the user independently installed our `0.1.4200`;
its startup is logged at 01:23:34 and its current signature is ad-hoc.
No subsequent signature refusal appears in the checked log.

At 01:31, the running process's selected update-feed environment, launchctl
and persistent LaunchAgent still name our GitHub feed. The feed offers
`f-v0.1.4200`, and the installed binary's `update check` exits0 with
`0.1.4200 is the latest`. Its `update signing` command is available and
reports off; automatic checking also remains explicitly disabled in Settings.
No identity or preference was selected or changed during that audit.
The new merge has not been pushed or published. This proves current source
selection and version lookup, not a future installed-app update.

| Copied evidence archive | SHA256 |
| --- | --- |
| Broad/source09cfa packet | `fd16896070300910f63fa741b3c0fca7128fb6abd61adc1ea4697efd809cde07` |
| Final/sourced4fb packet | `e2c413df70a0de11aa169494f00270d18e14a36bf5be5d3ea7e73de02152fd15` |
| Touched Chromium/site packet | `4bde00c7389c4d8050b75d22ac43bce37e3e661a1d4640c7965bce9badfd7424` |
| Additional updater Chromium packet | `38697245ebc0cea0cd953be6900d8a0f8d9886b860d9be6009aae9aba19fad45` |

The final Linux empty-catalog binary has SHA256
`ee746b06c7820aa033a3c1bcf49ef64efcaa41b2357240ef261375ae5614eb10`.
Receipts and the original failed runs are preserved separately; no failure,
platform replay or historical native-module acceptance is relabelled as a
new final-tree success.

### Local-signing correction after the installed-app audit

The user then requested enrollment and reported that the signing control was
under Network and that selecting a certificate did not enable it. The installed
`0.1.4200` CLI reproduced the refusal:
`local signing does not support this app's signature options`.
Its real `codesign --display --verbose=4 --all-architectures` output contains
both `CodeDirectory ... flags=0x2(adhoc)` and
`Executable Segment flags=0x1`. The old unanchored parser treated both as
signature options, so a supported app was rejected before policy publication.

The correction reads flags only from `CodeDirectory` records. Its new
enrollment regression uses the captured output structure and checks ad-hoc,
linker ad-hoc and certificate enrollment, refusal of hardened/restrictive
options, a missing directory, saved identity and absence of app signing during
enrollment. The unchanged test fails on the old parser in all three supported
cases and the missing-directory case; the two restrictive cases still pass.

On `usbox`, the corrected signing mocks pass twenty repetitions, 661 PASS
events; the ordinary complete Linux update package passes under `-race`,
207 PASS events. The Darwin mock replay explicitly substitutes the platform
implementation, enables the three common Darwin guards and supplies a
test-only ZIP extractor for `ditto`. An earlier incomplete replay's failures
are retained separately; they are not native macOS failures or passing results.
Darwin/arm64 `nogui` vet for update, CLI and GUI passes, and formatting prints
nothing.

The signing control now belongs to Settings → About, immediately after Download
source. Network retains its independent Update source menu. The exact GUI and
test bytes captured at `/tmp/magpie-signing-about-o9hILb` pass twenty Chromium
cases across five languages and desktop/mobile widths, plus seventy-one
neighboring update and agent-settings cases. The new case fails against the old
placement. It checks full-fingerprint selection between same-name certificates,
redraw/reload persistence, signing-only writes, visible discovery/save errors,
retention of the existing pin after refused replacement/disable, successful
retry and absence of the row on unsupported platforms. These fixtures do not
use Keychain. WebKit cannot start because system browser libraries are missing.

The same capture passes Linux GTK3, Linux no-GUI, Windows default and
Darwin no-GUI build/vet pairs, and JavaScript syntax checks. Its separate full
GUI-package attempt remains RED: two native hybrid fixtures cannot find `tsc`
on that runner's restricted PATH, and the proxy-bypass fixture receives HTTP403
without the existing isolated-network setup. That receipt is preserved rather
than relabelled as a signing regression or a passing full GUI suite; final
merged-source verification must use the established complete fixture environment.

To carry out the authorized enrollment without replacing the installed app,
the same corrected `update.UseSigning` operation was cross-compiled on `usbox`
for Darwin/arm64 and executed against `/Applications/magpie.app`. This performs
the common layout, signature, certificate and feed checks; it does not
handcraft a policy or use the private key. The installed CLI subsequently
reports signing on, and the policy has mode `0600`. The selected available
certificate expires September 5, 2027. The before/after executable SHA256 is
identical, the feed remains our GitHub feed, and the user's disabled automatic
checking and 360-minute interval are unchanged.

Enrollment is live; the code and UI corrections still require a new app
build. The running `0.1.4200` updater retains its old parser and cannot be
claimed to prepare the next locally signed update successfully. No app
replacement, restart, real private-key signing, permission-continuity check
or publication was performed.

### Fresh upstream integration after signing enrollment

The routing integration is committed as `4526cbc3794972c09b9348e2c2dc7641a0c361d0`
with upstream `ab7cb553718bb1e2787ee8c42e3db3d9c087a1cc`. The subsequent
`66ac2f7fd` merge adds upstream `0dbdc7481cf169253c3503d483d8270d864872fb`.
Its tree is exactly `94ec4c8de4e5b366a6e3fb854df2176007587ab8`, captured
before committing at
`usbox:/tmp/magpie-upstream-signing-closure.ofRrrBdz`. The source archive
SHA256 is `131aeaa8fa9052dab47854ead8cb8b57f7f4dadca92715dbad307f4424c2de9e`;
the inventory records 10,849 file and symlink identities.

The merge keeps upstream remote-routing metadata and the fork's freshness
timestamp together. Whole-response classification now precedes the SDK
terminal fence: an empty successful response cannot publish its usage twice
while being considered for fallback. Responses output must be explicitly
empty before classifying it as an empty answer; an omitted field remains
unknown. Chat and Gemini inspect every candidate's content before a finish
marker. Legacy `function_call` remains content. A multi-choice stream does
not fail the entire turn when only one choice finishes empty.

Permanent ordinary-path regressions retain the three independent classifier
probes, split choices, legacy/refusal siblings, Gemini and whole-response
accounting cases. Returning the old production code produces named behavioral
failures. A middleware fixture now proves cancellation before accepting the
bounded hook timeout, then rejects the replaced run without a second dispatch.
The production hook budget remains 250 ms.

Two test-input mistakes are recorded separately from production failures.
The initial Gemini helper selected Chat rather than a Gemini endpoint. The
earlier refusal table expected an omitted Responses output to be empty. The
corrected helper and table preserve the stricter production behavior; prior
RED receipts remain available. Independent bounded review approved the merge
without remaining high or medium findings.

The exact merged tree passes fourteen format, Node, Bun, TypeScript and
platform build/vet gates on `usbox`. Linux GTK3 and no-GUI, Darwin no-GUI,
Windows no-GUI and Windows GUI variants are covered; these are builds and
static checks, not native macOS or Windows runtime acceptance. The upstream
sharded gateway race suite exits zero. Its native Decisions fixture is kept
in the separate native-owner gate. The full safe no-GUI suite now exits zero
with 31,519 PASS events. Its unsafe installed-Claude launcher and explicitly
opted-in native/browser/live fixtures remain excluded; the receipt retains
every skip.

The same immutable tree passes the SDK race group (1,404 PASS events),
ordinary owner group (604), signing group (820), full usage/observer group
(792), empty-catalog race repetitions (320), and separately enabled native
owners (10). The complete mocked Darwin signing replay passes twenty
repetitions (660 PASS events); this remains Linux fixture replay rather than
real Keychain or macOS runtime acceptance. The built Linux binary passes the
empty-catalog smoke without installing modules. The concluding source audit
checks all 10,849 recorded identities without a changed file.

An additional unisolated observer diagnostic retains its RED receipts:
unrelated `GET /` requests reached the strictly checked test collector while
the expected terminal ledger row still arrived correctly. Temporary request
and goroutine diagnostics were removed without changing the test or production
observer. This does not relabel that diagnostic as a passing run or weaken its
delivery assertions.

The first rollout remains SDK-enabled Magpie with an empty external-plugin
catalog. New API 1.10 process, history and related-input work is deliberately
outside this validated API 1.9 tree. The local update feed still names the
fork's GitHub `latest.json`; the user's checking interval and disabled
automatic checks remain unchanged. No installed application or live plugin
catalog was replaced by these commits.
