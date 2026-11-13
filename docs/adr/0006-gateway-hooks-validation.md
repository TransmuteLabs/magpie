# SDK API 1.2 validation: hooks and private tools

Evidence checkpoint: November 13, 2026.

The [surface decision](0006-extension-surface-contracts.md) separates a
memory domain module from the host contracts it composes. This increment
implements request/completion/sign-in hooks and local-owner tools. It does
not complete the remaining adapters in the
[coverage census](../subsystems/extension-surface-coverage.md).

## Source checkpoints

The immutable aggregate source is tree
`7e2a72b6178eb2a51c4e6018ee9515fdbb321da7`, based on
`8c263a544308b98c58e6bde1fff3a4dad2420a47`. Its Git tree was independently
computed before and after verification; the source did not change.

That original aggregate exited nonzero solely because a Darwin tray-render
test lacked the cgo tag required by its AppKit implementation.
The private single-tag overlay passed full Darwin no-cgo vet; its effective
tree is `f69b609953ce20a41ca871de4bfe0d348d317044`. The tag correction is
committed separately as `038d53608`.

Independent review then found a generation race in tool authorization.
The follow-up overlay adds the final check against the executing lease,
explicit owner authority at the SDK CLI entry, and regression fixtures.
Its seven file hashes and source identity are recorded in
`/tmp/magpie-sdk-v11-verification-checks/sdk12-tool-generation-overlay.json`.
The complete SDK and affected integration race tests were rerun on that
overlay; the older aggregate is not relabeled as a run of these later files.

All new execution used **usbox**, regular user, Go **1.26.8**, temporary
HOME/XDG paths, pinned caches and offline Go dependency resolution.
The same declared toolchain was copied outside GOMODCACHE so overlay tests
could replace toolchain source without violating Go's cache protection.
`usbox-adm` remains administration only.

## Terminal aggregate results

Local record:
`/tmp/magpie-sdk12-final-evidence-7e2a72b6-mv825azl/completion.json`.
Remote record:
`usbox:/tmp/magpie-sdk12-final-verify-o_119pt9/`.

| Gate on original immutable tree | Result | Limits |
| --- | --- | --- |
| Complete SDK `-race` | Exit 0; 266 top-level / 848 including subtests; no test skips | Two example main packages have no test files. |
| Scoped integrations `-race` | Exit 0; 128 top-level / 291 including subtests; two skips | Windows installer is platform-specific. A legacy Pi package fixture skipped after its pinned dependency installation was killed at 240 seconds. |
| Complete GUI `-race` | Exit 0; 753 top-level / 914 including subtests; nine explicit skips | Runtime/platform fixture limits remain in the JSON record. |
| JS SDK on Node | 102/102 passed | Public/server conformance and real child-process fixtures. |
| JS SDK on Bun | 102/102 passed | Separate engine execution, not a simulated runtime. |
| TypeScript | Three strict fixture checks passed | Declaration and browser-author API checks. |
| Linux amd64 cgo nogui vet/build | Passed | Native Linux host code compiled. |
| Windows amd64 no-cgo nogui vet/build | Passed | Feature-off cross-build; no Windows native execution. |
| Darwin arm64 no-cgo nogui build | Passed | Feature-off cross-build. |
| Darwin arm64 no-cgo nogui vet | Original failed; single-tag overlay passed | Original failure and corrected overlay retained separately. |

The integration regex matched zero tests in `internal/sessions`. That
package compiled, but this is not session test execution evidence.
Session-specific follow-up verification is recorded separately rather
than inferred from the package pass: the four `TestSessionExtension*`
roots subsequently passed race count 3 on the follow-up overlay.

## Review regressions and follow-up

The hook review corrected three findings:

- read-only recall's host state writes are refused, including through a
  delegated read helper;
- protected tool comparisons preserve large JSON integers;
- oversized optional recall checks its byte limit before JSON validation
  and allocates nothing while skipping.

Four targeted regressions fail behaviorally when those fixes are removed.
The restored Go/public-runtime hook gate passed race count 3 with 258
test/subtest pass events and no failures or skips. The delegated-write
regression also asserts that the helper was actually invoked.

The private-tool review found one further HIGH issue: a shared catalog
check could precede an update, while execution acquired the new private
generation. `Lease.Call` now checks the requested name and exposure against
its actual frozen manifest before auth lookup, journaling or dispatch.
Already acquired shared leases retain their own declaration; current
private leases require host-derived owner authority.

| Follow-up on the seven-file overlay | Result |
| --- | --- |
| Remove final leased-tool guard | Two deterministic actual-worker regressions fail |
| Restore guard, generation race count 20 | 40 pass events; zero failures/skips |
| Complete SDK race targets | 850 pass events; zero failures; example main packages have no tests |
| Nearby CLI/gateway/Library/plugin race targets | 192 pass events; zero failures/skips |
| Session extension race count 3 | Passed; ordinary carry, selected-source guard, found-source conversion and read consent |
| Affected runtime/CLI vet | Passed on Linux cgo and Darwin/Windows no-cgo targets |

Independent source/evidence review closed the three hook findings and the
private-tool HIGH finding. This is bounded approval of those contracts and
fixes, not approval of every remaining SDK surface.

## Actual behavior exercised

- Real SDK worker: one agent saves module-private memory through a tool,
  another recalls it, completion captures it, and a restarted Manager reads
  the retained state.
- Real Go Manager/Bun module: tool write, recall, client EOF, completion
  persistence and sanitized sign-in notification. Both the new hook
  interop and the pre-existing worker interop actually ran.
- Ordinary gateway HTTP: streaming EOF, cancellation followed by a healthy
  request, legacy/SDK ordering and rollback, response-only hooks, browser
  default denial/explicit opt-in, Node fetch provenance and SDK-only
  sign-in dispatch without starting a legacy host.
- Tools MCP, native control and Library/hub: private list/call enforcement,
  manifest transitions, spoofed client metadata, named loopback keys and
  cached private names/descriptions.
- Independent external Go module: offline tidy/build using public SDK
  packages through a local replacement, native installation and handler
  PID equal to host PID. Its dependencies were only the standard library,
  public SDK packages and its own main package.

The external-consumer proof does not establish that an unpublished commit
can be fetched from GitHub. No new SDK repository is required, and no plugin
source was published as part of this checkpoint.

## Remaining scope

Whole-repository acceptance remains non-green from the completed earlier
broad runs; scoped results above do not overwrite that history. No live
vendor account, user session, installed application or production memory
store was used.

Devin, OpenCode and OMP first slices proceed under their own minimum SDK
contracts in separate plugin repositories, following the
[parallel delivery plan](../plans/plugin-first-delivery.md).
Their domain behavior is not established by the generic SDK fixtures.
ZCode migration remains held until the user's explicit signal.
