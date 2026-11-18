import { serve } from "../../index.js";

await serve({
  manifest: {
    schema: 1, id: "composite", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] },
    actions: [
      { id: "inspect", inputSchema: true, effect: "read" },
      { id: "write", inputSchema: {}, effect: "write" },
      { id: "stream", inputSchema: true, effect: "read" },
    ],
  },
  start(ctx, snapshot) {
    snapshot.config.values.privateEdit = "not shared";
    return { endpoints: { ui: "http://127.0.0.1:8080/" }, dispose() {} };
  },
  actions: {
    inspect(ctx, call) { return { module: ctx.module, action: call.action, input: call.params, auth: call.auth ?? null }; },
    async write(ctx, call, stream) {
      await stream.emit("accepted", { operationId: call.scope.operation });
      return stream.hostCall("component.call", { binding: "controller.run", input: call.params });
    },
    async stream(ctx, call, stream) {
      try { await stream.emit("data", ""); } catch {}
      return "must not succeed";
    },
  },
});
