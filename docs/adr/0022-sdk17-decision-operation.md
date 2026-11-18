# SDK 1.7: selected-account decision operation

Status: implemented; scoped final verification and upstream-ID follow-up passed.
Independent implementation review pending.

Base: shared `devzeus`, `d10cbaa9ddbdb59488c8f1cae4c90dc5865c5510`.
Implementation continued after the coordinated upstream merge at
`75fa28b1098539f000281385d9acfd85f7c3e99a`, containing upstream
`aba6aeac22c1fb943c27308a531f36a5fe1a2e4e`. The merge owner preserved
the dirty SDK files byte-for-byte; this slice does not own migration policy.
This slice follows the independently verified plugin-free SDK 1.6 core.
It does not change that deployment's acceptance or claim a complete SDK.
No new branch, worktree, runtime dependency or vendor-specific core adapter
is part of this work. The separate module stays at its clean `ff8a49b`
savepoint until the host contract is implemented and reviewed.

## Problem and boundary

The existing provider operation registry requires the chat envelope and
rejects decision models. The ordinary System One, Decisions translation
and classifier owners already exist. They need a generic selected-account
adapter, rather than another inbound route or a model-name heuristic.
Both account/group fallback loops in `decide_group.go` currently lack the
SDK acceptance fence. `postDecideSent` also loses typed transport errors
and ignores response read failures. Classifier rules currently turn an
error into a default or previous intent; a terminal SDK outcome must reach
the HTTP owner before any conversational request is sent.

Own-model agent output/effort metadata, agent routes, controller/no-op
work, source declarations, native account operations and provider baseline
fixes are outside this slice. ADR0021 belongs to the consolidation owner.
The module's donor wire evidence stays in that module; core fixtures use
generic provider/model IDs and the existing System One JSON shape.

The consolidation owner separately owns optional execution `Usage.Facts`,
its Go/JS validation and cloning, provider transport usage validation and
the shared gateway accounting mapper/media paths. This slice owns
`ExtensionAttempt.UsageFor(Provider)` and resets its measured snapshot at
each `extensionBegin`. The reader matches the selected module, provider,
account and current generation; all snapshot readers/writers use the SDK
deep clone. The decision owner consumes the shared mapper through one
fresh attempt per execute. Volta owns the late watched-attempt projection
in `gateway.go` and `observe.go`; the early classifier fences here do not
edit that range. Own-storage API admission is a separate future-minor
contract, not an increase to this slice's global 1.7 version.

## Minimum contract

1. Advertise SDK API 1.7 with the existing capabilities. Add the finite
   provider operation `systemone`, model kind `decision`, and
   `Model.operations`. Validate and clone these facts in Go and JavaScript.
   Model operations must be unique and declared by their provider.
2. A decision model declares `operations: ["systemone"]` and has no
   conversational protocols: missing, null and empty protocols all mean
   none. It never inherits a mixed provider's chat protocols. Missing,
   null and empty model operations never grant System One. Unknown,
   duplicate and non-array operations are rejected. Other model kinds
   retain their previous protocol defaults and existing operations do not
   acquire a model-association requirement. A provider may have no
   conversational protocols when its
   sole operation is `systemone`. Existing media operations keep their
   API 1.3 chat envelope and existing behavior.
3. Admission and leased execute both require negotiated minor >= 7.
   `provider.execute` uses `operation: "systemone"`, `protocol: ""`, the
   exact selected model, original body, sanitized request headers and
   existing execute options. No module negotiating 1.5/1.6 can borrow the
   new host version's authority. The selected transport rechecks current
   account generation, usage readiness and the account's model association
   before a journaled dispatch.
4. Discovery/cache facts authorize the selected account, never the
   provider-wide display union. With model discovery enabled, unknown or
   stale catalogs grant no decision operation; a successful empty catalog
   stays empty in memory and after a cold read. System One requires model
   discovery; admission rejects the unusable declaration without it.
   Static manifest entries are discovery hints, not account entitlements.
   Existing package/config/generation
   cache guards remain authoritative. No model is synthesized from its ID.
5. Reuse ordinary `POST /v1/systemone`, `/v1/decisions` translation, decision
   groups and classifiers. Project decision facts into the existing
   catalog decision marker, exclude them from chat APIs, and list the
   native System One endpoint. Reuse key/model/account authorization and
   the existing transport, lease, network, cancellation and usage journal.
6. After body completion/close, accepted, uncertain or completed-but-failed
   SDK work stops account/member retries, rest-driven continuation and
   classifier/default routing. Return typed errors through every wrapper.
   The HTTP owner emits 409, no-retry and operation receipt headers.
   Definite pre-dispatch failures retain ordinary safe fallback. Successful
   classifier levelling and its subsequent classification are distinct
   intentional executions; the fence must not block the second successful
   call or allow replay of a failed first one. A successful calibration
   followed by a definitely not-started classification may try the next
   candidate; the completed calibration itself is never replayed.
