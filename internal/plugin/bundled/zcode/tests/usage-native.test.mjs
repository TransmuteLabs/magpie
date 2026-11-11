import { afterAll, beforeAll, beforeEach, expect, test } from "bun:test"
import { createServer } from "node:http"
import { accountScope, createAccountRuntime } from "../account.mjs"
import { ZCodeAuthPlugin } from "../index.mjs"
import { nativeUsageOperation, readNativeUsage, startBalanceNow, startBucketModels } from "../usage.mjs"
import { resetOperation } from "../reset.mjs"
import { accountFixture } from "./account-fixtures.mjs"

let server, origin, route, calls
const send = (res, data, status = 200, headers = {}) => {
  res.writeHead(status, { "content-type": "application/json", ...headers })
  res.end(JSON.stringify(data))
}
const ok = (data) => ({ code: 0, data })
const stall = (res) => { res.writeHead(200, { "content-type": "application/json" }); res.write('{"code":0,"data":') }

beforeAll(async () => {
  server = createServer((req, res) => {
    let body = ""
    req.setEncoding("utf8")
    req.on("data", (part) => { body += part })
    req.on("end", () => {
      const call = { url: new URL(req.url, origin), method: req.method, headers: req.headers, body }
      calls.push(call)
      if (call.url.pathname.endsWith("/access_tokens")) {
        send(res, ok({ enable: true, accessToken: "project-token", tokenType: "Bearer", expiresIn: 3600, expiresAt: Math.floor(Date.now() / 1000) + 3600 }))
      } else route(call, res)
    })
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  origin = `http://127.0.0.1:${server.address().port}`
})
beforeEach(() => { calls = []; route = (_call, res) => send(res, {}, 404) })
afterAll(async () => { server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)) })

async function context(kind = "individual", site = "zai", overrides = {}) {
  const state = { site, kind, uid: "user-1", device: "device-1", base: `${origin}/api/anthropic`, business: "business-token", jwt: "zcode-token",
    ...(kind === "team" ? { org: "org-1", project: "project-1", product: "product-1" } : {}),
    loc: { organizationId: kind === "team" ? "org-1" : "personal-org", projectId: kind === "team" ? "project-1" : "personal-project", apiKeyId: "key-1" }, ...overrides }
  const origins = { business: origin, zcode: origin, zaiOAuth: "https://chat.z.ai" }
  let auth = { type: "oauth", refresh: JSON.stringify(state), accountEpoch: "epoch-1", accountScope: accountScope({ ...state, origins }) }
  const runtime = createAccountRuntime({ providerId: "zcode", stateOf: (value) => JSON.parse(value.refresh), origins: { [site]: origins }, claimVerification: async () => null,
    client: { auth: { set: async ({ body, expected }) => {
      if (JSON.stringify(expected) !== JSON.stringify(auth)) return { data: { saved: false, conflict: true } }
      auth = body
      return { data: { saved: true, conflict: false } }
    } } } })
  return runtime.capture(async () => auth)
}

