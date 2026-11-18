# Keep browser account selection tied to host discovery

Date: 2026-11-14
Status: Implemented; actual exec/HTTP workflow and repeated scoped gates pass.

## Scope

`createAccountWorkflow` in the browser SDK composes the existing SDK 1.5
`backend.accounts`, `backend.call`, `auth.start`, `auth.finish` and
`auth.cancel` methods. It adds no host capability, manifest grant, route or
wire field. `Binding.provider` still grants sanitized discovery and selected
actions; separate `AuthBindings` still grant specific authentication methods.
Core alone validates the requested account identity/current generation and
publishes credentials.

```ts
import { createAccountWorkflow } from "@magpie/extension-sdk/ui";

const workflow = createAccountWorkflow("account.inspect", "signin");
await workflow.refresh();
async function inspectChosenRow(chosenRowID: string) {
  workflow.select(chosenRowID);
  return workflow.call({ operation: "info" });
}
```

The example's row choice belongs to the page. The helper never chooses the
first account, manufactures an ID from form input, or selects the account
returned by authentication. `select(id)` accepts only an ID in the last
successfully enumerated host list and retains its canonical generation
string, including values beyond JavaScript's exact-number range.
`select(null)` clears the choice. Snapshots and their account values are
frozen; UI code reads `snapshot` after each operation to render the result.

## Stale and asynchronous state

A refresh that observes a removed account or a different generation clears
the selection and marks it stale. It retains the newly discovered list but
does not adopt a successor generation. A failed selected action clears both
selection and list: the bridge currently returns ordinary errors without a
typed stale discriminator, so every failure conservatively requires fresh
discovery and another explicit choice. The helper never retries the action.
An older overlapping discovery response cannot replace a newer result.

Business action input is copied as JSON, separately from the selected
`{id, expectedGeneration}` request option. A local selection asks core to use
that account; it neither obtains credentials nor authorizes an operation.
Unknown/foreign IDs and stale generations are still refused by host routes
even if a page bypasses the convenience helper.

## Authentication

```ts
await workflow.start({ method: "device" });
// Render snapshot.flow.url/userCode. Wait at least retryAfterMs before polling.
await workflow.finish();
await workflow.cancel();

// Reauthorize only the account the user has already selected.
await workflow.start({ method: "device" }, true);
```

`start` accepts method/input only. Its optional second boolean reuses the
current selected-account request; form JSON cannot supply account authority.
Concurrent identical starts share one promise. Changed method/input/account
intent is refused while a flow is active. A lost start reply retains that
rejected promise rather than silently starting a fresh exchange. Host durable
intent handling remains necessary for actual parent-message replay.

`finish` and `cancel` share their own in-flight calls and use only the
host-issued flow handle. Cancellation requested during start waits for that
handle. A terminal local view is not overwritten by a late nonterminal
response. Completion refreshes discovery, leaving selection explicit.
Polling is caller-driven using the safe `retryAfterMs` view; the helper starts
no timer, background worker or direct network request. The existing page
bridge owns frame disposal and the host owns durable cancellation/publication.

Client state contains only account summaries, exact selection strings and the
safe auth flow view. Continuations and credential exchange results remain
private to the host. The helper does not provide persistence or replay
identity across page reloads, and does not claim cancellation can undo an
already completed credential publication.

## Browser and server compatibility

The browser export loads `account-selection.js` and `errors.js`, neither of
which imports Node/Bun APIs. `SDKError` has one implementation in `errors.js`;
`bytes.js` re-exports that same constructor so existing server imports and
`instanceof SDKError` retain their identity. UTF-8 budgets use `TextEncoder`.
No SDK version bump or new dependency is needed.

SDK 1.4 root bindings continue using `ui.backend.call` without an account.
The selected-account helper requires a provider binding and does not turn
an old root binding into an account-bound action.

## Verification

The candidate is composed on main `9b8c60f47`, retaining its current 1.5
literal-version and browser/server type parity corrections. NEXT's unrelated
Agent edits are excluded from the temporary index and test source.

