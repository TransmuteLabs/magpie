# Adding a Core-owned provider

This is the implementation checklist for a provider whose protocol ships inside
magpie. ZCode is the worked reference. It is not a recipe for publishing a
community package or adding another provider engine.

Two mechanisms have similar names:

- **Feature plugins** under `plugins/`, managed by `internal/pluginrt` and
  `plugins.yml`, provide switchable pages and gateway features. See
  [Plugin runtime](plugin-runtime.md).
- **An intrinsic account provider**, currently ZCode, uses the existing
  OpenCode-compatible provider/host contract, but its runtime, source selection
  and account operations are owned by Core. It has no npm installation or
  community-update prerequisite. Disabling the Usage page does not disable its
  accounts or their account sheet.

The current intrinsic wiring is deliberately ZCode-specific. There is no general
`builtin:*` registration API: introducing another intrinsic provider requires
explicitly extending the existing ownership, discovery and activation seams
below. Merely copying a runtime folder or adding a preset is insufficient.

## 1. Choose the owner before writing the adapter

An API-key preset belongs in the existing provider/preset machinery. A provider
with its own sign-in, account selection and credential lifecycle also needs the
account integration. Only add vendor-specific operations that the actual vendor
supports; do not copy ZCode's quotas, concurrency numbers, reset policy or native
lanes into another provider.

Use the vendor's authoritative implementation or protocol evidence. Distinguish
HTTP failures, business failures inside successful HTTP responses, cancellation,
body deadlines, and failures after streaming output. A local loopback fixture is
not proof of a live vendor entitlement. An unavailable proprietary verification
or signature must stay explicitly unavailable, not be simulated.

## 2. Integration map

| Concern | Current owner/reference | Required integration |
| --- | --- | --- |
| Runtime and attribution | [`internal/plugin/bundled/zcode`](../../internal/plugin/bundled/zcode), [`bundled.go`](../../internal/plugin/bundled.go) | Embed the runtime and complete licences/notices; exclude development fixtures. Extend the existing intrinsic packaging explicitly. |
| Source authority and activation | [`host.go`](../../internal/plugin/host.go), [`host.js`](../../internal/plugin/host.js), [`providers.go`](../../internal/plugin/providers.go) | Bind the public provider ID to the exact Core-owned source and immutable generation on discovery, auth, models, account, loader and fetch paths. Reject external takeover at every seam. |
| Unsigned discovery | `PassiveProviders`, `providerAccounts`, `zcodeMetadata` in [`providers.go`](../../internal/plugin/providers.go) | Supply methods, declared switches and conservative capabilities without starting a host or fetching models. Unknown metadata is not an empty authoritative list. |
| Existing account adoption | [`zcode_adopt.go`](../../internal/provider/zcode_adopt.go) | Preserve credentials, slots, settings and unfinished operations. Remove obsolete serving/mover authority instead of running two implementations. |
| Sign-in and exact-slot re-login | [`pluginsignin.go`](../../internal/provider/pluginsignin.go), [`plugins_cli.go`](../../internal/cli/plugins_cli.go), [`plugins.go`](../../internal/gui/plugins.go) | Carry the requested account and proxy; ordinary add and selected-slot replacement are distinct. A stale authorization result cannot replace a newer login. |
| Model membership and capabilities | Provider `models` hook, [`provider/plugins.go`](../../internal/provider/plugins.go), [`plugin_relist.go`](../../internal/provider/plugin_relist.go) | Project actual membership, overlays, modalities and effort; distinguish unknown/fallback, authoritative empty and disabled membership. Preserve picks on an unavailable source. |
| Gateway requests | [`gateway.go`](../../internal/gateway/gateway.go), [`provider/plugins.go`](../../internal/provider/plugins.go), host loader/fetch | Reuse routing, compatibility and protocol conversion. Keep private account/execution metadata out of vendor headers and bodies. |
| Scoped account operations | [`plugin/account.go`](../../internal/plugin/account.go), [`zcode_account.go`](../../internal/provider/zcode_account.go) | Advertise an allowlist, validate each operation's input/result, select an explicit account and carry captured scope/generation. Do not expose a public raw-credential operation. |
| Quota projection and caches | [`plugin_usage.go`](../../internal/provider/plugin_usage.go), [`login_usage.go`](../../internal/provider/login_usage.go), [`quotas.go`](../../internal/provider/quotas.go) | Update the host normalizer, runtime DTO, account/card DTOs, cache keys and public report together. Preserve freshness and identity through every projection. |
| History, alerts and routing | [`quota_history.go`](../../internal/provider/quota_history.go), [`quota_alert.go`](../../internal/provider/quota_alert.go), [`routing.go`](../../internal/provider/routing.go), [`window_holds_history.go`](../../internal/usage/window_holds_history.go) | Never attach an old label's data to a replacement identity, record a stale sample as new, or turn partial display data into whole-account routing authority. |
| CLI and TUI | [`accounts_cli.go`](../../internal/cli/accounts_cli.go), [`quota_cli.go`](../../internal/cli/quota_cli.go), [`tui/pages.go`](../../internal/tui/pages.go) | Expose normal account addition, real model refetch, declared options and safe account controls; show errors, partial data and uncertainty rather than a healthy-looking omission. |
| GUI, panel and tray | [`app.js`](../../internal/gui/assets/app.js), [`quotatable.js`](../../internal/gui/assets/quotatable.js), [`zcode-account.js`](../../internal/gui/assets/zcode-account.js), [`traywindows.js`](../../internal/gui/assets/traywindows.js), [`trayusage.go`](../../internal/gui/trayusage.go) | Cover Providers accounts, Usage cards/table, panel and menu-bar selection. Reuse existing controls and load substantial sheets lazily. |
| Remote owners | [`remote_quotas.go`](../../internal/provider/remote_quotas.go), [`zcode_account_remote.go`](../../internal/provider/zcode_account_remote.go), [`gateway/zcode_accounts.go`](../../internal/gateway/zcode_accounts.go) | Forward safe inventory separately from authority. Namespace by actual owner endpoint and reject unsupported delegation. |
| Trusted MCP credentials | [`library/mcp_check.go`](../../internal/library/mcp_check.go), [`mcp_accounts.go`](../../internal/library/mcp_accounts.go) | Validate destination before reading a credential; use the exact account proxy for credential acquisition, independently of the MCP server transport. |
| Backup and synchronization | [`signins_carry.go`](../../internal/provider/signins_carry.go), [`plugin/carry.go`](../../internal/plugin/carry.go) | Identify portable selections by validated scope, not display names or host-local slot names. Preserve distinct selections and handle slot collisions. |

