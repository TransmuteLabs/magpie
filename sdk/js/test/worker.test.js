import assert from "node:assert/strict";
import { test } from "node:test";
import { PassThrough, Writable } from "node:stream";
import { SDKError, MAX_CHUNK_BYTES, MAX_CONCURRENT, MAX_FRAME_BYTES, encodeBytes, serve } from "../index.js";
import * as SDK from "../index.js";
import { harness, inbox, moduleFor } from "./helpers.js";

const call = (id, extra = {}) => ({ kind: "call", id, method: "operation.status", ...extra });
const turn = () => new Promise(resolve => setImmediate(resolve));

test("describe is built in and never publishes package runtime authority", async () => {
  const worker = harness(moduleFor(async () => null));
  try {
    worker.send({ ...call("describe"), method: "sdk.describe" });
    const result = await worker.received.next();
    assert.equal(result.kind, "result");
    assert.equal(result.result.id, "fixture");
    assert.equal(result.result.runtime, undefined);
    assert.equal(result.result.entry, undefined);
    worker.send(call("unknown", { method: "auth.refresh" }));
    assert.equal((await worker.received.next()).error.code, "method_not_found");
  } finally { await worker.close(); }
});

test("calls multiplex and carry immutable auth separately from exact uint64 scope", async () => {
  const waits = new Map();
  const worker = harness(moduleFor(async (ctx, request) => {
    assert(ctx.signal instanceof AbortSignal);
    if (request.id === "auth") {
      assert.deepEqual(request.auth, { token: "selected-account" });
      assert.equal(request.scope.generation, 18446744073709551615n);
      assert.equal(request.scope.attempt, 9007199254740993n);
      assert.throws(() => { request.scope.account = "other"; }, TypeError);
      return request.scope.generation;
    }
    await new Promise(resolve => waits.set(request.id, resolve));
    return request.id;
  }));
  try {
    worker.send(call("one"));
    worker.send(call("two"));
    await turn();
    waits.get("two")();
    assert.equal((await worker.received.next()).result, "two");
    waits.get("one")();
    assert.equal((await worker.received.next()).result, "one");
    worker.send(call("auth", { auth: { token: "selected-account" }, scope: { plugin: "fixture", account: "selected", generation: 18446744073709551615n, attempt: 9007199254740993n } }));
    assert.equal((await worker.received.next()).result, 18446744073709551615n);
  } finally { await worker.close(); }
});

test("stream emit waits for the exact acknowledgement and allows one event in flight", async () => {
  let advanced = false;
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    const first = stream.emit("data", encodeBytes(new Uint8Array([1, 2])));
    await assert.rejects(stream.emit("data", ""), error => error.code === "backpressure");
    await first;
    advanced = true;
    await stream.emit("flush");
    return "done";
  }));
  try {
    worker.send(call("stream"));
    assert.deepEqual(await worker.received.next(), { kind: "event", id: "stream", seq: 1, event: "data", data: "AQI=" });
    await turn();
    assert.equal(advanced, false);
    assert.equal(worker.received.frames.length, 0);
    worker.send({ kind: "ack", id: "stream", seq: 1 });
    assert.equal((await worker.received.next()).seq, 2);
    worker.send({ kind: "ack", id: "stream", seq: 2 });
    assert.equal((await worker.received.next()).result, "done");
  } finally { await worker.close(); }
});

test("host calls inherit parent scope without leaking auth and settle independently", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    const result = await stream.hostCall("state.get", { key: "counter", account: "untrusted" });
    return result;
  }));
  try {
    const scope = { plugin: "fixture", provider: "sample", account: "selected", generation: 5, session: "session", operation: "op", attempt: 2 };
    worker.send(call("parent", { scope, auth: { token: "private" } }));
    const frame = await worker.received.next();
    assert.equal(frame.kind, "host.call");
    assert.equal(frame.parent, "parent");
    assert.notEqual(frame.id, "parent");
    assert.deepEqual(frame.scope, scope);
    assert.equal(frame.auth, undefined);
    worker.send({ kind: "host.result", id: frame.id, result: { found: true, version: "v1", value: 3 } });
    assert.deepEqual((await worker.received.next()).result, { found: true, version: "v1", value: 3 });
  } finally { await worker.close(); }
});

