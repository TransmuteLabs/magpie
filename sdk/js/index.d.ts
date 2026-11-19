import type { StorageSourceDescriptor } from "./storage-source.js";
import type { AccountKeepAliveDescriptor } from "./account-keepalive.js";
import type { AccountTaskDescriptor } from "./account-tasks.js";
export * from "./account-tasks.js";
export * from "./agent-model-metadata.js";
export * from "./agent-decision-models.js";
export * from "./account-keepalive.js";
import type { CatalogAccessDescriptor } from "./catalog-projection.js";
import type { LedgerObservationDescriptor } from "./ledger.js";
export * from "./ledger.js";
import type { RequestObservationDescriptor } from "./request-observer.js";
export * from "./request-observer.js";
import type { BudgetPolicyDescriptor } from "./budget-policy.js";
export * from "./budget-policy.js";
import type { MaskingRuleSet } from "./masking-rules.js";
export * from "./masking-rules.js";
export * from "./catalog-projection.js";
export * from "./own-calls.js";
export * from "./storage-source.js";
import type { SearchSourceDescriptor } from "./search-sources.js";
import type { GatewayMiddlewareDescriptor } from "./middleware.js";
export * from "./middleware.js";
export { type SearchSourceDescriptor, type SearchRequest, type SearchPage, type SearchResult, validateSearchRequest, validateSearchResult } from "./search-sources.js";
export type UInt64 = number | bigint;
export type JSONValue = null | boolean | number | bigint | string | JSONValue[] | { [key: string]: JSONValue };
export interface AccountCheckinDescriptor {
  action: string;
  recheck?: boolean;
}
export interface AccountCheckinRequest {
  day: string;
  proxy?: string;
}
export interface AccountCheckinResult {
  outcome: "claimed" | "done" | "inactive" | "ineligible" | "failed" | "captcha" | "own-app";
  credit?: number | null;
  streak?: number | null;
  message?: string;
  until?: string;
}
export function validateAccountCheckinRequest(value: unknown): AccountCheckinRequest;
export function validateAccountCheckinResult(value: unknown): AccountCheckinResult;
export type Protocol = "chat" | "responses" | "messages" | "anthropic" | "gemini";
export type Capability = "inbound-codec" | "outbound-executor" | "agent-wiring" | "account-auth"
  | "model-discovery" | "usage-reporter" | "operation-control" | "tool-provider" | "session-migration" | "module-rpc" | "gateway-hooks"
  | "session-source" | "session-related-inputs" | "agent-process" | "native-account-operations" | "session-storage-source" | "request-observer" | "ledger-observer" | "gateway-middleware" | "request-policy" | "budget-policy" | "masking-rules";
export type Method = "inbound.decode" | "inbound.response" | "provider.execute" | "provider.models"
  | "provider.usage" | "provider.calls" | "auth.start" | "auth.finish" | "auth.refresh" | "auth.logout" | "operation.status"
  | "operation.cancel" | "tool.call" | "agent.plan" | "agent.inspect" | "session.convert" | "session.source.read" | "module.call"
  | "gateway.request" | "gateway.response" | "gateway.signin" | "session.storage.list" | "session.storage.read" | "budget.reserve" | "budget.spent" | "request.observe" | "ledger.observe"
  | "middleware.request" | "middleware.account" | "middleware.provider" | "middleware.event" | "middleware.response";
export type ComponentMethod = "component.configure" | "component.ready" | "component.stop";
export type HostMethod = "request.recover" | "state.get" | "state.cas" | "network.proxy" | "account.replace" | "component.call" | "usage.invalidate" | "usage.append" | "diagnostics.emit" | "memory.search"
  | "storage.native" | "storage.list" | "storage.read" | "storage.check" | "catalog.read"
  | "accountTasks.schedule" | "accountTasks.cancel" | ProcessHostMethod;