Shared JSON cases run through Go's actual `AccountSelection.UnmarshalJSON`
and the JS validator. A strict TypeScript fixture checks browser/server DTO
assignment, typed workflow results and forbidden caller-supplied authority.
The HTTP integration test builds a real Go exec module and starts a real
Manager/Store and the ordinary window routes. The actual served page bridge,
actual parent relay and public browser SDK execute in separate VM realms.
The iframe has no fetch transport or fake component client.

Immutable verification tree `a8743572a4759bc2b3192db2422fd52d90adf58a`
has archive SHA256
`031786c5642abbb0df940b98eb82e249609f697a442164c6596334da5d553a81`.
Its production workflow file has SHA256
`24f473dfeda11f176b63839b9f0f0f2f5879aa1204a4eb501f161f167dd6b4b3`.

| Gate | Terminal result |
| --- | --- |
| Actual exec/HTTP workflow, shared Go parity and core script budget, race ×20 | 60 passes, zero failures/skips |
| Complete Go SDK packages `pkg/extension` and `internal/extension`, race | 1023 passes, zero failures/skips |
| Complete JS SDK suite, Node and Bun | 129 passes under each runner, zero failures/skips |
| Strict TypeScript consumers | Ten fixtures, exit zero |
| Whole-source nogui vet/build, Linux CGO and Darwin/Windows without CGO | Six gates, all exit zero |

Every actual workflow run records 104 guarded iframe/parent boundaries and
25 ordinary HTTP calls. It covers foreign/invalid and changed-generation
selection refusal, no implicit completion selection, start coalescing plus
actual parent-message replay, changed-intent refusal, cancellation during a
held worker exchange, the preserved published generation and an actual
installed SDK 1.4 root binding. The Go owner checks there is only one saved
account, generation remains 2 after the canceled exchange, and exactly three
start/finish handler invocations occurred. Every crossing is checked for the
fixture's private continuation/credential canaries.

Independent read-only source review and an additional Linux Node concurrency
probe found no blocking issue on the frozen production bytes. The reviewer
separately checked constructor identity, frozen/copy semantics, concurrent
finish/cancel, lost-start refusal and lost-finish recovery on the same flow.
Behavioral negatives deliberately weaken foreign-ID selection, generation
matching and start coalescing: each causes an assertion failure. Their
unchanged control passes. The extra foreign-ID unit assertion used by those
negatives strengthens the delivery test after the aggregate snapshot;
final Node/Bun/type results are recorded separately with the delivery
metadata. Production, typed DTOs and Go fixtures are
unchanged from the aggregate snapshot.

The final driver additionally replays the identical parent start message
**after** its authorization has completed. Core returns the same completed
receipt and generation, with no second module exchange. This strengthened
driver passes another race ×20 gate (60 pass events, zero failures/skips)
on tree `8ab1f291d7958ab54a97a3eb6bd53a0768042868`. It adds one HTTP call and
four guarded boundaries to each run; the unchanged module trace still has
exactly three starts and three finishes. Node/Bun each pass all 129 cases
and the ten strict TS consumers pass on this source too.

The delivery commit is rebased on main `9157d371b`, preserving its five
reviewed catalog, fixture, signal-policy and read-only discovery corrections.
Those baseline changes do not overlap any of the 15 owned paths.
The rebased composition passes a separate actual exec/HTTP and Go-parity
race smoke gate (two passes), complete Node/Bun suites (129 each) and the
ten strict TS consumers.
The 1023-pass SDK aggregate and six static gates above describe the earlier
immutable source, not a rerun of those entire gates on the newer main base.
Final metadata records both source trees and the documentation-only
completion delta.

Earlier fixture failures remain in the verification records. They include
an incorrect test field (`Credentials` versus Store `Auth`),
direct registration of SDK-owned `module.call`, and a second fixture
attempting to claim an existing provider. These were test construction
errors; core correctly refused the latter two. They are not presented as
production defects or successful acceptance.

The VM fixture is not rendered-browser, browser-origin enforcement, visual
or live-vendor acceptance. Native ABI transport is not changed or established
by an exec fixture. Full application acceptance remains separate.
