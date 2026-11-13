# Extension surface coverage

Date: November 13, 2026.

Magpie has several kinds of extension: built-in Go plugins, legacy provider
and gateway plugins, agent adapters, Library resources and installable SDK
modules. The current SDK is API 1.3: native/exec/Bun and composite/window
integration, API 1.2 gateway hooks/private tools, and API 1.3 transactional
refresh, richer models and a finite provider-operation registry. It does not
replace every upstream extension surface. Public contracts, ordinary host
adapters and domain-module acceptance remain separate claims.
The [combined API 1.3 checkpoint](../adr/0009-sdk13-validation.md) records
the frozen source and completed SDK gates, without making this census a
full-repository or full-surface acceptance claim.

This census is for modules we maintain ourselves, keeping integration logic
outside frequently merged upstream code. It distinguishes three layers:

- **Contract:** public descriptors, DTOs, method negotiation and validation.
- **Adapter:** an ordinary Magpie flow discovers and calls the module through
  the owning subsystem, with its scope, lifecycle and failure policy.
- **Domain module:** our actual provider, memory backend or agent implementation,
  with its own behavior, account correctness and migration evidence.

A complete contract or fixture is not a complete vendor migration. Our
Devin, OpenCode, OMP and ZCode implementations have separate repositories and
bounded acceptance records; vendor logic stays outside the host.
`module.call`, checked file
plans and private `state.cas` do not supply missing adapters automatically.
The initial 29 categories below are an inventory of extension families,
not an exhaustive operation list or 29 SDK capabilities. The operation,
modality and transport matrix extends that inventory.

An external **memory domain module** is different from access to Magpie's
built-in shared Memory owner. API 1.2 gateway hooks and local-owner tools,
together with the existing private store/services and UI, let our module
implement its own memory: tool writes, recall for another agent, completed
exchange capture and durable private storage. It does not need the built-in
Memory API to be a memory module. The current approved slice is those
hooks/tools plus the module's own store; built-in Memory owner access remains
a proposed, optional next integration point.

## Initial extension-family inventory

“Implemented” describes an actual SDK path, with its minimum API version.
“Partial” identifies an actual boundary. “Remaining” means no SDK adapter.
Data/resource imports already supported by Library are identified separately;
an optional SDK ownership binding is not a missing extension runtime.

