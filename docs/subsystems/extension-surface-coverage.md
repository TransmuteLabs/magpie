# Extension surface coverage

Date: November 13, 2026.

Magpie has several kinds of extension: built-in Go plugins, legacy provider
and gateway plugins, agent adapters, Library resources and installable SDK
modules. SDK API 1.1 implements its ten negotiated capabilities and composite
runtime/window integration. It does not replace every upstream extension
surface. API 1.2 gateway hooks and tool exposure are being integrated; public
contracts alone do not establish completed host integration.

This census is for modules we maintain ourselves, keeping integration logic
outside frequently merged upstream code. It distinguishes three layers:

- **Contract:** public descriptors, DTOs, method negotiation and validation.
- **Adapter:** an ordinary Magpie flow discovers and calls the module through
  the owning subsystem, with its scope, lifecycle and failure policy.
- **Domain module:** our actual provider, memory backend or agent implementation,
  with its own behavior, account correctness and migration evidence.

A complete contract or fixture is not a migrated domain module. No vendor
implementation was migrated by this SDK work. `module.call`, checked file
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

“Implemented 1.1” describes the existing SDK path and its scoped fixtures.
“Partial” identifies an actual boundary. “Remaining” means no SDK adapter.
“1.2 integration” remains pending owner tests for the complete host path.
Data/resource imports already supported by Library are identified separately;
an optional SDK ownership binding is not a missing extension runtime.

