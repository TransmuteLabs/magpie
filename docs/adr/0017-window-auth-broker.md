# Window authentication with a host-owned polling flow

Date: 2026-11-13
Status: Implemented; independent correction review, corrected host gates and
bounded actual-module consumer review approved.

## Decision and scope

The unreleased SDK API 1.5 adds `authBindings` to the installed root manifest.
These are separate from selected-account action `bindings`. Granting a window
permission to call `reset.use` does not grant permission to start sign-in, and
granting sign-in does not grant arbitrary provider methods or account access.
Native ABI 1 and codec API 0.4 stay unchanged.

```json
{
  "authBindings": [
    {
      "id": "signin",
      "from": "window:page",
      "provider": "vendor",
      "methods": [{"id": "device", "mode": "poll"}],
      "reauthorize": true
    }
  ]
}
```

The root must declare `account-auth`, the same-root provider and a real window
part. Methods are a finite list. `mode` currently accepts only `poll`; the
method's ID is passed to the module. OpenCode's `device` and ZCode's
`browser` both use a verification link and polling. A method named `browser`
does not imply callback support. Devin's actual loopback/PKCE callback and
API-key/snapshot import remain separate work.

## Browser contract

The Node-free `@magpie/extension-sdk/ui` entry supplies:

```ts
const started = await ui.auth.start("signin", {method: "device"});
const progress = await ui.auth.finish("signin", started.flow);
const canceled = await ui.auth.cancel("signin", started.flow);
```

The page schedules subsequent finish calls using `retryAfterMs`. The host
enforces its own next-poll deadline even if the page polls early.
`WindowAuthFlowView` contains only `flow`, a finite `state`, an optional
verification `url`, `userCode`, `retryAfterMs` and a committed
`AccountSummary`. URLs must be HTTP(S), bounded and have no URL userinfo.
The verification link and code are intentional public display fields.

`Continuation`, credentials and raw module authentication errors never form
the broker's browser response. Start `input` is bounded module-specific form
data, not authority; it is not echoed into the response or stored as a
business-input body in the host flow record.

For explicit reauthorization the binding needs `reauthorize: true` and the
page supplies `account: {id, expectedGeneration}` outside `input`.
Generations retain the existing canonical uint64 string contract. The host
checks the exact account generation; it never selects the first account,
adopts a successor or accepts provider/account authority from form JSON.
An omitted result ID may resolve to that already selected ID; it cannot
select an account for a new sign-in.

The parent fixes the issued ref, exact part, frame nonce and request identity.
The `/api/plugin-pages/auth` route accepts only `start`, `finish` and `cancel`.
It does not expose refresh, logout, an arbitrary method, caller-supplied scope
or a redirect URI. Credential-bearing sign-in import is outside this cut.

## Core ownership

[`window_auth.go`](../../internal/extension/window_auth.go) captures the
Manager, Store, immutable package/configuration lease and policy for the
whole flow. Each RPC is bounded to 60 seconds within a ten-minute flow
lifetime. There are at most 64 owned flows per host and 16 active stored
flows per module. Metadata/listing creates no auth owner or worker.

The thin
[`provider policy bridge`](../../internal/provider/extension_window_auth.go)
checks account-discovery and scoped network policy. Core never imports a
vendor adapter, re-resolves a global provider host for publication or asks a
module to select credentials. Terminal receipt reads still pass the current
window grant and host policy.

Before dispatch, the host persists the intent and state. The intent names
module/window revision, part, parent and request. Its fingerprint binds the
binding, method, exact scope and compacted input. Changing method, input,
binding, account or generation for that intent conflicts.

Once an exchange is dispatched, losing the HTTP caller does not launch
another exchange. The owned operation can settle within its deadline.
Explicit cancellation is persisted; a consumed result whose authority was
revoked can remain only as a private uncertain result. An unresolved
dispatched exchange after process restart is uncertain, not automatically
repeated. This is fail-closed recovery, not automatic reconstruction of a
token lost before the host received it.

Off, package replacement and configuration replacement retire idle owners
on runtime reload. Active exchanges drain privately; they cannot publish
against a changed package/configuration or a canceled flow.

## Transaction and lock order