| Type | Source evidence | SDK contract and actual adapter | Status / remaining boundary |
| --- | --- | --- | --- |
| Native / exec / Bun installation and distribution | [`native.Register`](../../pkg/extension/native/native.go), [`release.go`](../../internal/extension/release.go), [`release_auth.go`](../../internal/extension/release_auth.go) | Real native C ABI without author C glue; executable/Bun options; full-package hashes, private developer auth, pins/updates | **Implemented 1.1.** Runtime conformance does not imply vendor migration or identical platform proof. |
| Composite components, readiness, config and disposal | [`composite.go`](../../pkg/extension/composite.go), [`components.go`](../../internal/extension/components.go) | Primary + private exec/Bun services, DAG, readiness, root CAS snapshot, opaque local State, same-generation leases | **Implemented 1.1.** Services are private components, not cross-module pluginrt services. |
| Provider execution, models, account auth, quota/usage, operations | [`extension_provider.go`](../../internal/provider/extension_provider.go), [`extension_auth.go`](../../internal/provider/extension_auth.go), [`extension_transport.go`](../../internal/provider/extension_transport.go) | Ordinary provider/account/routing owners use declared capabilities; API 1.3 adds finite operations, rich model projection, host-owned refresh and expiry preflight | **Implemented through 1.3** for the declared registry. Arbitrary operations and complete legacy-provider metadata parity remain outside it. See the extended matrix below. |
| Legacy provider extras | [`Provider`](../../internal/plugin/providers.go), [`plugin_checkin.go`](../../internal/provider/plugin_checkin.go), [`plugin_keepalive.go`](../../internal/provider/plugin_keepalive.go) | Transactional `auth.refresh`, persisted expiry and ordinary renewal cover credential rotation; usage has its own DTOs | **Partial:** check-in, outside calls, provider options and background keepAlive declarations/adapters remain. They are not supplied by the refresh transaction. |
| Incoming protocols and response codecs | [`protocol_extensions.go`](../../internal/gateway/protocol_extensions.go), [`inbound.go`](../../pkg/extension/inbound.go) | Installed codecs enter ordinary guards/canonical dispatch and retain the primary lease through response encoding | **Implemented 1.1** for negotiated canonical operations; not arbitrary gateway route registration. |
| Plugin tools / MCP tools | [`extension_tools.go`](../../internal/plugin/extension_tools.go), [`extension_tools.go`](../../internal/gateway/extension_tools.go) | `tool-provider` enters ordinary plugin-tools MCP/hub; API 1.2 adds shared/local-owner exposure | **Implemented through 1.2.** Discovery and actual leased invocation enforce local-owner authority, including after package replacement. |
| MCP server configuration and protocol integration | [`Server`](../../internal/library/mcp.go), [`ServeRun`](../../internal/library/mcp_run.go) | Library already supports configured servers, prompts/resources/templates and protocol notifications; SDK declares tools | **Existing data/protocol integration.** Automatic ownership binding of a module's MCP service is not supplied, but this is not an unimplemented extension runtime. |
| Ordinary agent wiring | [`extensionAgents`](../../internal/agent/extension_actions.go), [`AgentDescriptor`](../../pkg/extension/contract.go) | Rich descriptors reach ordinary discovery, inspect/connect/set/disconnect and checked plans | **Implemented 1.1.** Ordinary discovery requires ConfigFiles; ID/Name-only descriptors remain manual actions. |
| Agent launch / aliases / WSL / own-model metadata | [`agentplug.go`](../../internal/agentplug/agentplug.go) | Legacy adapter has launch/startup/own/aliases/WSL metadata; SDK has Bin/ConfigFiles/Fields | **Partial:** SDK does not declare equivalent launch/wrapper metadata. |
| Library target of an SDK agent | [`pluginTarget`](../../internal/library/targets.go), [`extensionAgent`](../../internal/agent/extension_actions.go) | Legacy `agent.library` binds instructions/MCP/skills; SDK ordinary agents do not declare that target | **Remaining:** automatic Library sync and memory digest for a newly declared SDK agent. |
| Session document conversion | [`extension_actions.go`](../../internal/sessions/extension_actions.go), [`extension_source_inputs.go`](../../internal/sessions/extension_source_inputs.go) | `session.convert`, checked output plans and ordinary CarryOver routes; optional read action `session.inputs.v1` acquires checked journal/blob inputs under one lease | **Implemented through 1.3.** Selected-store/file/parent snapshots and target-store guards protect ordinary application; deferred explicit callers retain source-validation responsibility. |
| Session source / discovery / reader / resume | [`allFilesWith`, `parserFor`, `resumeCommand`](../../internal/sessions/sessions.go) | Existing mappings discover, parse and resume supported native formats | **Remaining:** SDK source/reader/resume registration. Explicit unknown-format conversion does not add a Sessions indexer. |
| Synchronous gateway middleware | [`middleware.go`](../../internal/middleware/middleware.go), [`gwmw`](../../internal/gwmw/gwmw.go) | Legacy onRequest/onAccount/onProvider/onEvent/onResponse chain | **Remaining:** SDK adapter at those points. Request recall hooks do not replace stream/account/provider middleware. |
| Asynchronous request / completed-response / sign-in hooks | [`GatewayBegin`, `GatewayRun`, `GatewaySignIn`](../../internal/plugin/gateway.go), [`hooks.go`](../../pkg/extension/hooks.go) | API 1.2 `gateway-hooks` enters ordinary request/completion/sign-in owners with origin, effect and lease checks | **Implemented 1.2, scoped host proof.** Recall is read-only; capture keeps the selected generation and durable effect policy. This is not synchronous middleware or built-in Memory access. |
| Global request/attempt/ledger observation | [`Observer`, `Watch`](../../internal/usage/observe.go) | Existing Go/legacy observer points | **Remaining:** `usage-reporter` concerns its provider; it is not a global observer adapter. |
| Routing strategy / seat ordering | [`Strategy`, `Register`](../../internal/routes/routes.go), [`points.go`](../../internal/middleware/points.go) | Existing strategy registry and legacy JS strategies | **Remaining:** SDK descriptor and adapter. |
| Budget limiter / spender | [`Limiter`, `Spender`](../../internal/gwbudget/gwbudget.go) | Host reserve/release/spent contract | **Remaining:** SDK adapter preserving the point's fail-closed policy. |
| Reversible masking | [`Masker`](../../internal/gwmw/gwmw.go), [`plugins/redact`](../../plugins/redact/redact.go) | Dedicated host transformation point | **Remaining:** SDK adapter; ordinary request hooks are not this contract. |
| Search source / gateway web search | [`searchesOf`](../../internal/plugin/host.js), [`gwtool`](../../internal/gwtool/gwtool.go) | Legacy search hooks and the web-search point | **Remaining:** a tool named search does not register a search source. |
| Built-in shared Memory / scoped CRUD / digest | [`memoryMCPCall`](../../internal/gateway/memory_mcp.go), [`memory`](../../internal/memory/memory.go), [`Digest`](../../internal/memory/digest.go) | Separate Memory owner, local caller/project/agent resolution, MCP and Library instructions | **Remaining, optional:** SDK access to this built-in owner. An external memory module can instead use its own store/service with hooks/tools/UI. |
| Semantic Memory / distillation / background indexing | [`Semantic`](../../internal/memory/search.go), [`memembed`](../../internal/memembed/memembed.go), [`memory_distill.go`](../../internal/gui/memory_distill.go) | Built-in search fusion, embedder and jobs | **Remaining:** SDK registration and ownership. This is separate from a generic module service being ready. |
| Library resources and optional module contributions | [`Library`](../../internal/library/library.go), [`Hook`](../../internal/library/hooks.go), [`Subagent`](../../internal/library/subagents.go), [`EnvVar`](../../internal/library/env.go) | Instructions, skills, agent hooks, env and subagents already have apply/remove ownership | **Existing data/owner integration.** An SDK declaration/binding is a possible addition, not a missing runtime. Arbitrary file plans do not join regular Library reconciliation automatically. |
| Library sources and imported packages | [`Source`](../../internal/library/skills.go), [`Claude bundles`](../../internal/library/plugin.go), [`Pi management`](../../internal/library/piext.go) | GitHub/Git/folder sources and imported resources retain their existing owners | **Existing import category.** Skills/bundles/Pi package management are not unimplemented SDK runtimes; automatic SDK package/resource lifecycle binding would be a separate feature. |
| Window pages / panels / replacements / row actions | [`windowmeta`](../../internal/windowmeta/window.go), [`extension_window.go`](../../internal/gui/extension_window.go) | Existing page/panel renderer and package metadata; named action/service bindings, revision/config refs | **Implemented 1.1.** Adjacent assets alone do not register a window. |
| Other browser bridge functions | [`pagebridge.js`](../../internal/gui/pagebridge.js), [`ui.d.ts`](../../sdk/js/ui.d.ts) | Existing global bridge has theme/read/context/onAction/image/visibility APIs | **Partial:** host functionality exists; typed SDK ui facade exposes ready/service/backend/config only. |
| Generated settings and built-in UI slots | [`Manifest.Settings/Parts/Client`](../../internal/pluginrt/pluginrt.go), [`kernel.js`](../../internal/gui/assets/kernel.js) | SDK has root config CAS and package window metadata | **Partial:** no SDK settings-schema or general built-in-parts adapter. |
| pluginrt registry / InUse / Requires / Provides | [`Manifest`, `Ctx`, `Runtime.Reconcile`](../../internal/pluginrt/pluginrt.go) | Compile-time Go plugin registry with scoped disposal and services | **Remaining:** no equivalent SDK contribution registration. Private sidecar dependencies do not expose arbitrary Go services. |
| Headless/background activation | [`Module.Start`](../../pkg/extension/lifecycle.go), [`preparePrimary`](../../internal/extension/components.go) | Startup/dispose and eager/lazy services for an activated group | **Partial:** no declared app/gateway-wide activation owner or scheduler registration. |
| CLI command registration | [`sdkPluginRun`](../../internal/cli/sdk_cli.go), [`main.go`](../../internal/cli/main.go) | Manual SDK call/plan/config/lifecycle commands exist | **Partial:** modules cannot register arbitrary top-level commands/flags. |

