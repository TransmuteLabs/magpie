import assert from "node:assert/strict";
import { test } from "node:test";
import { MAX_CHUNK_BYTES, MAX_CONCURRENT, requiredMethods } from "../index.js";
import { harness } from "./helpers.js";

const turn = () => new Promise(resolve => setImmediate(resolve));
const read = (pipe, seq = "1") => ["process.read", { handle: "owned", pipe, seq, maxBytes: MAX_CHUNK_BYTES }];
const write = (seq = "1") => ["process.write", { handle: "owned", writeId: "write-" + seq, seq, bytes: "AA==" }];
const lanes = () => [read("stdout"), read("stderr"), write(), ["process.open", { recipeId: "run" }], ["state.get", { key: "legacy" }]];
const call = (id, input = null, extra = {}) => ({
  kind: "call", id, method: "module.call", params: { action: "run", input },
  scope: { plugin: "fixture", account: "selected", generation: 3, operation: "operation", attempt: 1 },
  auth: { token: "private" }, ...extra,
});
const reply = (worker, frame, result = null) => worker.send({ kind: "host.result", id: frame.id, parent: frame.parent, result });

async function configured(handler) {
  const manifest = {
    schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 10 }, capabilities: ["agent-process", "agent-wiring", "module-rpc"] },
    agents: [{ id: "fixture-agent", name: "Fixture", bin: "fixture-agent", launch: {} }],
    actions: [{ id: "run", effect: "write", inputSchema: true }],
    processes: [{ id: "run", agent: "fixture-agent", argv: ["--synthetic"] }],
  };
  const handlers = Object.fromEntries(requiredMethods(manifest).filter(method => method !== "module.call").map(method => [method, async () => ({})]));
  const worker = harness({ manifest, handlers, actions: { run: handler } });
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure", params: {
      component: "primary", instance: "fixture-process", contract: {}, config: { version: "1", values: {} },
    } });
    assert.equal((await worker.received.next()).error, undefined);
    return worker;
  } catch (error) {
    await worker.close();
    throw error;
  }
}

async function requests(worker, count) {
  const frames = [];
  for (let index = 0; index < count; index++) {
    const frame = await worker.received.next();
    assert.equal(frame.kind, "host.call");
    frames.push(frame);
  }
  return frames;
}

test("blocked stdout does not hold stderr, stdin, lifecycle or legacy callbacks", async () => {
  const worker = await configured(async (context, request, stream) => Promise.all(lanes().map(([method, params]) => stream.hostCall(method, params))));
  try {
    const parent = call("duplex", null, { deadlineUnixMs: Date.now() + 10000 });
    worker.send(parent);
    const frames = await requests(worker, 5);
    assert.deepEqual(frames.map(frame => [frame.method, frame.params.pipe]), lanes().map(([method, params]) => [method, params.pipe]));
    for (const frame of frames) {
      assert.deepEqual(frame.scope, parent.scope);
      assert.equal(frame.deadline, parent.deadlineUnixMs);
      assert.equal(frame.parent, parent.id);
      assert.equal(frame.auth, undefined);
    }
    for (let index = frames.length - 1; index >= 0; index--) reply(worker, frames[index], index);
    assert.deepEqual((await worker.received.next()).result, [0, 1, 2, 3, 4]);
  } finally { await worker.close(); }
});