[`window_auth_store.go`](../../internal/extension/window_auth_store.go)
stores account publication and its safe completion receipt in one
`accounts.json` replacement, using the existing Store lock, checked reads,
sync and rename primitives. Account deletion or a changed generation
prevents selected-account publication. A new sign-in cannot overwrite an
existing account or revive a tombstone.

Selected reauthorization shares the existing credential-refresh file lock.
The owner acquires that lock before catalog/Store work and retains it through
the exchange and publication. An active dispatched refresh remains a fence.
Known completed refreshes cannot be silently adopted by an old selection.

Catalog-sensitive publication uses catalog lock, then Store operations.
The configuration read releases its Store lock before the publication
transaction. The transaction's namespace-local configuration read does not
call `StateGet`, `Manifest` or catalog discovery recursively under Store lock.
No Store-held callback acquires the catalog lock.

Reauthorization replaces only the selected record's `Account` and `Deleted`
fields, matching existing `SaveAccount` semantics. Its `Usage`, `Refresh`
and durable metadata survive. Dirty usage still blocks inference until a
fresh acknowledgment; an older completed refresh operation stays queryable,
including when a later refresh archives that receipt. A new account alone
starts with an empty usage state.

Terminal auth receipts are copied into private flow/intent receipt files
before their active account-file entries are removed. The active file stays
bounded; archived intents still reject changed retries. Receipt files have
the same private-store access boundary as operation journals. This cut does
not introduce an automatic archive-retention/deletion policy.

## Schema 4 and co-running processes

The first admitted broker flow upgrades **that module's SDK account
namespace** to schema 4. Installation, metadata discovery, config reads and
ordinary pre-existing account operations do not perform that migration.
Schema 4 requires a non-null `authFlows` collection and strict typed nested
records. Corrupt/missing/null authority fields fail closed without rewriting
the existing bytes.

Older SDK Store readers, including the UI-only 1.5 checkpoint, cannot read
schema 4. Upgrade/restart every CLI or gateway process that accesses the
namespace before using this broker. The unreleased API minor alone does not
identify a Store-compatible binary. An old process refuses the account file;
it does not reinterpret unknown records as an empty store or downgrade the
file. Other module namespaces and legacy provider stores are unchanged.

## Evidence and limits

The original immutable implementation was tree
`6f292fd90e4a1489c18bd27589b1daed45cc35cd`. Its scoped gates passed, but
independent probes found two real defects: reauthorization reset a dirty
usage revision and removed a completed refresh receipt. Those original
failures remain evidence, not a green acceptance claim.

The corrected production file has SHA256:

```text
910b2421968829f27bfbd2bfda5a79c5356f549c9e29a765476810207dfbe182
```

The reviewer reran the **unchanged** probe
`78e4954f710c11f8555de43d237fd2f05b5b5b31d1d4558d9a6fec5dc4940bd5`
with only that production overlay on the old frozen tree: race ×20,
40 pass events, no failures or skips. The record is
`usbox:/tmp/magpie-auth-independent-review/closure-result.json`.
Permanent regressions additionally check the inference usage barrier and
later refresh-receipt archival.

The isolated correction source is tree
`5241a5059ae61b764f9d18ffb4952c485fa38c72`, with artifacts under
`usbox:/tmp/magpie-auth-fix.o1tQzYTh/`. Its permanent targeted regressions
pass race ×20 (440 pass events), and the unchanged review probe separately
passes race ×20 (40 pass events). This is separate from the independent
reviewer's closure above.

Final consumer verification uses an explicit composition of main
`a721fd647c016f857c965f3b61c72a1b5cdb6084` and only the 30 frozen Auth code
paths, producing tree `7f0f63b30c5859bce19cdca90f0e114ee979a212` under
`usbox:/tmp/magpie-auth-main.1lBjaRys/`. All 30 Auth files match the isolated
correction byte for byte; the main discovery/live-generation fixes and
literal-contract typing fix `541410c16` are retained. The composition
contains no in-progress Agent changes and does not modify the main checkout.
Its uncompressed source archive SHA256 is:

```text
5cfe9ffed8e73e6961e94127f48ae905753209e8e6cbf1d7d336c5c34689971e
```