## Provider operations, modalities and transports

The canonical protocol enum is not a complete provider-operation inventory.
[`Protocol`](../../pkg/protocolext/inbound.go) names four conversational
families: Chat, Responses, Messages/Anthropic and Gemini, plus Models/Tool
inbound operations. Image generation, transcription or token counting can
be operations within or beside those families.

Legacy [`pluginFetch`](../../internal/provider/plugins.go) retains HTTP
method/path/body and uses the selected plugin account. SDK API 1.3 separately
declares conversational `protocols` and a finite provider `operations` list.
[`extension_operations.go`](../../internal/provider/extension_operations.go)
matches exact method/path pairs; both the actual leased manifest and the
author SDK reject undeclared operations before dispatch. Selected upstream
bytes, content type, method/path/query and API 1.3 escaped path reach the
module through `ExecuteRequest` and typed `ExecuteOptions`.
The registry adds no arbitrary URL handler or public gateway route.
See [ADR 0008](../adr/0008-provider-operation-contracts.md).

| Operation / metadata / transport | Existing upstream or legacy path | SDK contract and actual adapter | Evidence / boundary |
| --- | --- | --- | --- |
| Multimodal conversation input | [`plugin.Model.Inputs/Outputs`](../../internal/plugin/providers.go), copied by [`serves`](../../internal/provider/plugins.go), includes image/audio/video/pdf | API 1.3 [`Model`](../../pkg/extension/types.go) declares kind, input/output modalities, default/maximum context, temperature, release, tiers, rates and reasoning defaults; ordinary projection retains them | **Implemented 1.3 metadata.** Nil/unknown and explicit empty modality lists stay distinct. Metadata grants no transport permission; legacy fields outside this DTO remain gaps. |
| Image generation / edits | [Ordinary image selection](../../internal/gateway/draw.go) supports OpenAI-compatible image routes and Gemini image generation | API 1.3 `images.generate`/`images.edit` declarations admit the corresponding exact upstream requests; image kinds/output modalities enter ordinary selection | **Implemented finite operation adapter.** Modules supply vendor execution. Gemini generateContent remains its native protocol path; no universal image codec or live-vendor parity is implied. |
| Audio transcription / translation / speech | [`transcribe`](../../internal/gateway/transcribe.go), [`speech`](../../internal/gateway/speech.go), [`retrieveOpen`](../../internal/gateway/embeddings.go) retain ordinary caller/signing/transport owners | API 1.3 `audio.transcribe`, `audio.translate`, `audio.speech` carry selected multipart/binary/streaming requests and model kinds | **Implemented declared upstream operations.** The registry does not create additional public audio routes or implement a vendor's audio service. |
| Video create / status / content | [`customVideomakersOf`, `keyMark`, `markedKey`](../../internal/gateway/video.go) select video models and retain result ownership | API 1.3 `videos.create/get/content/delete/remix`; stable module/provider/account marks bind continuations across credential refresh | **Implemented finite operation/account adapter.** Switching accounts cannot take over a video; vendor status/cancel semantics still belong to the module. |
| Embeddings / rerank | [`retrieve`, `retrieveOpen`](../../internal/gateway/embeddings.go) send selected requests through provider signing/transport | API 1.3 `embeddings`/`rerank`, projected model kinds and declared-operation checks enter ordinary retrieval | **Implemented finite operation adapter.** Accepted/uncertain/completed work cannot automatically fall through to another account/provider. |
| Anthropic token counting | [`countOn`](../../internal/gateway/gateway.go) forwards `/v1/messages/count_tokens` when supported and otherwise may estimate | The SDK messages-path branch accepts that path as Anthropic and preserves it in `Options.path` | **Reachable source path, targeted parity unverified here.** It is not simply omitted because there is no count protocol enum. |
| Dedicated decision models versus ordinary classifiers | Legacy [`Provider.Decide`, `Model.Decides`](../../internal/plugin/providers.go) feed [`DecidesModel`](../../internal/provider/decide.go) and System One; direct OpenAI Decisions eligibility is separately checked by [`canDecide`](../../internal/gateway/decisions.go) | SDK model/provider projection lacks that dedicated decision metadata/base. An ordinary SDK chat model can still answer the core's normal [classifier](../../internal/gateway/classify.go) requests | **Dedicated decision metadata/adapter gap; ordinary classifier path exists.** Legacy plugins are explicitly refused by direct `canDecide`; their System One integration is not full OpenAI Decisions parity. |
| Client Responses WebSocket | [`responsesSocket.turn`](../../internal/gateway/responses_ws.go) turns a socket frame into the ordinary POST handler path | That shared path can route to an SDK Responses provider; no SDK raw-socket callback is declared | **Indirect integration in source.** This census did not execute a WebSocket + SDK-provider fixture; no blanket unsupported or full parity claim. |
| Window service WebSocket | [`servePageServiceTarget`](../../internal/gui/pageservice.go), [`serveExtensionWindowService`, `verifyWindowServiceProof`](../../internal/gui/extension_window_service.go) support upgrade handling, retain duplex bodies and hold the response lease | Composite UI uses this existing HTTP/SSE/WebSocket service adapter with component proof | **Implemented adapter path.** WebSocket service transport must not be confused with a provider protocol or ModuleRPC streaming. |
| Module-owned upstream sockets | Legacy provider implementations own vendor I/O behind their fetch; exec/native/Bun SDK implementations likewise own their networking | [`network.proxy`](../../pkg/extension/README.md) resolves policy for HTTP(S)/WS(S), but is not a host socket-dial API | **Networking implementation choice.** No separate public provider-WebSocket registration point was established by this bounded source inspection. |

