import assert from "node:assert/strict";
import test from "node:test";
import { validateManifest, publicManifest, validateAgentProcessRecipe, processHostLane } from "../index.js";
import { validateAgentProcesses } from "../process.js";

function fixture() {
  return {
    schema: 1, id: "process.fixture", version: "1.0.0", runtime: "native", entry: "module.so",
    contract: { version: { major: 1, minor: 10 }, capabilities: ["agent-process", "agent-wiring", "module-rpc"] },
    agents: [{ id: "worker-agent", name: "Worker agent", bin: "worker-agent", launch: {} }],
    actions: [{ id: "run", effect: "write", inputSchema: { type: "object" } }],
    processes: [{ id: "run", agent: "worker-agent", argv: ["app-server", "--stdio"] }],
  };
}

test("owned process recipes select one foreground write action and launch profile", () => {
  const manifest = fixture();
  const checked = validateManifest(manifest);
  manifest.processes[0].argv[0] = "changed";
  assert.equal(checked.processes[0].argv[0], "app-server");
  const visible = publicManifest(checked);
  assert.equal(visible.processes[0].argv[0], "app-server");
  for (const change of [
    value => { value.contract.version.minor = 9; },
    value => { delete value.processes; },
    value => { value.contract.capabilities.shift(); },
    value => { value.actions[0].effect = "read"; },
    value => { value.processes[0].id = "other"; },
    value => { value.processes[0].agent = "foreign"; },
    value => { delete value.agents[0].launch; },
    value => { value.processes.push(value.processes[0]); },
    value => { value.processes[0].argv = []; },
    value => { value.processes[0].argv = ["nul\0arg"]; },
    value => { value.processes[0].cwd = "/foreign"; },
  ]) {
    const value = fixture();
    change(value);
    assert.throws(() => validateManifest(value));
  }
  assert.doesNotThrow(() => validateAgentProcessRecipe({
    id: "run", agent: "worker-agent", argv: ["", "'; $(touch forbidden)"],
  }));
  assert.throws(() => validateAgentProcessRecipe({
    id: "run", agent: "worker-agent", argv: ["x".repeat(65536)],
  }));
});

test("process callback lanes separate output, input and lifecycle with bounded selectors", () => {
  assert.equal(processHostLane("process.open", { recipeId: "run" }), 3);
  assert.equal(processHostLane("process.read", { handle: "opaque", pipe: "stdout", seq: "1", maxBytes: 64 }), 0);
  assert.equal(processHostLane("process.read", { handle: "opaque", pipe: "stderr", seq: "1", maxBytes: 64 }), 1);
  assert.equal(processHostLane("process.write", { handle: "opaque", writeId: "write-1", seq: "1", bytes: "YQ==" }), 2);
  assert.equal(processHostLane("process.stdinEOF", { handle: "opaque" }), 3);
  assert.equal(processHostLane("process.close", { handle: "opaque" }), 3);
  for (const [method, params] of [
    ["process.open", { recipeId: "run", executable: "/bin/sh" }],
    ["process.open", { recipeId: "run", env: { KEY: "secret" } }],
    ["process.open", { recipeId: "run", account: "foreign" }],
    ["process.read", { handle: "opaque", pipe: "combined", seq: "1", maxBytes: 64 }],
    ["process.read", { handle: "opaque", pipe: "stdout", seq: "01", maxBytes: 64 }],
    ["process.read", { handle: "opaque", pipe: "stdout", seq: "18446744073709551615", maxBytes: 64 }],
    ["process.read", { handle: "opaque", pipe: "stdout", seq: "1", maxBytes: 65537 }],
    ["process.write", { handle: "opaque", writeId: "write-1", seq: "0", bytes: "YQ==" }],
    ["process.write", { handle: "opaque", writeId: "write-1", seq: "1", bytes: "" }],
    ["process.write", { handle: "opaque", writeId: "write-1", seq: "1", bytes: "%%" }],
    ["process.close", { handle: "opaque", pid: 123 }],
  ]) assert.throws(() => processHostLane(method, params), error => error.code === "invalid_params");
});

test("foreground process recipes cannot also schedule account maintenance", () => {
  for (const provider of [
    { id: "vendor", checkin: { action: "run" } },
    { id: "vendor", accountTasks: [{ id: "maintenance", action: "run" }] },
  ]) {
    const manifest = fixture();
    manifest.providers = [provider];
    assert.throws(() => validateAgentProcesses(manifest), /account maintenance action/);
    delete manifest.processes;
    manifest.contract.capabilities.shift();
    assert.doesNotThrow(() => validateAgentProcesses(manifest));
  }
});