test("child IDs stay bounded and distinct from active host call IDs", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => stream.hostCall("state.get")));
  try {
    for (const id of ["sdk.host.1", "x".repeat(256)]) {
      worker.send(call(id));
      const request = await worker.received.next();
      assert.notEqual(request.id, id);
      assert(request.id.length <= 256);
      assert.equal(request.parent, id);
      worker.send({ kind: "host.result", id: request.id, result: true });
      assert.equal((await worker.received.next()).result, true);
    }
  } finally { await worker.close(); }
});

test("concurrent host calls serialize within a parent and keep result order", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => Promise.all([
    stream.hostCall("state.get", { key: "one" }),
    stream.hostCall("state.get", { key: "two" }),
  ])));
  try {
    worker.send(call("serial"));
    const first = await worker.received.next();
    await turn();
    assert.equal(worker.received.frames.length, 0);
    assert.equal(first.params.key, "one");
    worker.send({ kind: "host.result", id: first.id, result: 1 });
    const second = await worker.received.next();
    assert.equal(second.params.key, "two");
    worker.send({ kind: "host.result", id: second.id, result: 2 });
    assert.deepEqual((await worker.received.next()).result, [1, 2]);
  } finally { await worker.close(); }
});

test("terminal results await a host call even when author code forgets to await", async () => {
  const worker = harness(moduleFor((ctx, request, stream) => {
    void stream.hostCall("state.get", { key: "pending" });
    return "done";
  }));
  try {
    worker.send(call("pending"));
    const request = await worker.received.next();
    assert.equal(request.kind, "host.call");
    await turn();
    assert.equal(worker.received.frames.length, 0);
    worker.send({ kind: "host.result", id: request.id, result: null });
    assert.equal((await worker.received.next()).result, "done");
  } finally { await worker.close(); }
});

test("unknown errors are sanitized and explicitly public errors retain their outcome", async () => {
  const worker = harness(moduleFor(async (ctx, request) => {
    if (request.id === "public") throw new SDKError("refused", "Request refused", "uncertain");
    throw new Error("SECRET token and private path");
  }));
  try {
    worker.send(call("internal"));
    assert.deepEqual((await worker.received.next()).error, { code: "internal", message: "Internal extension error" });
    worker.send(call("public"));
    assert.deepEqual((await worker.received.next()).error, { code: "refused", message: "Request refused", outcome: "uncertain" });
  } finally { await worker.close(); }
});

test("cancel releases acknowledgement and host-call waiters with one terminal result", async () => {
  const released = [];
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    try {
      if (request.id === "event") await stream.emit("flush");
      else await stream.hostCall("state.get", { key: "value" });
    } finally { released.push(request.id); }
  }));
  try {
    worker.send(call("event"));
    assert.equal((await worker.received.next()).kind, "event");
    worker.send({ kind: "cancel", id: "event" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    worker.send(call("host"));
    const host = await worker.received.next();
    worker.send({ kind: "cancel", id: "host" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    worker.send({ kind: "host.result", id: host.id, parent: host.parent, result: null });
    worker.send({ kind: "ack", id: "event", seq: 1 });
    await turn();
    assert.deepEqual(released.sort(), ["event", "host"]);
    assert.equal(worker.received.frames.length, 0);
  } finally { await worker.close(); }
});

test("late ACK for an aborted event is consumed once and independent calls remain healthy", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.id === "abort") await stream.emit("flush");
    return "healthy";
  }));
  worker.send(call("abort"));
  const event = await worker.received.next();
  worker.send({ kind: "cancel", id: "abort" });
  assert.equal((await worker.received.next()).error.code, "canceled");
  worker.send({ kind: "ack", id: "abort", seq: event.seq });
  worker.send(call("healthy"));
  assert.equal((await worker.received.next()).result, "healthy");
  worker.send({ kind: "ack", id: "abort", seq: event.seq });
  worker.input.end();
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});

test("unknown and wrong-sequence ACKs remain protocol errors after cancellation", async () => {
  const unknown = harness(moduleFor(async () => null));
  unknown.send({ kind: "ack", id: "never-called", seq: 1 });
  unknown.input.end();
  await assert.rejects(unknown.done, SDKError);
  unknown.output.destroy();
  const worker = harness(moduleFor(async (ctx, request, stream) => stream.emit("flush")));
  worker.send(call("canceled"));
  await worker.received.next();
  worker.send({ kind: "cancel", id: "canceled" });
  await worker.received.next();
  worker.send({ kind: "ack", id: "canceled", seq: 2 });
  worker.input.end();
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});