The API 1.3 media/retrieval fixture is
[`extension_media_test.go`](../../internal/gateway/extension_media_test.go);
provider transport/catalog tests and
[`provider_operation_admission_test.go`](../../internal/extension/provider_operation_admission_test.go)
cover raw bytes and actual leased-generation admission. These are scoped
synthetic proofs, not live implementations of every declared vendor operation.
Dedicated decisions, arbitrary endpoints and the extras in the inventory
remain separate work.

The browser extension is another boundary:
[`browserext`](../../internal/browserext/browserext.go) packages a companion
client for the gateway's browser relay. It is not an installable SDK runtime.
Likewise, the legacy [Pi provider adapter](../../internal/plugin/pi.js) uses
Pi providers but deliberately leaves Pi session commands/tools/renderers/event
handlers unused; those are not already-supported Magpie hooks waiting for
a transport change.

The domain-memory fixture
[`TestGatewayHooksMemorySharesDurableStoreWithTools`](../../internal/extension/gateway_hooks_test.go)
exercises tool write,
another agent's recall, completed-response capture and reopening the same
private store after Manager restart. That is evidence for an external
module's own memory flow, not built-in Memory CRUD/digest integration.
The [API 1.2 validation record](../adr/0006-gateway-hooks-validation.md)
records its host evidence; the existence of a source fixture alone is not
a passing gate for a different tree.