test("same-lane callbacks retain order across both pipes, writes, lifecycle and legacy", async () => {
  const groups = [
    [read("stdout"), read("stdout", "2")],
    [read("stderr"), read("stderr", "2")],
    [write(), write("2")],
    [["process.open", { recipeId: "run" }], ["process.stdinEOF", { handle: "owned" }], ["process.close", { handle: "owned" }]],
    [["state.get", { key: "first" }], ["state.cas", { key: "second", version: "v1", value: 1 }]],
  ];
  const worker = await configured(async (context, request, stream) => Promise.all(groups.flat().map(([method, params]) => stream.hostCall(method, params))));
  try {
    worker.send(call("ordered"));
    const first = await requests(worker, groups.length);
    await turn();
    assert.equal(worker.received.frames.length, 0);
    for (let lane = 0; lane < groups.length; lane++) {
      let current = first[lane];
      for (let index = 0; index < groups[lane].length; index++) {
        const [method, params] = groups[lane][index];
        assert.equal(current.method, method);
        assert.deepEqual(current.params, params);
        reply(worker, current, method);
        if (index + 1 < groups[lane].length) {
          [current] = await requests(worker, 1);
          await turn();
          assert.equal(worker.received.frames.length, 0);
        }
      }
    }
    assert.deepEqual((await worker.received.next()).result, groups.flat().map(([method]) => method));
  } finally { await worker.close(); }
});

test("terminal result drains every lane when the handler forgets to await callbacks", async () => {
  const worker = await configured((context, request, stream) => {
    const pending = lanes().map(([method, params]) => stream.hostCall(method, params));
    void pending[0].then(() => stream.hostCall(...read("stdout", "2")), () => {});
    return "done";
  });
  try {
    worker.send(call("forgotten"));
    const frames = await requests(worker, 5);
    reply(worker, frames[0]);
    const [queued] = await requests(worker, 1);
    assert.deepEqual(queued.params, read("stdout", "2")[1]);
    for (let index = frames.length - 1; index > 0; index--) {
      reply(worker, frames[index]);
      await turn();
      assert.equal(worker.received.frames.length, 0);
    }
    reply(worker, queued);
    assert.equal((await worker.received.next()).result, "done");
    await turn();
    assert.equal(worker.received.frames.length, 0);
  } finally { await worker.close(); }
});

for (const mode of ["cancel", "deadline"]) {
  test(`${mode} settles active and queued callbacks in every lane and consumes late replies`, async () => {
    let joined;
    let released = 0;
    const settled = new Promise(resolve => { joined = resolve; });
    const worker = await configured(async (context, request, stream) => {
      if (request.params === "healthy") return "healthy";
      const pending = [...lanes(), ...lanes()].map(([method, params]) => stream.hostCall(method, params).finally(() => { released++; }));
      const results = await Promise.allSettled(pending);
      joined(results);
      context.signal.throwIfAborted();
    });
    try {
      worker.send(call("aborted", null, mode === "deadline" ? { deadlineUnixMs: Date.now() + 1000 } : {}));
      const frames = await requests(worker, 5);
      if (mode === "cancel") worker.send({ kind: "cancel", id: "aborted" });
      const terminal = await worker.received.next();
      assert.equal(terminal.kind, "result");
      assert.equal(terminal.error.code, mode === "cancel" ? "canceled" : "deadline");
      assert.equal(released, 10);
      for (const result of await settled) {
        assert.equal(result.status, "rejected");
        assert.equal(result.reason.code, terminal.error.code);
      }
      for (const frame of frames) reply(worker, frame);
      worker.send(call("healthy", "healthy"));
      assert.equal((await worker.received.next()).result, "healthy");
      await turn();
      assert.equal(worker.received.frames.length, 0);
    } finally { await worker.close(); }
  });
}

test("process parameters are snapshotted before waiting in their selected lane", async () => {
  const worker = await configured(async (context, request, stream) => {
    const queued = read("stdout", "2");
    const first = stream.hostCall(...read("stdout"));
    const second = stream.hostCall(...queued);
    queued[1].pipe = "stderr";
    queued[1].seq = "03";
    return Promise.all([first, second, stream.hostCall(...write())]);
  });
  try {
    worker.send(call("snapshot"));
    const [stdout, stdin] = await requests(worker, 2);
    assert.equal(stdout.params.pipe, "stdout");
    assert.equal(stdin.method, "process.write");
    reply(worker, stdin, "written");
    await turn();
    assert.equal(worker.received.frames.length, 0);
    reply(worker, stdout, "first");
    const [next] = await requests(worker, 1);
    assert.deepEqual(next.params, read("stdout", "2")[1]);
    reply(worker, next, "second");
    assert.deepEqual((await worker.received.next()).result, ["first", "second", "written"]);
  } finally { await worker.close(); }
});