test("call ID reuse is refused while the old generation still has canceled credit", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.params === "emit") await stream.emit("flush");
    return null;
  }));
  worker.send(call("reuse", { params: "emit" }));
  await worker.received.next();
  worker.send({ kind: "cancel", id: "reuse" });
  await worker.received.next();
  worker.send(call("reuse"));
  worker.input.end();
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});

test("call ID reuse after consuming closed credit acknowledges only the new event", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    await stream.emit("flush");
    return true;
  }));
  try {
    worker.send(call("reuse"));
    const old = await worker.received.next();
    worker.send({ kind: "cancel", id: "reuse" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    worker.send({ kind: "ack", id: "reuse", seq: old.seq });
    worker.send(call("reuse"));
    const current = await worker.received.next();
    assert.equal(current.kind, "event");
    await turn();
    assert.equal(worker.received.frames.length, 0);
    worker.send({ kind: "ack", id: "reuse", seq: current.seq });
    assert.equal((await worker.received.next()).result, true);
  } finally { await worker.close(); }
});

test("closed ACK history is bounded to 1024 canceled credits", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.id !== "healthy") await stream.emit("flush");
    return true;
  }));
  for (let index = 0; index < 1025; index++) {
    worker.send(call("closed-" + index));
    assert.equal((await worker.received.next()).seq, 1);
    worker.send({ kind: "cancel", id: "closed-" + index });
    assert.equal((await worker.received.next()).error.code, "canceled");
  }
  worker.send({ kind: "ack", id: "closed-1024", seq: 1 });
  worker.send(call("healthy"));
  assert.equal((await worker.received.next()).result, true);
  worker.send({ kind: "ack", id: "closed-0", seq: 1 });
  worker.input.end();
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});

test("late host result for a canceled child is consumed once and the next call stays healthy", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.id === "abort-host") return stream.hostCall("state.get");
    return "healthy";
  }));
  worker.send(call("abort-host"));
  const child = await worker.received.next();
  worker.send({ kind: "cancel", id: "abort-host" });
  assert.equal((await worker.received.next()).error.code, "canceled");
  worker.send({ kind: "host.result", id: child.id, parent: "abort-host", result: { found: true } });
  worker.send(call("healthy"));
  assert.equal((await worker.received.next()).result, "healthy");
  worker.send({ kind: "host.result", id: child.id, parent: "abort-host", result: null });
  worker.input.end();
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});

test("unknown host results and wrong-parent replies to canceled children fail closed", async () => {
  const unknown = harness(moduleFor(async () => null));
  unknown.send({ kind: "host.result", id: "never-issued", result: null });
  unknown.input.end();
  await assert.rejects(unknown.done, SDKError);
  unknown.output.destroy();
  for (const parent of ["other", undefined]) {
    const worker = harness(moduleFor(async (ctx, request, stream) => stream.hostCall("state.get")));
    worker.send(call("parent"));
    const child = await worker.received.next();
    worker.send({ kind: "cancel", id: "parent" });
    await worker.received.next();
    worker.send({ kind: "host.result", id: child.id, parent, result: null });
    worker.input.end();
    await assert.rejects(worker.done, SDKError);
    worker.output.destroy();
  }
});

test("closed host-result history evicts the oldest of 1025 canceled children", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.id === "healthy") return true;
    return stream.hostCall("state.get");
  }));
  let first, last;
  for (let index = 0; index < 1025; index++) {
    const id = "parent-" + index;
    worker.send(call(id));
    last = await worker.received.next();
    first ||= last;
    worker.send({ kind: "cancel", id });
    assert.equal((await worker.received.next()).error.code, "canceled");
  }
  worker.send({ kind: "host.result", id: last.id, parent: last.parent, result: null });
  worker.send(call("healthy"));
  assert.equal((await worker.received.next()).result, true);
  worker.send({ kind: "host.result", id: first.id, parent: first.parent, result: null });
  worker.input.end();
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});

test("deadline and handler errors retain only their outstanding event credit", async () => {
  for (const mode of ["deadline", "error"]) {
    const worker = harness(moduleFor(async (ctx, request, stream) => {
      if (request.id === "healthy") return true;
      const emitted = stream.emit("flush");
      if (mode === "error") {
        await turn();
        throw new SDKError("failed", "Fixture failure");
      }
      await emitted;
    }));
    try {
      worker.send(call("aborted", mode === "deadline" ? { deadline: Date.now() + 200 } : {}));
      const event = await worker.received.next();
      assert.equal(event.kind, "event");
      assert.equal((await worker.received.next()).error.code, mode === "deadline" ? "deadline" : "failed");
      worker.send({ kind: "ack", id: "aborted", seq: event.seq });
      worker.send(call("healthy"));
      assert.equal((await worker.received.next()).result, true);
    } finally { await worker.close(); }
  }
});

