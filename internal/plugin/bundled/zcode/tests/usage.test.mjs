// The public usage hook is the card consumer. These fixtures exercise it
// with legacy keys, account-scoped PATs and Start balances without networking.
import { afterAll, beforeEach, expect, test } from "bun:test"
import { homedir, tmpdir } from "node:os"
import { realpathSync } from "node:fs"
import { ZCodeAuthPlugin } from "../index.mjs"
import { accountFixture } from "./account-fixtures.mjs"

if (![tmpdir(), realpathSync(tmpdir())].some((root) => homedir().startsWith(root))) throw new Error("run with an isolated HOME")
const originalFetch = globalThis.fetch
let calls, route
const ok = (data) => Response.json({ code: 0, data })
beforeEach(() => {
  calls = []
  route = () => new Response("", { status: 404 })
  globalThis.fetch = async (input, init = {}) => {
    const call = { url: new URL(String(input)), headers: new Headers(init.headers), method: init.method ?? "GET", body: init.body }
    calls.push(call)
    return route(call)
  }
})
afterAll(() => { globalThis.fetch = originalFetch })

async function usage(initial) {
  const fixture = accountFixture(initial)
  const hooks = await ZCodeAuthPlugin({ client: fixture.client })
  return hooks.auth.usage(fixture.getAuth, { id: "zcode" })
}
const oauth = (state) => ({ type: "oauth", uid: state.uid, access: state.key ?? state.jwt ?? "", refresh: JSON.stringify(state), expires: 0 })
const jwt = (exp) => ["{}", JSON.stringify({ exp })].map((part) => Buffer.from(part).toString("base64url")).join(".") + ".sig"

test("a legacy key reads the plan without inventing a login JWT", async () => {
  for (const site of ["zai", "bigmodel"]) {
    calls = []
    route = (call) => {
      if (call.url.pathname.endsWith("/quota/limit")) return ok({ level: "pro", limits: [{ type: "CREDIT_LIMIT", unit: 3, number: 5, usage: 2000, remaining: 1500 }] })
      if (call.url.pathname === "/api/biz/subscription/list") return ok([{ productName: "GLM Coding Pro", status: "VALID", inCurrentPeriod: true, nextRenewTime: "2026-12-01 12:00:00", autoRenew: true }])
      if (call.url.pathname === "/api/biz/customer-package-reset/list") return ok({ fiveHourResets: [], weekResets: [] })
      return new Response("", { status: 404 })
    }
    const card = await usage({ type: "api", key: "example-key", metadata: { site } })
    expect(card).toMatchObject({ signIn: "kept", plan: "GLM Coding Pro", until: "2026-12-01T04:00:00.000Z", renew: "auto" })
    expect(card.windows.filter((window) => !window.aside)).toMatchObject([{ name: "5 hours", used: 25, amount: 500, limit: 2000, display: "500 / 2000", span: 18000 }])
    expect(card.windows.find((window) => window.name === "ZCode gift plans (-Trial models) need ZCode's sign-in")?.display).toBe("not read with an API key: add the account with a ZCode sign-in")
    const quota = calls.find((call) => call.url.pathname.endsWith("/quota/limit"))
    expect(quota.url.origin).toBe(site === "zai" ? "https://api.z.ai" : "https://open.bigmodel.cn")
    expect(quota.headers.get("authorization")).toBe("example-key")
    expect(calls.some((call) => call.url.pathname.startsWith("/api/v1/"))).toBe(false)
  }
})

