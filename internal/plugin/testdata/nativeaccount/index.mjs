export const NativeAccountFixture = async ({ client }) => {
  const provider = process.env.NATIVE_FIXTURE_PROVIDER || "nativefixture"
  return {
  config: async (cfg) => {
    cfg.provider ??= {}
    const models = provider === "zcode" ? {
      "GLM-5.3": { name: "GLM-5.3", limit: { context: 1000, output: 100 } },
      "GLM-5.3-Highspeed": { id: "GLM-5.3", name: "GLM-5.3 Highspeed", limit: { context: 1000, output: 100 } },
      "GLM-5.3-OffPeak": { id: "GLM-5.3", name: "GLM-5.3 OffPeak", limit: { context: 1000, output: 100 } },
      "native-wire": { id: "GLM-5.3-Highspeed", name: "Native wire alias", limit: { context: 1000, output: 100 } },
    } : { "fixture-1": { name: "Fixture", limit: { context: 1000, output: 100 } } }
    cfg.provider[provider] = { name: "Native fixture", npm: "@ai-sdk/openai-compatible", api: "https://fixture.invalid/v1",
      models }
  },
  auth: {
    provider,
    methods: [{ type: "api", label: "key" }],
    accountOperations: ["context", "selection.select", "cas", "error", "mcp.headers", "wait",
      "reset.status", "reset.opportunity", "reset.use", "reset.history.read", "highspeed.draw"],
    accountPreflight({ operation, params }) {
      if (operation === "mcp.headers" && params?.url !== "https://trusted.invalid/mcp") throw new Error("untrusted")
    },
    async select(getAuth, _provider, { selection }) {
      const auth = await getAuth()
      return { ...auth, accountScope: selection.scope, kind: selection.kind, secret: "selection-secret" }
    },
    async loader(getAuth) {
      return { baseURL: process.env.NATIVE_FIXTURE_HTTP ? process.env.FAKE_BASE : "https://fixture.invalid/v1", async fetch(url, init) {
        const mode = new Headers(init.headers).get("x-native-fixture-mode")
        if (mode && init.magpie?.execution?.lane === "highspeed") {
          if (mode === "partial") return new Response('data: {"choices":[{"delta":{"content":"native partial"}}]}\n\ndata: {"error":{"message":"cut"}}\n\n',
            { status: 200, headers: { "content-type": "text/event-stream" } })
          return new Response("", { status: 409, headers: {
            "x-magpie-zcode-fallback": "ordinary",
            "x-magpie-zcode-fallback-model": mode === "wrong-target" ? "GLM-5.3-Flash" : init.magpie.execution.rawModel,
          } })
        }
        if (process.env.NATIVE_FIXTURE_HTTP) {
          const headers = new Headers(init.headers)
          headers.set("x-fake-run", process.env.FAKE_RUN)
          headers.delete("x-native-fixture-mode")
          const auth = await getAuth()
          if (auth?.refresh) headers.set("x-native-fixture-session", auth.refresh)
          const { magpie: _private, ...request } = init
          return fetch(url, { ...request, headers })
        }
        const m = init.magpie
        const allowed = m?.model === "GLM-5.3-OffPeak" && m?.session === "task-1" &&
          m?.execution?.taskId === "task-1" && m?.execution?.scope === "scope-one"
        return new Response(allowed ? "admitted" : "missing admission", { status: allowed ? 200 : 409 })
      } }
    },
    async account(getAuth, _provider, { operation, params, expectedScope, expectedGeneration, signal }) {
      const auth = await getAuth()
      if (!auth || (expectedScope && expectedScope !== auth.accountScope) || (expectedGeneration && expectedGeneration !== auth.accountEpoch)) {
        throw Object.assign(new Error("changed"), { kind: "account_changed" })
      }
      const context = { scope: auth.accountScope, generation: auth.accountEpoch, origin: "https://fixture.invalid", family: "fixture",
        userId: "user-1", kind: auth.kind || "individual", organization: "", project: "", product: "" }
      if (operation.startsWith("reset.") || operation === "highspeed.draw") {
        const base = new URL(process.env.FAKE_BASE)
        if (!["http:", "https:"].includes(base.protocol) || base.username || base.password ||
            !["localhost", "127.0.0.1", "[::1]"].includes(base.hostname)) throw new Error("fixture requires a loopback server")
        const path = operation === "highspeed.draw" ? "highspeed/draw" :
          operation === "reset.history.read" ? "history/read" : operation.slice("reset.".length)
        const headers = new Headers()
        if (process.env.FAKE_RUN) headers.set("x-fake-run", process.env.FAKE_RUN)
        const init = { method: operation === "reset.status" ? "GET" : "POST", headers, signal }
        if (init.method === "POST") {
          headers.set("content-type", "application/json")
          init.body = JSON.stringify(params ?? {})
        }
        let data
        try {
          const response = await fetch(base.toString().replace(/\/$/, "") + "/" + path, init)
          if (!response.ok) throw Object.assign(new Error("fixture HTTP error"), { kind: "http", httpStatus: response.status })
          data = await response.json()
        } catch (error) {
          throw Object.assign(error, { kind: error.kind || "transport", dispatched: true, retryable: true })
        }
        const current = await getAuth()
        if (!current || current.accountScope !== auth.accountScope || current.accountEpoch !== auth.accountEpoch) {
          throw Object.assign(new Error("account changed"), { kind: "account_changed", dispatched: true })
        }
        return { context, data }
      }
      if (operation === "error") throw Object.assign(new Error("secret-credential-must-not-leak"), {
        kind: "throttled", code: 42901, httpStatus: 429, retryable: true, throttled: true, dispatched: true,
        requestId: "req-1", nextTakeAt: 1800000000000, nextDrawAt: 1800000001000, retryAfter: 2.5,
      })
      if (operation === "cas") {
        await client.auth.set({ path: { id: provider }, body: { ...auth, secret: "newer" }, expected: auth })
        const result = await client.auth.set({ path: { id: provider }, body: { ...auth, secret: "stale" }, expected: auth })
        return { context, data: result.data }
      }
      if (operation === "wait") {
        await new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true }))
      }
      return { context, data: operation === "mcp.headers" ? { headers: {} } : {} }
    },
    async usage() {
      return { resets: { source: "legacy-list", count: 1 }, observations: [
        { kind: "start-balance", name: "Plan", models: ["glm"], used: 0, limit: 20, remaining: 20, unit: "calls",
          at: "2026-11-01T00:00:00Z", identity: { userPlanId: "plan-1", expiresAt: "2026-12-01T00:00:00Z", secret: "do-not-copy" } },
        { kind: "server-mcp", name: "MCP", used: NaN, limit: 8, remaining: 7 },
        { kind: "secret", name: "not a supported observation" },
      ] }
    },
  },
  }
}