7. Preserve bounded-read, oversize and close errors, and require the SDK
   terminal result before success. Pass ordinary allowed reply headers,
   content type, retry/account signals; framing and fence receipt headers
   remain host-owned. Caller credentials never become module upstream auth.
8. Consume the final validated normalized usage snapshot once. A present
   result overrides cumulative events; a nil result retains the latest
   event. Usage and receipts come from the individual completed response,
   never the sequence fence. Calibration and main classification have
   distinct operation IDs, even under shared request metadata. Cached or
   concurrent diagnostic failures cannot expose another caller's receipt
   or lose the terminal stop condition. Only absent normalized usage uses
   raw answer usage. The ordinary
   decision owner and classifier owner each append their own execution's
   single record, including measured terminal failures. Transport and
   Decisions translation append no duplicate. Quota invalidation is not a
   second billing append.

## Exact owner files

Production edits are restricted to these paths and responsibilities:

| Paths | Responsibility |
| --- | --- |
| `pkg/extension/contract.go`, `types.go`, `provider_operations.go`, `manifest.go` | Minor 7, operation/model wire, validation and cloning. |
| `sdk/js/package.json`, `index.d.ts`, `manifest.js`, `provider-policy.js` | Matching author contract, version, validation and handler admission. No agent metadata/controller changes. |
| `internal/provider/extension_provider.go`, `extension_models_cache.go`, new `extension_decisions.go` | Account catalog projection, exact model-operation association and current/cold cache facts. |
| `internal/provider/extension_operations.go`, `extension_transport.go`, `decide.go` | Exact endpoint dispatch and selected-account decision resolution before legacy heuristics. |
| `internal/provider/extension_outcome.go` | Selected-scope `UsageFor`, per-execution snapshot reset and SDK deep cloning. Transport usage validation remains with the consolidation owner. |
| `internal/provider/decide_group.go` | Group display resolution accepts an account-union decision listing; actual candidates still require their own account association. |
| `internal/gateway/decide.go`, `decide_group.go`, `decisions_systemone.go`, `extension_outcome.go` | System One/Decisions calls, body/usage/header ownership, both retry fences and typed receipts. |
| `internal/gateway/classify.go`, `rules.go`, two bounded call sites in `gateway.go` | Preserve terminal classifier errors through cache/rules and stop before default/nested chat dispatch. |

Tests: new `pkg/extension/decision_operations_test.go`,
`sdk/js/test/decision-operations.test.js`,
`internal/provider/extension_decisions_test.go`,
`internal/provider/extension_attempt_usage_test.go`,
`internal/gateway/extension_decisions_test.go`,
`internal/gateway/testdata/extensiondecisionworker/main.go`, and focused
additions to `internal/extension/provider_operation_admission_test.go`.
Version-only assertions in `sdk/js/test/composite.test.js`,
`gateway-hooks.test.js`, `contract-version-types.ts` and Go contract tests
may change with the advertised minor; no behavioral agent changes.

Documentation: this ADR, `docs/subsystems/executable-extensions.md`,
`extension-providers.md`, the classifier/fallback paragraphs in
`gateway-routing.md`, and `sdk/js/README.md`. Any additional owner path
needs an explicit scope message before editing. Do not stage another
agent's files or commit before review.

## Execution plan and evidence gates

1. Record this contract and owner list before source edits. Preserve the
   dirty consolidation ADR and the clean module savepoint.
2. Add behavioral tests on the existing base, using JSON for the future
   model field so a compile error cannot masquerade as a red. Prove
   decision manifest admission, actual ordinary model/endpoint projection,
   typed read failure and no second dispatch after acceptance at the real
   fallback owners. Capture each failing terminal result before production.
   Include all missing/null/empty protocol and operation variants in both
   SDKs, exact model/account authorization, and two classifier executions
   with different or absent usage/receipts. At the original HTTP routing
   owner assert zero second-candidate, previous/default and nested-target
   dispatches after accepted failure, invalid answer or concurrent deadline.
3. Implement Go/JS admission and exact selected-account catalog/transport
   projection. Cover negotiated 1.5/1.6 refusals, protocol/model mismatch,
   known-empty cold catalogs, stale generations and account isolation.
4. Implement System One/Decisions/classifier fences at their actual
   owners. Exercise successful two-call classifiers, malformed/partial
   bodies, accepted 5xx, uncertain completion, request/reply headers and
   normalized versus raw usage without double accounting.
5. Run scoped race20, scoped vet/build, formatting and dependency-free JS
   checks on an immutable Linux copy. Use isolated HOME/XDG and
   pinned existing caches with offline dependency resolution. Native
   positive fixtures execute in the initial user namespace. Keep all red
   logs and run a causal fence/admission mutant where practical. Do not
   repeat the leader's old-source broad suite.