test("weekly units ignore the server number and absent share stays unknown", async () => {
  route = (call) => call.url.pathname.endsWith("/quota/limit") ? ok({ limits: [
    { type: "TOKENS_LIMIT", unit: 6, number: 1, percentage: 30 },
    { type: "CREDIT_LIMIT", unit: 6, number: 7 },
    { type: "TOKENS_LIMIT", unit: 6, currentValue: 200, usage: 800 },
    { type: "TIME_LIMIT", unit: 5, number: 1, percentage: 100, usage: 0 },
  ] }) : new Response("", { status: 404 })
  const card = await usage({ type: "api", key: "legacy" })
  const windows = card.windows.filter((window) => !window.name.startsWith("ZCode gift plans"))
  expect(windows).toMatchObject([
    { name: "Weekly", used: 30, span: 604800 },
    { name: "Weekly", span: 604800 },
    { name: "Weekly", used: 25, amount: 200, limit: 800, display: "200 / 800", span: 604800 },
    { name: "MCP · Month", used: 0, aside: true, span: 2592000 },
  ])
  expect(windows[1].used).toBeUndefined()
})

test("a malformed or unrelated entitlement does not erase authoritative quota", async () => {
  for (const subscriptions of [[{ productName: "Object Storage", status: "VALID", inCurrentPeriod: true }],
    [{ productName: "GLM Coding Pro", status: "VALID" }]]) {
    route = (call) => call.url.pathname.endsWith("/quota/limit") ? ok({ limits: [{ type: "TOKENS_LIMIT", unit: 3, number: 5, percentage: 42 }] }) :
      call.url.pathname.endsWith("/subscription/list") ? ok(subscriptions) : new Response("", { status: 404 })
    const card = await usage({ type: "api", key: "legacy" })
    expect(card.error).toBeUndefined()
    expect(card.plan).toBeUndefined()
    expect(card.windows[0]).toMatchObject({ name: "5 hours", used: 42 })
  }
})

test("legacy reset availability is distinct from a missing list", async () => {
  let list = { fiveHourResets: [{ available: true, expireTime: "2026-11-30 23:59:59" }], weekResets: [{ available: true }] }
  route = (call) => call.url.pathname.endsWith("/quota/limit") ? ok({ limits: [{ unit: 6, percentage: 10 }] }) :
    call.url.pathname === "/api/biz/customer-package-reset/list" ? list ? ok(list) : new Response("", { status: 500 }) : new Response("", { status: 404 })
  const auth = { type: "api", key: "legacy" }
  let card = await usage(auth)
  expect(card.resets).toMatchObject({ source: "legacy-list", count: 2, byWindow: true, fiveHour: 1, weekly: 1,
    until: "2026-11-30T15:59:59.000Z", each: [{ window: "fiveHour", until: "2026-11-30T15:59:59.000Z" }, { window: "weekly" }] })
  list = null
  card = await usage(auth)
  expect(card.resets).toBeUndefined()
  expect(card.windows[0].used).toBe(10)
})

test("a Team card uses the scoped project token and independent business entitlement", async () => {
  route = (call) => {
    if (call.url.pathname.endsWith("/access_tokens")) return ok({ enable: true, accessToken: "team-pat", tokenType: "Bearer", expiresIn: 3600, expiresAt: Math.floor(Date.now() / 1000) + 3600 })
    if (call.url.pathname.endsWith("/quota/limit")) return ok({ limits: [{ type: "CREDIT_LIMIT", unit: 6, number: 7, percentage: 42 }] })
    if (call.url.pathname.endsWith("/querySubscribeDetail")) return ok({ hasSubscription: true, status: "EFFECTIVE", memberGrantStatus: "VALID", productName: "Team Max", subscribeEndTime: "2026-12-31 23:59:59" })
    return new Response("", { status: 404 })
  }
  const auth = oauth({ site: "bigmodel", uid: "user-1", kind: "team", org: "org", project: "project", product: "product", business: "team-business",
    loc: { organizationId: "org", projectId: "project", apiKeyId: "team-key" } })
  const card = await usage(auth)
  expect(card).toMatchObject({ plan: "Team Max", until: "2026-12-31T15:59:59.000Z", renew: "off" })
  expect(card.windows[0]).toMatchObject({ name: "Weekly", used: 42, span: 604800 })
  const quota = calls.find((call) => call.url.pathname.endsWith("/quota/limit"))
  expect([quota.url.searchParams.get("type"), quota.headers.get("authorization"), quota.headers.get("bigmodel-organization"), quota.headers.get("bigmodel-project")]).toEqual(["2", "team-pat", "org", "project"])
  expect(calls.find((call) => call.url.pathname.endsWith("/querySubscribeDetail")).headers.get("authorization")).toBe("team-business")
  expect(calls.some((call) => call.url.pathname.endsWith("/reset/list"))).toBe(false)
})

