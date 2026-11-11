// Protocol examples follow the public ZCode 3.18 source and the
// captured marketing delivery; these are offline fixtures, not live proof.
import { expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { nativeLaneOperation, routeCodingPlanGateway, runNativeLane, trustedOfficialMcpURL } from "../lanes.mjs"

const ORIGIN = "https://zcode.z.ai"
const DEVICE = "11111111-2222-4333-8444-555555555555"
const MODEL = "GLM-5.3-Flash"
const realFeed = JSON.parse(readFileSync(new URL("./testdata/touch-1030-wk.json", import.meta.url), "utf8"))
const reply = (data, status = 200, headers = {}) => ({ ok: status >= 200 && status < 300, status, data, headers: new Headers(headers), emptyBody: false })
function context({ site = "zai", kind = "individual", request, verification = false } = {}) {
  let jwt = "jwt-one", business = "business-one", checks = 0, pats = 0
  const ctx = {
    scope: "account-scope", generation: "epoch-1",
    state: { site, kind, uid: "user", zcodeUserId: "user", device: DEVICE, org: "org-1", project: "project-2", product: "team", origins: { zcode: ORIGIN, business: site === "zai" ? "https://api.z.ai" : "https://open.bigmodel.cn", mcpDev: ["http://127.0.0.1:4321"] } },
    calls: [],
    async assertCurrent() { checks++ },
    async loginMaterial(options) {
      if (options.zcode === "required" && !jwt || options.business === "required" && !business) throw Object.assign(new Error("credential unavailable"), { kind: "unavailable" })
      return { zcode: options.zcode === "none" ? "" : jwt, business: options.business === "none" ? "" : business }
    },
    async projectToken() { return { token: `pat-${++pats}`, fingerprint: `f-${pats}`, expiresAt: Date.now() + 60000 } },
    async claimVerification() { if (verification instanceof Error) throw verification; return verification },
    async request(url, init, opts) {
      const call = { url: new URL(url), ...init, ...opts }
      this.calls.push(call)
      return request ? request(call) : reply({ code: 0, data: {} })
    },
    setJwt(value) { jwt = value }, setBusiness(value) { business = value },
    get checks() { return checks }, get pats() { return pats },
  }
  return ctx
}
const binding = (lane = "offpeak") => ({ lane, taskId: "conversation-1", scope: "account-scope", generation: "epoch-1", rawModel: MODEL, ...(lane === "offpeak" ? { ticketId: "ticket-1" } : { cardId: "card-1", expiresAt: Date.now() + 60000 }) })
const options = (lane, send, extra = {}) => ({ url: "https://api.z.ai/api/anthropic/v1/messages?beta=1", init: { method: "POST", headers: { "X-Api-Key": "old", "Content-Type": "application/json" }, body: JSON.stringify({ model: MODEL, output_config: { effort: "high" }, messages: [] }) }, model: MODEL, sessionId: "conversation-1", execution: binding(lane), send, fallback: async () => Response.json({ fallback: true }), ...extra })

test("lane business failures inside HTTP 200 retain the vendor code", async () => {
  const c = context({ request: () => reply({ code: 2007, msg: "temporarily unavailable" }) })
  await expect(nativeLaneOperation(c, "highspeed.draw", { task_id: "task", model: MODEL }))
    .rejects.toMatchObject({ kind: "business", code: 2007, httpStatus: 200 })
  await expect(nativeLaneOperation(c, "billing.status"))
    .rejects.toMatchObject({ kind: "business", code: 2007, httpStatus: 200 })
})

test("an unverified user cannot dispatch a financial claim or lane operation", async () => {
  const c = context()
  c.state.zcodeUserId = ""
  await expect(nativeLaneOperation(c, "billing.claim.raw", { plan_id: "gift" })).rejects.toMatchObject({ kind: "unavailable", dispatched: false })
  await expect(nativeLaneOperation(c, "highspeed.draw", { task_id: "task", model: MODEL })).rejects.toMatchObject({ kind: "unavailable" })
  expect(c.calls).toHaveLength(0)
})

test("Off-Peak recovers one rejected PAT without changing the ticket", async () => {
  const c = context()
  const sent = []
  const result = await runNativeLane(c, options("offpeak", async (_url, init) => {
    sent.push(new Headers(init.headers))
    return sent.length === 1 ? Response.json({ error: { message: "expired" } }, { status: 401 }) :
      Response.json({ type: "message", content: [{ type: "text", text: "ok" }] })
  }))
  expect(result.status).toBe(200)
  expect(sent).toHaveLength(2)
  expect(sent.map((h) => h.get("x-coding-plan-api-key"))).toEqual(["pat-1", "pat-2"])
  expect(sent.map((h) => h.get("x-off-peak-ticket-id"))).toEqual(["ticket-1", "ticket-1"])
})

// Preview may be anonymous; a POST is never made while verification is unknown.
test("preview query uses the paired version/platform and claim fails closed on verification and malformed success", async () => {
  const anon = context({ request: () => reply({ code: 0, data: { server_time: 1793369500, plans: [{ plan_id: "one", entitlements: [{ entitlement_id: "e1" }, { entitlement_id: "" }] }, { plan_id: "" }] } }) })
  anon.setJwt("")
  const preview = await nativeLaneOperation(anon, "billing.preview", {})
  expect(preview.plans.map((p) => p.plan_id)).toEqual(["one"])
  expect(preview.plans[0].entitlements.map((p) => p.entitlement_id)).toEqual(["e1"])
  expect(anon.calls[0].url.searchParams.get("app_version")).toMatch(/^\d+\.\d+\.\d+$/)
  expect(anon.calls[0].url.searchParams.get("platform")).toBe(`${process.platform}-${process.arch}`)
  expect(anon.calls[0].headers.Authorization).toBeUndefined()
  for (const verification of [true, null, new Error("config unreadable")]) {
    const c = context({ verification })
    await expect(nativeLaneOperation(c, "billing.claim.raw", { plan_id: "gift" })).rejects.toMatchObject({ kind: "verification_unavailable" })
    expect(c.calls).toHaveLength(0)
  }
  for (const answer of [reply({ code: 0 }), reply({ code: "0", data: {} }), reply({ code: 1003, data: {} }, 500)]) {
    const c = context({ request: () => answer })
    await expect(nativeLaneOperation(c, "billing.claim.raw", { plan_id: "gift" })).rejects.toThrow()
  }
  const c = context({ request: () => reply({ code: "0", data: { plan: { user_plan_id: "up", plan_id: "gift", status: "active", entitlements: [] } } }) })
  expect((await nativeLaneOperation(c, "billing.claim.raw", { plan_id: "gift" })).plan.plan_id).toBe("gift")
  expect(JSON.parse(c.calls[0].body)).toEqual({ plan_id: "gift" })
  expect(c.calls[0].headers["X-ZCode-App-Version"]).toBe(anon.calls[0].url.searchParams.get("app_version"))
  expect(c.calls[0].headers["X-Platform"]).toBe(`${process.platform}-${process.arch}`)
})

test("claim rejects missing, blank, or mismatched vendor plan identities after dispatch", async () => {
  const campaignPlan = realFeed.data.deliveries[0].banner.buttons[1].action.args.plan_id
  for (const requestedPlan of ["standalone-gift", campaignPlan]) {
    for (const plan of [{}, { plan_id: "" }, { plan_id: " \t " }, { plan_id: null }, { plan_id: 123 }, { plan_id: `${requestedPlan}-other` }]) {
      const c = context({ verification: false, request: () => reply({ code: 0, data: { plan } }) })
      await expect(nativeLaneOperation(c, "billing.claim.raw", { plan_id: requestedPlan }))
        .rejects.toMatchObject({ kind: "malformed", dispatched: true })
      expect(c.calls).toHaveLength(1)
      expect(c.calls[0].url.pathname).toBe("/api/v1/zcode-plan/billing/claim")
      expect(c.calls[0].method).toBe("POST")
      expect(JSON.parse(c.calls[0].body)).toEqual({ plan_id: requestedPlan })
    }
    const c = context({ verification: false, request: () => reply({ code: 0, data: { plan: { plan_id: requestedPlan, status: "active" } } }) })
    expect((await nativeLaneOperation(c, "billing.claim.raw", { plan_id: requestedPlan })).plan)
      .toMatchObject({ plan_id: requestedPlan, status: "active" })
    expect(c.calls).toHaveLength(1)
  }
})

test("marketing validates the real delivery and binds action reports to the current login", async () => {
  const c = context({ request: ({ url }) => url.pathname.endsWith("/action") ? reply({ code: 0 }) : reply(realFeed) })
  const first = await nativeLaneOperation(c, "marketing.feed", { locale: "zh-CN" })
  expect(first.deliveries).toHaveLength(1)
  expect(first.serverTime).toBe(realFeed.data.server_time * 1000)
  expect(first.rejectedCount).toBe(0)
  expect(first.deliveries[0].banner.buttons[0].text.format).toBe("plaintext")
  expect(first.deliveries[0].banner.buttons[0].action).toEqual({ type: "close" })
  const second = await nativeLaneOperation(c, "marketing.feed", { locale: "zh-CN" })
  expect(Number(c.calls[1].url.searchParams.get("seq"))).toBe(Number(c.calls[0].url.searchParams.get("seq")) + 1)
  expect(c.calls[0].headers["X-Device-Mid"]).toBe(DEVICE)
  expect(c.calls[0].headers["X-Client-Language"]).toBe("zh-CN")
  expect(c.calls[0].redirect).toBe("error")
  await nativeLaneOperation(c, "marketing.action", { locale: "zh-CN", scope: second.scope, campaign_id: "campaign-1", action_type: "confirm" })
  expect(JSON.parse(c.calls[2].body)).toEqual({ campaign_id: "campaign-1", action_type: "confirm" })
  c.setJwt("new-session-jwt")
  await expect(nativeLaneOperation(c, "marketing.action", { locale: "zh-CN", scope: first.scope, campaign_id: "campaign-1", action_type: "cancel" })).rejects.toMatchObject({ kind: "stale" })
  expect(c.calls).toHaveLength(3)
})

test("marketing rejects malformed envelopes but counts bad or duplicate positions independently", async () => {
  const data = structuredClone(realFeed)
  data.data.deliveries.push(structuredClone(data.data.deliveries[0]), { campaign_id: "bad", resource_position: "popup", priority: 101 })
  const c = context({ request: () => reply(data) })
  const feed = await nativeLaneOperation(c, "marketing.feed", { locale: "en-US" })
  expect(feed.deliveries).toHaveLength(1)
  expect(feed.rejectedCount).toBe(2)
  for (const payload of [{ code: 0 }, { code: 0, data: { deliveries: [] } }, { code: 0, data: { server_time: 1, language: "en-US", deliveries: Array(33).fill({}) } }]) {
    const bad = context({ request: () => reply(payload) })
    await expect(nativeLaneOperation(bad, "marketing.feed", {})).rejects.toMatchObject({ kind: "malformed" })
  }
  const anon = context({ request: () => reply(realFeed) })
  anon.setJwt("")
  await nativeLaneOperation(anon, "marketing.feed", { locale: "en-US" })
  expect(anon.calls[0].headers.Authorization).toBeUndefined()
  const changing = context({ request: () => { changing.setJwt("new-jwt"); return reply(realFeed) } })
  await expect(nativeLaneOperation(changing, "marketing.feed", { locale: "en-US" })).rejects.toMatchObject({ kind: "stale" })
})

test("Off-Peak ticket APIs keep selected family scope, exact schemas, and explicit settlement", async () => {
  for (const site of ["zai", "bigmodel"]) {
    const c = context({ site, kind: "team", request: ({ url }) => {
      if (url.pathname.endsWith("/availability")) return reply({ can_take_number: false, next_take_at: 1793369500000 })
      if (url.pathname.endsWith("/status")) return reply({ code: 0, data: { next_poll_after: 2.5, tickets: [{ ticket_id: "ticket-1", state: "ready", position: null }] } })
      if (url.pathname.endsWith("/settle")) return { ...reply(null), emptyBody: true }
      return reply({ ticket_id: "ticket-1", task_id: "conversation-1", state: "queued", position: 3, next_poll_after: 5 })
    } })
    expect((await nativeLaneOperation(c, "offpeak.availability", {})).next_take_at).toBe(1793369500000)
    expect((await nativeLaneOperation(c, "offpeak.ticket", { task_id: "conversation-1" })).ticket_id).toBe("ticket-1")
    const status = await nativeLaneOperation(c, "offpeak.status", { ticket_ids: ["ticket-1"] })
    expect(status.next_poll_after).toBe(2.5)
    expect(status.tickets[0].state).toBe("ready")
    expect(await nativeLaneOperation(c, "offpeak.settle", { ticket_id: "ticket-1" })).toEqual({})
    expect(c.calls.map((x) => x.timeout)).toEqual([10000, 10000, 10000, 10000])
    for (const [i, call] of c.calls.entries()) {
      expect(call.headers.Authorization).toBe("Bearer jwt-one")
      expect(call.headers["X-Coding-Plan-Api-Key"]).toBe(`pat-${i + 1}`)
      expect(call.headers["X-ZCode-App-Version"]).toMatch(/^\d+\.\d+\.\d+$/)
      expect(call.headers["X-Request-Id"]).toMatch(/^[0-9a-f-]+$/i)
      expect(call.headers["Bigmodel-Organization"]).toBe(site === "bigmodel" ? "org-1" : undefined)
      expect(call.headers["Bigmodel-Project"]).toBe(site === "bigmodel" ? "project-2" : undefined)
      expect(call.headers["Bigmodel-Target-Type"]).toBeUndefined()
    }
    expect(JSON.parse(c.calls[1].body)).toEqual({ task_id: "conversation-1" })
    expect(JSON.parse(c.calls[2].body)).toEqual({ ticket_ids: ["ticket-1"] })
    expect(c.calls[3].body).toBeUndefined()
  }
})

test("Off-Peak rejects malformed, truncated, ambiguous and ineligible admission outcomes", async () => {
  const c = context({ request: () => reply({ can_take_number: false }) })
  await expect(nativeLaneOperation(c, "offpeak.availability", {})).rejects.toMatchObject({ kind: "malformed" })
  await expect(nativeLaneOperation(c, "offpeak.status", { ticket_ids: Array(101).fill("t") })).rejects.toMatchObject({ kind: "invalid" })
  const wrongTask = context({ request: () => reply({ ticket_id: "ticket", task_id: "someone-else", state: "ready" }) })
  await expect(nativeLaneOperation(wrongTask, "offpeak.ticket", { task_id: "ours" })).rejects.toMatchObject({ kind: "malformed", dispatched: true })
  const denied = context({ request: () => reply({ code: 3103, data: { next_take_at: 1793369500000 } }, 429, { "retry-after": "17" }) })
  await expect(nativeLaneOperation(denied, "offpeak.ticket", { task_id: "ours" })).rejects.toMatchObject({ code: 3103, throttled: true, dispatched: true, nextTakeAt: 1793369500000, retryAfter: 17 })
  expect(denied.calls).toHaveLength(1)
  const invalidBody = context({ request: () => reply(null) })
  await expect(nativeLaneOperation(invalidBody, "offpeak.settle", { ticket_id: "ticket" })).rejects.toMatchObject({ kind: "malformed" })
  const unknown = context({ request: () => { throw Object.assign(new Error("lost reply"), { kind: "timeout", dispatched: true }) } })
  await expect(nativeLaneOperation(unknown, "offpeak.ticket", { task_id: "ours" })).rejects.toMatchObject({ kind: "timeout", dispatched: true })
  expect(unknown.calls).toHaveLength(1)
  const before = context()
  before.projectToken = async () => { throw Object.assign(new Error("mint failed"), { kind: "network", dispatched: true }) }
  await expect(nativeLaneOperation(before, "offpeak.ticket", { task_id: "ours" })).rejects.toMatchObject({ kind: "network", dispatched: false })
  expect(before.calls).toHaveLength(0)
  for (const kind of ["start", "legacy-key"]) {
    await expect(nativeLaneOperation(context({ kind }), "offpeak.ticket", { task_id: "ours" })).rejects.toMatchObject({ kind: "unavailable" })
    await expect(nativeLaneOperation(context({ kind }), "offpeak.status", { ticket_ids: [] })).rejects.toMatchObject({ kind: "unavailable" })
  }
})

test("highspeed draw binds the server card to task, provider and model; healthy uses only ZCode JWT", async () => {
  for (const site of ["zai", "bigmodel"]) {
    const provider = `builtin:${site}-coding-plan`
    const c = context({ site, kind: "team", request: ({ url }) => url.pathname.endsWith("/draw")
      ? reply({ code: 0, msg: "", data: { card: { card_id: "card-1", task_id: "task-1", provider, model: MODEL, issued_at: 1000, expires_at: 2000 }, next_draw_at: 3000 } })
      : reply({ code: 0, msg: "", data: { card_id: "card-1", prompt_tokens: 3, completion_tokens: 5, duration_seconds: 2 } }) })
    expect((await nativeLaneOperation(c, "highspeed.draw", { task_id: "task-1", model: MODEL })).next_draw_at).toBe(3000)
    expect(JSON.parse(c.calls[0].body)).toEqual({ task_id: "task-1", provider, model: MODEL })
    expect(c.calls[0].headers["X-Bigmodel-Authorization"]).toBe("Bearer business-one")
    expect(c.calls[0].headers["Bigmodel-Target-Type"]).toBe("TEAM")
    expect(c.calls[0].headers["Bigmodel-Organization"]).toBe("org-1")
    expect(c.calls[0].headers["Bigmodel-Project"]).toBe("project-2")
    expect(c.calls[0].timeout).toBe(20000)
    expect((await nativeLaneOperation(c, "highspeed.healthy", { card_id: "card-1" })).completion_tokens).toBe(5)
    expect(c.calls[1].headers).toMatchObject({ Authorization: "Bearer jwt-one" })
  }
  const mismatch = context({ request: () => reply({ code: 0, msg: "", data: { card: { card_id: "c", task_id: "other", provider: "builtin:zai-coding-plan", model: MODEL, issued_at: 1, expires_at: 2 }, next_draw_at: 3 } }) })
  await expect(nativeLaneOperation(mismatch, "highspeed.draw", { task_id: "task-1", model: MODEL })).rejects.toMatchObject({ kind: "malformed" })
  const broken = context({ request: () => reply({ code: 3402, msg: "expired" }) })
  await expect(nativeLaneOperation(broken, "highspeed.draw", { task_id: "task-1", model: MODEL })).rejects.toMatchObject({ kind: "business", code: 3402, httpStatus: 200 })
  const denied = context({ request: () => reply({ code: 3402, msg: "expired", data: { card: null, next_draw_at: 12345 } }) })
  await expect(nativeLaneOperation(denied, "highspeed.draw", { task_id: "task-1", model: MODEL })).rejects.toMatchObject({ code: 3402, nextDrawAt: 12345 })
  const unsupported = context()
  await expect(nativeLaneOperation(unsupported, "highspeed.draw", { task_id: "task-1", model: "GLM-4.5" })).rejects.toMatchObject({ kind: "invalid", dispatched: false })
  expect(unsupported.calls).toHaveLength(0)
  for (const kind of ["start", "legacy-key"]) {
    await expect(nativeLaneOperation(context({ kind }), "highspeed.draw", { task_id: "task-1", model: MODEL })).rejects.toMatchObject({ kind: "unavailable" })
  }
})

test("public lane results project declared fields and preserve source-declared marketing args", async () => {
  const secret = "vendor-injected-credential"
  const c = context({ request: ({ url }) => {
    const credential = { access_token: secret }
    if (url.pathname.endsWith("/preview")) return reply({ code: 0, data: { ...credential, plans: [{ plan_id: "gift", ...credential, entitlements: [{ entitlement_id: "ent", ...credential }] }] } })
    if (url.pathname.endsWith("/claim")) return reply({ code: 0, data: { ...credential, plan: { plan_id: "gift", status: "active", ...credential, entitlements: [{ entitlement_id: "ent", ...credential }] } } })
    if (url.pathname.endsWith("/marketing/touch")) {
      const feed = structuredClone(realFeed)
      feed.data.deliveries[0].access_token = secret
      feed.data.deliveries[0].banner.background.args.presentation = { theme: "seasonal", values: [1, { label: "campaign" }] }
      return reply(feed)
    }
    if (url.pathname.endsWith("/ticket")) return reply({ ticket_id: "ticket-1", state: "ready", ...credential })
    if (url.pathname.endsWith("/status")) return reply({ tickets: [{ ticket_id: "ticket-1", state: "ready", ...credential }], ...credential })
    if (url.pathname.endsWith("/draw")) return reply({ code: 0, msg: "", data: { ...credential, next_draw_at: 10, card: { card_id: "card-1", task_id: "task-1", provider: "builtin:zai-coding-plan", model: MODEL, issued_at: 1, expires_at: 2, ...credential } } })
    return reply({ code: 0, msg: "", data: { card_id: "card-1", prompt_tokens: 1, completion_tokens: 2, duration_seconds: 3, ...credential } })
  } })
  const results = [
    await nativeLaneOperation(c, "billing.preview", {}),
    await nativeLaneOperation(c, "billing.claim.raw", { plan_id: "gift" }),
    await nativeLaneOperation(c, "marketing.feed", { locale: "zh-CN" }),
    await nativeLaneOperation(c, "offpeak.ticket", { task_id: "task-1" }),
    await nativeLaneOperation(c, "offpeak.status", { ticket_ids: ["ticket-1"] }),
    await nativeLaneOperation(c, "highspeed.draw", { task_id: "task-1", model: MODEL }),
    await nativeLaneOperation(c, "highspeed.healthy", { card_id: "card-1" }),
  ]
  expect(results[0].plans[0].entitlements[0].entitlement_id).toBe("ent")
  expect(results[1].plan.plan_id).toBe("gift")
  expect(results[2].deliveries[0].banner.background.args.zcode_plan.plan_id).toBe("zcode-v3-start-plan-1030-wk")
  expect(results[2].deliveries[0].banner.background.args.presentation).toEqual({ theme: "seasonal", values: [1, { label: "campaign" }] })
  expect(results[3].ticket_id).toBe("ticket-1")
  expect(results[5].card.card_id).toBe("card-1")
  expect(JSON.stringify(results)).not.toContain(secret)
})

test("official MCP trust precedes credential access and team families send different identities", async () => {
  const origins = { zcode: ORIGIN, mcpDev: ["http://127.0.0.1:4321"] }
  expect(trustedOfficialMcpURL("https://ZCODE.z.ai:443/mcp?q=1", origins)).toBe(true)
  expect(trustedOfficialMcpURL("http://127.0.0.1:4321/mcp", origins)).toBe(true)
  for (const url of ["https://zcode.z.ai.evil.test/mcp", "https://user@zcode.z.ai/mcp", "http://zcode.z.ai/mcp", "http://localhost:4321/mcp", "https://zcode.z.ai:444/mcp"]) expect(trustedOfficialMcpURL(url, origins)).toBe(false)
  for (const [site, kind] of [["zai", "individual"], ["bigmodel", "individual"], ["zai", "team"], ["bigmodel", "team"], ["zai", "start"]]) {
    const c = context({ site, kind })
    let reads = 0
    const login = c.loginMaterial
    c.loginMaterial = async (...args) => { reads++; return login(...args) }
    await expect(nativeLaneOperation(c, "mcp.headers", { url: "https://bad.test/mcp" })).rejects.toMatchObject({ kind: "untrusted_url" })
    expect(reads).toBe(0)
    const { headers } = await nativeLaneOperation(c, "mcp.headers", { url: `${ORIGIN}/api/v1/mcp` })
    expect(headers.Authorization).toBe("Bearer jwt-one")
    expect(headers["X-Bigmodel-Authorization"]).toBe(kind === "start" ? undefined : "Bearer business-one")
    expect(headers["Bigmodel-Target-Type"]).toBe(kind === "start" || site === "zai" && kind === "team" ? undefined : kind === "team" ? "TEAM" : "PERSONAL")
    expect(headers["Bigmodel-Organization"]).toBe(site === "bigmodel" && kind === "team" ? "org-1" : undefined)
    c.setJwt("renewed")
    expect((await nativeLaneOperation(c, "mcp.headers", { stdio: true })).headers.Authorization).toBe("Bearer renewed")
    await expect(nativeLaneOperation(c, "mcp.headers", { stdio: true, url: "https://bad.test" })).rejects.toMatchObject({ kind: "untrusted_url" })
  }
  const incomplete = context(); incomplete.setBusiness("")
  await expect(nativeLaneOperation(incomplete, "mcp.headers", { url: `${ORIGIN}/mcp` })).rejects.toMatchObject({ kind: "unavailable" })
})

test("ultra static routing changes only exact endpoints and clears Host on a routed request", () => {
  const headers = new Headers({ Host: "open.bigmodel.cn", Authorization: "Bearer paid" })
  const route = routeCodingPlanGateway("https://OPEN.bigmodel.cn:443/api/anthropic/v1/messages/?x=1", headers, { enabled: true, zcodeOrigin: ORIGIN })
  expect(route.url).toBe(`${ORIGIN}/api/v1/ultra/anthropic/v1/messages?x=1`)
  expect(route.headers.get("host")).toBeNull()
  expect(route.headers.get("authorization")).toBe("Bearer paid")
  expect(routeCodingPlanGateway("https://api.z.ai/api/anthropic/v1/messages?q=1", headers, { enabled: true, zcodeOrigin: ORIGIN }).url).toBe(`${ORIGIN}/api/v1/ultra-zai/anthropic/v1/messages?q=1`)
  for (const input of ["https://api.z.ai:444/api/anthropic/v1/messages", "https://api.z.ai/api/anthropic/v1/messages/count_tokens", "http://api.z.ai/api/anthropic/v1/messages", "https://evil.api.z.ai/api/anthropic/v1/messages"]) expect(routeCodingPlanGateway(input, headers, { enabled: true, zcodeOrigin: ORIGIN }).url).toBe(input)
})

test("Off-Peak retries queue responses with the same ticket and a freshly resolved project PAT", async () => {
  const c = context({ site: "bigmodel", kind: "team" }), calls = []
  const input = options("offpeak", async (url, init) => {
    calls.push({ url: new URL(url), init })
    return calls.length === 1 ? Response.json({ code: 3105, msg: "queued" }, { status: 429, headers: { "Retry-After": "0" } }) : new Response("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"ok\"}}\n\n", { headers: { "Content-Type": "text/event-stream" } })
  })
  const response = await runNativeLane(c, input)
  expect(await response.text()).toContain('"text":"ok"')
  expect(calls).toHaveLength(2)
  expect(calls[0].url.href).toBe(`${ORIGIN}/api/v1/off-peak/anthropic/v1/messages?beta=1`)
  expect(calls.map((x) => x.init.headers.get("x-coding-plan-api-key"))).toEqual(["pat-1", "pat-2"])
  for (const call of calls) {
    expect(call.init.headers.get("authorization")).toBe("Bearer jwt-one")
    expect(call.init.headers.get("x-api-key")).toBe("jwt-one")
    expect(call.init.headers.get("x-off-peak-ticket-id")).toBe("ticket-1")
    expect(call.init.headers.get("bigmodel-organization")).toBe("org-1")
    expect(call.init.headers.get("bigmodel-project")).toBe("project-2")
    expect(JSON.parse(call.init.body).output_config.effort).toBe("high")
  }
})

test("expired Off-Peak tickets never retake or fall back, including SSE errors before output", async () => {
  for (const code of [3102, 3106]) {
    let sends = 0, fallbacks = 0
    const input = options("offpeak", async () => { sends++; return new Response(`event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":${code}}}\n\n`, { headers: { "content-type": "text/event-stream" } }) }, { fallback: async () => { fallbacks++; return Response.json({}) } })
    const res = await runNativeLane(context(), input)
    expect(res.status).toBe(400)
    expect(res.headers.get("x-magpie-offpeak-expired")).toBe(String(code))
    expect((await res.json()).error.message).toContain("off-peak-ticket-expired")
    expect([sends, fallbacks]).toEqual([1, 0])
  }
})

test("Off-Peak recognizes a queued 200 JSON envelope before output without retaking", async () => {
  const ctx = context()
  let sends = 0
  const input = options("offpeak", async () => ++sends === 1
    ? Response.json({ code: 3105, msg: "queued" }, { headers: { "Retry-After": "0" } })
    : Response.json({ type: "message", content: [{ type: "text", text: "done" }] }))
  const response = await runNativeLane(ctx, input)
  expect((await response.json()).content[0].text).toBe("done")
  expect(sends).toBe(2)
  expect(ctx.pats).toBe(2)
  expect(ctx.calls).toHaveLength(0)
})

test("highspeed falls back once on pre-output card errors, but never on context overflow or accepted output", async () => {
  const c = context({ site: "zai", kind: "team" })
  let fallbacks = 0, sent
  const fallback = async () => { fallbacks++; return Response.json({ fallback: true }) }
  const fail = options("highspeed", async (url, init) => {
    sent = { url, init }
    return Response.json({ type: "error", error: { code: 3402, message: "expired" } }, { status: 400 })
  }, { fallback })
  expect(await (await runNativeLane(c, fail)).json()).toEqual({ fallback: true })
  expect(fallbacks).toBe(1)
  expect(sent.url).toBe(`${ORIGIN}/api/v1/highspeed/anthropic/v1/messages?beta=1`)
  expect(sent.init.headers.get("x-highspeed-card-id")).toBe("card-1")
  expect(sent.init.headers.get("x-bigmodel-authorization")).toBe("Bearer business-one")
  expect(sent.init.headers.get("bigmodel-target-type")).toBe("TEAM")
  expect(JSON.parse(sent.init.body).model).toBe(MODEL)
  const overflow = options("highspeed", async () => Response.json({ type: "error", error: { code: "context_length_exceeded", message: "prompt is too long" } }, { status: 400 }), { fallback })
  expect((await runNativeLane(c, overflow)).status).toBe(400)
  const stream = 'event: content_block_start\ndata: {"type":"content_block_start","content_block":{"type":"tool_use","id":"t1"}}\n\nevent: error\ndata: {"type":"error","error":{"code":3402}}\n\n'
  const partial = options("highspeed", async () => new Response(stream, { headers: { "content-type": "text/event-stream" } }), { fallback })
  expect(await (await runNativeLane(c, partial)).text()).toBe(stream)
  expect(fallbacks).toBe(1)
  const started = 'event: message_start\ndata: {"type":"message_start","message":{"content":[]}}\n\nevent: error\ndata: {"type":"error","error":{"code":3402}}\n\n'
  const afterStart = options("highspeed", async () => new Response(started, { headers: { "content-type": "text/event-stream" } }), { fallback })
  expect(await (await runNativeLane(c, afterStart)).text()).toBe(started)
  expect(fallbacks).toBe(1)
  const beforeOutput = options("highspeed", async () => new Response('event: ping\ndata: {"type":"ping"}\n\nevent: error\ndata: {"type":"error","error":{"code":3402}}\n\n', { headers: { "content-type": "text/event-stream" } }), { fallback })
  expect(await (await runNativeLane(c, beforeOutput)).json()).toEqual({ fallback: true })
  expect(fallbacks).toBe(2)
  const mixed = options("highspeed", async () => Response.json({ type: "error", error: { code: 3402 }, content: [{ type: "tool_use", id: "already-created" }] }, { status: 400 }), { fallback })
  expect((await runNativeLane(c, mixed)).status).toBe(400)
  expect(fallbacks).toBe(2)
})

test("execution rejects mismatched task, epoch, model and cancellation before upstream", async () => {
  for (const update of [{ taskId: "other" }, { generation: "old" }, { scope: "other" }, { rawModel: "GLM-5.3" }]) {
    let calls = 0
    const input = options("highspeed", async () => { calls++; return Response.json({}) }, { execution: { ...binding("highspeed"), ...update } })
    await expect(runNativeLane(context(), input)).rejects.toMatchObject({ kind: "invalid" })
    expect(calls).toBe(0)
  }
  let calls = 0
  const mismatchedBody = options("highspeed", async () => { calls++; return Response.json({}) }, { init: { body: JSON.stringify({ model: "GLM-5.3", messages: [] }) } })
  await expect(runNativeLane(context(), mismatchedBody)).rejects.toMatchObject({ kind: "invalid" })
  expect(calls).toBe(0)
  const c = context(), ctl = new AbortController()
  ctl.abort(new DOMException("cancelled", "AbortError"))
  let fallback = 0
  const input = options("highspeed", async () => { throw new Error("must not send") }, { init: { signal: ctl.signal }, fallback: async () => { fallback++; return Response.json({}) } })
  await expect(runNativeLane(c, input)).rejects.toThrow()
  expect(fallback).toBe(0)
})

test("an account change during PAT resolution or a context-exceeded exception cannot replay on paid fallback", async () => {
  const switched = context()
  let checks = 0, sends = 0, fallbacks = 0
  switched.assertCurrent = async () => {
    if (++checks >= 2) throw Object.assign(new Error("account_scope_changed"), { kind: "account_scope_changed" })
  }
  const fail = async () => { fallbacks++; return Response.json({}) }
  await expect(runNativeLane(switched, options("offpeak", async () => { sends++; return Response.json({}) }, { fallback: fail }))).rejects.toMatchObject({ kind: "account_scope_changed" })
  expect([sends, fallbacks]).toEqual([0, 0])
  const input = options("highspeed", async () => {
    throw Object.assign(new Error("request rejected"), { code: "context_length_exceeded" })
  }, { fallback: fail })
  await expect(runNativeLane(context(), input)).rejects.toMatchObject({ code: "context_length_exceeded" })
  expect(fallbacks).toBe(0)
})

test("highspeed pre-acceptance network failure and local card expiry fall back once without drawing again", async () => {
  let sends = 0, fallbacks = 0
  const fallback = async () => { fallbacks++; return Response.json({ route: "ordinary" }) }
  const broken = options("highspeed", async () => { sends++; throw new Error("connection reset") }, { fallback })
  expect((await (await runNativeLane(context(), broken)).json()).route).toBe("ordinary")
  expect([sends, fallbacks]).toEqual([1, 1])
  const expired = options("highspeed", async () => { sends++; return Response.json({}) }, { execution: { ...binding("highspeed"), expiresAt: Date.now() - 1 }, fallback })
  expect((await (await runNativeLane(context(), expired)).json()).route).toBe("ordinary")
  expect([sends, fallbacks]).toEqual([1, 2])
  const overflow = options("highspeed", async () => new Response("input token count exceeds maximum number of tokens allowed", { status: 400 }), { fallback })
  expect((await runNativeLane(context(), overflow)).status).toBe(400)
  expect(fallbacks).toBe(2)
})

test("an ordinary-selection fallback refusal propagates without replaying the native request", async () => {
  let sends = 0, fallbacks = 0
  const input = options("highspeed", async () => {
    sends++
    return Response.json({ type: "error", error: { code: 3402 } }, { status: 400 })
  }, { fallback: async () => {
    fallbacks++
    throw new Error("ordinary selection is unavailable")
  } })
  await expect(runNativeLane(context(), input)).rejects.toThrow("ordinary selection is unavailable")
  expect([sends, fallbacks]).toEqual([1, 1])
})

test("cancelling an Off-Peak queue wait ends it without another send or paid fallback", async () => {
  const controller = new AbortController()
  let sends = 0, fallbacks = 0
  const input = options("offpeak", async () => {
    sends++
    setTimeout(() => controller.abort(new DOMException("cancelled", "AbortError")), 0)
    return Response.json({ code: 3105, msg: "queued" }, { status: 429, headers: { "Retry-After": "120" } })
  }, { init: { method: "POST", body: JSON.stringify({ model: MODEL, messages: [] }), signal: controller.signal }, fallback: async () => { fallbacks++; return Response.json({}) } })
  await expect(runNativeLane(context(), input)).rejects.toThrow()
  expect([sends, fallbacks]).toEqual([1, 0])
})