| Type | Source evidence | SDK contract and actual adapter | Status / remaining boundary |
| --- | --- | --- | --- |
| Native / exec / Bun installation and distribution | [`native.Register`](../../pkg/extension/native/native.go), [`release.go`](../../internal/extension/release.go), [`release_auth.go`](../../internal/extension/release_auth.go) | Real native C ABI without author C glue; executable/Bun options; full-package hashes, private developer auth, pins/updates | **Implemented 1.1.** Runtime conformance does not imply vendor migration or identical platform proof. |
| Composite components, readiness, config and disposal | [`composite.go`](../../pkg/extension/composite.go), [`components.go`](../../internal/extension/components.go) | Primary + private exec/Bun services, DAG, readiness, root CAS snapshot, opaque local State, same-generation leases | **Implemented 1.1.** Services are private components, not cross-module pluginrt services. |
| Provider execution, models, account auth, quota/usage, operations | [`extension_provider.go`](../../internal/provider/extension_provider.go), [`extension_auth.go`](../../internal/provider/extension_auth.go), [`extension_transport.go`](../../internal/provider/extension_transport.go) | Ordinary provider/account/routing owners use declared SDK capabilities | **Implemented 1.1** within the DTOs and executor path whitelist; not every provider operation or legacy model metadata field. See the extended matrix below. |
| Legacy provider extras | [`Provider`](../../internal/plugin/providers.go), [`plugin_checkin.go`](../../internal/provider/plugin_checkin.go), [`plugin_keepalive.go`](../../internal/provider/plugin_keepalive.go) | `auth.refresh` and usage DTOs cover part of the lifecycle | **Partial:** no equivalent declarations/adapters for check-in, outside calls, provider options and all rotating/keepAlive metadata. |
| Incoming protocols and response codecs | [`protocol_extensions.go`](../../internal/gateway/protocol_extensions.go), [`inbound.go`](../../pkg/extension/inbound.go) | Installed codecs enter ordinary guards/canonical dispatch and retain the primary lease through response encoding | **Implemented 1.1** for negotiated canonical operations; not arbitrary gateway route registration. |
| Plugin tools / MCP tools | [`extension_tools.go`](../../internal/plugin/extension_tools.go), [`extension_tools.go`](../../internal/gateway/extension_tools.go) | `tool-provider` enters ordinary plugin-tools MCP/hub | **Implemented 1.1.** API 1.2 adds `ToolDescriptor.exposure`; local-owner filtering remains a separately tested host responsibility. |
| MCP server configuration and protocol integration | [`Server`](../../internal/library/mcp.go), [`ServeRun`](../../internal/library/mcp_run.go) | Library already supports configured servers, prompts/resources/templates and protocol notifications; SDK declares tools | **Existing data/protocol integration.** Automatic ownership binding of a module's MCP service is not supplied, but this is not an unimplemented extension runtime. |
| Ordinary agent wiring | [`extensionAgents`](../../internal/agent/extension_actions.go), [`AgentDescriptor`](../../pkg/extension/contract.go) | Rich descriptors reach ordinary discovery, inspect/connect/set/disconnect and checked plans | **Implemented 1.1.** Ordinary discovery requires ConfigFiles; ID/Name-only descriptors remain manual actions. |
| Agent launch / aliases / WSL / own-model metadata | [`agentplug.go`](../../internal/agentplug/agentplug.go) | Legacy adapter has launch/startup/own/aliases/WSL metadata; SDK has Bin/ConfigFiles/Fields | **Partial:** SDK does not declare equivalent launch/wrapper metadata. |
| Library target of an SDK agent | [`pluginTarget`](../../internal/library/targets.go), [`extensionAgent`](../../internal/agent/extension_actions.go) | Legacy `agent.library` binds instructions/MCP/skills; SDK ordinary agents do not declare that target | **Remaining:** automatic Library sync and memory digest for a newly declared SDK agent. |
| Session document conversion | [`extension_actions.go`](../../internal/sessions/extension_actions.go), [`CarryOver`](../../internal/sessions/carry_convert.go) | `session.convert`, checked output plans and declared ordinary CarryOver routes | **Implemented 1.1.** Conversion pins the selected native source snapshot. |
| Session source / discovery / reader / resume | [`allFilesWith`, `parserFor`, `resumeCommand`](../../internal/sessions/sessions.go) | Existing mappings discover, parse and resume supported native formats | **Remaining:** SDK source/reader/resume registration. Explicit unknown-format conversion does not add a Sessions indexer. |
| Synchronous gateway middleware | [`middleware.go`](../../internal/middleware/middleware.go), [`gwmw`](../../internal/gwmw/gwmw.go) | Legacy onRequest/onAccount/onProvider/onEvent/onResponse chain | **Remaining:** SDK adapter at those points. Request recall hooks do not replace stream/account/provider middleware. |
| Asynchronous request / completed-response / sign-in hooks | [`GatewayBegin`, `GatewayRun`, `GatewaySignIn`](../../internal/plugin/gateway.go), [`hooks.go`](../../pkg/extension/hooks.go) | API 1.2 declares `gateway-hooks`, gateway.request/response/signin and primary handler flags | **1.2 integration:** contract/server parity and runtime/gateway adapters must be proven together. Not complete solely because DTOs exist. |
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
method/path/body and uses the selected plugin account. The SDK
[`execute`](../../internal/provider/extension_transport.go) accepts chat,
responses, messages and Gemini-style paths, rejecting other paths before
calling `provider.execute`. It nevertheless passes the original method,
path and query in `ExecuteRequest.Options`. That distinction matters for
token counting and prevents an incorrect blanket “all non-chat unsupported”
claim. Model DTO/projection coverage is a separate question from transport.