## 3. Package and activate one immutable source

`bundled.go` currently embeds the root ZCode `.mjs` modules, manifest and
attribution. Its digest includes file names and bytes. Extraction writes and
flushes a temporary generation, publishes by rename, and validates every expected
file before using a generation another process published. Missing, extra,
symlinked or changed files fail validation. Existing generations are not deleted
while another process might still use them.

For another provider, extend these existing owners deliberately. A version string
or package name alone does not establish source authority. Keep an existing host
until its replacement is ready, fence requests to the generation they captured,
and isolate activation failure from unrelated providers.

A passive listing must work before Bun is available. `Cached` is not the passive
API: it can schedule a background refresh. Use `PassiveProviders` for a picker
that must not activate plugins, and handle its unknown state without silently
falling back to an active listing.

## 4. Define identity and credential roles separately

Treat these as different values:

| Value | Purpose | Must not be used as |
| --- | --- | --- |
| Public account label or alias | Display and user selection | Proof of identity, a cache owner, or permission to mutate |
| Local slot | Locate the saved record on this machine | A portable cross-machine account identity |
| Stable selection scope | Bind vendor origin, family, user and full selection | A credential or a login freshness token |
| Login generation/epoch | Reject stale work after re-login or selection replacement | A reason to erase an unresolved financial operation |
| Verified vendor user ID | The vendor-defined subject of user-wide operations | An unrelated family/customer/PAT identity |
| Credential material | Authenticate the operation requiring that credential | Public account metadata or a substitute for another credential role |

ZCode's bootstrap OAuth, saved business JWT, ZCode session and inference PAT have
independent roles and lifetimes. Its linked BigModel customer identity is not the
verified ZCode user identity used for billing. Use the equivalent distinctions
from the new vendor's protocol, rather than borrowing credentials across families
or manufacturing an identity from an email or plan name.

Resolve own-app labels only after positively matching the relevant identity and
selection. Preserve explicitly supported legacy inference without promoting an
unverified legacy record to financial authority.