export interface RequestFailureFacts {
  status: number; code?: string; type?: string; message?: string;
  headers?: Record<string, string>; operation: "provider.execute" | "tool.call" | "module.call";
  /** Body hints: nonnegative integer milliseconds (at most 30 days), or RFC3339 timestamp. */
  retryAfterMs?: number; resetAt?: string;
  phase: "response"; complete: boolean;
}
export type RequestFailureClass = "unknown" | "transient-rate" | "quota" | "auth-refusal"
  | "request-refusal" | "content-refusal" | "transient-outage";
export interface RequestFailureRule {
  id: string; status?: number[]; code?: string; type?: string; contains?: string;
  headers?: Record<string,string>; operation?: string; phase?: string;
  class: RequestFailureClass; scope: "request" | "model" | "account" | "unknown";
}
export interface RequestRecoveryRequest {
  step: string; facts: RequestFailureFacts; rules: RequestFailureRule[];
  evidence: "" | "read-only" | "rejected-before-effect";
}
export interface RequestFailureWindow {
 id: string; models?: string[]; notModels?: string[]; retryAfterMs?: number; resetAt?: string;
}
export interface RequestRefusal extends RequestRecoveryRequest { windows?: RequestFailureWindow[] }
export function refusedExecution(failure: RequestRecoveryRequest, windows?: RequestFailureWindow[]): ExecuteResult;
export interface RequestRecoveryResult {
 step: string; action: "wait" | "next-account" | "terminal";
 totalRetries: number; providerRetries: number;
  retry: boolean; reason: string; class: RequestFailureClass;
  scope: "request" | "model" | "account" | "unknown"; rule: string; attempt: number;
  nextAllowedAt?: string;
}
export interface RequestPolicyHost {
  hostCall(method: "request.recover", params: RequestRecoveryRequest): Promise<RequestRecoveryResult>;
}
/** A retry requires the exact named step, action wait, complete safe response facts and explicit evidence.
 * Non-retry diagnostics remain classification only. No credential recovery in API 1.12. */
export function decideRequest(signal: AbortSignal, host: RequestPolicyHost, request: RequestRecoveryRequest): Promise<RequestRecoveryResult>;
/** Host owns budgets and waiting; transport receives redirect: manual. Transport errors are not retried.
 * Classify a clone to preserve the caller's body. Redirects may be classified, but never ask for recovery. */
export function requestWithPolicy(signal: AbortSignal, host: RequestPolicyHost, step: string,
  request: Request, transport: typeof fetch,
  classify: (response: Response) => RequestRecoveryRequest | null | Promise<RequestRecoveryRequest | null>
): Promise<{response: Response; decision: RequestRecoveryResult | null}>;
export interface Contract {
  version: { major: 1; minor: 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12 | 13 | 14 | 15 };
  capabilities: readonly Capability[];
}
export type Effort = "none" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";
export type ProviderOperation = "embeddings" | "rerank" | "images.generate" | "images.edit"
  | "audio.transcribe" | "audio.translate" | "audio.speech"
  | "videos.create" | "videos.get" | "videos.content" | "videos.delete" | "videos.remix" | "systemone";