test("Team card keeps weekly quota, separate Start identities, and scoped Server MCP", async () => {
  const now = Math.floor(Date.now() / 1000)
  route = (call, res) => {
    switch (call.url.pathname) {
      case "/api/monitor/usage/quota/limit": return send(res, ok({ level: "pro", limits: [
        { type: "CREDIT_LIMIT", unit: 3, number: 5, usage: 100, remaining: 60 },
        { type: "CREDIT_LIMIT", unit: 6, number: 7 },
        { type: "TIME_LIMIT", unit: 5, number: 1, usage: 200, remaining: 50 },
      ] }))
      case "/api/biz/team/subscribe/product/querySubscribeDetail": return send(res, ok({ hasSubscription: true, status: "EFFECTIVE", memberGrantStatus: "VALID", productName: "Team Pro" }))
      case "/api/v1/zcode-plan/billing/balance": return send(res, ok({ server_time: now - 86400,
        plans: [{ user_plan_id: "old", plan_id: "same", status: "active", ends_at: now - 10 }, { user_plan_id: "new", plan_id: "same", name: "Grant", status: "active", ends_at: now + 3600 }],
        balances: [{ user_plan_id: "old", plan_id: "same", bucket_id: "expired", show_name: "old", total_units: 5, remaining_units: 4 },
          { user_plan_id: "new", plan_id: "same", bucket_id: "bucket-a", entitlement_id: "ent-a", show_name: "GLM-5.3-Flash", capabilities: ["tool:web"], total_units: "100", remaining_units: "75", period_start: now - 60, period_end: now + 3540, expires_at: now + 1800 },
          { user_plan_id: "new", plan_id: "same", bucket_id: "bucket-b", entitlement_id: "ent-b", show_name: "Other", capabilities: ["MODEL:glm-5.3", "model:GLM-5.3"], total_units: 100 }] }), 200, { date: new Date(now * 1000).toUTCString() })
      case "/api/v1/mcp/usage": return send(res, ok({ server_time: now, next_refresh_at: now + 600, total_usage: { used: 4, limit: 20, remaining: 16 } }))
      default: return send(res, {}, 404)
    }
  }
  const card = await readNativeUsage(await context("team", "bigmodel"))
  expect(card.error).toBeUndefined()
  const weekly = card.windows.find((window) => window.name === "Weekly")
  expect(weekly).toMatchObject({ name: "Weekly", span: 7 * 86400 })
  expect(weekly.used).toBeUndefined()
  expect(weekly.notModels).toEqual(expect.arrayContaining(["glm-5.3-flash-trial", "glm-5.3-trial"]))
  expect(card.windows.find((window) => window.name === "MCP · Month")).toMatchObject({ aside: true, used: 75 })
  const starts = card.observations.filter((observation) => observation.kind === "start-balance")
  expect(starts).toHaveLength(2)
  expect(starts[0]).toMatchObject({ models: ["GLM-5.3-Flash"], used: 25, limit: 100, remaining: 75,
    identity: { userPlanId: "new", planId: "same", bucketId: "bucket-a", entitlementId: "ent-a",
      periodStart: new Date((now - 60) * 1000).toISOString(), periodEnd: new Date((now + 3540) * 1000).toISOString(), expiresAt: new Date((now + 1800) * 1000).toISOString() } })
  expect(starts[1]).toMatchObject({ models: ["GLM-5.3"], used: null, remaining: null, identity: { bucketId: "bucket-b", entitlementId: "ent-b" } })
  expect(card.observations.find((observation) => observation.kind === "server-mcp")).toMatchObject({ used: 4, limit: 20, remaining: 16,
    at: new Date(now * 1000).toISOString(), nextRefreshAt: new Date((now + 600) * 1000).toISOString() })
  const quota = calls.find((call) => call.url.pathname.endsWith("/quota/limit"))
  expect(quota.url.searchParams.get("type")).toBe("2")
  expect(quota.headers.authorization).toBe("project-token")
  expect([quota.headers["bigmodel-organization"], quota.headers["bigmodel-project"]]).toEqual(["org-1", "project-1"])
  const mcp = calls.find((call) => call.url.pathname === "/api/v1/mcp/usage")
  expect([mcp.headers.authorization, mcp.headers["x-bigmodel-authorization"], mcp.headers["bigmodel-target-type"]]).toEqual(["Bearer zcode-token", "Bearer business-token", "TEAM"])
  expect(calls.some((call) => /credit-usage|model-usage|tool-usage|reset/.test(call.url.pathname))).toBe(false)
})