## 5. Keep publication at the existing store owner

The Go auth owner serializes the exact snapshot and durable publication under
its persistent OS lock. The Bun host proposes a correlated change; it does not
publish while merely borrowing a Go-held lock. That arrangement would permit an
orphan child to publish after its parent died. Preserve cancellation, drain and
host-death behavior through the actual private protocol.

For provider configuration, obtain an edit snapshot through the existing checked
read owner and publish through the guarded store. Its private source baseline is
not part of the JSON schema. Do not construct an unknown-baseline file to bypass
the guard, overwrite an unreadable source, or publish a stale whole-file snapshot.
Keep post-publication effects ordered after successful publication.

The native account policy migration records an intent before moving references,
keeps source copies while staging, and blocks the account until publication and
guarded cleanup complete. Do not replace that obligation with best-effort
rollback or cancel a staged intent without accounting for its copies. See
[identity and checked updates](providers-accounts.md#account-identity-and-checked-updates).
Do not apply a generation-sensitive native migration to an unrelated provider
without the identity contract it requires.

These are cooperative-writer guarantees, not universal exclusion of manual
editors or older versions. The auth upgrade requirements are documented in
[Plugin runtime](plugin-runtime.md#provider-sign-in-storage-during-upgrades).

## 6. Make model refresh truthful

Apply vendor config overlays to actual provider members. A rule enabling a model
is not automatically membership. Preserve the exact API/site/template/model
precedence, model IDs, modalities, context/output limits and effort semantics.
A successful empty list must survive persistence and must not revive a default
catalog. An unavailable source may retain a last-good catalog, but an explicit
refetch must report that attempt's failure before fallback projection clears it.

CLI, GUI and TUI must all call that same provider-owned refresh. Do not reject a
provider merely because its transport uses the shared plugin host. Keep static
fallback metadata separate from a fresh vendor list.

## 7. Carry quota provenance end to end

Follow the public [quota provenance contract](providers-accounts.md#partial-and-retained-quota-readings).
The runtime result, host normalization, `plugin.Usage`, `SubscriptionQuota`, public
`Quota` and CLI account row are separate projections; adding a flag to only one
loses it elsewhere.

The checklist includes normal cards, table mode, provider account rows including
lapsed sign-ins, panel, tray choices/text, terminal Usage, CLI JSON and the shared
statusline. Test observation-only data as well as a subset with model windows.
History, alerts, reminders and routing are consumers too. A display fix must not
make a failed source healthy or clear a mark belonging to a source not observed.

Cache/pending reads need current scope and generation. Historical identity may
survive a same-selection re-login, but a reused label must not inherit the old
selection's allowance or history. Preserve actual reading time when handing a
cached result out again.

## 8. Expose controls without granting new authority

Use the existing `AccountRequest`/`AccountResult` contract and structured
`AccountError`. Preserve known versus unknown dispatch state: losing a reply is
not evidence that a mutation did not happen. Bind mutating calls to the account
context the user confirmed, not whichever account is first when the request runs.

If the vendor has irreversible claims, resets or admissions, implement only its
required durable coordinator at the vendor-specific owner. Commit the operation
claim before I/O, release storage fences during network I/O, and reconcile an
unknown result before another dispatch. A successful HTTP response is not always
confirmation. Do not copy Codex automatic-spending policy or add cross-machine
consensus just because another provider has a button.

Read-only remote inventory is useful even where a mutation cannot be routed. A
nested remote card must not advertise an actionable native button when only a
direct owner is supported. Full/unheld-key checks and scope/generation checks
remain below the UI. MCP destination trust runs before credential access, and
credentials never travel in public quota/account DTOs.

## 9. Finish every user interface

- Preserve selected account and proxy on API-key re-login as on browser sign-in;
  ordinary account addition must remain a distinct action.
- Expose intrinsic declared switches through existing provider metadata and
  `settings.EditPluginOptions`, not a fictional installed package. Validate the
  whole input before any write. Reset removes overrides; it does not invent a
  second option store.
- Keep first-party labels, prompt messages/options, descriptions and disabled
  explanations in the existing `en`, `zh`, `zh-TW`, `ja` and `de` conventions.
  Passive and active metadata must agree. Vendor diagnostics remain diagnostics.
- Resolve menu-bar window choices from exact selected account cards. Where
  preferences are provider-keyed, identify the selected-account union or the
  possible in-use-account union rather than pretending the first card is current.
- Use existing modal ownership, field/menu components and confirmation controls.
  Closing or replacing a read view cancels its reads, not an already dispatched
  mutation. Keep substantial account sheets and picker modules lazy.
- Measure code-only startup growth with the existing core-budget rule. Explain
  necessary growth; comments, inflated limits and broad eager imports are not a
  workaround.

## 10. Verification checklist

Use isolated profiles and the repository's existing test harness. Prepare the
pinned dependencies and real Bun path before changing HOME/PATH. Run sequentially
with supported job/concurrency settings at one; do not move an unavailable remote
browser run onto a workstation automatically.

- [ ] Empty-profile and installed-but-uncached discovery do not start a host.
- [ ] The compiled artifact can activate its exact embedded generation; no npm
      installation or external source silently takes over.
- [ ] Positive and failed sign-in, exact-slot re-login, ordinary add, credential
      refresh/rejection, cancellation, host stop and concurrent CAS are covered.
- [ ] Personal/Team/other selections and both same-label and changed-label cases
      remain distinct; same-scope generation changes preserve appropriate history.
- [ ] Stale source publication, corrupt/missing state, staged migration conflicts
      and restart points retain data and block unsafe reuse.
- [ ] Membership, overlays, authoritative empty, unavailable source and explicit
      failed refresh reach real catalog/pick consumers.
- [ ] HTTP/business/stream errors use the actual transport boundary; accepted
      output is never replayed. Optional quota failures cannot erase valid primary
      data, and primary failures cannot erase independent fresh subsets.
- [ ] Partial, observation-only, stale/deferred and late pre-reset readings reach
      every display/report consumer without creating new history or authority.
- [ ] Alerts retain old absent-source marks and update only newly observed data.
- [ ] Remote endpoint/key replacement, late answers, held keys, read-only inventory
      and disabled unsupported delegation preserve the owner boundary.
- [ ] Trusted and untrusted MCP destinations exercise the real credential hook.
- [ ] CLI/TUI actions and both supported browser engines exercise actual surfaces,
      including narrow widths and all five GUI languages.
- [ ] Attribution, owning references, measured startup budget and all affected
      callers are updated; temporary fixtures and processes are removed.

Current reference commands, from the repository root, for the corresponding
ZCode contracts (use a private test profile and prepared dependency caches):

```sh
go test -tags nogui -p=1 -parallel=1 -count=1 ./internal/plugin -run '^(TestAuthLockPublic.*|TestNativeAccountSelectionAndCAS|TestFallenBackListKeepsTheLastOne)$'
go test -tags nogui -p=1 -parallel=1 -count=1 ./internal/provider -run '^(TestProviderSnapshotRejectsConcurrentSave|TestZCodePendingRenameResumesAfterStagedConflict|TestZCodePartialUsageAcrossEmbeddedHostAndLoginCache)$'
go test -tags nogui -p=1 -parallel=1 -count=1 ./internal/tui -run '^(TestProviderPickerPassivePluginDiscoveryAndSignIn|TestTUIPluginRefetchReportsSourceFailure|TestTUIZCodeRefetchEmptyAndFailure)$'
go test -tags nogui -p=1 -parallel=1 -count=1 ./internal/cli -run '^TestIntrinsicZCodeOptions$'
make -j1 test-ui UI_TEST_CONCURRENCY=1 UI_TESTS='internal/gui/tests/plugin-option-words.test.cjs internal/gui/tests/plugin-api-prompt-key.test.cjs internal/gui/tests/zcode-reset.test.cjs internal/gui/tests/tray-windows.test.cjs internal/gui/tests/usage-quota-table.test.cjs'
```

Within `internal/plugin/bundled/zcode`, the nine affected SDK files are exercised
with Bun's `--max-concurrency=1`; its fixture requires HOME to be under the private
TMPDIR. Run an actual compiled CLI/TUI smoke as well as unit tests. The reference
acceptance used intrinsic option list/set/readback/reject/reset and a real PTY
Providers → Add → ZCode picker. It did not perform a live vendor login or spend.

The [ZCode parity inventory](zcode-provider-parity.md) records the complete
comparison and the exercised proof boundary. Passing these selected scenarios is
not a claim of every future vendor response, every platform or physical
power-loss parity.