test("deadline and handler errors tolerate a known in-flight host reply", async () => {
  for (const mode of ["deadline", "error"]) {
    const worker = harness(moduleFor(async (ctx, request, stream) => {
      if (request.id === "healthy") return true;
      const requested = stream.hostCall("state.get");
      if (mode === "error") {
        await turn();
        throw new SDKError("failed", "Fixture failure");
      }
      return requested;
    }));
    try {
      worker.send(call("parent", mode === "deadline" ? { deadline: Date.now() + 200 } : {}));
      const child = await worker.received.next();
      assert.equal(child.kind, "host.call");
      assert.equal((await worker.received.next()).error.code, mode === "deadline" ? "deadline" : "failed");
      worker.send({ kind: "host.result", id: child.id, parent: "parent", result: null });
      worker.send(call("healthy"));
      assert.equal((await worker.received.next()).result, true);
    } finally { await worker.close(); }
  }
});

test("a queued event canceled before writing creates no acceptable closed ACK", async () => {
  const input = new PassThrough(), frames = [];
  let release;
  const output = new Writable({
    write(bytes, encoding, callback) {
      frames.push(JSON.parse(bytes.toString()));
      if (frames.length === 1) release = callback; else callback();
    },
  });
  const done = serve(moduleFor(async (ctx, request, stream) => stream.emit("flush")), {
    input, output, shutdownTimeoutMs: 20,
  });
  done.catch(() => {});
  input.write(JSON.stringify(call("first")) + "\n" + JSON.stringify(call("queued")) + "\n");
  await turn();
  input.write('{"kind":"cancel","id":"queued"}\n');
  await turn();
  release();
  await turn();
  assert.equal(frames.filter(frame => frame.kind === "event").length, 1);
  assert.equal(frames.find(frame => frame.id === "queued").error.code, "canceled");
  input.end('{"kind":"ack","id":"queued","seq":1}\n');
  await assert.rejects(done, SDKError);
  output.destroy();
});

test("ACK cannot release a live event still queued behind another output write", async () => {
  const input = new PassThrough(), written = [];
  const output = new Writable({
    write(bytes) { written.push(JSON.parse(bytes.toString())); },
  });
  const done = serve(moduleFor(async (ctx, request, stream) => stream.emit("flush")), {
    input, output, shutdownTimeoutMs: 10,
  });
  done.catch(() => {});
  input.write(JSON.stringify(call("writing")) + "\n" + JSON.stringify(call("queued")) + "\n");
  await turn();
  input.end('{"kind":"ack","id":"queued","seq":1}\n');
  await assert.rejects(done, SDKError);
  assert.deepEqual(written.map(frame => frame.id), ["writing"]);
  output.destroy();
});

test("both deadline spellings cancel blocked work and expired calls never execute", async () => {
  let executed = 0;
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    executed++;
    await stream.emit("flush");
  }));
  try {
    for (const key of ["deadline", "deadlineUnixMs"]) {
      worker.send(call(key, { [key]: Date.now() + 100 }));
      assert.equal((await worker.received.next()).kind, "event");
      assert.equal((await worker.received.next()).error.code, "deadline");
    }
    worker.send(call("expired", { deadlineUnixMs: 1 }));
    assert.equal((await worker.received.next()).error.code, "deadline");
    assert.equal(executed, 2);
  } finally { await worker.close(); }
});

