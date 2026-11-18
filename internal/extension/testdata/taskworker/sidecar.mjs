#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { serve, serviceManifest, reportAccountTaskUsage, validateAccountTaskRequest } from "./sdk/index.js";

const root = JSON.parse(await readFile("magpie-plugin.json", "utf8"));
const manifest = serviceManifest(root, "paid");
await serve({ manifest, actions: {
  "paid.ping": async (context, call, stream) => {
    const task = validateAccountTaskRequest(call.params);
    await stream.emit("accepted", { operationId: call.scope.operation });
    const response = await fetch(call.auth.url + "/paid", {
      method: "POST", body: JSON.stringify({ scope: call.scope, task, token: call.auth.token }),
      signal: context.signal,
    });
    if (!response.ok) throw new Error("Synthetic paid endpoint rejected selected authority");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffered = "", snapshots = 0;
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) throw new Error("Synthetic endpoint ended before cancellation");
        buffered += decoder.decode(value, { stream: true });
        for (let end = buffered.indexOf("\n"); end !== -1; end = buffered.indexOf("\n")) {
          const measured = JSON.parse(buffered.slice(0, end));
          buffered = buffered.slice(end + 1);
          await reportAccountTaskUsage(stream, measured);
          if (++snapshots === 2) {
            const acknowledged = await fetch(call.auth.url + "/snapshots-ready", {
              method: "POST", body: JSON.stringify(call.scope), signal: context.signal,
            });
            if (!acknowledged.ok) throw new Error("Synthetic snapshot barrier rejected authority");
            await acknowledged.arrayBuffer();
          }
        }
      }
    } finally {
      await reader.cancel().catch(() => {});
      reader.releaseLock();
    }
  },
} });
