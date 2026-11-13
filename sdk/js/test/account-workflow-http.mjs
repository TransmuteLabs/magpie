import assert from "node:assert/strict";
import { readFile, writeFile, rm, access } from "node:fs/promises";
import { webcrypto } from "node:crypto";
import { pathToFileURL, fileURLToPath } from "node:url";
import vm from "node:vm";
import path from "node:path";

const config = JSON.parse(await readFile(process.argv[2], "utf8"));
const sdkRoot = path.join(config.source, "sdk/js");
const relay = await readFile(path.join(config.source, "internal/gui/assets/extensionwindows.js"), "utf8");
let crossings = 0, requests = 0;
const publicValue = value => {
  const encoded = JSON.stringify(value);
  assert(!/PRIVATE|"credentials"|"continuation"/iu.test(encoded), "secret crossed the iframe/parent boundary");
  crossings++;
  return structuredClone(value);
};
const sleep = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
async function frame(page) {
  const listeners = new Map(), messages = [], parentProxy = {};
  let dead = false;
  const parentContext = vm.createContext({ crypto: webcrypto, Uint8Array, clearTimeout,
    t: value => value,
    api: async (route, body) => {
      publicValue(body);
      requests++;
      const result = await fetch(config.origin + "/api/" + route, {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
      });
      const text = await result.text();
      publicValue(text);
      if (!result.ok) throw new Error("Host refused workflow request");
      return JSON.parse(text);
    },
  });
  const parentModule = new vm.SourceTextModule(relay, { context: parentContext });
  await parentModule.link(() => { throw new Error("unexpected parent import"); });
  await parentModule.evaluate();
  const owner = { entry: page };
  const answer = async (message, deliver = false) => {
    publicValue(message);
    let reply;
    try {
      reply = { magpie: 1, id: message.id,
        result: await parentModule.namespace.answerPluginPage(owner, message,
          () => ({ dark: false, lang: "en", tokens: {} }), () => "Workflow") };
    } catch {
      reply = { magpie: 1, id: message.id, error: "Host refused workflow request" };
    }
    publicValue(reply);
    if (deliver && !dead) for (const listener of listeners.get("message") ?? []) {
      listener({ source: parentProxy, data: reply });
    }
    return reply;
  };
  parentProxy.postMessage = message => { messages.push(structuredClone(message)); void answer(message, true); };
  const context = vm.createContext({
    parent: parentProxy, URL, TextEncoder, Uint8Array, Blob, setTimeout, clearTimeout,
    location: { href: config.origin + page.url },
    document: { readyState: "loading", documentElement: { dataset: {}, style: { setProperty() {} } },
      createElement: () => ({ dataset: {} }), head: { prepend() {} } },
    addEventListener(name, listener) { listeners.set(name, [...(listeners.get(name) ?? []), listener]); },
    fetch() { throw new Error("iframe may not perform HTTP"); },
  });
  context.window = context;
  const bridgeURL = new URL("_magpie/bridge.js", config.origin + page.url);
  const response = await fetch(bridgeURL);
  assert(response.ok, "actual served pagebridge missing");
  const bridge = await response.text();
  assert.equal(bridge, await readFile(path.join(config.source, "internal/gui/pagebridge.js"), "utf8"));
  vm.runInContext(bridge, context);
  const modules = new Map();
  const load = async filename => {
    if (modules.has(filename)) return modules.get(filename);
    assert(filename.startsWith(sdkRoot + path.sep), "browser imported a server dependency");
    const module = new vm.SourceTextModule(await readFile(filename, "utf8"), {
      context, identifier: pathToFileURL(filename).href,
    });
    modules.set(filename, module);
    await module.link((specifier, importer) => {
      assert(specifier.startsWith("./"), "browser imported Node or Bun");
      return load(fileURLToPath(new URL(specifier, importer.identifier)));
    });
    return module;
  };
  const module = await load(path.join(sdkRoot, "ui.js"));
  await module.evaluate();
  await module.namespace.ui.ready;
  return {
    ...module.namespace, messages, answer,
    async forged(method, params) {
      return answer({ magpie: 1, id: 100000 + messages.length, method, params });
    },
    dispose() {
      dead = true;
      for (const listener of listeners.get("message") ?? []) {
        listener({ source: parentProxy, data: { magpie: 1, event: "dispose" } });
      }
    },
  };
}
async function complete(workflow) {
  for (let attempt = 0; attempt < 6; attempt++) {
    const view = workflow.snapshot.flow;
    if (view.state === "complete") return view;
    assert(["waiting", "pending"].includes(view.state));
    await sleep(Math.max(view.retryAfterMs ?? 1000, 1000) + 25);
    await workflow.finish();
  }
  throw new Error("workflow never completed");
}
const current = await frame(config.page);
const legacy = await frame(config.legacy);
try {
  assert.equal((await legacy.ui.backend.call("root", { legacy: true })).root, true);
  const workflow = current.createAccountWorkflow("inspect", "signin");
  assert.throws(() => workflow.select("chosen"));
  await workflow.refresh();
  assert.equal(workflow.snapshot.accounts.length, 0);
  assert.equal(workflow.snapshot.selection, null);
  assert.throws(() => workflow.select("foreign"));
  const first = workflow.start({ method: "device" });
  assert.equal(workflow.start({ method: "device" }), first);
  assert.throws(() => workflow.start({ method: "browser" }));
  await first;
  const startMessage = current.messages.find(value => value.method === "auth.start");
  assert.equal((await current.answer(startMessage)).result.flow, workflow.snapshot.flow.flow);
  const changed = structuredClone(startMessage);
  changed.params.options.method = "browser";
  assert((await current.answer(changed)).error);
  await complete(workflow);
  const consumedReplay = await current.answer(startMessage);
  assert.equal(consumedReplay.result.state, "complete");
  assert.equal(consumedReplay.result.flow, workflow.snapshot.flow.flow);
  assert.equal(consumedReplay.result.account.id, "chosen");
  assert.equal(consumedReplay.result.account.generation, "1");
  assert.equal(workflow.snapshot.selection, null);
  assert.equal(workflow.snapshot.accounts.length, 1);
  const firstSelection = workflow.select(workflow.snapshot.accounts[0].id);
  assert.equal(firstSelection.id, "chosen");
  assert.equal(firstSelection.expectedGeneration, "1");
  assert.equal((await workflow.call()).generation, "1");
  const staleWorkflow = current.createAccountWorkflow("inspect");
  await staleWorkflow.refresh();
  staleWorkflow.select("chosen");
  for (const account of [
    { id: "foreign", expectedGeneration: "1" },
    { id: "chosen", expectedGeneration: "0" },
    { id: "chosen", expectedGeneration: "2" },
    { id: "chosen", expectedGeneration: "1", provider: "foreign" },
  ]) {
    await assert.rejects(current.ui.backend.call("inspect", null, { account }));
  }
  await workflow.start({ method: "device" }, true);
  await complete(workflow);
  assert.equal(workflow.snapshot.selection, null);
  assert.equal(workflow.snapshot.accounts[0].generation, "2");
  await assert.rejects(staleWorkflow.call());
  assert.equal(staleWorkflow.snapshot.selection, null);
  assert.equal(staleWorkflow.snapshot.stale, true);
  await staleWorkflow.refresh();
  assert.equal(staleWorkflow.snapshot.selection, null);
  assert.equal(staleWorkflow.select("chosen").expectedGeneration, "2");
  assert.equal((await staleWorkflow.call()).generation, "2");
  workflow.select("chosen");
  await rm(config.started, { force: true });
  await rm(config.release);
  await workflow.start({ method: "device" }, true);
  await sleep(Math.max(workflow.snapshot.flow.retryAfterMs ?? 1000, 1000) + 25);
  const finishing = workflow.finish();
  let observed = false;
  for (let attempt = 0; attempt < 200; attempt++) {
    try { await access(config.started); observed = true; break; } catch {}
    await sleep(10);
  }
  assert(observed, "actual auth worker never entered held exchange");
  const canceled = workflow.cancel();
  assert.equal(workflow.cancel(), canceled);
  assert.equal((await canceled).state, "canceled");
  await writeFile(config.release, "ready");
  await finishing;
  assert.equal(workflow.snapshot.flow.state, "canceled");
  await workflow.refresh();
  assert.equal(workflow.snapshot.accounts[0].generation, "2");
  assert.equal((await workflow.call()).generation, "2");
  assert.equal((await legacy.ui.backend.call("root")).root, true);
  console.log(JSON.stringify({ crossings, requests, selectedGenerations: ["1", "2"],
    duplicateStartReplayed: true, changedIntentRefused: true, cancellationPublished: false,
    legacy14: true, realExecModule: true, renderedBrowser: false }));
} finally {
  await writeFile(config.release, "ready");
  current.dispose();
  legacy.dispose();
}