test("pending calls never exceed 32, including handlers ignoring cancellation", async () => {
  let started = 0;
  const worker = harness(moduleFor(async () => { started++; return new Promise(() => {}); }));
  try {
    for (let index = 0; index < MAX_CONCURRENT; index++) worker.send(call(String(index)));
    worker.send(call("overflow"));
    assert.equal((await worker.received.next()).error.code, "resource_exhausted");
    assert.equal(started, MAX_CONCURRENT);
    worker.send({ kind: "cancel", id: "0" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    worker.send(call("still-full"));
    assert.equal((await worker.received.next()).error.code, "resource_exhausted");
    assert.equal(started, MAX_CONCURRENT);
  } finally { await worker.close(); }
});

test("completed calls release capacity and permit sequential reuse of an ID", async () => {
  const worker = harness(moduleFor(async () => true));
  try {
    for (let index = 0; index < MAX_CONCURRENT * 2; index++) {
      worker.send(call("reused"));
      assert.equal((await worker.received.next()).result, true);
    }
  } finally { await worker.close(); }
});

test("parent host-call queue has a fixed bound and cancellation releases queued calls", async () => {
  let settled;
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    const queued = Array.from({ length: MAX_CONCURRENT }, () => stream.hostCall("state.get"));
    await assert.rejects(stream.hostCall("state.get"), error => error.code === "resource_exhausted");
    settled = Promise.allSettled(queued);
    await settled;
  }));
  try {
    worker.send(call("queued"));
    const request = await worker.received.next();
    assert.equal(request.kind, "host.call");
    worker.send({ kind: "cancel", id: "queued" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    const results = await settled;
    assert.equal(results.length, MAX_CONCURRENT);
    assert(results.every(result => result.status === "rejected" && result.reason.code === "canceled"));
    assert.equal(worker.received.frames.length, 0);
  } finally { await worker.close(); }
});

test("event byte payloads accept 64 KiB and reject larger chunks", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    await stream.emit("data", encodeBytes(new Uint8Array(request.params)));
    return null;
  }));
  try {
    worker.send(call("max", { params: MAX_CHUNK_BYTES }));
    const event = await worker.received.next();
    assert.equal(Buffer.from(event.data, "base64").length, MAX_CHUNK_BYTES);
    worker.send({ kind: "ack", id: "max", seq: 1 });
    assert.equal((await worker.received.next()).kind, "result");
    worker.send(call("large", { params: MAX_CHUNK_BYTES + 1 }));
    assert.equal((await worker.received.next()).error.code, "invalid");
  } finally { await worker.close(); }
});

test("malformed, duplicate, invalid ack and oversized frames fail closed", async () => {
  for (const send of [
    worker => worker.input.write("{broken}\n"),
    worker => worker.input.write(Buffer.from([0xff, 10])),
    worker => worker.input.write("[]\n"),
    worker => worker.input.write('{"kind":"call","id":1,"method":"operation.status"}\n'),
    worker => worker.input.write("x".repeat(MAX_FRAME_BYTES + 1)),
    worker => { worker.send(call("duplicate")); worker.send(call("duplicate")); },
    worker => { worker.send(call("bad-ack")); worker.send({ kind: "ack", id: "bad-ack", seq: 2 }); },
  ]) {
    const worker = harness(moduleFor(async () => new Promise(() => {})));
    send(worker);
    await assert.rejects(worker.done, SDKError);
    worker.output.destroy();
  }
});

test("split UTF-8 framing works and EOF cancels and awaits cooperative cleanup", async () => {
  let cleaned = false;
  const worker = harness(moduleFor(async (ctx, request) => {
    if (request.params) return request.params;
    await new Promise(resolve => ctx.signal.addEventListener("abort", resolve, { once: true }));
    await turn();
    cleaned = true;
  }));
  const bytes = Buffer.from(JSON.stringify(call("unicode", { params: "日本語" })) + "\n");
  for (const byte of bytes) worker.input.write(Buffer.from([byte]));
  assert.equal((await worker.received.next()).result, "日本語");
  worker.send(call("cleanup"));
  await turn();
  await worker.close();
  assert.equal(cleaned, true);
});

test("output failures are sanitized and shutdown is bounded for blocked writers", async () => {
  const input = new PassThrough();
  const output = new Writable({ write(chunk, encoding, callback) { callback(new Error("SECRET disk failure")); } });
  const done = serve(moduleFor(async () => null), { input, output, shutdownTimeoutMs: 10 });
  input.write(JSON.stringify(call("broken")) + "\n");
  await assert.rejects(done, error => error instanceof SDKError && !error.message.includes("SECRET"));
  const blockedInput = new PassThrough();
  const blockedOutput = new Writable({ write() {} });
  const blocked = serve(moduleFor(async () => null), { input: blockedInput, output: blockedOutput, shutdownTimeoutMs: 10 });
  blockedInput.end(JSON.stringify(call("blocked")) + "\n");
  await blocked;
  assert.equal(blockedOutput.destroyed, true);
});

