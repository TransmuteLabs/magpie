import {
  CONTRACT, MAX_GATEWAY_STATE_BYTES, serve,
  type Module, type GatewayOrigin, type GatewayHookDescriptor, type GatewayContext,
  type GatewayRequest, type GatewayRequestResult, type GatewayResponse,
  type GatewaySignInEvent, type GatewaySignIn, type ToolDescriptor, type Compatibility,
} from "../index.js";

const origin: GatewayOrigin = "magpie-key";
const hooks: GatewayHookDescriptor = { request: true, response: true, signIn: true, timeoutMs: 1000, origins: ["local", origin, "browser"] };
const context: GatewayContext = {
  origin, protocol: "chat", model: "model", stream: false, path: "/v1/chat/completions",
  agent: "editor", session: "session", cwd: "/work", query: "question", kind: "",
};
const request: GatewayRequest = { body: { messages: [] }, context };
const recalled: GatewayRequestResult = { body: request.body, state: { session: context.session } };
const response: GatewayResponse = { request: request.body, reply: {}, success: true, state: recalled.state, context };
const event: GatewaySignInEvent = { provider: "provider", providerName: "Provider", account: "Account", state: "ending", endsAt: "2026-11-13T10:00:00Z" };
const notification: GatewaySignIn = { event };
const tool: ToolDescriptor = { name: "remember", inputSchema: {}, exposure: "local-owner" };
const shared: ToolDescriptor = { ...tool, exposure: "shared" };
const compatibility: Compatibility = { hostAPI: "^1.2" };
const module: Module = {
  manifest: {
    schema: 1, id: "memory", version: "1.0.0", runtime: "bun", entry: "memory.js",
    contract: { version: CONTRACT.version, capabilities: ["gateway-hooks", "tool-provider"] },
    hooks, tools: [tool, shared], compatibility,
  },
  handlers: {
    "gateway.request": async () => recalled,
    "gateway.response": async (_ctx, call, stream) => {
      await stream.emit("accepted", { operationId: call.scope.operation });
      return { outcome: "domain-captured" };
    },
    "gateway.signin": async () => null,
    "tool.call": async () => ({ content: [] }),
  },
};
// @ts-expect-error Expiry is RFC3339 text, not an epoch number.
const badEvent: GatewaySignInEvent = { ...event, endsAt: 123 };
// @ts-expect-error An undeclared exposure policy is not a public contract.
const badTool: ToolDescriptor = { ...tool, exposure: "public" };
void response; void notification; void badEvent; void badTool;
const bound: number = MAX_GATEWAY_STATE_BYTES;
void bound; void serve(module);
