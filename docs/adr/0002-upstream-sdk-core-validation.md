# Validation of the upstream SDK core split

Date: 2026-11-12

This records local verification of [ADR 0002](0002-upstream-sdk-core.md).
It is not evidence of a published release or a complete external plugin runtime.

## Tested state

- Archived fork: `893f560cccdeb8fa2e98eadb60427990c10eedcc`.
- New branch base at verification: `e102b660f5fa14445cbf88e756c7e63e8db1c84c`,
  containing upstream `477bbb7a1` and its subsequent documentation merge.
- Immutable code tree: `e9d9ad63617a8f11526d2ec81b3e8ec70eeda473`.
- Toolchain: Go 1.27.3, macOS arm64.
- Go checks used temporary HOME/XDG directories, pinned existing Go caches,
  `GOPROXY=off` and `GOSUMDB=off`.

The verification snapshot contains the implementation and references. This
results document and the statusline assertion updates described below were
added afterwards; they do not alter executable code.

## Passed checks

| Check | Result |
| --- | --- |
| Formatting of changed Go files and staged whitespace checks | Passed |
| `go vet ./...` on macOS | Passed |
| Linux amd64 `go vet -tags nogui ./...`, CGO disabled | Passed |
| Windows amd64 `go vet ./...`, CGO disabled | Passed |
| macOS arm64 build | Passed |
| Linux amd64 build with `nogui`, CGO disabled | Passed |
| Windows amd64 cross-build, CGO disabled | Passed |
| Fork version-gate fixtures | 13/13 passed, including later-page releases, draft/prerelease filtering, invalid versions, downgrade and API failure |
| Update-source/signing/day-start UI fixtures | Chromium 26/26 and WebKit 26/26, all five languages |
| Tray-window UI fixtures | 37/37 per engine, all five languages |
| SDK contracts, registry, bridge and host regressions | Targeted ordinary and race checks passed |
| Native JSON aliases before decoding | Regression failed before the fix through both Front and Handler; race check passed after it |
| Stale/deferred statusline reset behavior | Regression failed before the fix; race check passed with `-count=20` |
| Gateway statusline freshness and key scopes | Updated fixture assertions passed with `-race -count=3`, six root-test passes; scoped vet passed |
| Provider snapshots, remote quota cache, MCP policy and DSH aliases | Scoped race regressions passed with `-count=3` |
| Go adapter outside the kernel module | Concrete Devin source compiled and passed its race tests in a temporary independent module using the public SDK |
| Documentation links, JS syntax and excluded vendor paths | Passed |

UI checks used the existing local Playwright installation and fake APIs; no
dependencies were installed. The independent-module check proves the Go import
boundary, not external-worker loading or native Devin client compatibility.

## Repository-wide failures and test isolation

The repository-wide command is:

```sh
go test -tags nogui -timeout 30m -p 4 -json ./...
```

It completed with exit status 1 after 1,413.85 seconds. Package outcomes were
97 passed, 21 skipped/no test files and four failed. Test/subtest events were
28,327 passed, 97 skipped and 11 failed; these counts include parent tests and
their children.

It is not a green acceptance gate. These failures were observed:

- `internal/agent.TestAlmaProvidersSeenDecodeOnce`: the repeated read used
  401 allocations against a maximum of 400. A clean base worktree failed all
  20 repetitions, with 401 or 402 allocations.
- `internal/agent.TestPrimeAgent`: the initial fixture expected the agent to
  be undetected. The same test also failed on the clean base.
- `internal/plugin.TestPluginSOCKSProxy`: the refused local SOCKS connection
  produced an error form the assertion did not accept. The clean base
  reproduced the same failure.
- `internal/gateway.TestMarshalRawIsEncodingJSON` and
  `TestMarshalSizedIsJSONMarshal`, plus `FuzzMarshalRaw/seed#7` and its parent:
  the existing custom JSON writers differ from this toolchain's
  `encoding/json` on Unicode separators and invalid UTF-8 replacement.
  All reproduced on the clean base.
- `internal/gateway.TestServeWSLListensBesideLoopback`: the IPv6 loopback
  listener still answered after its fixture removed the WSL network.
  The clean base reproduced the same failure.
- `internal/gateway.TestStatuslineEndpoint` and `TestStatuslineHeldKeyScope`
  still expected text without freshness timestamps. Their fixtures and exact
  text/JSON assertions were subsequently updated for the intentional
  statusline behavior in this port. Both passed three repetitions under the
  race detector, including exact account scopes and stored timestamps.
  They are not classified as baseline failures.
- `internal/desktopiso.TestScriptFindsClaudeMoved`: despite a temporary HOME,
  its unchanged launcher searches `/Applications/Claude.app` before the fake
  application in HOME. It started the installed executable with a temporary
  test profile and waited for it. Only the processes identified by that
  test's parent PID and profile path were terminated. SIGKILL was required
  after SIGTERM did not end them; a subsequent process check found none using
  that exact profile. This test was not repeated on the clean base because
  that would repeat the isolation failure.

A broader update-package race run also reported the existing unsynchronized
fake-server counter in `TestDownloadDroppedConnectionErrorKeepsAddressOut`;
the signing-specific race checks passed. Additional, unchanged GUI checks
reported Chromium tray scroll assertions and Japanese/German placeholder
parity failures; the same asset checks failed on the clean base.

The baseline failures and isolation finding are recorded without adding
unrelated fixes to the branch split. They must be addressed before treating
the full acceptance gate as green. The repository-wide run was not repeated
after the final assertion update; executable source stayed identical to its
verification snapshot.

## Limits

No remote branch was rewritten, no workflow was triggered, and no release was
published. The installed Magpie app, update source, account material and
sessions were not migrated. The unintended test-only Claude launch above is
an isolation failure, not an authorized live integration test.

Real Keychain signing, app replacement, permission continuity and native
Linux/Windows execution were not exercised. External Go/Bun loading, IPC,
supervision and the four custom plugin migrations remain unimplemented in
this core snapshot.
