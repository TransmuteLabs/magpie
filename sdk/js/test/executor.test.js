import assert from "node:assert/strict";
import { test } from "node:test";
import { harness } from "./helpers.js";

test("decoded executor constraints require API 1.4 and a paired conversation executor", async () => {
  for (const scenario of ["paired", "empty", "legacy", "inbound-only", "models", "other", "null"]) {
    const capabilities = ["inbound-codec"];
    if (scenario !== "inbound-only") capabilities.push("outbound-executor");
    const manifest = {
      schema: 1, id: "executor-fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
      contract: { version: { major: 1, minor: scenario === "legacy" ? 3 : 4 }, capabilities },
      inbounds: [{ id: "fixture", path: "/executor.fixture/Call" }],
      ...(scenario !== "inbound-only" ? { providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"] }] } : {}),
    };
    const executor = scenario === "empty" ? undefined : scenario === "other" ? "another-module" : scenario === "null" ? null : "same-module";
    const worker = harness({
      manifest,
      handlers: {
        "inbound.decode": () => ({ protocol: scenario === "models" ? "models" : "chat", executor }),
        "inbound.response": () => ({}),
        ...(scenario !== "inbound-only" ? { "provider.execute": () => ({ outcome: "not-started" }) } : {}),
      },
    });
    try {
      worker.send({ kind: "call", id: "configure", method: "component.configure",
        params: { component: "primary", contract: {}, instance: "instance", config: { version: "0", values: {} } } });
      assert.equal((await worker.received.next()).error, undefined);
      worker.send({ kind: "call", id: "decode", method: "inbound.decode",
        params: { id: "fixture", responseId: "response" }, scope: { plugin: manifest.id } });
      const result = await worker.received.next();
      if (["paired", "empty"].includes(scenario)) {
        assert.equal(result.error, undefined, scenario);
        assert.equal(result.result.executor, executor);
      } else {
        assert.equal(result.error?.code, "internal", scenario);
      }
    } finally {
      await worker.close();
    }
  }
});
