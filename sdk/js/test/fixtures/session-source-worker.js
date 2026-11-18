import { serve, decodeBytes } from "../../index.js";
import { parseJSON } from "../../manifest.js";
import { readFile } from "node:fs/promises";
import { Buffer } from "node:buffer";

const manifest = parseJSON(await readFile(new URL("../../magpie-plugin.json", import.meta.url), "utf8"));
await serve({
  manifest,
  handlers: {
    async "session.source.read"(ctx, call) {
      ctx.signal.throwIfAborted();
      const request = call.params;
      const document = JSON.parse(Buffer.from(decodeBytes(request.documents[0].body, 16 << 20)).toString("utf8"));
      if (request.mode === "summary") return {
        mode: "summary", sessionId: request.locator.sessionId,
        summary: { title: document.text, cwd: "/synthetic", usage: [] },
      };
      return { mode: "transcript", sessionId: request.locator.sessionId,
        parts: [{ role: "user", kind: "text", text: document.text }], cut: false };
    },
  },
});