6. Record exact source hashes, commands, terminal results and limits here.
   Provide a bounded review packet without staging or committing the shared
   host union; the consolidation owner owns that checkpoint. A separate
   reviewed module checkpoint may use Lore trailers, with no push. Module
   work remains a subsequent negotiated-contract integration.

## Evidence status

Before production edits, the initial SDK 1.6 behavioral run returned exit 1 for each
command: Go contract 2 FAIL, gateway typed/read/close/size 5 FAIL, JS 2 FAIL.
All compiled; none skipped. See `red-evidence.json` and `red-*.log`.
Additional real exec/native reds, with SDK admission/projection already
implemented and gateway owners unchanged, are in `red-native3-native.log`:
accepted failures reached a second account, classifier failures dispatched
the original turn to its default conversational target, and raw usage erased
the normalized calibration snapshot. Typed transport, cache and deadline
causes were also lost. A preceding `red-native` run failed to compile due to
a fixture import and is retained as a failed attempt, not behavioral proof.

The first merged-source iteration passed Go decision contract 13,
provider 5 and gateway 30 tests with zero
skips, plus the focused JS cases. Gateway tests used actual installed exec
and C-shared native fixtures in the initial user namespace. Later added
accounting, shared-receipt and lease controls require the final scoped run;
that earlier green is not acceptance of the final bytes.
The SDK 1.6 empty-plugin core proof and its committed cleanup
remain separate evidence at `d10cbaa9`.

On merged shared HEAD `87359a70169661ff9b5b4f71e0831e2c606b293f`,
the results were SDK Go 476 PASS, provider 5 PASS, actual exec/native gateway 43 PASS,
lease 5 PASS and neighboring decision/classifier controls 161 PASS, all
with zero failures/skips. JavaScript ran with the existing pinned Bun
binary available: 142 PASS, zero failures/skips. Strict TypeScript exited
0. A legacy Ark alias regression in the first iteration was caused by
requiring per-model authority for non-SDK keys; the final candidate filter
retains the ordinary legacy predicate and checks exact associations only
for SDK accounts.

Before the attempt-reset implementation,
`attempt-usage-red/red-attempt-usage-provider.log` under the same proof
root compiled and returned two behavioral failures: a new execute
inherited input/output 31/7, and no selected-scope usage reader existed.
The pre-Facts green6 provider follow-up passed all 11 scoped tests.
The actual exec/native classifier deadline cases also passed in the
45-test gateway run. A neighboring repeat exposed extra fixture backend
requests during the deadline wait; that red and its diagnosis are retained
and are not called a passing final run.

`green6/mutants/evidence.json` records two isolated source-copy mutants.
Disabling the outer HTTP routing fence produced HTTP 200 and one
downstream conversation after the classifier failed terminally.
Ignoring the actual lease's operation validator let both minor 5 and 6
System One requests reach durable dispatch. Each test compiled and failed
on the behavior. Both mutant files were restored to their original SHA256,
and the immutable positive source remained unchanged. These proofs
precede the separately owned rich-usage integration; final race20,
format/vet/build and independent review remain required.

### Final verification follow-ups

The immutable 1.7 projection is based on shared `9e88f109`,
overlay SHA256
`e5478ccb8d766f7e88bee1123fa37bf759138faed34984837377e49a5baedce9`.
It includes the separately owned rich accounting and diagnostic callback
dependencies and terminal observation fix, while explicitly projecting
out in-progress storage/search 1.8 work. `source.json` records captured
and tested hashes. It is not an assertion that every later shared dirty
byte was tested. Initial SDK Go 499, provider 11, lease 5, actual
exec/native gateway 55, Node 146 and strict TypeScript passed; format,
scoped vet and scoped build also passed.

The 1.7 compatibility follow-up owns new
`pkg/extension/compatibility_versions_test.go` and bounded assertions in
the existing JS/TypeScript decision tests. The Go compatibility validator
was a real missed admission seam: the first projection still rejected
`^1.7`, as did the consolidation owner's diagnostic freeze. The existing
shared `composite.go` correction for 1.7/1.8 belongs to its version owner;
this slice preserves it and does not change global FullContract to 1.8.
Decision fixtures now explicitly require `^1.7`. The System One operation
floor remains 7 even when the coordinated host advances to 8.

The first gateway `-race -count=20` run is retained as a failed attempt:
688 PASS, 412 FAIL, zero skips, exit 1. Repeated fresh native catalog
namespaces exhausted the process-wide native image admission capacity.
The first initial-namespace run had passed all native cases; the repeated
process must not bypass the production limit to make a test pass.

Fixture isolation plan, recorded before changing the test harness:

