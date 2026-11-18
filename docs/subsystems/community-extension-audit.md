# Community donor audit for our SDK modules

Original census: November 14, 2026. Current follow-up: November 16, 2026.
This is a bounded source audit, not community-package, vendor or whole-host
acceptance. It complements the
[owner coverage reference](extension-surface-coverage.md) without treating
every possible extension point as a requirement.

The exact donor for the original census was `magpie-community-plugins` commit
`26ea8faf7793ca66dd8e7a8b90128a5235552a1f`, tree
`385b56a62fc08a6fb56ce4dc9131ff3b60397fcf`. The donor stayed read-only.
No package was installed or executed. Host inspection used the shared
API 1.8 candidate over `9e88f10982a1d66c1b402fba27caddda8ac4ac4e`;
that commit alone does not contain the dirty candidate.

In the historical sections below, “present” means an adapter existed in that
inspected source.
“Scoped proof” names an actual immutable test input. “Gap” identifies a
missing ordinary owner or projection. Proposed repairs have not been
implemented by this audit. The plugin-free SDK rollout and the readiness
of the original five modules are separate acceptance decisions.

## November 16 follow-up

The later donor comparison used community commit
`b35d4b35b2446bd929896ddeb58f3cb299e9e2d1`. The maintained module set now
includes MiniMax alongside CommandCode, Devin, OpenCode, OMP and ZCode.
Exact versions and comparison paths live in each package's
`docs/source-lock.json` and the sibling workspace's
status record (`magpie-sdk-community-plugins/docs/STATUS.md` in the separately maintained workspace).
The counts in the original census remain counts at its pinned November 14
commit; they are not recomputed counts at the later donor.

The six concrete gaps from that census are now represented by ordinary SDK
owners. The old proposals below are retained to explain what motivated each
contract; they must not be read as current missing implementations.

