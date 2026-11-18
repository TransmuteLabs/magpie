import { type Manifest, type SearchRequest, type SearchResult, validateSearchRequest, validateSearchResult } from "../index.js";

const request: SearchRequest = { source: "specstream", query: "ordinary search", count: 2, proxy: "direct" };
const result: SearchResult = { pages: [{ title: "Found", url: "https://example.invalid", text: "page" }] };
const manifest: Manifest = {
  schema: 1, id: "search.fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["module-rpc"] },
  searchSources: [{ id: "specstream", name: "SpecStream", action: "search-read" }],
};
validateSearchRequest(request);
validateSearchResult(result, request.count);
void manifest;