| Operation / metadata / transport | Existing upstream or legacy path | SDK contract and actual adapter | Evidence / boundary |
| --- | --- | --- | --- |
| Multimodal conversation input | [`plugin.Model.Inputs/Outputs`](../../internal/plugin/providers.go), copied by [`serves`](../../internal/provider/plugins.go), includes image/audio/video/pdf | [`sdk.Model`](../../pkg/extension/types.go) has Vision but no complete input/output modality declarations; its [catalog projection](../../internal/provider/extension_provider.go) copies only that narrower metadata | **Partial metadata parity.** Image input in supported conversational protocols is not the same as an image-generation operation. |
| Image generation / edits | [Plugin image selection](../../internal/gateway/draw.go) explicitly supports OpenAI-compatible image routes and Gemini image generation, using model IDs/output modalities | `/images/*` is outside the SDK executor whitelist; SDK lacks the rich output metadata used by ordinary image selection | **Ordinary image-operation gap.** A Gemini module can still implement image-producing generateContent within Gemini; not all image behavior requires a new protocol enum. No new media fixture was run for this census. |
| Audio transcription / translation / speech | [`transcribe`](../../internal/gateway/transcribe.go) preserves multipart bodies; [`speech`](../../internal/gateway/speech.go) and [`retrieveOpen`](../../internal/gateway/embeddings.go) use provider signing/transport and stream answers | `/audio/*` is outside the SDK executor whitelist; full audio model-kind metadata is absent | **Operation/catalog gap.** Legacy generic fetch is broader than the current SDK dispatch; declaring a service or a tool does not add these ordinary routes. |
| Video create / status / content | [`customVideomakersOf`, `keyMark`, `markedKey`](../../internal/gateway/video.go) support plugin video outputs and retain the account that created a video | Video metadata and the required POST/GET operations are not represented by the current SDK executor dispatch | **Operation/lifecycle gap**, including result-account affinity; no generic video capability is claimed from byte transport alone. |
| Embeddings / rerank | [`retrieve`, `retrieveOpen`](../../internal/gateway/embeddings.go) send non-chat operations through the selected provider's signing and `Do`; legacy fetch retains their paths | `/embeddings` and `/rerank` are rejected by SDK dispatch; SDK model projection does not declare catalog `Serves` kinds | **Operation/model-kind gap.** These existing provider paths extend the initial provider row rather than proving a separate registered plugin runtime. |
| Anthropic token counting | [`countOn`](../../internal/gateway/gateway.go) forwards `/v1/messages/count_tokens` when supported and otherwise may estimate | The SDK messages-path branch accepts that path as Anthropic and preserves it in `Options.path` | **Reachable source path, targeted parity unverified here.** It is not simply omitted because there is no count protocol enum. |
| Dedicated decision models versus ordinary classifiers | Legacy [`Provider.Decide`, `Model.Decides`](../../internal/plugin/providers.go) feed [`DecidesModel`](../../internal/provider/decide.go) and System One; direct OpenAI Decisions eligibility is separately checked by [`canDecide`](../../internal/gateway/decisions.go) | SDK model/provider projection lacks that dedicated decision metadata/base. An ordinary SDK chat model can still answer the core's normal [classifier](../../internal/gateway/classify.go) requests | **Dedicated decision metadata/adapter gap; ordinary classifier path exists.** Legacy plugins are explicitly refused by direct `canDecide`; their System One integration is not full OpenAI Decisions parity. |
| Client Responses WebSocket | [`responsesSocket.turn`](../../internal/gateway/responses_ws.go) turns a socket frame into the ordinary POST handler path | That shared path can route to an SDK Responses provider; no SDK raw-socket callback is declared | **Indirect integration in source.** This census did not execute a WebSocket + SDK-provider fixture; no blanket unsupported or full parity claim. |
| Window service WebSocket | [`servePageServiceTarget`](../../internal/gui/pageservice.go), [`serveExtensionWindowService`, `verifyWindowServiceProof`](../../internal/gui/extension_window_service.go) support upgrade handling, retain duplex bodies and hold the response lease | Composite UI uses this existing HTTP/SSE/WebSocket service adapter with component proof | **Implemented adapter path.** WebSocket service transport must not be confused with a provider protocol or ModuleRPC streaming. |
| Module-owned upstream sockets | Legacy provider implementations own vendor I/O behind their fetch; exec/native/Bun SDK implementations likewise own their networking | [`network.proxy`](../../pkg/extension/README.md) resolves policy for HTTP(S)/WS(S), but is not a host socket-dial API | **Networking implementation choice.** No separate public provider-WebSocket registration point was established by this bounded source inspection. |

These rows separate contract shape, reachable production paths and fixture
proof. They do not add API 1.3 fields or adapters. A future provider-operation
phase must decide operation dispatch, rich model metadata, binary/streaming
bounds and account affinity together; a new capability name alone would not
complete any of them.

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
Its execution result belongs to the API 1.2 integration owner's test record;
the existence of the fixture alone is not a passing gate.

## API 1.2 boundary

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
Keep the complete gateway/tool integration marked pending until those owner
tests pass.

## Smallest reusable next contracts

Use common declaration/lifecycle machinery with typed subsystem adapters,
rather than a new capability for every feature or an unrestricted host RPC.
The following are proposed boundaries, not API 1.1 fields:

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

The current slice is gateway hooks, local-owner tools and module-owned
storage. Explicit built-in Memory owner access is an optional follow-up,
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

API 1.2 is a separate integration increment. Its tests must name the actual
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
Final acceptance records belong to the integration
owner. This census adds no full-repository green, benchmark, vendor parity
or migrated-domain-module claim.
