# SDK 1.3 validation

Date: 2026-11-13
Status: Combined SDK gates passed; whole-repository acceptance is separate.

## Source and environment

The upstream integration commit is
`01bcc7c7d155b029769327f909372b2ff72252e2`, merging
`414517ccf50507acce9ba2e994712e8714fd044d` into the SDK fork.
`b236cdc98` separately restores lazy sync-setup parsing after the merge.
The fork release workflow and local update-signing implementation remain.

The combined implementation snapshot is the immutable archive
SHA256 `5e05a2c42f15323b2c96f8ac8440c5d4aa20f412c000ce606599720672858443`,
extracted at `usbox:/tmp/magpie-sdk13-acceptance-v1/source`.
Checks use Go 1.26.8, temporary HOME/XDG, pinned caches and offline Go
dependency lookup. Native/vendor fixtures use synthetic accounts and local
HTTP servers. The installed app, real credentials and live sessions are untouched.

## Contracts exercised

- [Transactional refresh](0007-transactional-account-refresh.md): strict
  cross-process ownership, durable dispatch fencing, caller-independent
  completion, atomic credential/expiry/generation/receipt publication, restart
  recovery and owned shutdown. Public results omit credentials.
- [Provider operations](0008-provider-operation-contracts.md): explicit
  operation and model metadata, binary/multipart preservation, leased
  generation admission, ordinary media routes and accepted-operation replay
  suppression.
- Request-driven credential preflight for execution, model refresh and usage.
  Cached providers follow only their own or receipt-proven refresh successors;
  unrelated manual replacement remains a stale-generation error.
- Terminal RPC IDs retire before response bytes become visible. Cleanup cannot
  remove a newer call reusing the ID. JS retains independent capacity for
  canceled handlers that have not finished.
- Root ModuleRPC input uses its frame budget; scoped callbacks retain their
  smaller callback budget. Refresh results use the bounded account-document
  contract rather than the callback envelope limit.
- Session input planning and conversion retain one lease, bounded attachment
  reads, file/directory/store identities and digest checks before publication.

## Completed combined gates

The acceptance runner records full commands and exits in
`usbox:/tmp/magpie-sdk13-acceptance-v1/gates-result.json`.

| Gate | Result |
| --- | --- |
| SDK/provider policy, auth, metadata and operation regressions, race ×3 | 231 test/subtest pass events; exit 0 |
| Actual leased operation admission and durable successor checks, race ×20 | 40 pass events; exit 0 |
| Terminal-ID lifecycle regressions, plain ×50 | 100 pass events; exit 0 |
| Terminal-ID lifecycle regressions, race ×50 | 100 pass events; exit 0 |
| Ordinary SDK provider/media gateway integration, race ×3 | 36 pass events; exit 0 |
| Complete JS SDK under Node and Bun | 108 tests in each engine; both exit 0 |
| Three strict TypeScript consumer fixtures | Exit 0 |
| Complete Go SDK and extension runtime, race | 898 pass events, zero failures or test skips; exit 0 |
| Update retry, Claude cache assertion and sync setup regressions, race ×20 | 80 pass events; exit 0 |
| Repository lazy-regexp guard | Exit 0 |
| Linux cgo nogui vet/build | Both exit 0 |
| Darwin and Windows no-cgo nogui cross-vet/build | All four exit 0 |

Formatting after these gates is recorded by per-file before/after SHA256 in
`format-hashes.json`; newer work is never overwritten. Platform/static gates
run on the formatted copy and are recorded in `platform-results.json`.
Broader repository acceptance retains its own source/result records.

## Independent and negative evidence

Two independent auth-review probes first failed for an ordinary-RPC shutdown
deadlock and rejection of valid 2 MiB rotated credentials. The unchanged probes
pass after the fixes: six events across race ×3, with an unchanged-source audit
and scoped vet. Successor durability was separately corrected to confirm the
directory sync before adopting a completed refresh.

Behavioral negative overlays separately disable expiry preflight, bypass
leased-operation admission and allow fallback after acceptance. Each produces
an assertion failure in its named regression, not a compilation failure.
Their record is `usbox:/tmp/magpie-sdk-finish-current/negative-results.json`.
The original metadata/operation tests also fail against the pre-fix merge.

Independent JS probes also remove only the conditional final cleanup and
replace the independent inflight admission count with the live-ID map size.
The strengthened second-call ACK fixture and existing 32-call capacity test
fail respectively; the unchanged production pair passes all 108 tests in both
engines. The independent cached-provider/media gate passes 21 events across
race ×3, including a real worker and manual-replacement refusal.

The frozen merge's original broad run remains exit 1 with 29,703 pass events.
Later targeted fixes do not retroactively turn it green. Its report records
the actual failures, skips and isolated source; it is separate from this
implementation snapshot.

## Module evidence and limits

The standalone OpenCode module has actual native-host proof for all four
packaged models, automatic pre-dispatch renewal, two isolated accounts,
rotating publication, rejected stale retries, chat/Gemini and usage. That
module's evidence records its exact older host snapshot; it is not evidence
that every later source tree was rerun.

Devin, OMP and ZCode keep their own source inventories and domain evidence.
Direct C ABI tests, ordinary gateway tests and live-client/vendor parity are
different gates. Real-vendor parity, Windows native execution, current macOS
native execution and the full browser matrix are not established by Linux
fixtures or cross-builds. The extension-surface census retains missing adapters.
