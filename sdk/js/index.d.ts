export type UInt64 = number | bigint;
export type JSONValue = null | boolean | number | bigint | string | JSONValue[] | { [key: string]: JSONValue };
export type Protocol = "chat" | "responses" | "messages" | "anthropic" | "gemini";
export type Capability = "inbound-codec" | "outbound-executor" | "agent-wiring" | "account-auth"
  | "model-discovery" | "usage-reporter" | "operation-control" | "tool-provider" | "session-migration";
export type Method = "inbound.decode" | "inbound.response" | "provider.execute" | "provider.models"
  | "provider.usage" | "auth.start" | "auth.finish" | "auth.refresh" | "auth.logout" | "operation.status"
  | "operation.cancel" | "tool.call" | "agent.plan" | "agent.inspect" | "session.convert";
export type HostMethod = "state.get" | "state.cas" | "network.proxy" | "account.replace";
export interface Contract {
  version: { major: 1; minor: 0 };
  capabilities: readonly Capability[];
}
export type Effort = "none" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max";
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
  protocols?: Protocol[];
  contextWindow?: number;
  maxOutput?: number;
  tools?: boolean;
  vision?: boolean;
  reasoning?: boolean;
  price?: ModelPrice | null;
  efforts?: Effort[];
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
}
export interface SessionDescriptor {
  id: string;
  name: string;
  from?: string;
  to?: string;
  outputDir?: string;
}
export interface Manifest {
  schema: 1;
  id: string;
  version: string;
  runtime: "exec" | "bun";
  entry: string;
  args?: string[];
  contract: Contract;
  inbounds?: { id: string; path: string }[];
  providers?: { id: string; name: string; protocols: Protocol[]; models?: Model[] }[];
  agents?: AgentDescriptor[];
  sessions?: SessionDescriptor[];
  tools?: { name: string; description?: string; inputSchema: { [key: string]: JSONValue } | boolean }[];
}
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
  readonly method: Method | "sdk.describe";
  readonly params: JSONValue;
  readonly auth?: JSONValue;
  readonly scope: Scope;
  readonly deadlineUnixMs?: number;
}
export interface Context {
  readonly signal: AbortSignal;
}
export interface Stream {
  emit(event: string, data?: unknown): Promise<void>;
  hostCall<T = JSONValue>(method: HostMethod, params?: unknown): Promise<T>;
}
export type Handler = (ctx: Context, call: Call, stream: Stream) => unknown | Promise<unknown>;
export interface Module {
  manifest: Manifest;
  handlers: Partial<Record<Method, Handler>>;
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
export class SDKError extends Error implements RPCError {
  constructor(code: string, message: string, outcome?: string);
  readonly code: string;
  readonly outcome?: string;
}
export function encodeBytes(value: Uint8Array, maxBytes?: number): string;
export function decodeBytes(value: string, maxBytes?: number): Uint8Array;
export function validateManifest(value: unknown): Readonly<Manifest>;
export function publicManifest(manifest: Manifest): Readonly<Omit<Manifest, "runtime" | "entry" | "args">>;
export function requiredMethods(manifest: Manifest): Method[];
export function serve(module: Module, options?: ServeOptions): Promise<void>;

export type Headers = Record<string, string[]>;
export interface NativeRequest { id: string; responseId: string; headers?: Headers; body?: string }
export interface DecodedRequest {
  protocol: Protocol | "models" | "tool"; body?: string; session?: string; timeoutMs?: number;
  requestId?: string; metadata?: JSONValue;
}
export interface ResponseEvent {
  id: string; responseId: string; action: "start" | "write" | "flush" | "finish" | "cancel";
  status?: number; headers?: Headers; body?: string; error?: RPCError;
}
export interface HeadersEvent { status: number; headers?: Headers }
export interface ExecuteRequest {
  protocol: Protocol; body?: string; headers?: Headers; model: string; operationId?: string;
  credentials?: JSONValue; options?: JSONValue;
}
export type ProviderRequest = ExecuteRequest;
export interface Usage {
  inputTokens?: number; outputTokens?: number; cachedTokens?: number; cacheWriteTokens?: number;
  cost?: number; currency?: string;
}
export interface ExecuteResult { status?: number; headers?: Headers; usage?: Usage; outcome?: string }
export interface AcceptedEvent { operationId: string; reference?: JSONValue }
export interface ModelsRequest { credentials?: JSONValue }
export interface ModelsResult { models: Model[] }
export interface AuthStartRequest { method: string; redirectUri?: string; input?: JSONValue }
export interface AuthStartResult { url?: string; userCode?: string; pollAfterMs?: number; continuation?: JSONValue }
export interface AuthFinishRequest { code?: string; continuation?: JSONValue; input?: JSONValue }
export interface AuthRefreshRequest { credentials: JSONValue }
export interface AuthLogoutRequest { credentials?: JSONValue }
export interface AuthResult { id?: string; name?: string; credentials?: JSONValue; expiresAt?: number; pending?: boolean }
export interface UsageRequest { credentials?: JSONValue }
export interface UsageWindow {
  name: string; used: number; limit?: number; resetsAt?: number;
  unknown?: boolean; error?: string; asOf?: number;
}
export interface UsageResult {
  usage: Usage; windows?: UsageWindow[]; asOf?: number; readDeferred?: boolean; error?: string;
}
export interface OperationRequest { operationId: string; reference?: JSONValue }
export interface OperationResult { operationId: string; outcome: string; result?: JSONValue }
export interface ToolRequest { id: string; arguments: JSONValue }
export interface ToolResult { content: JSONValue; isError?: boolean }
export interface AgentInspectRequest { id: string; options?: JSONValue; settings?: JSONValue }
export interface AgentInspectResult { connected: boolean; values: Record<string, string>; detail?: string }
export interface AgentPlanRequest { id: string; action: string; options?: JSONValue; settings?: JSONValue }
export interface FileChange { path: string; body?: string; beforeHash?: string; mode?: number; remove?: boolean }
export interface AgentPlan { files: FileChange[] }
export interface SessionDocument { path: string; format: string; body: string }
export interface SessionRequest { id: string; from: string; to: string; files: SessionDocument[]; options?: JSONValue }
export interface SessionResult { files: SessionDocument[]; id?: string }
export interface StateGetRequest { key: string }
export interface StateValue { found: boolean; version?: string; value?: JSONValue }
export interface StateCASRequest { key: string; version?: string; value: JSONValue }
export interface StateCASResult { swapped: boolean; version: string }
export interface NetworkProxyRequest { url: string }
export interface NetworkProxyResult { url: string }
export interface AccountReplaceRequest { credentials: JSONValue }
export interface AccountReplaceResult { id: string; generation: UInt64 }