export type Modality = "text" | "image" | "audio" | "video" | "pdf";
export interface ModelPrice {
  currency?: "USD";
  input?: number;
  output?: number;
  cacheRead?: number;
  cacheWrite?: number;
  cacheWrite1h?: number;
}
export interface Model {
  id: string;
  name?: string;
  protocols?: Protocol[] | null;
  operations?: ProviderOperation[] | null;
  contextWindow?: number;
  maxContextWindow?: number;
  maxOutput?: number;
  tools?: boolean;
  vision?: boolean;
  reasoning?: boolean;
  price?: ModelPrice | null;
  efforts?: Effort[] | null;
  kind?: "chat" | "image" | "video" | "embedding" | "rerank" | "speech" | "transcription" | "decision";
  inputModalities?: Modality[] | null;
  outputModalities?: Modality[] | null;
  released?: string;
  temperature?: boolean | null;
  serviceTiers?: ("priority" | "ultrafast")[];
  free?: boolean;
  rate?: number;
  rateWas?: number;
  alwaysThinks?: boolean;
  dynamicTools?: boolean;
  defaultEffort?: Effort;
}
export interface AgentField {
  key: string;
  label: string;
}
export interface AgentDescriptor {
  id: string;
  name: string;
  bin?: string;
  configFiles?: string[];
  fields?: AgentField[];
  aliases?: string[];
  ua?: string[];
  launch?: AgentLaunch;
  profile?: AgentProfile;
  library?: AgentLibrary;
}
export interface AgentLaunch {
  env?: Record<string, string>;
  args?: Record<string, string>;
  model?: string;
  plain?: string[];
}
export interface AgentProfile { target: string }
export interface AgentLibrary {
  instructions?: string;
  mcp?: string;
  mcpFormat?: string;
  skills?: string;
}
export interface SessionDescriptor {
  id: string;
  name: string;
  from?: string;
  to?: string;
  outputDir?: string;
}
export interface SessionSourceDescriptor {
  id: string; agent: string; codec: string; container: "jsonl";
  stores: SessionSourceStore[]; layout: SessionSourceLayout;
  transcript: boolean; resume: "none" | "native" | "agent";
  agentResume?: SessionResumeRecipe;
  descendants?: SessionSourceDescendants;
}
export interface SessionResumeRecipe { agent: string; args: string[] }
export function validateSessionResume(manifest: Manifest, owner: string, mode: string, recipe?: SessionResumeRecipe): void;
export interface SessionSourceDescendants {
  kind: "journal-stem-jsonl";
  excludeDirectories?: string[];
}
export interface SessionSourceIdentity { path: string; sessionId: string }
export type SessionSourceStore = { slot: string } & (
  { nativeRootSlot: "claude.projects" | "codex.sessions" | "codex.archived-sessions" | "omp.sessions" | "devin.file-sessions"; homeRelativePath?: never }
  | { homeRelativePath: string; nativeRootSlot?: never });
export type SessionSourceLayout =
  | { kind: "flat"; idFrom: "filename-stem"; entry?: never }
  | { kind: "bucketed"; idFrom: "filename-after-first-underscore"; entry?: never }
  | { kind: "directory"; idFrom: "parent-directory"; entry: "log.jsonl" };
export interface SessionSourceLocator { storeRef: string; path: string; sessionId: string }
export interface SessionSourceLimits { maxParts: number; maxTextBytes: number }
export interface SessionSourceReadRequest {
  sourceId: string; locator: SessionSourceLocator; snapshot: string;
  mode: "summary" | "transcript"; documents: SessionDocument[]; limits: SessionSourceLimits;
  descendants?: string[];
}
export interface SessionSourceSummary {
  title: string; cwd: string; start?: string; last?: string; parentId?: string; rootId?: string;
  usage?: SessionUsagePoint[];
  subs?: SessionSourceIdentity[];
}
export interface SessionUsagePoint {
  at: string; model: string; input: UInt64; output: UInt64; cacheRead: UInt64; cacheWrite: UInt64; cacheWrite1h: UInt64;
}
export interface SessionSourcePart {
  role: "user" | "assistant" | "tool";
  kind: "text" | "thinking" | "tool_use" | "tool_result" | "context" | "image";
  text: string; name?: string; model?: string; cut?: number;
}
export type SessionSourceReadResult =
  | { mode: "summary"; sessionId: string; summary: SessionSourceSummary; parts?: never; cut?: never }
  | { mode: "transcript"; sessionId: string; parts: SessionSourcePart[]; cut: boolean; summary?: never };