test("ordinary quota windows never claim native lane aliases or gift trials", async () => {
  const now = Math.floor(Date.now() / 1000)
  route = (call, res) => {
    if (call.url.pathname.endsWith("/quota/limit")) return send(res, ok({ limits: [
      { type: "TOKENS_LIMIT", unit: 3, number: 5, percentage: 100 },
      { type: "CREDIT_LIMIT", unit: 6, number: 7, percentage: 100 },
      { type: "TIME_LIMIT", unit: 5, number: 1, percentage: 100 },
    ] }))
    if (call.url.pathname.endsWith("/billing/balance")) return send(res, ok({ server_time: now,
      plans: [{ plan_id: "start", user_plan_id: "user-start", name: "Start", status: "active", ends_at: now + 3600 }],
      balances: [{ plan_id: "start", user_plan_id: "user-start", show_name: "GLM-5.3", capabilities: ["model:GLM-5.3"], total_units: 100, remaining_units: 30 }],
    }))
    return send(res, {}, 404)
  }
  const aliases = ["glm-5.3-offpeak", "glm-5.3-highspeed", "glm-5.3-flash-offpeak", "glm-5.3-flash-highspeed"]
  const card = await readNativeUsage(await context("team", "bigmodel"))
  for (const name of ["5 hours", "Weekly"]) {
    const window = card.windows.find((entry) => entry.name === name)
    expect(window.used).toBe(100)
    expect(window.notModels).toEqual(expect.arrayContaining([...aliases, "glm-5.3-trial"]))
    expect(window.notModels).not.toContain("glm-5.3")
    expect(window.notModels).not.toContain("glm-5.3-flash")
  }
  const gift = card.windows.find((entry) => entry.models?.includes("GLM-5.3-Trial"))
  expect(gift.models).toEqual(["GLM-5.3-Trial"])
  for (const alias of aliases) expect(gift.models.map((model) => model.toLowerCase())).not.toContain(alias)
  expect(card.windows.find((entry) => entry.name === "MCP · Month").aside).toBe(true)

  const startOnly = await readNativeUsage(await context("start", "zai"))
  const startWindow = startOnly.windows.find((entry) => entry.models?.includes("GLM-5.3"))
  expect(startWindow.models).toEqual(["GLM-5.3", "GLM-5.3-Trial"])
  for (const alias of aliases) expect(startWindow.models.map((model) => model.toLowerCase())).not.toContain(alias)
})

test("Date and model helpers retain source precedence and identity", () => {
  expect(startBucketModels({ capabilities: [" MoDeL:glm-5.3-flash ", "MODEL:GLM-5.3-FLASH", "tool:web"], show_name: "Other" })).toEqual(["GLM-5.3-Flash"])
  expect(startBucketModels({ capabilities: ["tool:web"], show_name: "GLM-5.3" })).toEqual(["GLM-5.3"])
  expect(startBalanceNow({ server_time: 10 }, "Wed, 21 Oct 2015 07:28:00 GMT", 123)).toBe(Date.parse("2015-10-21T07:28:00Z"))
  expect(startBalanceNow({ server_time: 10 }, "bad date", 123)).toBe(10_000)
  expect(startBalanceNow({}, "bad date", 123)).toBe(123)
})

test("explicit Start balance preserves zero and identity without other account requests", async () => {
  const now = Math.floor(Date.now() / 1000)
  route = (call, res) => call.url.pathname === "/api/v1/zcode-plan/billing/balance"
    ? send(res, ok({ server_time: now, plans: [{ user_plan_id: "user-plan", plan_id: "plan", status: "active", ends_at: now + 3600 }],
      balances: [{ user_plan_id: "user-plan", plan_id: "plan", bucket_id: "bucket", entitlement_id: "grant",
        show_name: "GLM-5.3", capabilities: ["model:GLM-5.3"], total_units: 50, remaining_units: 0,
        period_start: now - 10, period_end: now + 100, expires_at: now + 100 }] }))
    : send(res, {}, 404)
  const result = await nativeUsageOperation(await context("start"), "usage.balance")
  expect(result).toMatchObject({ kind: "available", observations: [{ kind: "start-balance", remaining: 0, limit: 50,
    models: ["GLM-5.3"], identity: { userPlanId: "user-plan", planId: "plan", bucketId: "bucket", entitlementId: "grant",
      periodStart: new Date((now - 10) * 1000).toISOString(), periodEnd: new Date((now + 100) * 1000).toISOString(),
      expiresAt: new Date((now + 100) * 1000).toISOString() } }] })
  expect(calls.map((call) => call.url.pathname)).toEqual(["/api/v1/zcode-plan/billing/balance"])
  calls = []
  expect(await nativeUsageOperation(await context("start", "zai", { jwt: "" }), "usage.balance")).toEqual({ kind: "unknown" })
  expect(calls).toEqual([])
  route = (_call, res) => send(res, { code: 500 }, 500)
  expect(await nativeUsageOperation(await context("start"), "usage.balance")).toEqual({ kind: "unknown" })
  const stale = await context("start")
  stale.assertCurrent = async () => { throw Object.assign(new Error("account changed"), { kind: "account_generation_changed" }) }
  await expect(nativeUsageOperation(stale, "usage.balance")).rejects.toMatchObject({ kind: "account_generation_changed" })
})

