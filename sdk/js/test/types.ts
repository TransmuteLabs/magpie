import {
  CONTRACT, MAX_HOST_BYTES, SDKError, decodeBytes, encodeBytes, serve,
  type AccountReplaceRequest, type AccountReplaceResult, type AgentDescriptor, type AgentField, type AgentInspectRequest, type AgentInspectResult,
  type Call, type Context, type DecodedRequest, type Effort, type Frame, type HostMethod, type Manifest, type Model, type ModelPrice, type Module,
  type NetworkProxyRequest, type NetworkProxyResult,
  type Protocol, type ProviderRequest, type SessionDescriptor, type SessionResult, type StateValue, type Stream, type ToolRequest, type UsageResult,
} from "@magpie/extension-sdk";

const manifest: Manifest = {
  schema: 1, id: "typed", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: CONTRACT.version, capabilities: ["operation-control"] },
};
const nativeManifest: Manifest = {
  schema: 1, id: "typed-native", version: "1.0.0", runtime: "native", entry: "lib/worker.so",
  args: [], contract: { version: CONTRACT.version, capabilities: ["operation-control"] },
};
const execManifest: Manifest = { ...manifest, runtime: "exec", entry: "bin/worker", args: ["--stdio"] };
type NativeManifest = Extract<Manifest, { runtime: "native" }>;
type AssertNativeNoArgs<Valid extends true> = Valid;
type NativeRejectsProcessArgs = AssertNativeNoArgs<["--stdio"] extends NativeManifest["args"] ? false : true>;
void nativeManifest;
void execManifest;
const handler = async (ctx: Context, call: Call, stream: Stream) => {
  ctx.signal.throwIfAborted();
  const bytes: Uint8Array = decodeBytes(encodeBytes(new Uint8Array([1])));
  await stream.emit("data", encodeBytes(bytes));
  const state = await stream.hostCall<StateValue>("state.get", { key: "sample" });
  if (!state.found) throw new SDKError("missing", "Missing state", "uncertain");
  const proxyRequest: NetworkProxyRequest = { url: "https://example.invalid/api" };
  const method: HostMethod = "network.proxy";
  const proxy = await stream.hostCall<NetworkProxyResult>(method, proxyRequest);
  await stream.emit("accepted", { operationId: call.scope.operation ?? "fixture" });
  const replaceRequest: AccountReplaceRequest = { credentials: { token: "replacement-token" } };
  const replaced = await stream.hostCall<AccountReplaceResult>("account.replace", replaceRequest);
  return { auth: call.auth ?? null, generation: replaced.generation, proxy: proxy.url };
};
const module: Module = { manifest, handlers: { "operation.status": handler, "operation.cancel": handler } };
const frame: Frame = { kind: "call", id: "one", auth: { token: "selected" }, scope: { generation: 18446744073709551615n } };
void frame;
const decoded: DecodedRequest = { protocol: "chat", requestId: "request", metadata: { source: "fixture" } };
const tool: ToolRequest = { id: "start", arguments: { operation: "fixture" } };
const decodedTool: DecodedRequest = { protocol: "tool", body: encodeBytes(new TextEncoder().encode(JSON.stringify(tool))) };
type ExcludeProviderTool<Excluded extends false> = Excluded;
type ProviderToolExcluded = ExcludeProviderTool<"tool" extends Protocol ? true : false>;
const usage: UsageResult = {
  usage: {}, asOf: 123, readDeferred: true, error: "not refreshed",
  windows: [{ name: "Window", used: 0, unknown: true, error: "unavailable", asOf: 123 }],
};
const request: ProviderRequest = { protocol: "chat", model: "echo" };
const modelPrice: ModelPrice = { currency: "USD", input: 3, output: 5, cacheRead: 0.5, cacheWrite: 1, cacheWrite1h: 2 };
const efforts: Effort[] = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];
const models: Model[] = [{ id: "unknown" }, { id: "nullable", price: null }, { id: "free", price: {} }, { id: "priced", price: modelPrice, efforts }];
const field: AgentField = { key: "model", label: "Model" };
const agent: AgentDescriptor = { id: "editor", name: "Editor", bin: "editor", configFiles: [".config/editor/config.json"], fields: [field] };
const session: SessionDescriptor = { id: "convert", name: "Convert", from: "source", to: "target", outputDir: ".config/converted" };
const inspection: AgentInspectRequest = { id: "editor", options: {}, settings: {} };
const inspected: AgentInspectResult = { connected: true, values: { model: "echo" }, detail: "Fixture" };
const converted: SessionResult = { id: "converted-session", files: [] };
const rich: Module = {
  manifest: { ...manifest, contract: { version: CONTRACT.version, capabilities: ["agent-wiring", "session-migration"] }, agents: [agent], sessions: [session] },
  handlers: { "agent.plan": handler, "agent.inspect": async () => inspected, "session.convert": async () => converted },
};
void decoded;
void decodedTool;
void usage;
void request;
void models;
void inspection;
void rich;
const hostBytes: number = MAX_HOST_BYTES;
void hostBytes;
void serve(module);