## API 1.2 hooks and private-tool boundary

The authoritative public contract is
[`pkg/extension/hooks.go`](../../pkg/extension/hooks.go), with matching JS
declarations in [`sdk/js`](../../sdk/js/index.d.ts). Gateway hooks are primary
methods, unary, without selected-provider/account/auth scope. Request is read;
response/sign-in are write and may emit an acknowledged acceptance receipt.
State is at most 64 KiB of JSON. Origins default to local and magpie-key;
the five SDK origins are local, magpie-key, gateway-key, remote and browser.
Browser is explicit opt-in: a loopback page is not treated as default local
recall/capture authority, even with the default key. Legacy hooks retain
their existing four-origin semantics. Sign-in still requires local permission.
These constraints do not turn
hooks into a Memory owner API, stream middleware or global observer.

The host's read-only request marker denies `state.cas` from recall.
Response/sign-in writes use the root operation's durable acceptance and
no-replay rules. Those checks belong to the runtime/gateway owner tests;
JS fixtures that acknowledge a write or return a host refusal do not prove
the real host marker or journal behavior.

API 1.2 tool exposure distinguishes shared and local-owner. Descriptor
validation and public projection are SDK responsibilities; deciding which
ordinary MCP/hub callers may discover or invoke a local-owner tool belongs
to the host adapter. A JS unit test cannot prove those host access paths.
The [owner validation](../adr/0006-gateway-hooks-validation.md) includes these
host checks; JS conformance alone must not be substituted for them.