test("optional stalled response bodies cannot erase completed quota", async () => {
  route = (call, res) => call.url.pathname.endsWith("/quota/limit")
    ? send(res, ok({ limits: [{ type: "TOKENS_LIMIT", unit: 6, number: 1, percentage: 27 }] }))
    : stall(res)
  const ctx = await context("team")
  const start = Date.now()
  const card = await readNativeUsage(ctx)
  expect(card.error).toBeUndefined()
  expect(card.windows).toHaveLength(1)
  expect(card.windows[0]).toMatchObject({ name: "Weekly", used: 27, span: 7 * 86400 })
  expect(card.observations).toEqual([])
  expect(Date.now() - start).toBeLessThan(6000)
}, 10000)

test("a failed quota envelope is not mistaken for a zero-percent allowance", async () => {
  route = (call, res) => call.url.pathname.endsWith("/quota/limit")
    ? send(res, { code: 0, success: false, data: { limits: [{ type: "TOKENS_LIMIT", unit: 3, number: 5, percentage: 0 }] } })
    : send(res, {}, 404)
  const card = await readNativeUsage(await context())
  expect(card.error).toBeDefined()
  expect(card.windows).toEqual([])
})

test("Team entitlement remains visible if project-token resolution fails", async () => {
  route = (call, res) => {
    if (call.url.pathname.endsWith("/querySubscribeDetail")) return send(res, ok({ hasSubscription: true, status: "EFFECTIVE", memberGrantStatus: "VALID", productName: "Team Plan" }))
    return send(res, {}, 404)
  }
  // Force the project-key discovery path, which this fixture deliberately refuses.
  const ctx = await context("team", "bigmodel", { loc: undefined })
  const card = await readNativeUsage(ctx)
  expect(card.plan).toBe("Team Plan")
  expect(card.error).toBeDefined()
  expect(card.windows).toEqual([])
})

test("missing ZCode JWT cannot disable project-token quota or dispatch reset", async () => {
  route = (call, res) => call.url.pathname.endsWith("/quota/limit")
    ? send(res, ok({ limits: [{ type: "TOKENS_LIMIT", unit: 6, number: 7, percentage: 16 }] }))
    : send(res, {}, 404)
  const ctx = await context("individual", "zai", { jwt: "" })
  await expect(resetOperation(ctx, "reset.status")).rejects.toMatchObject({ kind: "unavailable", dispatched: false })
  expect((await readNativeUsage(ctx)).windows[0]).toMatchObject({ name: "Weekly", span: 604800, used: 16 })
  expect(calls.some((call) => call.url.pathname.includes("/reset/"))).toBe(false)
})

test("legacy API key quota keeps its separately identified read-only reset list", async () => {
  let list = { fiveHourResets: [{ available: true, expireTime: "2026-11-30 23:59:59" }], weekResets: [{ available: false }] }
  route = (call, res) => {
    if (call.url.pathname.endsWith("/quota/limit")) return send(res, ok({ limits: [{ type: "CREDIT_LIMIT", unit: 6, number: 7, percentage: 30 }] }))
    if (call.url.pathname === "/api/biz/customer-package-reset/list") return send(res, ok(list))
    return send(res, {}, 404)
  }
  const ctx = await context("legacy-key", "bigmodel", { key: "legacy-key", business: "", jwt: "" })
  let card = await readNativeUsage(ctx)
  expect(card.windows[0]).toMatchObject({ name: "Weekly", used: 30, span: 604800 })
  expect(card.resets).toMatchObject({ source: "legacy-list", count: 1, fiveHour: 1, weekly: 0,
    each: [{ window: "fiveHour", until: "2026-11-30T15:59:59.000Z" }] })
  const legacyList = calls.find((call) => call.url.pathname === "/api/biz/customer-package-reset/list")
  expect(legacyList.headers.authorization).toBe("legacy-key")
  expect(legacyList.url.searchParams.get("targetType")).toBe("PERSONAL")
  list = { fiveHourResets: [], weekResets: [] }
  card = await readNativeUsage(ctx)
  expect(card.resets).toMatchObject({ source: "legacy-list", count: 0, fiveHour: 0, weekly: 0 })
  list = { fiveHourResets: [] }
  card = await readNativeUsage(ctx)
  expect(card.resets).toBeUndefined()
  expect(card.windows[0].used).toBe(30)
})