| Original gap | Current implementation and remaining acceptance boundary |
| --- | --- |
| Provider concurrency defaults | API 1.9 bounded `maxConcurrency`/`maxAccounts` join the existing queue and active-account owners through [provider defaults](../../internal/provider/extension_defaults.go). Explicit user settings retain priority. |
| Billed scheduled account actions | API 1.9 [selected-account tasks](extension-providers.md#paid-selected-account-tasks-api-19) acquire fresh host-owned policy, account and journal scopes. They remain distinct from idle credential renewal and daily check-in. |
| Provider option UI | API 1.9 [declared switches](extension-providers.md#declared-provider-switches-api-19) join ordinary confirmation and config CAS. Independent installed CommandCode checks close its ZDR adoption path, including both conversational and decision requests. |
| Rich quota presentation | API 1.9 `UsageWindow` carries optional amount, balance, display, unit and span through the [provider quota owner](extension-providers.md#quota-and-execution). Informational amounts do not grant spending room. |
| Effective own-model agent facts | API 1.9 [agent metadata](extension-agents-sessions.md#inspection-settings-and-plans) projects normalized output/effort facts into ordinary inspect/plan without changing user values. |
| OMP descendant identity | API 1.9 [session sources](extension-agents-sessions.md#api-16-jsonl-session-sources) acquire bounded descendant documents and carry checked child identities to ordinary gateway deduplication. Conversion correctness and actual Codex acceptance remain separate module evidence. |

The original auth-only-provider gap is covered by API 1.11 account-service
admission through existing auth/tool/search owners, without fake inference
models. Bulk account-file import remains a distinct optional adapter; it is
not supplied by a single-account sign-in page.

The later module-entry audit also found real adoption gaps despite these
contracts. Independent scoped closure now covers MiniMax MCP account selection,
Devin/MiniMax first-account onboarding and subsequent ordinary inference,
usage-auth freshness, paid-task revocation, and CommandCode/OpenCode accounting
and decision refusal behavior. ZCode has adopted native foreground/source
resume with bounded independent native/store closure and 16 passing installed
request-policy scenarios on the final combined host.
The current [ordinary-entry table](extension-surface-coverage.md) records those
boundaries. Neither a source census nor these scoped closures substitutes for
the frozen host's final broad/browser gates or later live-account tests.
The subsequent aggregate review also found `AUTH-TOOL-1` at selected MCP
admission after usage-auth refusal. Independent review closed the shared-owner
correction, including the unchanged ordinary Front probe, prepared dispatch,
callback and publication-generation checks. Candidate07 includes that exact
correction; [the local acceptance record](../adr/0029-sdk112-local-validation.md)
separates its closure from the final browser and application gates.

## Historical inventory and evidence

The scan read every `packages/*/package.json` using `git ls-tree` and
`git show` at the donor commit: **103 packages**, including 20 without any
`magpie` metadata. Main/exports and returned hook objects therefore matter;
metadata alone misses providers, tools and search sources.

Exact top-level `magpie` key counts are: `agent` 20, `middleware` 14,
`gateway` 1, `hooks` 8, `page` 23, `options` 36, `permissions` 12,
`actions` 1, `maxConcurrency` 17, `maxAccounts` 1, `icon` 52,
`name` 12 and `summary` 7. Counts overlap. All 103 packages belong to
at least one inspected auth, tool, agent, middleware, hooks or page family.

The retained evidence records the inventory, hook sites and snapshot inputs.

| Artifact | Meaning | SHA256 |
| --- | --- | --- |
| `packages.json` | All 103 names/versions, raw metadata, main/exports, package blob IDs and hashes | `16a30cb9ee7017c4dda4ee4b17c9cf22830c651f95f9f0ac0089834f87d502c0` |
| `hooks.json` | Immutable entry/source hashes, named hook exports and navigable object-key source sites | `06c38c512d9d75abf8a167caeb032783248ccbd48f349873b37ee047d6dd62dd` |
| `review-source.json` | Host and five-module source captures, repository HEADs and per-file hashes | `1a2e50296a3fc43b9dac9f0d69fdbbe2a662756d37c3b4b870a1f30736402885` |

`hooks.json` deliberately labels regex object-key matches as navigation
signals, not a JavaScript parser or proof that every similarly named object
is registered. The owner distinctions below were checked in the actual
returned objects and host callers. `inventory.py`, `hooks.py` and
`snapshot.py` in the packet preserve the collection method. Source captures
are inspection inputs, not a newly tested Git tree.
The 68 host/module files were captured at `2026-11-14T08:12:55.038939Z`;
later parent changes are outside that source identity.

The provider source scan finds 47 package families with `auth` declaration
sites. That is not 47 conversational providers: Search1API supplies auth
and tools without chat models, and one package can supply multiple
providers. Actual returned families include `config`, `auth`, `provider`,
`tool` and `search`. Eight provider packages additionally return
`"chat.headers"`: CodeArts, MiniMax, OrcaTerm, Tabbit, Trae, WorkBuddy,
ZCode and Zed; the legacy host explicitly invokes it in
[`host.js`](../../internal/plugin/host.js), in its fetch path.
SDK provider execution supplies sanitized request context and lets the
module construct its own vendor headers; it does not run that legacy hook.
Default `{id, server}` and OpenCode-2 `setup` exports are loader entry
shapes, not additional host capabilities.

## Historical donor families and owner paths

| Actual family and donor evidence | Current SDK adapter and precise remaining boundary |
| --- | --- |
| Provider/auth/model/decision/media: Command Code (`magpie-community-plugins/packages/commandcode/index.mjs`), ZCode (`magpie-community-plugins/packages/zcode/index.mjs`), MiniMax (`magpie-community-plugins/packages/minimax/index.mjs`) and the 47 auth source families | Selected auth/refresh, provider models, protocols and finite operations exist. API 1.7 SystemOne now has a model-operation association and ordinary decision owners; the old module proposal is not evidence that this host adapter is still missing. Ordinary provider switches, bulk import, concurrency defaults and rich quota display remain distinct gaps below. |
| Four actual tool packages: Calc (`magpie-community-plugins/packages/calc/index.mjs`), LayerFS (`magpie-community-plugins/packages/layerfs/index.mjs`), Search1API (`magpie-community-plugins/packages/search1api/src/index.ts`), SpecStream (`magpie-community-plugins/packages/specstream/index.mjs`) | `tool-provider` enters ordinary MCP/hub selection. [Account resolution](../../internal/gateway/extension_tools.go) supports explicit host-validated `_magpie` selection and strips it before dispatch; tools need not retain credentials. Search1API's auth-only provider declaration is not faithfully representable by the present provider admission rule. |
| Three actual search-hook packages: SpecStream (`magpie-community-plugins/packages/specstream/index.mjs`) `search.specstream`, Antigravity (`magpie-community-plugins/packages/antigravity/index.mjs`) `search.antigravity`, MiniMax `createSearchHook` (`magpie-community-plugins/packages/minimax/native.mjs`) | API 1.8 `searchSources` plus a declared primary read action joins Settings and ordinary web search, selected accounts, preflight, proxy, sanitation and search accounting. A tool named `search` is separate: Search1API is a tool donor, not a fourth search-hook package. |
| Twenty agent metadata packages, including Aider (`magpie-community-plugins/packages/aider/aider.agent.js`), Kimi Desktop (`magpie-community-plugins/packages/kimi-desktop/kimi-desktop.agent.js`), Nowledge agent (`magpie-community-plugins/packages/nowledge-mem-agent/nowledge-mem.agent.js`) | API 1.5 checked agent/profile/launch/Library adapters exist. Porting each agent's native schema remains module work. Own-model capability metadata in ordinary agent options and cross-WSL ownership are not supplied by the current contract. |
| Fourteen synchronous middleware packages plus the `gateway` alias Reserve (`magpie-community-plugins/packages/reserve/reserve.gateway.js`) | The named hook union is `onRequest/onAccount/onProvider/onEvent/onResponse`, with separate `strategies` and `onUsage`. API 1.8 synchronous middleware now supplies the five request hooks; API 1.4 supplies seat ordering; API 1.8 ledger observation supplies sanitized completion rows. These are three registrations, not one capability. |
| Eight async-hook packages: Aelios, Agent Office, Mail Watch, Notify, Nowledge Mem, Harnessweight, PowerContext, Respire | Actual exports are `onRequest/onResponse`, with Notify also exporting `onSignIn`. API 1.2 hooks, private tools/state and windows can serve external memory and completion consumers. PowerContext (`magpie-community-plugins/packages/powercontext/powercontext.hooks.mjs`) and Nowledge (`magpie-community-plugins/packages/nowledge-mem/nowledge-mem.hooks.mjs`) use their own services/stores; they do not require ownership of Magpie's built-in Memory. |
| Twenty-three pages and one action-metadata package, including Usage (`magpie-community-plugins/packages/usage/page/app.js`), Fingerprint (`magpie-community-plugins/packages/fingerprint/package.json`), usage board (`magpie-community-plugins/packages/page-usage-board/server.mjs`), Window Warmup (`magpie-community-plugins/packages/window-warmup/src/plugin.ts`) | Existing window metadata, panels/replacements/actions, bridge reads, config CAS and private component services are present. `usage:summary`, `usage:day` and `providers:list` already reach [permission-checked reads](../../internal/gui/pluginpage.go). The detailed usage board additionally reads historical `usage.jsonl` and configured providers; a live sanitized ledger observer does not reproduce that historical/private-file API. |
| Provider extras: WorkBuddy (`magpie-community-plugins/packages/workbuddy/index.mjs`) `auth.checkin/refresh/rotating/options/import`; Cursor (`magpie-community-plugins/packages/cursor/index.mjs`) `auth.calls` | New API 1.8 check-in, opt-in idle auth renewal, gateway activation and outside-call readers are present in source. [Check-in](../../internal/provider/extension_checkin.go) joins the existing daily/manual owner; [outside calls](../../internal/usage/extension_calls.go) use `IngestReported`, not a second live `Append`. Parent owns final acceptance of these concurrent additions. |
| Paid post-reply pings and scheduled quota-window opening: Devin (`magpie-community-plugins/packages/devin/index.mjs`) `scheduleKeepalive/reportPing`; Warmup scheduler (`magpie-community-plugins/packages/window-warmup/src/scheduler.ts`) | Module lifetime and gateway activation can keep components alive. They do not create an account-bearing scheduled write invocation. Auth `keepAlive` refreshes expiring credentials; it does not authorize another billed prompt. This is a real scheduling gap, with vendor ping/rule policy remaining in the module. |
| Observation, budgets and privacy: Privacy Gate (`magpie-community-plugins/packages/privacy-gate/privacy-gate.middleware.js`), Reserve (`magpie-community-plugins/packages/reserve/reserve.gateway.js`), ordinary [Observer/Watch](../../internal/usage/observe.go), [Limiter/Spender](../../internal/gwbudget/gwbudget.go), [Masker](../../internal/gwmw/gwmw.go) | Ledger observation exists; full SDK request/attempt observer registration, budget replacement and reversible masking registration do not. Privacy Gate explicitly rejects structural egress and counts usage; it is not a reversible text masker. Reserve orders seats; it is not a reserve/release budget limiter. No inspected donor requires replacing those built-in owners for our five modules. |

Package `options` defaults are not a generated settings schema. The donor
has no top-level `magpie.settings`, `parts`, `requires` or `provides` keys
in these 103 manifests. SDK root config plus custom UI covers ordinary
module settings, while the compile-time `pluginrt` schema/slots/services
registry remains separate. Similarly, Library import/apply/remove owners
already exist; arbitrary plans do not automatically register new Library
resources. Neither boundary is a demonstrated five-module blocker.

Private memory capture is best effort and origin/effect restricted, not
durable automatic replay. Built-in Memory CRUD, embedding, digest and
distillation would need their own explicit owner contract if requested.

## Historical gaps and proposed slices

The following are proposals only, ordered by their effect on our modules.
None justifies weakening current account, model, ownership or replay checks.

1. **Provider concurrency defaults — required for ZCode's ordinary donor
   policy.** Donor `zcode/package.json` and `index.mjs` declare
   `maxConcurrency: 3` and `maxAccounts: 2`. The comments distinguish the
   per-account queue from the limit on accounts used from one address.
   [`ProviderDescriptor`](../../pkg/extension/contract.go) declares neither;
   [`PluginConcurrency/PluginAtOnce`](../../internal/provider/concurrency.go)
   return zero for an SDK provider because it is not a legacy plugin.
   Existing user limits still work. ZCode's own `start_guard.go` gates Start
   calls, but does not supply the host's ordinary paid-lane defaults or its
   cross-account fanout rule. Minimum: optional bounded provider defaults,
   projected into the existing queue and active-account owners, with user
   overrides winning. Controls: held concurrent Front requests on one and
   three accounts, explicit zero/user override, cancellation, replacement,
   fallback and unchanged quota/rest policy. No extra module semaphore is
   needed for the host default.

2. **Billed scheduled account actions — required for Devin's optional
   keepalive parity, not basic inference.** Donor pings are off by default,
   read the entire cached context, default to 285 seconds/six pings and
   stop on foreground work or refusal. `Stream.HostCall` refuses a finished
   parent; [`appendOwnCall`](../../internal/extension/own_calls.go) requires
   a current accepted `module.call/tool.call` write and admits one append.
   Retaining a Stream/Auth on a timer would bypass that boundary.
   Minimum: a declared bounded account task invoked by the host with a
   fresh current package/config/account/model/proxy/usage lease and a new
   journaled write intent per tick. Join/cancel on disable, replacement,
   reauth, shutdown and the relevant foreground activity; never replay an
   uncertain accepted tick. Keep ping bytes, conversation identity, default
   off, cadence and miss policy in Devin. Use fake-clock/loopback tests for
   no opt-in/no dispatch, revocation, one tick/one usage append and unknown
   completion. Existing daily check-in is the narrower implemented owner
   for ZCode; it should not be replaced by this proposal.

3. **Ordinary provider options — required only for matching that UI
   surface.** WorkBuddy's `freeOnly()` calls `magpie.option`, and its
   `auth.options` declares localized defaults and `filtersModels`.
   Devin declares a paid-cache switch; Command Code declares confirmed ZDR.
   Legacy [`Option/SetProviderOptions`](../../internal/plugin/providers.go)
   and [`pluginSubs`](../../internal/gui/plugins.go) serve these switches.
   SDK `ProviderDescriptor` has no such declarations. Command Code already
   implements `Config.ZDR`, so enforcement is not blocked; the ordinary
   switch/confirmation surface is missing. Minimum: typed option metadata
   mapped to existing settings/UI and checked config/catalog invalidation.
   Test explicit values over defaults, confirmation, cold listing, model
   refresh and stale generation. Do not put vendor policy in core.

4. **Rich quota presentation — needed for full Devin/ZCode allowance-card
   parity, not a new spending grant.** Donor Devin returns ACU display,
   balance and plan end; ZCode returns amount/display metadata.
   [`UsageWindow`](../../pkg/extension/types.go) and
   [`extensionQuota`](../../internal/provider/extension_provider.go) carry
   percentage/applicability/reset/unknown facts; `Limit` is not projected
   as a display amount. Execution `Usage.Facts` does not add those quota
   fields. Minimum: optional bounded display/amount/unit/span metadata,
   cloned through Go/JS and ordinary cards. Keep allowance dependent on
   measured percentage, applicability and freshness. Controls distinguish
   absent/zero/unknown, informational credits, spent model buckets and
   failed reads without inventing currency or capacity.

5. **Own-model agent metadata — required for faithful Command Code
   generated configuration.** The module's
   separate contract section (`magpie-sdk-plugin-protocol-commandcode/SYSTEMONE-SDK-CONTRACT.md`) asks for max output and effort
   facts. SDK models have them, but
   [`extensionAgentOptions`](../../internal/agent/extension_actions.go)
   sends `[]agent.Option`, whose [definition](../../internal/agent/agent.go)
   omits `maxOutput/efforts/defaultEffort`. Minimum: project the selected
   effective model's normalized facts into checked inspect/plan inputs,
   preserving unknown values and user BYOK settings. This is separate from
   the implemented SystemOne operation. Verify generated files and
   conflict/stale/no-op/disconnect controls.

6. **OMP descendants and source identity — required for automatic
   aggregate history parity.** The OMP reader contract (`magpie-sdk-plugin-session-omp/SOURCE-PREVIEW.md`) accepts
   supplied descendant JSONL, but Source6
   [`session.inputs.v1`](../../internal/sessions/extension_source_inputs.go)
   asks for exact paths and preknown digests. Native OMP journals have no
   such digest index. The public summary also lacks the native
   `Session.Subs` identities used to exclude gateway double counting.
   First assess reuse of API 1.8 rooted storage trees for bounded
   enumeration; do not add another filesystem grant by default. That
   contract can list approved descendants, but currently returns the same
   `SessionSourceSummary` without `Subs`, so enumeration alone does not
   close the identity gap. A bounded typed descendant-ID projection must
   validate ownership and join the existing exclusion owner. Keep all
   file/parent snapshots and revocation through publication. Controls:
   descendants counted once, unrelated/changed/symlink files refused,
   missing/malformed children explicit, gateway exclusion and consent off.

Two community-wide gaps are concrete but **not required by our five current
module contracts**:

- **Bulk account-file import.** WorkBuddy declares `auth.importFrom`,
  localized `importFroms` and `import(texts)`. The ordinary
  [`ImportPluginFiles`](../../internal/provider/pluginsignin.go) and GUI
  file picker have no SDK descriptor/adapter. Reuse that owner with explicit
  selected files, bounded results and generation-checked auth publication;
  a custom single-account window flow is not the same importer.
- **Auth-only provider declarations.** Search1API explicitly says it
  supplies sign-in without chat models. Current
  [`Manifest.Validate`](../../pkg/extension/manifest.go) requires an
  executor/discovery capability and conversational protocols or the
  isolated decision operation. Minimum: an explicitly non-conversational
  account-service declaration, reused by existing auth/tool selection,
  which never appears as a callable chat route. Do not fake chat/SystemOne
  support to pass admission. Test ordinary sign-in/account-bound tools
  alongside absence from chat/models and old-version refusal.

`model-map` and AgentCompass model rerouting are intentional semantic
differences, not permission to relax middleware. Their request hooks change
`body.model`; SDK middleware rolls that transformation and its state back.
If a future module needs rerouting, it must use a core-authorized model
selection design with caller-key checks on every candidate.

## Historical needs of the original five modules

Module evidence was read from their independent repositories; exact HEADs
and working-file hashes are in `review-source.json`. Older proposal
paragraphs were checked against current host source rather than copied as
current blockers.

| Module | Needed for the existing bounded slice | Remaining complete-parity work |
| --- | --- | --- |
| Devin | Paired native execution/account operations, Source6, durable model cache and API 1.7 execution facts/diagnostics exist. Module adoption/testing remains its owner's scope. | Paid cache task invocation (2); rich quota display (4); ordinary paid-cache switch (3). Model-family/effort/history codec limitations in its donor matrix are module work. |
| Command Code | SystemOne operation/model/decision-owner adapter is present in current host source; its separate old proposal is historical. `Config.ZDR` already enforces module policy. | Own-model agent facts (5); ordinary ZDR option UI/confirmation (3). Actual client/vendor readiness is separate. |
| ZCode | Selected account actions/UI/auth, usage invalidation/cancel, native account operations and new check-in owner exist. Do not implement another daily scheduler. | Ordinary 3/2 concurrency defaults (1); rich quota presentation (4); module adoption and actual native client execution. Its explicit financial state/uncertainty rules remain module-owned. |
| OpenCode | Auth/provider/agent/UI exist; candidate API 1.8 rooted storage has a real test-enabled Linux SQLite/WAL reader in the module. | Default history enablement, package floor, consent/media UI and actual platform proof remain. Its `HISTORY_CONTRACT.md` explicitly distinguishes the test-enabled reader from ordinary 0.5.0/API1.5. Detailed historical dashboards/host file access are not prerequisites for the current package. |
| OMP | Ordinary Source6 reader and conversion package exist with checked binding/consent. | Automatic descendant acquisition and deduplication identity (6); original image/source display parity remains separate from lossless explicit conversion. |

None of these five currently needs a new built-in Memory store, budget
limiter, reversible masker, arbitrary CLI command, pluginrt service registry
or unsanitized global request-body observer. Those are optional future
contracts with their own failure policy, not missing runtime plumbing to
add speculatively.

## Historical closure and acceptance limits

The independent terminal Watch and basic media approval covers the source
hashes recorded before the rich accounting and diagnostics changes.

Search scoped proof: tree `ca43b5d92369ffaa30907b5fc032b844468e67f0`,
archive SHA256
`08e91332b83d4f2b272cacb340b74e9fe5058354749155d7513e9a1a8c9f2000`;
18 scoped/360 race20 pass events, 58 neighbors, nine JS tests and
types, vet and formatting passed. The retained report records two
behavioral failures from the unchanged probes on the older source.

Middleware scoped proof: tree `e4f2e76d00950a7a6a4e27571b66a17c4f20ef3e`,
archive SHA256
`d36ecab4d4ff944509ab331e0953114a97200953c946e8627eed220bcbc6201e`;
22 scoped/440 race20, 23 neighbors and 37 parser/middleware pass events,
24 JS tests, strict types and scoped vet green. Its two unchanged-probe
negatives cover missing ordinary dispatch and protected-field rollback.
The historical shared `contract.go` formatting red remains in the packet;
it was not relabeled green. The two owned formatting files match this
tested tree and the final Linux `gofmt -l` returned no output.

Parent snapshot `43b7e290d9c3370881780b23ad49222f5d27f44b` has separately
reported SDK/owner/JS/type/static greens; its broad run found a real
registry defect: `init()` in
[`extension_observer.go`](../../internal/usage/extension_observer.go)
eagerly calls `Observe` even with no SDK listener. Built-in middleware/OTel
tests observed two observers instead of one. The later
[`ServeExtensionLedgerObservers`](../../internal/usage/extension_observer_registration.go)
owner registers observation only for active SDK listeners; the empty catalog
stays inert. Packed usage-row `UpstreamResponseID` and embedded
`reported_cost.go` repairs were also subsequent parent changes. The historical
audit does not transfer its earlier greens onto those later sources; their
acceptance belongs to the corresponding frozen host records.

Ordinary host telemetry and optional module observation are different:
terminal Watch/OTLP, one ledger billing owner, sparse execution facts and
code-only diagnostics must remain intact even when no SDK observer is
active. Raw module stderr is not the replacement telemetry channel.
`ledger.observe` has no request/attempt body access and no historical query
or write authority.

The referenced execution used isolated Linux homes and caches and
synthetic local peers. The audit itself records source inspection.