test("cancellation drops queued child calls before output resumes", async () => {
  const input = new PassThrough(), frames = [];
  let release;
  const output = new Writable({
    write(bytes, encoding, callback) {
      frames.push(JSON.parse(bytes.toString()));
      if (frames.length === 1) release = callback; else callback();
    },
  });
  const done = serve(moduleFor(async (ctx, request, stream) => {
    await stream.hostCall("state.get", { key: request.id });
  }), { input, output, shutdownTimeoutMs: 20 });
  input.write(JSON.stringify(call("first")) + "\n");
  input.write(JSON.stringify(call("second")) + "\n");
  await turn();
  input.write('{"kind":"cancel","id":"second"}\n');
  await turn();
  release();
  await turn();
  assert.equal(frames.filter(frame => frame.kind === "host.call").length, 1);
  assert.equal(frames.find(frame => frame.id === "second").error.code, "canceled");
  input.end();
  await done;
  output.destroy();
});

test("host requests are bounded, matched to parent, and public host errors are preserved", async () => {
  const hostBytes = 1024 * 1024 + MAX_CHUNK_BYTES;
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.id === "large") return stream.hostCall("state.get", { key: "x".repeat(hostBytes) });
    if (request.id === "method") return stream.hostCall("host.admin", {});
    return stream.hostCall("state.cas", { key: "value", value: 1 });
  }));
  try {
    worker.send(call("large"));
    assert.equal((await worker.received.next()).error.code, "resource_exhausted");
    worker.send(call("method"));
    assert.equal((await worker.received.next()).error.code, "method_not_found");
    worker.send(call("refused"));
    const request = await worker.received.next();
    worker.send({ kind: "host.result", id: request.id, parent: "refused", error: { code: "stale", message: "Account generation changed", outcome: "uncertain" } });
    assert.deepEqual((await worker.received.next()).error, { code: "stale", message: "Account generation changed", outcome: "uncertain" });
    worker.send(call("result-bound"));
    const next = await worker.received.next();
    worker.send({ kind: "host.result", id: next.id, result: "x".repeat(hostBytes) });
    assert.equal((await worker.received.next()).error.code, "resource_exhausted");
  } finally { await worker.close(); }
});

test("the public host payload limit includes 1 MiB state plus envelope overhead", () => {
  assert.equal(SDK.MAX_HOST_BYTES, 1024 * 1024 + MAX_CHUNK_BYTES);
  assert.equal(MAX_CHUNK_BYTES, 64 * 1024);
});

test("one MiB JSON state survives a scoped host request and response", async () => {
  const value = "x".repeat(1024 * 1024 - 2);
  assert.equal(Buffer.byteLength(JSON.stringify(value)), 1024 * 1024);
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    await stream.hostCall("state.cas", { key: "large-state", version: "v1", value });
    const result = await stream.hostCall("state.get", { key: "large-state" });
    return Buffer.byteLength(JSON.stringify(result.value));
  }));
  try {
    worker.send(call("state"));
    const request = await worker.received.next();
    assert.equal(request.kind, "host.call");
    assert.equal(request.method, "state.cas");
    assert.equal(request.params.value, value);
    worker.send({ kind: "host.result", id: request.id, parent: request.parent, result: { swapped: true, version: "v2" } });
    const read = await worker.received.next();
    assert.equal(read.method, "state.get");
    assert.deepEqual(read.params, { key: "large-state" });
    worker.send({ kind: "host.result", id: read.id, parent: read.parent, result: { found: true, version: "v2", value } });
    assert.equal((await worker.received.next()).result, 1024 * 1024);
  } finally { await worker.close(); }
});

test("host request and result JSON have exact UTF-8 byte boundaries independent of stream chunks", async () => {
  const limit = 1024 * 1024 + MAX_CHUNK_BYTES;
  const sized = (bytes, template) => {
    const overhead = Buffer.byteLength(JSON.stringify({ ...template, value: "" }));
    const remaining = bytes - overhead;
    const payload = { ...template, value: "é".repeat(Math.floor(remaining / 2)) + (remaining % 2 ? "x" : "") };
    assert.equal(Buffer.byteLength(JSON.stringify(payload)), bytes);
    return payload;
  };
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    const params = request.params.side === "request" ? sized(request.params.bytes, { key: "value" }) : { key: "value" };
    const result = await stream.hostCall(request.params.side === "request" ? "state.cas" : "state.get", params);
    return result.value === undefined ? 0 : Buffer.byteLength(JSON.stringify(result));
  }));
  try {
    for (const side of ["request", "result"]) for (const bytes of [limit, limit + 1]) {
      worker.send(call(side + bytes, { params: { side, bytes } }));
      const frame = await worker.received.next();
      if (side === "request" && bytes > limit) {
        assert.equal(frame.kind, "result");
        assert.equal(frame.error.code, "resource_exhausted");
        continue;
      }
      assert.equal(frame.kind, "host.call");
      if (side === "request") assert.equal(Buffer.byteLength(JSON.stringify(frame.params)), limit);
      worker.send({ kind: "host.result", id: frame.id, parent: frame.parent,
        result: side === "result" ? sized(bytes, { found: true, version: "v1" }) : { swapped: true, version: "v1" } });
      const result = await worker.received.next();
      if (side === "result" && bytes > limit) assert.equal(result.error.code, "resource_exhausted");
      else assert.equal(result.result, side === "result" ? bytes : 0);
    }
    worker.send(call("healthy", { params: { side: "result", bytes: 1 } }));
    const request = await worker.received.next();
    worker.send({ kind: "host.result", id: request.id, result: {} });
    assert.equal((await worker.received.next()).result, 0);
  } finally { await worker.close(); }
});