test("migrated Team keys keep Team card reads without gaining native credentials", async () => {
  const realFetch = globalThis.fetch
  const destinations = []
  globalThis.fetch = (input, init) => {
    const url = new URL(input instanceof Request ? input.url : String(input))
    if (!["api.z.ai", "open.bigmodel.cn", "bigmodel.cn", "zcode.z.ai"].includes(url.hostname))
      throw new Error("unexpected external destination")
    destinations.push({ origin: url.origin, path: url.pathname })
    return realFetch(new URL(url.pathname + url.search, origin), init)
  }
  try {
    for (const site of ["zai", "bigmodel"]) {
      calls = []
      destinations.length = 0
      const business = site === "zai" ? "Bearer legacy-business" : "legacy-business"
      const reports = {
        "/api/monitor/usage/model-usage": { tokensUsage: [7] },
        "/api/monitor/usage/tool-usage": { networkSearchCount: [1] },
        "/api/monitor/credit-usage/activity": { series: [{ date: "2026-11-01", mcpCalls: 2 }] },
        "/api/monitor/credit-usage/usage-detail": { mcpUsage: { mcpDataList: [{ mcpName: "web", creditsUsage: ["1.5"] }] } },
        "/api/monitor/usage/model-performance-day": { liteDecodeSpeed: [4] },
      }
      route = (call, res) => {
        const scoped = call.headers["bigmodel-organization"] === "org-1" && call.headers["bigmodel-project"] === "project-1"
        if (call.url.pathname.endsWith("/quota/limit"))
          return scoped && call.url.searchParams.get("type") === "2" && call.headers.authorization === "legacy-project-key"
            ? send(res, ok({ limits: [{ type: "CREDIT_LIMIT", unit: 6, number: 7, percentage: 73 }] })) : send(res, {}, 403)
        if (call.url.pathname.endsWith("/querySubscribeDetail"))
          return scoped && call.headers.authorization === business
            ? send(res, ok({ hasSubscription: true, status: "EFFECTIVE", memberGrantStatus: "VALID", productName: "Team Pro" })) : send(res, {}, 403)
        if (call.url.pathname.endsWith("/customer-package-reset/list"))
          return scoped && call.url.searchParams.get("targetType") === "TEAM" && call.headers.authorization === business
            ? send(res, ok({ fiveHourResets: [{ available: true }], weekResets: [{ available: true }] })) : send(res, {}, 403)
        if (call.url.pathname in reports) {
          const expectedType = call.url.pathname.includes("/credit-usage/") ? "3" :
            call.url.pathname.endsWith("/model-performance-day") ? null : "2"
          return scoped && call.url.searchParams.get("type") === expectedType && call.headers.authorization === "legacy-project-key"
            ? send(res, ok(reports[call.url.pathname])) : send(res, {}, 403)
        }
        if (call.url.pathname.endsWith("/messages"))
          return call.headers["x-api-key"] === "legacy-project-key"
            ? send(res, { type: "message", role: "assistant", content: [] }) : send(res, {}, 403)
        return send(res, {}, 404)
      }
      // This is the old migration shape: no kind, product, UID, or ZCode JWT.
      const state = { site, device: "11111111-2222-4333-8444-555555555555",
        base: site === "zai" ? "https://api.z.ai/api/anthropic" : "https://open.bigmodel.cn/api/anthropic",
        key: "legacy-project-key", token: business, org: "org-1", project: "project-1", plan: "Team Pro" }
      const fixture = accountFixture({ type: "oauth", access: state.key, refresh: JSON.stringify(state), expires: 0 })
      const hooks = await ZCodeAuthPlugin({ client: fixture.client })
      const card = await hooks.auth.usage(fixture.getAuth, { id: "zcode" })
      expect(card.error).toBeUndefined()
      expect(card.plan).toBe("Team Pro")
      expect(card.windows.find((window) => window.name === "Weekly")).toMatchObject({ used: 73, span: 604800 })
      expect(card.resets).toMatchObject({ source: "legacy-list", team: true, count: 2, fiveHour: 1, weekly: 1 })
      expect(calls).toHaveLength(3)
      for (const call of calls) {
        expect(call.headers["bigmodel-organization"]).toBe("org-1")
        expect(call.headers["bigmodel-project"]).toBe("project-1")
        expect(call.headers["set-language"]).toBe(site === "bigmodel" ? "zh" : "en")
        expect(call.headers["accept-language"]).toBe("en-US,en")
      }
      expect(destinations.every((entry) => entry.origin === (site === "bigmodel" ? "https://bigmodel.cn" : "https://api.z.ai"))).toBe(true)
      expect(calls.some((call) => call.url.pathname.endsWith("/subscription/list") ||
        call.url.searchParams.get("targetType") === "PERSONAL" || call.url.pathname.endsWith("/access_tokens"))).toBe(false)

      const account = await hooks.auth.account(fixture.getAuth, { id: "zcode" }, { operation: "context" })
      expect(account.context).toMatchObject({ kind: "legacy-key", organization: "org-1", project: "project-1", product: "" })
      const expected = { expectedScope: account.context.scope, expectedGeneration: account.context.generation }
      const entitlement = await hooks.auth.account(fixture.getAuth, { id: "zcode" }, { operation: "entitlement", ...expected })
      expect(entitlement.data).toMatchObject({ kind: "available", subscription: { productName: "Team Pro" } })
      const range = { startTime: "2026-11-01 00:00:00", endTime: "2026-11-07 23:59:59" }
      for (const operation of ["usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.health"]) {
        const params = operation === "usage.detail" ? { ...range, usageType: "MCP" } : range
        const result = await hooks.auth.account(fixture.getAuth, { id: "zcode" }, { operation, params, ...expected })
        if (operation === "usage.model") expect(result.data.tokensUsage).toEqual([7])
        if (operation === "usage.tool") expect(result.data.networkSearchCount).toEqual([1])
        if (operation === "usage.activity") expect(result.data.series[0].mcpCalls).toBe(2)
        if (operation === "usage.detail") expect(result.data.mcpUsage.mcpDataList[0].creditsUsage).toEqual(["1.5"])
        if (operation === "usage.health") expect(result.data.liteDecodeSpeed).toEqual([4])
        const report = calls.at(-1)
        expect(report.url.searchParams.get("startTime")).toBe(range.startTime)
        expect(report.url.searchParams.get("endTime")).toBe(range.endTime)
        expect(report.headers.authorization).toBe("legacy-project-key")
        expect(report.headers["bigmodel-organization"]).toBe("org-1")
        expect(report.headers["bigmodel-project"]).toBe("project-1")
        if (operation === "usage.detail") expect(report.url.searchParams.get("usageType")).toBe("MCP")
      }
      await expect(hooks.auth.account(fixture.getAuth, { id: "zcode" }, {
        operation: "reset.status", ...expected,
      })).rejects.toMatchObject({ kind: "unavailable", dispatched: false })
      expect((await hooks.auth.account(fixture.getAuth, { id: "zcode" }, {
        operation: "usage.mcp", ...expected,
      })).data).toBeNull()
      expect(calls).toHaveLength(9)
      expect(calls.some((call) => call.url.pathname.endsWith("/subscription/list") ||
        call.url.searchParams.get("targetType") === "PERSONAL" || call.url.pathname.endsWith("/access_tokens"))).toBe(false)

      const withoutBusiness = { ...state }
      delete withoutBusiness.token
      const keyOnly = accountFixture({ type: "oauth", access: withoutBusiness.key, refresh: JSON.stringify(withoutBusiness), expires: 0 })
      const keyOnlyHooks = await ZCodeAuthPlugin({ client: keyOnly.client })
      calls = []
      const keyOnlyCard = await keyOnlyHooks.auth.usage(keyOnly.getAuth, { id: "zcode" })
      expect(keyOnlyCard.error).toBeUndefined()
      expect(keyOnlyCard.windows.find((window) => window.name === "Weekly")).toMatchObject({ used: 73 })
      expect(keyOnlyCard.resets).toBeUndefined()
      expect(calls).toHaveLength(1)
      expect(calls[0].url.pathname).toBe("/api/monitor/usage/quota/limit")
      expect(calls[0].url.searchParams.get("type")).toBe("2")

      for (const missing of ["org", "project"]) {
        const partial = { ...state }
        delete partial[missing]
        const partialFixture = accountFixture({ type: "oauth", access: partial.key, refresh: JSON.stringify(partial), expires: 0 })
        const partialHooks = await ZCodeAuthPlugin({ client: partialFixture.client })
        calls = []
        const unavailable = await partialHooks.auth.usage(partialFixture.getAuth, { id: "zcode" })
        expect(unavailable.error).toContain("unavailable")
        const partialAccount = await partialHooks.auth.account(partialFixture.getAuth, { id: "zcode" }, { operation: "context" })
        const partialExpected = { expectedScope: partialAccount.context.scope, expectedGeneration: partialAccount.context.generation }
        for (const operation of ["entitlement", "usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.health"]) {
          const params = operation === "usage.detail" ? { ...range, usageType: "MCP" } : range
          await expect(partialHooks.auth.account(partialFixture.getAuth, { id: "zcode" }, { operation, params, ...partialExpected }))
            .rejects.toMatchObject({ kind: "unavailable", dispatched: false })
        }
        expect(calls).toHaveLength(0)
        const loader = await partialHooks.auth.loader(partialFixture.getAuth, { id: "zcode" })
        const response = await loader.fetch(`${partial.base}/v1/messages`, {
          method: "POST", headers: { "content-type": "application/json" },
          body: JSON.stringify({ model: "GLM-5.3", max_tokens: 10, messages: [{ role: "user", content: "hi" }] }),
        })
        expect(response.status).toBe(200)
        await response.text()
        expect(calls).toHaveLength(1)
        expect(calls[0].headers["x-api-key"]).toBe("legacy-project-key")
      }
    }
  } finally {
    globalThis.fetch = realFetch
  }
})