export function sessionSourceID(layout: SessionSourceLayout, name: string): string;
export function sessionSourceDescendantPath(rule: SessionSourceDescendants, selected: string, name: string): boolean;
export function validateSessionSources(manifest: Manifest): void;
export function validateSessionSourceRequest(manifest: Manifest, request: SessionSourceReadRequest, stringify: (value: unknown) => string): SessionSourceReadRequest;
export function validateSessionSourceResult(request: SessionSourceReadRequest, result: SessionSourceReadResult, stringify: (value: unknown) => string): SessionSourceReadResult;
export interface RoutingFlags {
  exact?: boolean; byUse?: boolean; byFree?: boolean; byPlan?: boolean; bySpeed?: boolean; byPrice?: boolean;
}
export interface RoutingStrategyDescriptor {
  id: string; name: string; action: string; flags?: RoutingFlags; timeoutMs?: number;
}
export interface RoutingSeat {
  key: string; account?: boolean; known?: boolean; learns?: boolean; used?: number;
  weekLow?: boolean; soon?: number[]; unreached?: number; lapse?: number; lapsing?: number;
  pace?: number; weight: number; resting?: boolean; spent?: boolean; tokens?: number;
  free?: boolean; freePlan?: boolean; credits?: boolean; first?: boolean; lead?: boolean;
  ttft?: number; tps?: number; left?: number; leftIn?: string; priced?: boolean; cost?: number; cacheCost?: number;
}
export interface RoutingRequest { strategy: string; pool: string; seats: RoutingSeat[] }
export interface RoutingResult { order: number[] }
export function validateRoutingRequest(request: unknown): RoutingRequest;
export function validateRoutingResult(result: unknown, seats: number): RoutingResult;
interface ManifestFields {
  budgetPolicies?: readonly BudgetPolicyDescriptor[];
  maskingRuleSets?: readonly MaskingRuleSet[];
  requestObserver?: RequestObservationDescriptor;
  ledger?: LedgerObservationDescriptor;
  schema: 1;
  id: string;
  version: string;
  entry: string;
  contract: Contract;
  inbounds?: { id: string; path: string }[];
  providers?: { id: string; name: string; protocols?: Protocol[] | null; models?: Model[]; operations?: ProviderOperation[]; calls?: boolean; checkin?: AccountCheckinDescriptor | null; keepAlive?: AccountKeepAliveDescriptor | null; options?: ProviderOption[]; accountTasks?: AccountTaskDescriptor[]; maxConcurrency?: number; maxAccounts?: number }[];
  activation?: "gateway"[] | null;
  agents?: AgentDescriptor[];
  processes?: AgentProcessRecipe[];
  sessions?: SessionDescriptor[];
  sessionSources?: SessionSourceDescriptor[];
  storageSources?: StorageSourceDescriptor[];
  tools?: ToolDescriptor[];
  actions?: ActionDescriptor[];
  services?: ServiceDescriptor[];
  compatibility?: Compatibility;
  window?: WindowDescriptor;
  bindings?: BindingDescriptor[];
  authBindings?: WindowAuthBindingDescriptor[];
  component?: ComponentIdentity;
  hooks?: GatewayHookDescriptor;
  middleware?: GatewayMiddlewareDescriptor;
  catalogAccess?: CatalogAccessDescriptor;
  routingStrategies?: RoutingStrategyDescriptor[];
  searchSources?: SearchSourceDescriptor[];
  accountOperations?: AccountOperationDescriptor[];
}
export type Manifest = ManifestFields & (
  { runtime: "exec" | "bun"; args?: string[] }
  | { runtime: "native"; args?: [] }
);
export interface Scope {
  readonly plugin?: string;
  readonly provider?: string;
  readonly account?: string;
  readonly generation?: UInt64;
  readonly session?: string;
  readonly operation?: string;
  readonly attempt?: UInt64;
}
export interface Call {
  readonly id: string;
  readonly method: Method | ComponentMethod | "sdk.describe";
  readonly action?: string;
  readonly params: JSONValue;
  readonly auth?: JSONValue;
  readonly scope: Scope;
  readonly deadlineUnixMs?: number;
}
export interface Context {
  readonly signal: AbortSignal;
  readonly module?: ModuleContext;
}
export interface Stream {
  emit(event: string, data?: unknown): Promise<void>;
  hostCall<T = JSONValue>(method: HostMethod, params?: unknown): Promise<T>;
}
export type ProcessHostMethod = "process.open" | "process.read" | "process.write" | "process.stdinEOF" | "process.close";
export interface AgentProcessRecipe { id: string; agent: string; argv: string[] }
export interface ProcessOpenRequest { recipeId: string }
export interface ProcessLimits { maxChunkBytes: number; queueChunks: number }
export interface ProcessOpenResult { handle: string; incarnation: string; limits: ProcessLimits }
export interface ProcessReadRequest { handle: string; pipe: "stdout" | "stderr"; seq: string; maxBytes: number }
export interface ProcessReadResult { seq: string; bytes: string; eof: boolean }
export interface ProcessWriteRequest { handle: string; writeId: string; seq: string; bytes: string }
export interface ProcessWriteResult { written: number; delivery: string }
export interface ProcessHandleRequest { handle: string }
export interface ProcessCloseResult { joined: boolean; exitCode?: number; reason: string }
export interface ProcessOutcome { outcome: string }
export const PROCESS_HOST_METHODS: readonly ProcessHostMethod[];
export const MAX_PROCESS_ARGV: 128;
export const MAX_PROCESS_ARGV_BYTES: 65536;
export function validateAgentProcessRecipe(recipe: unknown): void;
export function validateAgentProcesses(manifest: Manifest): void;
export function processHostLane(method: ProcessHostMethod, params: unknown): 0 | 1 | 2 | 3;
export type Handler = (ctx: Context, call: Call, stream: Stream) => unknown | Promise<unknown>;
export interface Module {
  manifest: Manifest;
  handlers?: Partial<Record<Method, Handler>>;
  actions?: Record<string, Handler>;
  start?: StartFunc;
}
export interface RPCError {
  code: string;
  message: string;
  outcome?: string;
}
export interface Frame {
  kind: "call" | "cancel" | "ack" | "host.result" | "event" | "result" | "host.call";
  id: string;
  method?: string;
  params?: JSONValue;
  auth?: JSONValue;
  result?: JSONValue;
  error?: RPCError;
  seq?: number;
  event?: string;
  data?: JSONValue;
  scope?: Scope;
  deadline?: number;
  deadlineUnixMs?: number;
  parent?: string;
}
export interface WorkerInput extends AsyncIterable<Uint8Array | string> {
  destroy?(): unknown;
}
export interface WorkerOutput {
  write(chunk: string, callback: (error?: Error | null) => void): unknown;
  on?(event: "error", listener: (error: Error) => void): unknown;
  off?(event: "error", listener: (error: Error) => void): unknown;
  destroy?(): unknown;
}
export interface ServeOptions {
  input?: WorkerInput;
  output?: WorkerOutput;
  shutdownTimeoutMs?: number;
}
export const CONTRACT: Readonly<Contract>;
export const MAX_FRAME_BYTES: number;
export const MAX_CHUNK_BYTES: number;
export const MAX_HOST_BYTES: number;
export const MAX_NATIVE_BODY_BYTES: number;
export const MAX_CONCURRENT: number;
export const MAX_MODULE_SERVICES: 16;
export class SDKError extends Error implements RPCError {
  constructor(code: string, message: string, outcome?: string);
  readonly code: string;
  readonly outcome?: string;
}
export function encodeBytes(value: Uint8Array, maxBytes?: number): string;
export function decodeBytes(value: string, maxBytes?: number): Uint8Array;
export function validateManifest(value: unknown): Readonly<Manifest>;
export function publicManifest(manifest: Manifest): Readonly<Omit<Manifest, "runtime" | "entry" | "args" | "services" | "window" | "bindings" | "authBindings" | "compatibility">>;
export function serviceManifest(root: Manifest, id: string): Manifest;
export function checkCompatibility(manifest: Manifest, platform?: string): Manifest;
export function moduleContextFrom(ctx: Context): ModuleContext | undefined;
/** Fresh HTTP response proof headers; excludes state, config values and auth. */
export function moduleHTTPHeaders(context: ModuleContext): Record<string, string>;
export function requiredMethods(manifest: Manifest): Method[];
export function serve(module: Module, options?: ServeOptions): Promise<void>;

