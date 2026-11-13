# SDK 1.4 validation

Date: 2026-11-13
Status: Combined SDK checkpoint passed; final application acceptance pending.

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

The native session target-store/reuse work subsequently exposed shared
Pi/OMP discovery and read-consent cases. A fresh full source after those
owner fixes must run the ordinary provider/gateway/session integrations and
safe whole-repository suite. Its source and terminal result belong here
before an application-success claim. Existing SDK evidence is not replaced
with an unverified global-green statement.

The whole-repository run excludes `TestScriptFindsClaudeMoved`, which can
launch the real installed app. That exclusion is explicit rather than a
passing test. Native Windows/macOS execution, live vendor parity and the
full browser matrix remain separate evidence requirements.