1. Retain every exec/native assertion and the immutable capacity failure.
2. Run the native branch in a fresh child of the actual race-instrumented
   gateway test binary, using `proc.CommandContext` and a private test
   flag, as existing native integration tests do.
3. Keep the parent in the initial user namespace, bound and wait for each
   child, propagate any failing assertion or race through its exit code,
   and retain its verbose test output.
4. Repeat the full scoped gateway race20 with the actual native branch
   executing on every iteration. Do not change native runtime capacity,
   introduce an opt-in skip, or weaken the routing mutant.

The initial neighboring-suite failure was an ordinary legacy fixture
counter, 17 requests versus the expected 16, while its previous-intent
route and response remained correct. The diagnostic copy passed 173
neighbors; clean base `9e88f109` passed 118. The isolated legacy test
passed both count20 controls and a race20 control. The extra request's
origin was not captured; the original failed log remains evidence and
the legacy fixture is unchanged.

The completed successor's `source.json` identifies 66 captured and tested
paths, based on `9e88f109`,
with source identity SHA256
`db3550aeab08624fca2bfda12aed5e5f9c126d3ee7ecff02dd58a72e77ebc2b8`.
The permanent gateway test SHA256 at that checkpoint is
`4eeb69b7c215c3e18a76429b0ed34269dd4b36831f8e803edeb5819f633f6ad3`.
SDK contract race20 passed 700 events, provider race20 220, actual leased
dispatch race20 100 and gateway race20 660, all with zero failures/skips.
Each native iteration ran the real race-instrumented child test binary
in the initial user namespace; 660 counts outer Go JSON test events,
not a second count of the child's verbose assertions.
The gateway race log SHA256 is
`00785dcabbf0e6ffbd2d635cc21c1ae38efca262a55b8a78e82e36cadac34c06`.
The neighboring ordinary System One/Decisions/classifier suite passed
151 events. Scoped `go vet`, `go build -tags nogui` and `gofmt -l` exited
0, with empty formatting output.

Node and Bun each passed 147 tests without failures/skips. Strict
TypeScript checks passed for the immutable 1.7 declarations and, separately,
the exact current 1.8 declarations with their search/storage type
dependencies. This is compatibility evidence, not combined 1.8 acceptance.
`mutants/evidence.json` records a repeat of the outer routing-fence mutant:
the original conversational HTTP request returned 200 and dispatched one
downstream conversation after a terminal classifier failure, so the
behavioral assertion failed. Removing the typed `^1.7` compatibility union
also produced the expected TS2322 failure. Neither mutant changed the
positive source bytes. The earlier actual lease-validator mutant remains
the causal proof for minor-5/6 pre-journal admission.

The consolidation owner subsequently separated upstream response facts
from released client object IDs. `Facts.ResponseID` maps only to
`Usage.UpstreamResponseID` and `usage.Record.UpstreamResponseID`; it cannot
populate the ordinary client `ResponseID`. This slice changes only decision
assertions for that delta. They cover ordinary success and terminal 409,
ledger/observer/recent upstream IDs, empty-final snapshot clearing and the
route's empty client response ID. RouteUsage remains pricing data and gains
no identifier field. An initial follow-up mistakenly asserted IDs on
RouteUsage and failed compilation; that attempt remains under
`responseid/`, is not a behavioral red, and is not called passing evidence.
The corrected immutable `responseid2/` follow-up retains all earlier
positive source bytes except those four captured files. Source identity is
`f533b5fc38b63955d78a0e85e6d3d43126f60165f3301cdea4f63330eafd732b`,
overlay SHA256
`e43d42400f4625e4f83a332b0b1f406de6cbb13a665a5aa52c0ad4c82a5d81d4`.
The actual exec/native gateway race20 passed 660 events with zero failures
or skips, exit 0, in 606.121 seconds. Gateway test SHA256 is
`6fa2bc495071256e63f57febfca547917b0f8f40e408ff37b93de6aa51892872`;
race log SHA256 is
`3c8ec93d87fbd4d502e6e5a35f4f14ff9788cafc8f0f445a78a61a231d53187a`.
Scoped vet, build and formatting exited 0 with empty formatting output.
The isolated upstream-ID mutant compiled and failed the ordinary rich
execution assertion when a vendor fact populated the released client ID.
`upstream-id-negative/evidence.json` records behavioral rejection and
unchanged positive source. Negative log SHA256 is
`ab98bbf74fef8b75454ec093ff923431eb8621bab3a6485767371bceddb9de62`.

No independent implementation approval is claimed. The earlier Godel
contract review is a contract review, not acceptance of this final diff.
ADR0024 is a separate check-in/activation proposal with no production
implementation. Own-model metadata and the module's actual Jev adapter
are separate remaining capabilities.
