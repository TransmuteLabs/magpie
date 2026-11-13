import {
  CONTRACT, serve, serviceManifest, checkCompatibility, moduleContextFrom, moduleHTTPHeaders,
  type Module, type Manifest, type ModuleContext, type ModuleReady, type StartFunc,
  type ActionDescriptor, type ServiceDescriptor, type Compatibility, type WindowDescriptor,
  type BindingDescriptor, type ComponentIdentity, type ModuleCallRequest, type ModuleCallResult,
  type ComponentCallRequest, type ComponentConfigureRequest, type ComponentReadyResult, type ConfigSnapshot,
} from "../index.js";
import { ui, type UIConfigSnapshot } from "../ui.js";

const action: ActionDescriptor = { id: "read", inputSchema: {}, effect: "read" };
const service: ServiceDescriptor = {
  id: "backend", runtime: "exec", entry: "backend", contract: { id: "backend", version: 1 },
  methods: ["run"], start: "lazy", ready: { timeoutMs: 1000, httpEndpoints: ["ui"] },
};
const compatibility: Compatibility = { hostAPI: "^1.1", nativeABI: 1, platforms: ["darwin/arm64"] };
const window: WindowDescriptor = { metadata: "ui/package.json" };
const binding: BindingDescriptor = { id: "page.read", from: "window:page", action: "read" };
const component: ComponentIdentity = { id: "backend", contract: service.contract };
const snapshot: ConfigSnapshot = { version: "18446744073709551615", values: { mode: "configured" } };
const ready: ComponentReadyResult = { component: "primary", contract: { id: "", version: 0 }, instance: "instance", configVersion: snapshot.version, endpoints: {} };
const configure: ComponentConfigureRequest = { component: "primary", contract: ready.contract, instance: ready.instance, config: snapshot };
const context: ModuleContext = { component: "primary", instance: ready.instance, config: snapshot, services: {} };
const proof: Record<string, string> = moduleHTTPHeaders(context);
const start: StartFunc = (ctx, runtime) => {
  ctx.signal.throwIfAborted();
  const result: ModuleReady = { endpoints: {} };
  return { ...result, state: new Map<string, number>(), async dispose() { runtime.config.version.toString(); } };
};
const manifest: Manifest = {
  schema: 1, id: "module", version: "1.0.0", runtime: "bun", entry: "index.js",
  contract: { version: CONTRACT.version, capabilities: ["module-rpc"] },
  actions: [action], compatibility, window, bindings: [binding],
};
const module: Module = { manifest, start, actions: {
  async read(ctx, call, stream) {
    const runtime = moduleContextFrom(ctx);
    const request: ComponentCallRequest = { binding: "controller.run", input: call.params };
    const result = await stream.hostCall<ModuleCallResult>("component.call", request);
    return { result, version: runtime?.config.version };
  },
} };
const request: ModuleCallRequest = { action: "read", input: null };
const uiConfig: Promise<UIConfigSnapshot> = ui.config.get();
void ui.backend.call("page.read", { mode: "test" });
void ui.config.cas(snapshot.version, null);
void uiConfig; void configure; void context; void request; void component; void proof;
void serviceManifest; void checkCompatibility; void serve(module);
