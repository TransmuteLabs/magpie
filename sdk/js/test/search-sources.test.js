import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, publicManifest, serviceManifest, validateSearchRequest, validateSearchResult } from "../index.js";

function source() {
  return {
    schema: 1, id: "search.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 8 }, capabilities: ["module-rpc"] },
    compatibility: { hostAPI: "^1.8" },
    actions: [{ id: "search-read", effect: "read", inputSchema: { type: "object" } }],
    searchSources: [{ id: "specstream", name: "SpecStream fixture", action: "search-read" }],
  };
}

test("search sources require API8 and a declared primary read action separate from tool names", () => {
  assert.equal(validateManifest(source()).searchSources[0].id, "specstream");
  for (const change of [
    value => { value.contract.version.minor = 7; },
    value => { value.contract.capabilities = ["tool-provider"]; },
    value => { value.actions[0].effect = "write"; },
    value => { value.searchSources[0].action = "web_search"; },
    value => { value.searchSources[0].provider = "foreign"; },
    value => { value.searchSources[0].id = "other/source"; },
    value => { value.searchSources[0].name = "\n"; },
    value => { value.searchSources.push(value.searchSources[0]); },
    value => { value.searchSources = Array(65).fill(value.searchSources[0]); },
    value => {
      value.actions[0].component = "backend";
      value.services = [{ id: "backend", runtime: "exec", entry: "worker", contract: { id: "search.backend", version: 1 }, methods: ["search-read"], start: "lazy" }];
    },
  ]) {
    const value = source();
    change(value);
    assert.throws(() => validateManifest(value));
  }
  const root = source();
  const view = publicManifest(root);
  root.searchSources[0].name = "changed";
  assert.equal(view.searchSources[0].name, "SpecStream fixture");
  root.services = [{ id: "backend", runtime: "exec", entry: "worker", contract: { id: "search.backend", version: 1 }, start: "lazy" }];
  assert.equal(serviceManifest(root, "backend").searchSources, undefined);
  root.contract.capabilities.push("outbound-executor", "account-auth");
  root.providers = [{ id: "vendor", name: "Vendor", protocols: ["chat"] }];
  root.searchSources[0].provider = "vendor";
  assert.doesNotThrow(() => validateManifest(root));
  assert.equal(serviceManifest(root, "backend").providers, undefined);
});

test("search DTOs are bounded and contain no caller credentials", () => {
  const request = { source: "specstream", query: "ordinary search", count: 2 };
  assert.equal(validateSearchRequest(request), request);
  for (const patch of [
    { source: "foreign/source" }, { query: " \t" }, { query: "q".repeat(16385) }, { query: "query\0secret" },
    { query: "\ud800" }, { count: 0 }, { count: 21 }, { proxy: "direct\n" }, { key: "secret" },
    { account: "alice" }, { credentials: {} },
  ]) assert.throws(() => validateSearchRequest({ ...request, ...patch }));
  const page = { title: "Found", url: "https://example.invalid/page", text: "one\npage" };
  const result = { pages: [page] };
  assert.equal(validateSearchResult(result, 2), result);
  for (const patch of [
    { url: "file:///private/account" }, { url: "https://secret:password@example.invalid" },
    { text: "x".repeat(65537) }, { title: "title\nhidden" }, { credentials: "private" },
  ]) assert.throws(() => validateSearchResult({ pages: [{ ...page, ...patch }] }, 2));
  for (const value of [
    {}, { pages: null }, { pages: [page, page, page] }, { pages: [page], status: 429, error: "refused" },
    { pages: [], status: 300, error: "redirect" }, { pages: [], error: "false success" },
    { pages: [], retryAfter: "30" },
  ]) assert.throws(() => validateSearchResult(value, 2));
  assert.doesNotThrow(() => validateSearchResult({ pages: [], status: 429, error: "rate limited", retryAfter: "30" }, 2));
});
