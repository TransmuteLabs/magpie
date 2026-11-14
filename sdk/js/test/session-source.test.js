import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { validateManifest, requiredMethods, publicManifest } from "../index.js";
import { stringifyJSON, parseJSON, validateModule } from "../manifest.js";
import { sessionSourceID, validateSessionSourceRequest, validateSessionSourceResult } from "../session-source.js";

const source = () => ({
  id: "history", agent: "fixture", codec: "fixture-jsonl-v1", container: "jsonl",
  stores: [{ slot: "main", homeRelativePath: ".fixture/sessions" }],
  layout: { kind: "flat", idFrom: "filename-stem" }, transcript: true, resume: "none",
});
const manifest = () => ({
  schema: 1, id: "fixture.source", version: "1.0.0", runtime: "native", entry: "plugin.so",
  contract: { version: { major: 1, minor: 6 }, capabilities: ["session-source"] },
  sessionSources: [source()],
});
const request = () => ({
  sourceId: "history", locator: { storeRef: "store", path: "sess.jsonl", sessionId: "sess" },
  snapshot: "a".repeat(64), mode: "summary",
  documents: [{ path: "sess.jsonl", format: "fixture-jsonl-v1", body: Buffer.from("{}\n").toString("base64") }],
  limits: { maxParts: 100, maxTextBytes: 400000 },
});

test("source composition preserves agent lifecycle and window-only authentication grants", () => {
  const value = manifest();
  value.contract.capabilities.push("agent-wiring", "account-auth", "model-discovery");
  value.agents = [{
    id: "fixture-agent", name: "Fixture agent", bin: "fixture-agent",
    configFiles: [".fixture/config.json"], launch: { env: { FIXTURE_GATEWAY: "{gateway}" } },
  }];
  value.providers = [{ id: "vendor", name: "Fixture vendor", protocols: ["chat"] }];
  value.window = { metadata: "package.json" };
  value.authBindings = [{
    id: "signin", from: "window:page", provider: "vendor", methods: [{ id: "browser", mode: "poll" }],
  }];
  const frozen = validateManifest(value);
  for (const method of ["session.source.read", "agent.inspect", "agent.plan", "auth.start", "auth.finish"]) {
    assert(requiredMethods(frozen).includes(method), method);
  }
  const published = publicManifest(frozen);
  assert.equal(published.sessionSources.length, 1);
  assert.equal(published.agents.length, 1);
  assert.equal(published.authBindings, undefined);
  assert.equal(published.window, undefined);
  value.sessionSources[0].stores[0].homeRelativePath = ".changed";
  value.agents[0].launch.env.FIXTURE_GATEWAY = "changed";
  value.authBindings[0].methods[0].id = "changed";
  assert.equal(frozen.sessionSources[0].stores[0].homeRelativePath, ".fixture/sessions");
  assert.equal(frozen.agents[0].launch.env.FIXTURE_GATEWAY, "{gateway}");
  assert.equal(frozen.authBindings[0].methods[0].id, "browser");
});

test("source API negotiates one read-only method and preserves legacy manifest", () => {
  assert.deepEqual(requiredMethods(validateManifest(manifest())), ["session.source.read"]);
  for (const change of [
    value => { value.contract.version.minor = 5; },
    value => { value.sessionSources[0].stores[0].homeRelativePath = "/etc"; },
    value => { value.sessionSources[0].stores[0].homeRelativePath = "../etc"; },
    value => { value.sessionSources[0].stores[0].homeRelativePath = "."; },
    value => { value.sessionSources[0].layout.kind = "recursive"; },
    value => { value.sessionSources[0].resume = "sh -c x"; },
    value => { value.sessionSources[0].stores[0].nativeRootSlot = "omp.sessions"; },
  ]) {
    const value = manifest();
    change(value);
    assert.throws(() => validateManifest(value));
  }
  const legacy = manifest();
  delete legacy.sessionSources;
  legacy.contract = { version: { major: 1, minor: 0 }, capabilities: ["session-migration"] };
  legacy.sessions = [{ id: "manual", name: "Manual" }];
  assert.doesNotThrow(() => validateManifest(legacy));
});

test("source finite layouts never accept arbitrary relative locators", () => {
  assert.equal(sessionSourceID(source().layout, "a_b.jsonl"), "a_b");
  assert.equal(sessionSourceID({ kind: "bucketed", idFrom: "filename-after-first-underscore" }, "bucket/time_a_b.jsonl"), "a_b");
  assert.equal(sessionSourceID({ kind: "directory", idFrom: "parent-directory", entry: "log.jsonl" }, "a_b/log.jsonl"), "a_b");
  for (const bad of ["../sess.jsonl", "/sess.jsonl", "extra/sess.jsonl", "sess.jsonl.zst"]) {
    assert.throws(() => sessionSourceID(source().layout, bad));
  }
});

test("source result cannot change identity, authority or counter semantics", () => {
  const input = request();
  validateSessionSourceRequest(manifest(), input, stringifyJSON);
  const result = () => ({ mode: "summary", sessionId: "sess", summary: {
    title: "hi", cwd: "/work", usage: [{ at: "2026-11-13T00:00:00Z", model: "m", input: 1,
      output: 2, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0 }],
  } });
  assert.doesNotThrow(() => validateSessionSourceResult(input, result(), stringifyJSON));
  for (const change of [
    value => { value.sessionId = "other"; },
    value => { value.resume = "sh -c x"; },
    value => { value.summary.cost = 1; },
    value => { value.summary.usage[0].input = -1; },
    value => { value.summary.usage[0].input = 18446744073709551615n; },
    value => { value.mode = "transcript"; },
  ]) {
    const value = result();
    change(value);
    assert.throws(() => validateSessionSourceResult(input, value, stringifyJSON));
  }
});

test("source worker denies borrowed session/account authority before the handler", async () => {
  let calls = 0;
  const module = validateModule({ manifest: manifest(), handlers: {
    "session.source.read": async () => {
      calls++;
      return { mode: "summary", sessionId: "sess", summary: { title: "", cwd: "" } };
    },
  } });
  const handler = module.handlers.get("session.source.read");
  for (const scope of [{ session: "other" }, { session: "sess", provider: "foreign" }, { session: "sess", account: "account", generation: 1 }]) {
    await assert.rejects(() => handler({}, { params: request(), scope }, {}));
  }
  assert.equal(calls, 0);
  await handler({}, { params: request(), scope: { session: "sess" } }, {});
  assert.equal(calls, 1);
});

test("Go/JS share required-field, zero, null, unknown, time and overflow corpus", () => {
  const cases = JSON.parse(readFileSync(new URL("../../../pkg/extension/testdata/session-source-results.json", import.meta.url), "utf8"));
  for (const sample of cases) {
    const input = { ...request(), mode: sample.mode, limits: { maxParts: 10, maxTextBytes: 1000 } };
    if (sample.valid) {
      const result = validateSessionSourceResult(input, parseJSON(sample.raw), stringifyJSON);
      if (sample.mode === "summary") assert.equal(result.summary.usage !== undefined, sample.usageKnown, sample.name);
    } else {
      assert.throws(() => validateSessionSourceResult(input, parseJSON(sample.raw), stringifyJSON), undefined, sample.name);
    }
  }
});