test("network.proxy uses scoped fake host responses without exporting account auth", async () => {
  const scope = { plugin: "fixture", provider: "sample", account: "selected", generation: 3 };
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    return stream.hostCall(request.id === "denied" ? "network.dial" : "network.proxy", { url: request.params.url });
  }));
  try {
    for (const url of ["http://proxy.example.invalid:8080", ""]) {
      worker.send(call("proxy-" + url.length, { scope, auth: { token: "private" }, params: { url: "https://vendor.example.invalid/api" } }));
      const request = await worker.received.next();
      assert.equal(request.kind, "host.call");
      assert.equal(request.method, "network.proxy");
      assert.deepEqual(request.params, { url: "https://vendor.example.invalid/api" });
      assert.deepEqual(request.scope, scope);
      assert.equal(request.auth, undefined);
      worker.send({ kind: "host.result", id: request.id, parent: request.parent, result: { url } });
      assert.deepEqual((await worker.received.next()).result, { url });
    }
    worker.send(call("denied", { params: { url: "https://vendor.example.invalid/api" } }));
    assert.equal((await worker.received.next()).error.code, "method_not_found");
  } finally { await worker.close(); }
});

test("account.replace reaches the host after accepted ACK with unchanged parent authority", async () => {
  const scope = { plugin: "fixture", provider: "sample", account: "selected", generation: 9007199254740993n, operation: "replace", attempt: 1 };
  const credentials = { token: "replacement-token" };
  const replacement = { id: "selected", generation: 9007199254740994n };
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    await stream.emit("accepted", { operationId: request.scope.operation });
    return stream.hostCall("account.replace", { credentials: request.params.credentials });
  }));
  try {
    worker.send(call("replace", { scope, auth: { token: "previous-token" }, params: { credentials } }));
    const accepted = await worker.received.next();
    assert.equal(accepted.event, "accepted");
    assert.deepEqual(accepted.data, { operationId: "replace" });
    await turn();
    assert.equal(worker.received.frames.length, 0, "replacement must wait for durable acceptance ACK");
    worker.send({ kind: "ack", id: "replace", seq: accepted.seq });
    const request = await worker.received.next();
    assert.equal(request.kind, "host.call");
    assert.equal(request.method, "account.replace");
    assert.equal(request.parent, "replace");
    assert.deepEqual(request.scope, scope);
    assert.deepEqual(request.params, { credentials });
    assert.equal(request.auth, undefined);
    worker.send({ kind: "host.result", id: request.id, parent: request.parent, result: replacement });
    assert.deepEqual((await worker.received.next()).result, replacement);
  } finally { await worker.close(); }
});

test("account.replace propagates host refusal without retrying or changing scope", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    await stream.emit("accepted", { operationId: request.scope.operation });
    return stream.hostCall("account.replace", { credentials: { token: "replacement-token" } });
  }));
  try {
    worker.send(call("refused-replace", { scope: { plugin: "fixture", provider: "sample", account: "selected", generation: 3, operation: "replace" } }));
    const accepted = await worker.received.next();
    worker.send({ kind: "ack", id: "refused-replace", seq: accepted.seq });
    const request = await worker.received.next();
    assert.equal(request.kind, "host.call");
    assert.equal(request.method, "account.replace");
    worker.send({ kind: "host.result", id: request.id, parent: request.parent, error: { code: "host_error", message: "scoped host operation failed" } });
    assert.deepEqual((await worker.received.next()).error, { code: "host_error", message: "scoped host operation failed" });
    await turn();
    assert.equal(worker.received.frames.length, 0, "a denied replacement is never retried");
  } finally { await worker.close(); }
});

