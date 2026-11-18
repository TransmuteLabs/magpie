import { serve } from "../../index.js";

await serve({
  manifest: {
    schema: 1, id: "hooks", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 2 }, capabilities: ["gateway-hooks", "tool-provider"] },
    compatibility: { hostAPI: "^1.2" },
    hooks: { request: true, response: true, signIn: true },
    tools: [{ name: "remember", inputSchema: {}, exposure: "local-owner" }],
  },
  handlers: {
    async "tool.call"(_ctx, call, stream) {
      await stream.emit("accepted", { operationId: call.scope.operation });
      await stream.hostCall("state.cas", { key: "memory", version: "0", value: call.params.arguments });
      return { content: [] };
    },
    async "gateway.request"(_ctx, call, stream) {
      if (call.params.context.query === "private-error") throw new Error("PRIVATE hook diagnostic");
      if (call.params.context.query === "stream") {
        try { await stream.emit("progress", { value: "unsupported" }); } catch {}
        return {};
      }
      const value = await stream.hostCall("state.get", { key: call.params.context.query === "wait" ? "pending" : "memory" });
      return { body: { ...call.params.body, memory: value.value }, state: { session: call.params.context.session } };
    },
    async "gateway.response"(_ctx, call, stream) {
      await stream.emit("accepted", { operationId: call.scope.operation });
      await stream.hostCall("state.cas", { key: "capture", version: "0", value: call.params.state });
      return { outcome: "domain-captured" };
    },
    async "gateway.signin"(_ctx, call, stream) {
      await stream.emit("accepted", { operationId: call.scope.operation });
      await stream.hostCall("state.cas", { key: "notification", version: "0", value: call.params.event });
      return null;
    },
  },
}, { shutdownTimeoutMs: 100 });