test("a full worker stdout lane cannot consume stdin or legacy callback capacity", async () => {
  const worker = await configured(async (context, request, stream) => Promise.all([
    stream.hostCall(...read("stdout")), stream.hostCall(...write()), stream.hostCall("state.get", { key: "legacy" }),
  ]));
  try {
    for (let index = 0; index < MAX_CONCURRENT; index++) worker.send(call("parent-" + index));
    const frames = await requests(worker, MAX_CONCURRENT * 3);
    for (const method of ["process.read", "process.write", "state.get"]) {
      assert.equal(frames.filter(frame => frame.method === method).length, MAX_CONCURRENT);
    }
    for (let index = 0; index < MAX_CONCURRENT; index++) worker.send({ kind: "cancel", id: "parent-" + index });
    for (let index = 0; index < MAX_CONCURRENT; index++) assert.equal((await worker.received.next()).error.code, "canceled");
    for (const frame of frames) reply(worker, frame);
    await turn();
    assert.equal(worker.received.frames.length, 0);
  } finally { await worker.close(); }
});

test("each callback lane bounds its own queue and cancellation joins every queued waiter", async () => {
  let joined;
  const settled = new Promise(resolve => { joined = resolve; });
  const worker = await configured(async (context, request, stream) => {
    const pending = lanes().flatMap(([method, params]) => Array.from({ length: MAX_CONCURRENT + 1 }, () => stream.hostCall(method, params)));
    joined(await Promise.allSettled(pending));
    context.signal.throwIfAborted();
  });
  try {
    worker.send(call("bounded"));
    const frames = await requests(worker, 5);
    worker.send({ kind: "cancel", id: "bounded" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    const results = await settled;
    assert(results.every(result => result.status === "rejected"));
    assert.equal(results.filter(result => result.reason.code === "resource_exhausted").length, 5);
    assert.equal(results.filter(result => result.reason.code === "canceled").length, MAX_CONCURRENT * 5);
    for (const frame of frames) reply(worker, frame);
    await turn();
    assert.equal(worker.received.frames.length, 0);
  } finally { await worker.close(); }
});

test("malformed process callbacks are refused before any host frame", async context => {
  const cases = [
    ["open-extra", "process.open", { recipeId: "run", argv: [] }],
    ["open-missing", "process.open", {}],
    ["read-pipe", "process.read", { ...read("stdout")[1], pipe: "both" }],
    ["read-sequence", "process.read", { ...read("stdout")[1], seq: "01" }],
    ["read-max-sequence", "process.read", { ...read("stdout")[1], seq: "18446744073709551615" }],
    ["read-byte-bound", "process.read", { ...read("stdout")[1], maxBytes: MAX_CHUNK_BYTES + 1 }],
    ["read-handle", "process.read", { ...read("stdout")[1], handle: "bad handle" }],
    ["write-sequence", "process.write", { ...write()[1], seq: 1 }],
    ["write-empty", "process.write", { ...write()[1], bytes: "" }],
    ["write-base64", "process.write", { ...write()[1], bytes: "AB==" }],
    ["write-extra", "process.write", { ...write()[1], account: "another" }],
    ["close-extra", "process.close", { handle: "owned", signal: "KILL" }],
    ["eof-control", "process.stdinEOF", { handle: "owned\n" }],
  ];
  const worker = await configured((context, request, stream) => stream.hostCall(request.params.method, request.params.params));
  try {
    for (const [name, method, params] of cases) {
      await context.test(name, async () => {
        worker.send(call(name, { method, params }));
        const terminal = await worker.received.next();
        assert.equal(terminal.kind, "result");
        assert.equal(terminal.error.code, "invalid_params");
        await turn();
        assert.equal(worker.received.frames.length, 0);
      });
    }
  } finally { await worker.close(); }
});
