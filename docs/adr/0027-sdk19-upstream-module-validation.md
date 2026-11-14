# SDK 1.9 upstream and module validation checkpoint

Date: November 14, 2026.
Status: the consolidated host passed plugin-free acceptance after the explicit
upstream221 merge. Module, browser and live-platform boundaries remain
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
three connections. The fixture repair has separate acceptance and does not
change the bounded native-package result above.

WebKit lacks `libgtk-4.so.1`, `libjpeg.so.8` and codec `libx264.so`; no rendered
WebKit/full-GUI-matrix pass or OS-dependency workaround is claimed.
Native macOS/Windows runtime, real Keychain/codesign, permission continuity,
installed-app updates and live vendor/account use were not checked.
The 820 signing events prove host policy/routes, not actual Darwin signing.
Domain history/client/UI/parity limits remain in each module's own evidence.
This checkpoint authorizes neither external-plugin rollout nor live deployment.