export type Headers = Record<string, string[]>;
export interface NativeRequest { id: string; responseId: string; headers?: Headers; body?: string }
export interface DecodedRequest {
  protocol: Protocol | "models" | "tool" | "account-operation"; body?: string; session?: string; timeoutMs?: number;
  requestId?: string; metadata?: JSONValue;
  executor?: "same-module";
  accountOperation?: string;
}
export interface AccountOperationDescriptor { id: string; agent: string; provider: string; action: string }
export interface AccountOperationRequest { model: string; body?: string; callerId?: string }
export interface AccountOperationResult { status: number; headers?: Headers; body?: string }
export interface ResponseEvent {
  id: string; responseId: string; action: "start" | "write" | "flush" | "finish" | "cancel";
  status?: number; headers?: Headers; body?: string; error?: RPCError;
}
export interface HeadersEvent { status: number; headers?: Headers; sentModel?: string }
export interface ExecuteRequest {
  protocol: Protocol | ""; operation?: ProviderOperation; body?: string; headers?: Headers; model: string; operationId?: string;
  credentials?: JSONValue; options?: JSONValue;
}
export type ProviderRequest = ExecuteRequest;
export interface ExecuteOptions {
  method: string;
  path: string;
  query: string;
  proxy: string;
  escapedPath?: string;
  callerId?: string;
  requestId?: string;
  native?: JSONValue;
}
export interface Usage {
  inputTokens?: number; outputTokens?: number; cachedTokens?: number; cacheWriteTokens?: number;
  cost?: number; currency?: string;
  facts?: ExecutionUsageFacts;
}
export interface ExecutionCounts {
  inputTokens?: number; outputTokens?: number; cachedTokens?: number; cacheWriteTokens?: number;
  cacheWrite1hTokens?: number; reasoningTokens?: number;
}
export interface ExecutionCost { amount: number; currency: string }
export interface ExecutionUsageFacts extends ExecutionCounts {
  cacheUnknown?: boolean; deferred?: ExecutionCounts; servedModel?: string; upstreamProvider?: string;
  requestId?: string; responseId?: string; stop?: string; tier?: string; errorType?: string;
  credits?: number; cost?: ExecutionCost;
}
export function validateExecutionUsage(manifest: Manifest, value: Usage | null | undefined): void;
export interface ExecuteResult { refusal?: RequestRefusal; status?: number; headers?: Headers; sentModel?: string; usage?: Usage; outcome?: string }
export interface AcceptedEvent { operationId: string; reference?: JSONValue }
export interface ModelsRequest { credentials?: JSONValue }
export interface ModelsResult { models: Model[] }
export interface AuthStartRequest { method: string; redirectUri?: string; input?: JSONValue }
export interface AuthStartResult { url?: string; userCode?: string; pollAfterMs?: number; continuation?: JSONValue }
export interface AuthFinishRequest { code?: string; continuation?: JSONValue; input?: JSONValue }
export interface AuthRefreshRequest { credentials: JSONValue }
export interface AuthLogoutRequest { credentials?: JSONValue }
export interface AuthResult { id?: string; name?: string; credentials?: JSONValue; expiresAt?: number; pending?: boolean }
export interface UsageRequest { credentials?: JSONValue; revision?: string }
export interface UsageWindow {
  amount?: number | null; balance?: number | null; display?: string; unit?: string; span?: number;
  name: string; used: number; limit?: number; resetsAt?: number;
  unknown?: boolean; error?: string; asOf?: number; models?: string[]; notModels?: string[]; aside?: boolean;
}
export interface UsageResult {
  usage: Usage; windows?: UsageWindow[]; asOf?: number; readDeferred?: boolean; error?: string; revision?: string;
}
export interface UsageInvalidateRequest {}
export interface UsageInvalidateResult { revision: string }
export function parseUsageRevision(value: string): bigint;
export function validateUsageResult(manifest: Manifest, value: UsageResult): boolean;
export interface OperationRequest { operationId: string; reference?: JSONValue }
export interface OperationResult { operationId: string; outcome: string; result?: JSONValue }
export interface ToolRequest { id: string; arguments: JSONValue }
export interface ToolResult { content: JSONValue; isError?: boolean }
export interface AgentBindingSnapshot { target: string; revision: string; connected: boolean; values: Record<string, string> }
export interface AgentBindingChange { revision: string; connected: boolean; values: Record<string, string> }
export function validateAgentBindingSnapshot(manifest: Manifest, descriptor: AgentDescriptor, value?: AgentBindingSnapshot | null): void;
export function validateAgentBindingChange(manifest: Manifest, descriptor: AgentDescriptor, before: AgentBindingSnapshot | null, action: string, value?: AgentBindingChange | null): void;
export interface AgentInspectRequest { id: string; options?: JSONValue; settings?: JSONValue; binding?: AgentBindingSnapshot }
export interface AgentInspectResult { connected: boolean; values: Record<string, string>; detail?: string }
export interface AgentPlanRequest { id: string; action: string; options?: JSONValue; settings?: JSONValue; binding?: AgentBindingSnapshot }
export interface FileChange { path: string; body?: string; beforeHash?: string; mode?: number; remove?: boolean }
export interface AgentPlan { files: FileChange[]; binding?: AgentBindingChange }
export interface SessionDocument { path: string; format: string; body: string }
export interface SessionInputProvenance {
  path: string; rolloutId: string; encoding: "jsonl" | "jsonl.zst";
  rawBytes: string; rawSha256: string; decodedBytes: string; decodedSha256: string;
  byteStart: string; byteEndExclusive: string; ordinalStart: string; ordinalEndExclusive: string; prefixSha256: string;
}
export interface SessionRelatedInput { body: string; source: SessionInputProvenance }
export interface SessionRelatedInputSet {
  version: 1; kind: "codex-rollout-lineage"; selected: SessionInputProvenance; ancestors: SessionRelatedInput[];
}
export interface SessionRequest { id: string; from: string; to: string; files: SessionDocument[]; options?: JSONValue; related?: SessionRelatedInputSet }
export function validateSessionRelatedContract(manifest: Manifest): void;
export function validateSessionRelatedInputs(manifest: Manifest, request: SessionRequest): void;
export interface SessionResult { files: SessionDocument[]; id?: string }
export interface StateGetRequest { key: string }
export interface StateValue { found: boolean; version?: string; value?: JSONValue }
export interface StateCASRequest { key: string; version?: string; value: JSONValue }
export interface StateCASResult { swapped: boolean; version: string }
export interface NetworkProxyRequest { url: string }
export interface NetworkProxyResult { url: string }
export interface Diagnostic {
  level: "debug" | "info" | "warn" | "error"; code: string; phase?: string;
  status?: number; millis?: number; count?: number; retryable?: boolean;
}
export interface DiagnosticResult { recorded: boolean }
export function validateDiagnostic(manifest: Manifest, value: Diagnostic): void;
export interface AccountReplaceRequest { credentials: JSONValue }
export interface AccountReplaceResult { id: string; generation: UInt64 }

