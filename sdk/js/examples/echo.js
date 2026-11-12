import { decodeBytes, encodeBytes, serve } from "../index.js";

export const module = {
  manifest: {
    schema: 1,
    id: "echo",
    version: "1.0.0",
    runtime: "bun",
    entry: "echo.js",
    contract: { version: { major: 1, minor: 0 }, capabilities: ["tool-provider"] },
    tools: [{ name: "echo", description: "Echo a base64 byte payload", inputSchema: { type: "object" } }],
  },
  handlers: {
    async "tool.call"(ctx, call, stream) {
      ctx.signal.throwIfAborted();
      const body = encodeBytes(decodeBytes(call.params.arguments.body, 64 * 1024));
      await stream.emit("data", body);
      return { content: [{ type: "text", text: "Echo complete" }] };
    },
  },
};

await serve(module);