test("host result limits count original JSON escapes and nested whitespace", async () => {
  const limit = 1024 * 1024 + MAX_CHUNK_BYTES;
  const escaped = bytes => {
    const content = bytes - 2;
    return '"' + "\\u0078".repeat(Math.floor(content / 6)) + "x".repeat(content % 6) + '"';
  };
  const padded = bytes => '{"value":[' + " ".repeat(bytes - Buffer.byteLength('{"value":[0]}')) + "0]}";
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    await stream.hostCall("state.get", { key: "value" });
    return true;
  }));
  try {
    for (const payload of [escaped, padded]) for (const bytes of [limit, limit + 1]) {
      worker.send(call(payload.name + bytes));
      const request = await worker.received.next();
      assert.equal(request.kind, "host.call");
      const body = payload(bytes);
      assert.equal(Buffer.byteLength(body), bytes);
      worker.input.write('{"kind":"host.result","id":' + JSON.stringify(request.id) + ',"parent":' + JSON.stringify(request.parent) + ',"res\\u0075lt":' + body + '}\n');
      const result = await worker.received.next();
      if (bytes > limit) assert.equal(result.error?.code, "resource_exhausted");
      else assert.equal(result.result, true);
    }
  } finally { await worker.close(); }
});

test("host request serialization is snapshotted once before its bounded frame is queued", async () => {
  let serialized = 0;
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    return stream.hostCall("state.get", {
      toJSON() { return ++serialized === 1 ? { key: "value" } : { key: "x".repeat(2 * 1024 * 1024) }; },
    });
  }));
  try {
    worker.send(call("snapshot"));
    const request = await worker.received.next();
    assert.equal(serialized, 1);
    assert.deepEqual(request.params, { key: "value" });
    worker.send({ kind: "host.result", id: request.id, result: { found: false } });
    assert.deepEqual((await worker.received.next()).result, { found: false });
  } finally { await worker.close(); }
});

test("24 MiB line boundary is accepted and truncated frames are rejected", async () => {
  const worker = harness(moduleFor(async (ctx, request) => request.params.length));
  const prefix = '{"kind":"call","id":"boundary","method":"operation.status","params":"';
  const suffix = '"}';
  const size = MAX_FRAME_BYTES - Buffer.byteLength(prefix + suffix);
  worker.input.write(prefix);
  const chunk = Buffer.from("x".repeat(64 * 1024));
  for (let remaining = size; remaining > 0; remaining -= chunk.length) {
    worker.input.write(chunk.subarray(0, Math.min(chunk.length, remaining)));
  }
  worker.input.write(suffix + "\n");
  assert.equal((await worker.received.next()).result, size);
  await worker.close();
  const incomplete = harness(moduleFor(async () => null));
  incomplete.input.end('{"kind":"call"');
  await assert.rejects(incomplete.done, error => error.code === "invalid");
  incomplete.output.destroy();
});

test("non-data event payloads and accepted events respect the shared contract", async () => {
  const worker = harness(moduleFor(async (ctx, request, stream) => {
    if (request.id === "large") await stream.emit("usage", "x".repeat(MAX_CHUNK_BYTES));
    else await stream.emit("accepted", "operation");
  }));
  try {
    worker.send(call("large"));
    assert.equal((await worker.received.next()).error.code, "resource_exhausted");
    worker.send(call("accepted"));
    assert.equal((await worker.received.next()).error.code, "invalid_params");
  } finally { await worker.close(); }
});

test("bad frame fields, mismatched parents and scope escalation fail closed", async () => {
  const refused = harness(moduleFor(async () => "should not run"));
  refused.send(call("scope", { scope: { plugin: "other" } }));
  assert.equal((await refused.received.next()).error.code, "invalid_params");
  await refused.close();
  for (const frame of [
    call("extra", { unrecognized: true }),
    call("reserved", { parent: "other" }),
    call("scope", { scope: { generation: -1 } }),
    call("authority", { scope: { auth: { token: "untrusted" } } }),
    call("deadline", { deadline: 1, deadlineUnixMs: 2 }),
  ]) {
    const worker = harness(moduleFor(async () => null));
    worker.send(frame);
    await assert.rejects(worker.done, SDKError);
    worker.output.destroy();
  }
  const worker = harness(moduleFor(async (ctx, request, stream) => stream.hostCall("state.get")));
  worker.send(call("parent"));
  const request = await worker.received.next();
  worker.send({ kind: "host.result", id: request.id, parent: "other", result: null });
  await assert.rejects(worker.done, SDKError);
  worker.output.destroy();
});