export interface ActionDescriptor {
  id: string; component?: string; inputSchema: { [key: string]: JSONValue } | boolean; effect: "read" | "write";
}
export interface ServiceContract { id: string; version: number }
export interface ServiceReady { timeoutMs?: number; httpEndpoints?: string[] }
export interface ServiceDescriptor {
  id: string; runtime: "exec" | "bun"; entry: string; args?: string[];
  contract: ServiceContract; methods: string[]; start: "eager" | "lazy";
  dependsOn?: string[]; ready?: ServiceReady;
}
export interface Compatibility { hostAPI: "^1" | "^1.1" | "^1.2" | "^1.3" | "^1.4" | "^1.5" | "^1.6" | "^1.7" | "^1.8" | "^1.9" | "^1.10" | "^1.11" | "^1.12" | "^1.13" | "^1.14" | "^1.15"; nativeABI?: number; platforms?: string[] }
export interface ProviderOption {
  key: string; label: string; description?: string; default?: boolean; confirm?: string;
  labels?: Record<string, string>; descriptions?: Record<string, string>; confirms?: Record<string, string>;
  filtersModels?: boolean; placement?: "" | "top";
}
export function providerOptionOn(option: ProviderOption, values: Record<string, JSONValue>): boolean;
export function validateProviderOptionValues(manifest: Manifest, values: Record<string, JSONValue>): void;
export interface WindowDescriptor { metadata: string }
export interface BindingDescriptor { id: string; from: string; action?: string; service?: string; endpoint?: string; provider?: string }
export interface AccountSelection { id: string; expectedGeneration: string }
export interface AccountSummary { id: string; generation: string; expiresAt?: number }
export interface WindowCallOptions { account?: AccountSelection }
export interface WindowAuthMethod { id: string; mode: "poll" }
export interface WindowAuthBindingDescriptor { id: string; from: string; provider: string; methods: WindowAuthMethod[]; reauthorize?: boolean }
export interface WindowAuthStartOptions { method: string; input?: JSONValue; account?: AccountSelection }
export interface WindowAuthFlowView {
  flow: string; state: "waiting" | "pending" | "complete" | "canceled" | "expired" | "uncertain";
  url?: string; userCode?: string; retryAfterMs?: number; account?: AccountSummary;
}
export function validateAccountSelection(value: unknown): AccountSelection;
export interface ComponentIdentity { id: string; contract: ServiceContract; accountTaskActions?: string[] }
export interface ModuleCallRequest { action: string; input: JSONValue }
export type ModuleCallResult = JSONValue;
export interface ComponentCallRequest { binding: string; input: JSONValue }
export interface ConfigSnapshot { version: string; values: JSONValue }
export interface ComponentConfigureRequest {
  component: string; contract: ServiceContract; instance: string; config: ConfigSnapshot;
  services?: Record<string, ComponentReadyResult>;
}
export interface ComponentReadyResult {
  component: string; contract: ServiceContract; instance: string; configVersion: string;
  endpoints: Record<string, string>;
}
export interface ModuleContext {
  config: ConfigSnapshot; component: string; instance: string; services: Record<string, ComponentReadyResult>;
  /** Instance-local opaque reference. Never serialized or cloned by the SDK. */
  state?: unknown;
}
export interface ModuleReady { endpoints?: Record<string, string>; state?: unknown }
export interface ModuleStartResult extends ModuleReady { dispose?: () => void | Promise<void> }
export type StartFunc = (ctx: Context, module: ModuleContext) => ModuleStartResult | Promise<ModuleStartResult>;