test("entitlement distinguishes unrelated, malformed, expired and valid subscriptions", async () => {
  let data = []
  route = (call, res) => send(res, ok(data))
  const personal = await context()
  data = [{ productName: "Cloud storage", status: "VALID", inCurrentPeriod: true }]
  expect(await nativeUsageOperation(personal, "entitlement")).toEqual({ kind: "unavailable" })
  data = [{ productName: "GLM Coding Pro", status: "VALID" }]
  expect(await nativeUsageOperation(personal, "entitlement")).toEqual({ kind: "unknown" })
  data = [{ productName: "GLM Coding Pro", status: "VALID", inCurrentPeriod: false }]
  expect(await nativeUsageOperation(personal, "entitlement")).toEqual({ kind: "unavailable" })
  data = [{ productName: "GLM Coding Pro", status: "VALID", inCurrentPeriod: true, accessToken: "must-not-leak" }]
  const available = await nativeUsageOperation(personal, "entitlement")
  expect(available).toMatchObject({ kind: "available" })
  expect(available.subscription.accessToken).toBeUndefined()
  const team = await context("team", "zai")
  data = { status: "EFFECTIVE", memberGrantStatus: "VALID" }
  expect(await nativeUsageOperation(team, "entitlement")).toEqual({ kind: "unknown" })
  data = { hasSubscription: true, status: "EXPIRED" }
  expect(await nativeUsageOperation(team, "entitlement")).toEqual({ kind: "unavailable", reason: "expired" })
  data = { hasSubscription: true, status: "EFFECTIVE", memberGrantStatus: "UNASSIGNED" }
  expect(await nativeUsageOperation(team, "entitlement")).toEqual({ kind: "unavailable", reason: "unassigned" })
  const teamCall = calls.find((call) => call.url.pathname.includes("querySubscribeDetail"))
  expect(teamCall.headers.authorization).toBe("Bearer business-token")
})

