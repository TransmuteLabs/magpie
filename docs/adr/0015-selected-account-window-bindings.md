# Selected-account window actions

Date: 2026-11-13
Status: Implemented with scoped verification and independent review approval; combined SDK 1.5 acceptance pending.

This cut implements selected-account UI bindings only. Agent launch/profile
adoption remains a separate in-progress SDK 1.5 contribution; this record
does not approve that adapter or claim a complete SDK 1.5 release.

## Decision

SDK API 1.5 adds an optional `provider` to a window action binding. The
provider must belong to the same installed root, which must declare
`account-auth`. Only window action bindings grant selection; primary bindings
and HTTP service endpoints do not. This is an invocation grant, not a module
callback for selecting credentials.

The author declares an ordinary action and its effect as before. For example,
the tested native hybrid bundle grants `page.forward` its `hybrid-fixture`
provider. The page supplies business input and a separate typed
`AccountSelection`; the host derives provider/account/generation scope and
passes selected credentials only to backend calls.

## Public shape

`BindingDescriptor.Provider` is a same-root provider ID. Its nonempty value
requires API 1.5, a window caller, a declared action and `account-auth`.
Absence retains the existing plugin-root binding behavior.

```ts
interface AccountSelection {
  id: string;
  expectedGeneration: string;
}

interface AccountSummary {
  id: string;
  generation: string;
  expiresAt?: number;
}

interface WindowCallOptions {
  account?: AccountSelection;
}
```

Generation is a canonical nonzero uint64 decimal string, including values
above JavaScript's exact integer range. The Go/JS validators reject numbers,
leading zeroes, overflow and caller-supplied provider/auth fields.
`expiresAt` is Unix milliseconds; omitted means unknown.

The browser facade adds `ui.backend.accounts(binding)` and the optional third
argument `ui.backend.call(binding, input, {account: selection})`.
The user chooses from sanitized summaries; no first-account fallback is
performed. Provider comes from the descriptor, never the selection or input.
`input.account`, `input.auth` and similar business fields grant no authority.
Supplying selection to an ungranted root binding is an error.

The parent bridge supplies the issued ref, exact part and frame/request
identity. The iframe cannot replace the module, provider, revision or scope.
Old two-argument calls retain their existing envelope.

## Host owner

[`WindowAccounts`](../../internal/extension/window_accounts.go) validates
the current window grant before reading accounts, explicitly projects
`AccountSummary` and checks the window again before returning. The host never
serializes `Store.Account`, which contains credentials. Listing starts no
worker and returns at most 1024 summaries.

[`CallWindowBindingWithOptions`](../../internal/extension/module_window.go)
resolves the selected ID under the grant's module/provider and checks exact
generation before acquiring the window's package/configuration lease.
[`Lease.Call`](../../internal/extension/manager.go) retains selected-account
checks before and after lazy service startup. The same scope/auth is
propagated through private action delegation; the service cannot substitute
another account.

A host-owned admission context rechecks the window after lazy startup,
before accepting write events and before host callbacks/private delegation.
Disabled, changed, canceled or revoked selections fail before new dispatch
or acceptance. This does not claim that an already accepted external effect
can be undone or arbitrary native code forcibly interrupted.

## Intent and replay

Selected-account writes require the parent's stable request identity.
Their operation key binds module, window revision, exact part and request
identity. Account and binding changes do not manufacture a second operation
key for the same frame intent.

Before dispatch, a durable host-only record binds that operation to its
scope, binding ID and compacted business-input fingerprint. Changed binding,
account, generation or input conflicts even if the original request never
reached dispatch. Identical calls still pass through the existing operation
journal, so concurrent requests dispatch once. The record survives Store
reopening; unreadable records fail closed. No credentials or business body
are stored in this fingerprint record.

The host does not silently refresh, choose another account or adopt a
successor generation for this UI action. The page refreshes summaries after
a separate authorized account change. Provider-only sign-in flows and an
auth broker are separate work.

## Compatibility and boundaries

- Native ABI 1 and codec API 0.4 remain unchanged.
- Root bindings and API 1.0–1.4 callers keep their existing behavior.
- Account grants require SDK 1.5; an older host rejects that contract.
- HTTP/SSE/WebSocket service proxies receive no account grant.
- The legacy tool `_magpie` path remains unchanged. Reusing this typed
  resolver for authorized local tools is a later adapter.
- Modules remain responsible for not returning credentials in domain
  results. The host supplies no credentials in account summaries, browser
  metadata, selection envelopes or frontend configuration.

## Verification

The immutable UI-only source is Git tree
`8511ce2304d4d08bee88b12f4ca58a856db2c45e`, uncompressed archive SHA256:

```text
098a1727536316b86676296f12ef77d1b3aa83597777ced8f144463e1cd69b00
```

Adjacent `race-results.json`, `sdk-results.json`, `native-results.json`,
`js-results.json`, `static-results.json`, `bunhost-result.json` and
`negative-results.json` record terminal commands. All use temporary HOME/XDG,
pinned offline Go caches and synthetic accounts. Subsequent documentation-only
clarifications do not change the tested production or test sources.

- Selected-account public/host tests pass race ×20, including current
  generation, foreign IDs, root/no grant, post-lazy off/config/cancel,
  generation above 2^53, concurrent writes, store reopening and corruption.
- The actual native hybrid/GUI owner fixture passes under race. Primary PID
  equals the host; helper PID differs. Both observe the selected credential
  fingerprint and scope. Nested accepted writes mutate account state once;
  repeated or changed intents do not. Page responses contain no fixture
  credentials. The selected-account TS page compiles in that bundle.
- Legacy root-binding regression, core script budget and CLI/API parity
  checks pass.
- The three Go SDK packages pass under race with 951 test pass events.
  Two Bun-dependent cases skip because Bun is absent from that gate's PATH.
  A separate gate supplies actual Bun and passes both cases race ×3; this
  does not change the earlier skip record.
- Node and Bun each pass 119 tests without skips; six strict TS consumer
  fixtures pass. The earlier mixed development snapshot passed 121 tests,
  including parallel Agent metadata work; those extra cases are outside this
  UI-only candidate.
- Whole-source `vet` and `build` with `nogui` pass for Linux with CGO and
  Darwin/Windows without CGO: six terminal gates. These are not native
  Darwin/Windows execution or whole-application test results.
- Four negative overlays produce behavioral assertion failures: plugin-only
  scope, silent generation adoption, mutable undispatched input and missing
  post-startup revocation checks.

Independent review approved this UI-only candidate with no confirmed findings.
Separate Linux probes cover foreign module/provider isolation and package
replacement during lazy startup under race ×10 (21 pass events, no skips).
The reviewer also ran the actual native GUI/root/budget cases (four pass events,
no skips), three-package vet, six strict TS fixtures and Node selection/UI tests.
The retained review records include the commands, probe hashes and logs.
The reviewer used vet and strict TypeScript. No gopls/LSP diagnostics were
available.

These scoped results do not claim whole-application green,
live ZCode financial operations, an auth broker or a complete SDK surface.
