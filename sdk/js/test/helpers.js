import { PassThrough } from "node:stream";
import { serve } from "../index.js";
import { parseJSON, stringifyJSON } from "../manifest.js";

export function manifest(capabilities = ["operation-control"]) {
  return {
    schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 0 }, capabilities },
  };
}

export function moduleFor(handler, extra = {}) {
  return {
    manifest: manifest(),
    handlers: { "operation.status": handler, "operation.cancel": async () => null },
    ...extra,
  };
}

export function inbox(output) {
  let text = "", failure;
  const frames = [], waiting = [];
  output.setEncoding("utf8");
  output.on("data", chunk => {
    text += chunk;
    for (let index = text.indexOf("\n"); index !== -1; index = text.indexOf("\n")) {
      const frame = parseJSON(text.slice(0, index));
      text = text.slice(index + 1);
      const waiter = waiting.shift();
      if (waiter) { clearTimeout(waiter.timer); waiter.resolve(frame); } else frames.push(frame);
    }
  });
  output.on("error", error => {
    failure = error;
    for (const waiter of waiting.splice(0)) { clearTimeout(waiter.timer); waiter.reject(error); }
  });
  return {
    frames,
    next() {
      if (frames.length) return Promise.resolve(frames.shift());
      if (failure) return Promise.reject(failure);
      return new Promise((resolve, reject) => {
        const waiter = { resolve, reject };
        waiter.timer = setTimeout(() => {
          const index = waiting.indexOf(waiter);
          if (index !== -1) waiting.splice(index, 1);
          reject(new Error("Timed out waiting for RPC frame"));
        }, 3000);
        waiting.push(waiter);
      });
    },
  };
}

export function harness(module, options = {}) {
  const input = new PassThrough(), output = new PassThrough();
  const received = inbox(output);
  const done = serve(module, { input, output, shutdownTimeoutMs: 50, ...options });
  done.catch(() => {});
  return {
    input, output, received, done,
    send(frame) { input.write(stringifyJSON(frame.kind === "call" ? frame : { scope: {}, ...frame }) + "\n"); },
    async close() { input.end(); await done; output.destroy(); },
  };
}