Its headless host binary SHA256 is
`253f76efb7ffbdf9553a79b74252897f5b988175ca5b6222b81e504d415ab586`;
the flat `source-inventory.json` SHA256 is
`a278427150cc3dd39829c5768d490d4144d043987fab8703b1cad41baedb6d29`.
The composition's schema/Store/public tests pass race ×20 with 900 pass
events, owner/revocation/refresh-interoperation tests pass race ×20 with 180,
and ordinary native authentication passes race ×3 with six; all three
terminal gates have no failures or skips.

The composed source has eight strict TS fixtures, including the main literal
contract-version regression. Its initial JS run had 117 passes and five
Bun-PATH skips per runner. Separate runs with Bun on PATH pass all 122 tests
under Node and all 122 under Bun, without skips; the initial skip record is
retained. These are separate runs, not retroactive changes to that record.

The complete three-package Go SDK race run passes with 1017 test pass events,
no failures and no skips, with actual `tsc` and Bun on PATH. The initial run
is retained separately: 1010 passes, one hybrid-example build failure because
`tsc` was missing from PATH, and two Bun-PATH skips. No test was removed or
replaced with precompiled assets. Separate actual Go/Bun interoperability
cases pass race ×3 with six pass events.

All six whole-source `nogui` vet/build gates pass: Linux with CGO and
Darwin/Windows without CGO. Those cross-platform gates do not claim native
Darwin/Windows authentication execution.

Actual ZCode poll/link and OpenCode device consumers pass in two independent
isolated runs, `positive3` and `positive4`, under
`usbox:/tmp/magpie-auth-final-consumers-OfzeV5lp/`. Both terminal exits are
zero. Each `result.json` has SHA256
`b11a9eb529c1fbde362e31411b6de0c3dd27a9452e310f88510f743f28926e8f`;
different HOME/Store directories, flow identities and transcripts establish
separate runs. The six-file fixture manifest has SHA256
`d1290c9496929456b5a7f1b995260e97b9ca90a00718e502653edcb6711e1eb9`,
and the combined module-source inventory has SHA256
`d1b16f00a239cd45d873e37eb0258697278cbb99683d965e4e13ea8a9d5811fe`.
The accepted record is `AUTH-WEB-PLAN.md` in sibling
`magpie-sdk-plugin-protocol-zcode`, commit `6cb7873`; its detailed historical
packet is `AUTH-WEB-EVIDENCE.md`. Independent consumer review approved the
frozen six-file fixture and terminal packet. A further fresh `positive5`
run passes on the same host/source and fixture inventory, with the same
result hash and a distinct public transcript SHA256
`6f708aef11d8153d3a0a72634655c9c6124161297cb6faed3c0fd52d99fd014e`.

The actual public browser SDK and served parent/page bridge execute in
separate VM realms and call ordinary web HTTP routes, the captured host
Store and native vendor controllers. ZCode reauthorization preserves the
dirty usage barrier until an ordinary GUI quota read acknowledges it.
OpenCode ordinary gateway expiry renewal completes before reauthorization;
its completed refresh receipt survives. Both long-running gateways use the
new generations without restart. Changed intent, stale selection, foreign
parent/binding, cancellation, module off and secret-projection assertions
also pass. Synthetic VM documents do not establish rendered-browser,
visual, browser-origin enforcement or full-app DOM acceptance.

Individual scoped, schema/recovery, broad and actual-module consumer results
must be read separately. Older-base fixture failures remain retained and
are not reclassified as final consumer proof.

The local Auth-only delivery candidate uses main
`1b88b14ca0dd5e06e067779aff589dfb397327f5` as its parent: exactly 30 frozen
Auth code/test paths and five documentation paths. Its Auth files retain
the tested bytes above; main's UI, contract typing, live-generation,
Codex and Pi baseline changes are preserved. This rebased delivery tree
is distinct from the tested `7f0f63b3` composition. Main application and
post-application scoped gates belong to the leader; no aggregate gate on
that newer parent is inferred from the earlier composition.

All verification uses synthetic accounts/vendors and isolated HOME/XDG on
usbox. No live vendor proof, callback support, API-key import, arbitrary
account selection, account-bound MCP adapter, completed Agent profile
integration or complete SDK/application acceptance is claimed.