test("Start card follows the response Date and does not merge different buckets", async () => {
  const now = Math.floor(Date.now() / 1000)
  route = (call) => call.url.pathname.endsWith("/billing/balance") ? Response.json({ code: 0, data: { server_time: now - 86400,
    plans: [{ user_plan_id: "old", plan_id: "p", status: "active", ends_at: now - 10 }, { user_plan_id: "new", plan_id: "p", name: "Start Plan", status: "active", ends_at: now + 3600 }],
    balances: [{ user_plan_id: "old", plan_id: "p", show_name: "Expired", total_units: 1, remaining_units: 1 },
      { user_plan_id: "new", plan_id: "p", bucket_id: "b1", entitlement_id: "e1", show_name: "GLM-5.3", capabilities: ["MODEL:glm-5.3"], total_units: 100, remaining_units: 75 },
      { user_plan_id: "new", plan_id: "p", bucket_id: "b2", entitlement_id: "e2", show_name: "GLM-5.3-Flash", total_units: 100 }] } }, { headers: { date: new Date(now * 1000).toUTCString() } }) : new Response("", { status: 404 })
  const card = await usage(oauth({ site: "zai", jwt: jwt(now + 3600), device: "11111111-2222-4333-8444-555555555555" }))
  expect(card.plan).toBe("Start Plan")
  expect(card.windows).toHaveLength(2)
  expect(card.windows[0]).toMatchObject({ used: 25, models: ["GLM-5.3", "GLM-5.3-Trial"] })
  expect(card.windows[1].used).toBeUndefined()
  expect(card.observations.map((entry) => entry.identity.bucketId)).toEqual(["b1", "b2"])
  expect(card.observations[1].used).toBeNull()
})

test("a successful HTTP response with success:false is not a quota result", async () => {
  route = (call) => call.url.pathname.endsWith("/quota/limit") ? Response.json({ code: 0, success: false, data: { limits: [{ unit: 6, percentage: 0 }] } }) : new Response("", { status: 404 })
  const card = await usage({ type: "api", key: "legacy" })
  expect(card.error).toBeDefined()
  expect(card.windows).toEqual([])
})

test("a claimable gift hint stays informational and never claims the offer", async () => {
  const now = Math.floor(Date.now() / 1000)
  route = (call) => {
    if (call.url.pathname.endsWith("/billing/balance")) return ok({
      server_time: now, plans: [{ plan_id: "start", name: "Start Plan", status: "active", ends_at: now + 3600 }],
      balances: [{ plan_id: "start", show_name: "GLM-5.3", total_units: 100, remaining_units: 75 }],
    })
    if (call.url.pathname.endsWith("/marketing/touch")) return ok({ server_time: now, language: "en-US", deliveries: [{
      campaign_id: "campaign-1", priority: 100, resource_position: "banner",
      banner: { background: { type: "zcode_plan", args: { zcode_plan: { plan_id: "trust", name: "Trust Build" } } },
        buttons: [{ action: { type: "claim_zcode_plan", args: { plan_id: "trust" } } }] },
    }] })
    return new Response("", { status: 404 })
  }
  const card = await usage(oauth({ site: "zai", jwt: jwt(now + 3600), device: "11111111-2222-4333-8444-555555555555" }))
  expect(card.windows.find((window) => window.name === "Gift plans to claim in the ZCode app")).toMatchObject({
    aside: true, display: "1 · Trust Build",
  })
  expect(calls.some((call) => call.url.pathname.endsWith("/claim"))).toBe(false)
})
