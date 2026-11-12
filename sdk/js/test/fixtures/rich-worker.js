import { serve } from "../../index.js";

const manifest = {
  schema: 1, id: "rich-conformance", version: "1.0.0", runtime: "bun", entry: "rich-worker.js",
  contract: { version: { major: 1, minor: 0 }, capabilities: ["agent-wiring", "session-migration"] },
  agents: [{
    id: "editor", name: "Editor", bin: "editor",
    configFiles: [".config/editor/settings.json"], fields: [{ key: "model", label: "Model" }],
  }],
  sessions: [{ id: "convert", name: "Convert", from: "source", to: "target", outputDir: ".config/converted" }],
};

try {
  await serve({
    manifest,
    handlers: {
      async "agent.inspect"(ctx, call) {
        ctx.signal.throwIfAborted();
        const model = typeof call.params.options?.model === "string" ? call.params.options.model : "echo";
        return { connected: true, values: { model }, detail: "Fixture only" };
      },
      async "agent.plan"() { return { files: [] }; },
      async "session.convert"(ctx, call) {
        ctx.signal.throwIfAborted();
        return { id: "converted-session", files: call.params.files.map(file => ({ ...file, format: call.params.to })) };
      },
    },
  });
} catch {
  process.exitCode = 1;
}
