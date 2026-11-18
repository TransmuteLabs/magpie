import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { test } from "node:test";
import { encodeBytes, serve } from "../index.js";
import { moduleFor } from "./helpers.js";

test("terminal results release IDs before a peer receives bytes and preserve the next call", async () => {
  const input = new PassThrough();
  const frames = [];
  const secondStarted = Promise.withResolvers();
  const secondRelease = Promise.withResolvers();
  const secondWritten = Promise.withResolvers();
  let firstCallback;
  let invocations = 0;
  const module = moduleFor(async (_ctx, _call, stream) => {
    const invocation = ++invocations;
    if (invocation === 2) {
      secondStarted.resolve();
      await secondRelease.promise;
      await stream.emit("data", encodeBytes(new TextEncoder().encode(String(invocation))));
    }
    return invocation;
  });
  const output = {
    write(text, callback) {
      const frame = JSON.parse(text);
      frames.push(frame);
      if (frames.length === 1) {
        firstCallback = callback;
        input.write(JSON.stringify({ kind: "call", id: "reuse", method: "operation.status", scope: {} }) + "\n");
      } else {
        if (frame.kind === "event") {
          input.write(JSON.stringify({ kind: "ack", id: frame.id, seq: frame.seq, scope: {} }) + "\n");
        }
        callback();
        if (frame.kind === "result") secondWritten.resolve();
      }
    },
  };
  const done = serve(module, { input, output, shutdownTimeoutMs: 100 });
  done.catch(() => {});
  let timer;
  const timeout = new Promise((_, reject) => { timer = setTimeout(() => reject(new Error("reused call did not progress")), 10000); });
  try {
    input.write(JSON.stringify({ kind: "call", id: "reuse", method: "operation.status", scope: {} }) + "\n");
    await Promise.race([secondStarted.promise, done, timeout]);
    assert.equal(invocations, 2);
    firstCallback();
    firstCallback = undefined;
    await new Promise(resolve => setImmediate(resolve));
    secondRelease.resolve();
    await Promise.race([secondWritten.promise, done, timeout]);
    assert.deepEqual(frames.filter(frame => frame.kind === "result").map(frame => [frame.kind, frame.id, frame.result]), [
      ["result", "reuse", 1], ["result", "reuse", 2],
    ]);
    assert.deepEqual(frames.filter(frame => frame.kind === "event").map(frame => [frame.id, frame.event, frame.data]), [
      ["reuse", "data", "Mg=="],
    ]);
  } finally {
    clearTimeout(timer);
    secondRelease.resolve();
    firstCallback?.(new Error("fixture closed"));
    input.end();
    await done;
  }
});