## API 1.3 account and session boundaries

[ADR 0007](../adr/0007-transactional-account-refresh.md) defines the host-owned
refresh transaction: strict per-account OS ownership, a durable dispatch
fence, one unary exchange, atomic credentials/expiry/generation/receipt
publication, and an uncertain fence after an ambiguous result. API 1.3
ordinary provider preflight renews known near-expiry credentials. It accepts
only a receipt-proven refresh successor of a cached account; manual
replacement or deletion remains stale. A module lock or state CAS is not
that transaction, and no unseen rotated token is recoverable after a crash.

The session adapter's `session.inputs.v1` action plans additional source
files. The host chooses the selected native store, checks paths/digests and
bounded inputs, retains planner/converter on one lease, and rechecks source
file, permission, parent-directory and store identities before ordinary
application. This enables attachment-aware native OMP conversion through
existing readers/resume owners; it is not generic SDK reader registration.
Explicit callers retaining an output plan still own deferred source checks.

## Remaining contracts and a possible API 1.4

Use common declaration/lifecycle machinery with typed subsystem adapters,
rather than a new capability for every feature or an unrestricted host RPC.
The following remain proposed boundaries, not current API 1.3 fields. A
future API 1.4 is a possible delivery increment, not an accepted manifest
version or a promise that these all ship together:

1. **Contribution registration:** a versioned descriptor names an ID, a
   host-known point/contract and a declared action. The host validates DTOs,
   activates/revokes the adapter and owns its failure policy. Existing nine
   primary paths remain on the primary; ModuleRPC remains unary.
2. **Owner services/resources:** scoped Memory operations and declarative
   Library resources reuse their current owners. The host derives agent,
   project and caller context; a plugin-supplied session string alone does
   not select Memory authority. Checked writes, backups and reconciliation
   stay in the owning subsystem.
3. **Targets and activation:** Library metadata for SDK agents, session
   source/read/resume descriptors, settings schema and gateway-owned
   background activation reuse existing flows. They are separate adapters
   with separate proofs, not implied by a ready sidecar or UI binding.

The implemented memory slice is gateway hooks, local-owner tools and
module-owned storage. Explicit built-in Memory owner access is an optional follow-up,
alongside independent Library targets/resources and session readers.
Global routing, observation, budget and masking retain their own timing and
failure contracts; a shared registration envelope cannot erase those.

## Evidence and limits

The completed API 1.1 evidence includes real native host-PID / external-helper
separation, held old lazy v1/config1 after v2/config2 publication, actual GUI
backend/status/SSE/cookies/off/lease hold, Go-host/Bun race count 3, and the
87 Node / 87 Bun checks with strict types. See the
[runtime reference](executable-extensions.md) and
[comparison](extension-runtime-comparison.md) for those boundaries.
The [native/composite validation record](../adr/0005-native-composite-validation.md)
is historical API 1.1 evidence, not an API 1.2 acceptance result.

API 1.2 was a separate integration increment. Its tests name the actual
snapshot and distinguish public/server conformance from ordinary gateway
and tool-owner behavior. JS public/server parity has 102 Node and 102 Bun
passes without skips, plus strict declarations, on an independent usbox copy
of base tree `18509754f88c867ae799be359d430480dc8470b0` with the JS overlay.
The [hook tests](../../sdk/js/test/gateway-hooks.test.js) include browser
default-deny/explicit opt-in, credential-scope refusal, exact original-wire
state bounds and unary effects. The
[real process checks](../../sdk/js/test/gateway-process.test.js) use Node/Bun
children with fixture host state; they do not prove ordinary gateway routing,
local-owner tool filtering or a durable Go Memory owner.
API 1.3 refresh/operation/source-input acceptance is recorded separately by
the owners; [ADR 0007](../adr/0007-transactional-account-refresh.md) and
[ADR 0008](../adr/0008-provider-operation-contracts.md) define current
responsibilities. OpenCode's independent native slice passed ordinary
expiry renewal and all four packaged model routes on Linux. Other module
slices have their own source/proof boundaries in the
[delivery plan](../plans/plugin-first-delivery.md).
This census adds no full-repository green, benchmark or live vendor-parity
claim and does not turn the remaining rows into implemented SDK adapters.