test("explicit reports use exact dates and Team credit type, preserving partial metrics", async () => {
  const payloads = {
    "/api/monitor/usage/model-usage": { x_time: ["2026-11-01"], tokensUsage: [23] },
    "/api/monitor/usage/tool-usage": { totalUsage: { totalSearchMcpCount: 7 } },
    "/api/monitor/credit-usage/activity": { series: [{ date: "2026-11-01", mcpCalls: 2 }] },
    "/api/monitor/credit-usage/usage-detail": { summary: { totalCredits: { value: "2.5" } }, mcpUsage: { mcpDataList: [{ mcpName: "web", creditsUsage: ["1.5", null, 1], accessToken: "must-not-leak" }] } },
    "/api/monitor/usage/model-performance-day": { xTime: ["2026-11-01"], liteDecodeSpeed: [3] },
  }
  route = (call, res) => send(res, ok(payloads[call.url.pathname]))
  const ctx = await context("team", "bigmodel")
  const params = { startTime: "2026-11-01 00:00:00", endTime: "2026-11-07 23:59:59" }
  for (const operation of ["usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.health"]) {
    const data = await nativeUsageOperation(ctx, operation, operation === "usage.detail" ? { ...params, usageType: "MCP" } : params)
    if (operation === "usage.model") { expect(data.tokensUsage).toEqual([23]); expect(data.modelCallCount).toBeUndefined() }
    if (operation === "usage.tool") expect(data.totalUsage.totalSearchMcpCount).toBe(7)
    if (operation === "usage.activity") { expect(data.series[0].mcpCalls).toBe(2); expect(data.summary).toBeUndefined() }
    if (operation === "usage.detail") {
      expect(data.summary.totalCredits.value).toBe("2.5")
      expect(data.mcpUsage.mcpDataList[0].creditsUsage).toEqual(["1.5", null, 1])
      expect(data.mcpUsage.mcpDataList[0].accessToken).toBeUndefined()
      expect(data.summary.cacheHitRate).toBeUndefined()
    }
    if (operation === "usage.health") expect(data.liteDecodeSpeed).toEqual([3])
    const call = calls.at(-1)
    expect(call.url.searchParams.get("startTime")).toBe(params.startTime)
    expect(call.url.searchParams.get("endTime")).toBe(params.endTime)
    expect(call.headers.authorization).toBe("project-token")
    expect(call.headers["bigmodel-project"]).toBe("project-1")
    if (operation === "usage.model" || operation === "usage.tool") expect(call.url.searchParams.get("type")).toBe("2")
    if (operation === "usage.activity" || operation === "usage.detail") expect(call.url.searchParams.get("type")).toBe("3")
    if (operation === "usage.health") expect(call.url.searchParams.has("type")).toBe(false)
    if (operation === "usage.detail") expect(call.url.searchParams.get("usageType")).toBe("MCP")
  }
  const personal = await context()
  await nativeUsageOperation(personal, "usage.activity", params)
  expect(calls.at(-1).url.searchParams.get("type")).toBe("1")
  payloads["/api/monitor/credit-usage/activity"] = { series: [{ mcpCalls: "invalid" }] }
  await expect(nativeUsageOperation(personal, "usage.activity", params)).rejects.toMatchObject({ kind: "invalid_response" })
  await expect(nativeUsageOperation(personal, "usage.detail", params)).rejects.toMatchObject({ kind: "invalid_request" })
})

test("Z.ai Team MCP wire omits target headers while retaining its own aggregate", async () => {
  const now = Math.floor(Date.now() / 1000)
  route = (_call, res) => send(res, ok({ server_time: now, total_usage: { used: 1, limit: 2, remaining: 1 } }))
  const result = await nativeUsageOperation(await context("team", "zai"), "usage.mcp")
  expect(result).toMatchObject({ kind: "server-mcp", used: 1, limit: 2 })
  const call = calls.at(-1)
  expect(call.headers["x-bigmodel-authorization"]).toBe("Bearer business-token")
  expect(call.headers["bigmodel-target-type"]).toBeUndefined()
  expect(call.headers["bigmodel-organization"]).toBeUndefined()
})