export const MAX_GATEWAY_STATE_BYTES: number;
export type GatewayOrigin = "local" | "magpie-key" | "gateway-key" | "remote" | "browser";
export interface GatewayHookDescriptor {
  request?: boolean;
  response?: boolean;
  signIn?: boolean;
  timeoutMs?: number;
  origins?: GatewayOrigin[];
}
export interface GatewayContext {
  origin: GatewayOrigin;
  protocol: string;
  model: string;
  stream: boolean;
  path: string;
  agent: string;
  session: string;
  cwd: string;
  query: string;
  kind: string;
}
export interface GatewayRequest { body: { [key: string]: JSONValue }; context: GatewayContext }
export interface GatewayRequestResult { body?: { [key: string]: JSONValue }; state?: JSONValue }
export interface GatewayResponse {
  request: { [key: string]: JSONValue };
  reply: JSONValue;
  success: boolean;
  state?: JSONValue;
  context: GatewayContext;
}
export interface GatewaySignInEvent {
  provider: string;
  providerName: string;
  account: string;
  state: "signed-out" | "ending";
  /** RFC3339 text; never an epoch number. */
  endsAt?: string;
}
export interface GatewaySignIn { event: GatewaySignInEvent }
export interface ToolDescriptor {
  name: string;
  description?: string;
  inputSchema: { [key: string]: JSONValue } | boolean;
  /** Omitted exposure retains legacy shared behavior. Host adapters enforce owner access. */
  exposure?: "local-owner" | "shared";
  memorySearch?: boolean;
}
export interface MemorySearchRequest { query: string; limit?: number }
export interface MemorySearchHit { kind: string; text: string }
export interface MemorySearchResult { hits: MemorySearchHit[] }

/** Classifies a provider with no inference surface; admission checks API 1.11 and account-auth separately. */
export function isAccountOnlyProvider(provider: NonNullable<Manifest["providers"]>[number]): boolean;
