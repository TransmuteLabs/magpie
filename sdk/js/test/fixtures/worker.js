import { CONTRACT, SDKError, decodeBytes, encodeBytes, serve } from "../../index.js";

const manifest = {
  schema: 1, id: "conformance", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 0 }, capabilities: [...CONTRACT.capabilities] },
  inbounds: [{ id: "native", path: "/conformance/native" }],
  providers: [{ id: "sample", name: "Sample", protocols: ["chat", "responses", "messages", "gemini"], models: [{ id: "echo" }] }],
  agents: [{ id: "editor", name: "Editor" }],
  sessions: [{ id: "text", name: "Text" }],
  tools: [{ name: "echo", inputSchema: { type: "object" } }],
};
const handlers = {
  async "inbound.decode"(ctx, call) {
    ctx.signal.throwIfAborted();
    return { protocol: "chat", body: call.params.body, session: "fixture" };
  },
  async "inbound.response"(ctx, call, stream) {
    if (call.params.action === "start") await stream.emit("headers", { status: 200 });
    else if (call.params.action === "write") await stream.emit("data", call.params.body);
    else if (call.params.action === "flush") await stream.emit("flush");
    return null;
  },
  async "provider.execute"(ctx, call, stream) {
    await stream.emit("accepted", { operationId: call.scope.operation });
    await stream.emit("headers", { status: 200, headers: { "Content-Type": ["application/json"] } });
    await stream.emit("data", encodeBytes(decodeBytes(call.params.body)));
    await stream.emit("usage", { inputTokens: 1, outputTokens: 1 });
    return { status: 200, outcome: "completed" };
  },
  async "provider.models"() { return { models: manifest.providers[0].models }; },
  async "provider.usage"() { return { usage: { inputTokens: 1 }, windows: [{ name: "Window", used: 1, limit: 10 }] }; },
  async "auth.start"() { return { url: "https://example.invalid/auth", continuation: { nonce: "fixture" } }; },
  async "auth.finish"() { return { id: "fixture-account", credentials: { token: "fixture-token" } }; },
  async "auth.refresh"(ctx, call) { return { credentials: call.auth }; },
  async "auth.logout"() { return null; },
  async "operation.status"(ctx, call, stream) {
    if (call.params?.mode === "private-error") throw new Error("SECRET diagnostic");
    if (call.params?.mode === "public-error") throw new SDKError("refused", "Fixture refusal", "uncertain");
    if (call.params?.mode === "wait") {
      await stream.emit("flush");
      await new Promise(resolve => ctx.signal.addEventListener("abort", resolve, { once: true }));
      ctx.signal.throwIfAborted();
    }
    if (call.params?.mode === "host") return stream.hostCall("state.get", { key: "fixture" });
    if (call.params?.mode === "host-wait") return stream.hostCall("state.get", { key: "pending" });
    if (call.params?.mode === "proxy") return stream.hostCall("network.proxy", { url: call.params.url });
    if (call.params?.mode === "replace") {
      await stream.emit("accepted", { operationId: call.scope.operation });
      return stream.hostCall("account.replace", { credentials: call.params.credentials });
    }
    return { operationId: call.scope.operation, outcome: "accepted", result: call.auth ?? null };
  },
  async "operation.cancel"(ctx, call) { return { operationId: call.scope.operation, outcome: "canceled" }; },
  async "tool.call"(ctx, call, stream) {
    if (call.params.arguments?.replace === true) {
      await stream.emit("accepted", { operationId: call.scope.operation });
      const replacementResult = await stream.hostCall("account.replace", { credentials: { token: "fixture-replaced" } });
      return { content: replacementResult };
    }
    return { content: call.params.arguments };
  },
  async "agent.plan"() { return { files: [{ path: "config.json", body: encodeBytes(new TextEncoder().encode("{}")) }] }; },
  async "session.convert"(ctx, call) { return { files: call.params.files.map(file => ({ ...file, format: call.params.to })) }; },
};

try {
  await serve({ manifest, handlers }, { shutdownTimeoutMs: 100 });
} catch {
  process.exitCode = 1;
}
