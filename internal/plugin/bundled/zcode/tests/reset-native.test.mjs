import { afterAll, beforeAll, beforeEach, expect, test } from "bun:test"
import { createServer } from "node:http"
import { accountScope, createAccountRuntime } from "../account.mjs"
import { resetOperation } from "../reset.mjs"

let server, origin, route, calls
const send = (res, data, status = 200, headers = {}) => {
  res.writeHead(status, { "content-type": "application/json", ...headers })
  res.end(data === null ? "" : JSON.stringify(data))
}
const status = {
  available_five_hour_resets: [{ expire_at: 1793000000123 }, { expire_at: 1794000000456 }],
  available_week_resets: [{ expire_at: 1795000000789 }],
  latest_five_hour_reset_history: { used_at: 1792000000123 },
  latest_week_reset_history: null,
  has_unread_history: true,
}
beforeAll(async () => {
  server = createServer((req, res) => {
    let body = ""
    req.setEncoding("utf8")
    req.on("data", (part) => { body += part })
    req.on("end", () => {
      const call = { url: new URL(req.url, origin), method: req.method, headers: req.headers, body }
      calls.push(call)
      route(call, res)
    })
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  origin = `http://127.0.0.1:${server.address().port}`
})
beforeEach(() => { calls = []; route = (_call, res) => send(res, {}, 404) })
afterAll(async () => { server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)) })

async function context(kind, site, signal, overrides = {}, slot = null) {
  const state = { site, kind, uid: "user-1", zcodeUserId: "user-1", base: `${origin}/api/anthropic`, business: "business-jwt", jwt: "zcode-jwt",
    ...(kind === "team" ? { org: "org-1", project: "project-1", product: "product-1" } : {}), ...overrides }
  const origins = { business: origin, zcode: origin, zaiOAuth: "https://chat.z.ai" }
  let auth = { type: "oauth", refresh: JSON.stringify(state), accountEpoch: "epoch-1", accountScope: accountScope({ ...state, origins }) }
  if (slot) slot.changeEpoch = () => { auth = { ...auth, accountEpoch: "epoch-2" } }
  const runtime = createAccountRuntime({ providerId: "zcode", stateOf: (value) => JSON.parse(value.refresh), origins: { [site]: origins }, claimVerification: async () => null })
  return runtime.capture(async () => auth, { signal })
}

test("four reset operations use raw same-family JWT and exact per-endpoint scopes", async () => {
  route = (call, res) => {
    switch (call.url.pathname) {
      case "/api/v1/coding-plan/reset/status": return send(res, { code: 0, data: status })
      case "/api/v1/coding-plan/reset/opportunity": return send(res, { code: 0, data: { granted: true } })
      case "/api/v1/coding-plan/reset/use": return send(res, { code: 0, data: { used: true } })
      case "/api/v1/coding-plan/reset/history/read": return send(res, { code: 0 })
      default: return send(res, {}, 404)
    }
  }
  for (const site of ["zai", "bigmodel"]) {
    for (const kind of ["individual", "team"]) {
      const ctx = await context(kind, site)
      const snapshot = await resetOperation(ctx, "reset.status")
      expect(snapshot.available_five_hour_resets.map((item) => item.expire_at)).toEqual([1793000000123, 1794000000456])
      expect(snapshot.available_week_resets[0].expire_at).toBe(1795000000789)
      expect(snapshot.latest_five_hour_reset_history.used_at).toBe(1792000000123)
      expect(snapshot.latest_week_reset_history).toBeNull()
      expect(snapshot.has_unread_history).toBe(true)
      expect(await resetOperation(ctx, "reset.opportunity", { idempotency_key: "  opportunity-key  " })).toEqual({ granted: true })
      expect(await resetOperation(ctx, "reset.use", { idempotency_key: "  use-key  ", reset_type: "WEEK" })).toEqual({ used: true })
      expect(await resetOperation(ctx, "reset.history.read")).toBeNull()
      const four = calls.slice(-4)
      expect(four.map((call) => call.method)).toEqual(["GET", "POST", "POST", "POST"])
      expect(four.map((call) => call.url.pathname)).toEqual(["/api/v1/coding-plan/reset/status", "/api/v1/coding-plan/reset/opportunity", "/api/v1/coding-plan/reset/use", "/api/v1/coding-plan/reset/history/read"])
      for (const call of four) {
        expect(call.headers.authorization).toBe("Bearer zcode-jwt")
        expect(call.headers["x-bigmodel-authorization"]).toBe("business-jwt")
      }
      for (const call of four.slice(0, 3)) {
        expect(call.headers["bigmodel-target-type"]).toBe(kind === "team" ? "TEAM" : "PERSONAL")
        expect(call.headers["bigmodel-organization"]).toBe(kind === "team" ? "org-1" : undefined)
        expect(call.headers["bigmodel-project"]).toBe(kind === "team" ? "project-1" : undefined)
      }
      expect(four[3].headers["bigmodel-target-type"]).toBeUndefined()
      expect(four[3].headers["bigmodel-organization"]).toBeUndefined()
      expect(four[3].headers["bigmodel-project"]).toBeUndefined()
      expect(four[3].body).toBe("")
      expect(JSON.parse(four[1].body)).toEqual({ idempotency_key: "opportunity-key" })
      expect(JSON.parse(four[2].body)).toEqual({ idempotency_key: "use-key", reset_type: "WEEK" })
    }
  }
})

test("3301 denial is accepted before HTTP status only with complete millisecond data", async () => {
  let response = { code: 3301, data: { granted: false, next_try_at: 1793000000123 } }
  route = (_call, res) => send(res, response, 409)
  const ctx = await context("individual", "zai")
  expect(await resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).toEqual({ granted: false, next_try_at: 1793000000123 })
  for (const data of [{ next_try_at: 1793000000123 }, { granted: false }, { granted: false, next_try_at: 1.5 }, { granted: true, next_try_at: 1 }]) {
    response = { code: 3301, data }
    await expect(resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).rejects.toMatchObject({ kind: "invalid_response", code: 3301, dispatched: true })
  }
  await expect(resetOperation(ctx, "reset.status")).rejects.toMatchObject({ kind: "business", code: 3301 })
})

test("business, HTTP and malformed response precedence yields safe typed errors", async () => {
  let reply = { code: 2007, msg: "secret-token", data: {} }, http = 503
  route = (_call, res) => send(res, reply, http, { "x-request-id": "request-123" })
  const ctx = await context("individual", "bigmodel")
  let error
  try { await resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" }) } catch (caught) { error = caught }
  expect(error).toMatchObject({ kind: "business", code: 2007, httpStatus: 503, retryable: true, throttled: false, dispatched: true, requestId: "request-123" })
  expect(error.message).not.toContain("secret-token")
  reply = { code: 0, data: { granted: true } }; http = 500
  await expect(resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).rejects.toMatchObject({ kind: "http", httpStatus: 500, dispatched: true })
  reply = null; http = 429
  await expect(resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).rejects.toMatchObject({ kind: "throttled", throttled: true, dispatched: true })
  reply = { code: 429 }; http = 429
  await expect(resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).rejects.toMatchObject({ kind: "business", code: 429, throttled: true })
  reply = { code: 3101 }; http = 400
  await expect(resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).rejects.toMatchObject({ kind: "business", code: 3101, dispatched: true })
  reply = { code: 0, data: { granted: false, next_try_at: 1793000000123 } }; http = 200
  await expect(resetOperation(ctx, "reset.opportunity", { idempotency_key: "key" })).rejects.toMatchObject({ kind: "invalid_response" })
  reply = { code: 0, data: { used: false } }
  await expect(resetOperation(ctx, "reset.use", { idempotency_key: "key", reset_type: "FIVE_HOUR" })).rejects.toMatchObject({ kind: "invalid_response" })
})

test("invalid keys, types, status shapes and unavailable credentials cannot spend", async () => {
  route = (_call, res) => send(res, { code: 0, data: { ...status, latest_week_reset_history: { used_at: 0 } } })
  const ctx = await context("team", "zai")
  for (const key of ["", "   ", "x".repeat(65)]) {
    await expect(resetOperation(ctx, "reset.use", { idempotency_key: key, reset_type: "WEEK" })).rejects.toMatchObject({ kind: "invalid_idempotency_key", dispatched: false })
  }
  await expect(resetOperation(ctx, "reset.use", { idempotency_key: "key", reset_type: "MONTH" })).rejects.toMatchObject({ kind: "invalid_reset_type", dispatched: false })
  expect(calls).toHaveLength(0)
  await expect(resetOperation(ctx, "reset.status")).rejects.toMatchObject({ kind: "invalid_response", dispatched: true })
  const withoutBusiness = await context("individual", "bigmodel", undefined, { business: "" })
  await expect(resetOperation(withoutBusiness, "reset.status")).rejects.toMatchObject({ kind: "unavailable", dispatched: false })
  expect(calls).toHaveLength(1)
})

test("the sixty-four-character idempotency boundary is trimmed and accepted", async () => {
  route = (_call, res) => send(res, { code: 0, data: { granted: true } })
  const key = "x".repeat(64)
  expect(await resetOperation(await context("individual", "zai"), "reset.opportunity", { idempotency_key: `  ${key}  ` })).toEqual({ granted: true })
  expect(JSON.parse(calls[0].body).idempotency_key).toBe(key)
})

test("a stalled response body respects the request deadline and leaves outcome unknown", async () => {
  route = (_call, res) => { res.writeHead(200, { "content-type": "application/json" }); res.write('{"code":0,"data":') }
  const ctx = await context("individual", "zai")
  const timed = { ...ctx, request: (url, init, options) => {
    expect(options.timeout).toBe(15_000)
    return ctx.request(url, init, { timeout: 100 })
  } }
  await expect(resetOperation(timed, "reset.use", { idempotency_key: "key", reset_type: "FIVE_HOUR" }))
    .rejects.toMatchObject({ kind: "timeout", retryable: true, dispatched: true })
  expect(calls).toHaveLength(1)
}, 5000)

test("a selected login changing during a dispatched use cannot be reported accepted", async () => {
  let dispatch, release
  const started = new Promise((resolve) => { dispatch = resolve })
  route = (_call, res) => { release = () => send(res, { code: 0, data: { used: true } }); dispatch() }
  const slot = {}
  const ctx = await context("individual", "zai", undefined, {}, slot)
  const operation = resetOperation(ctx, "reset.use", { idempotency_key: "key", reset_type: "WEEK" })
  await started
  slot.changeEpoch()
  release()
  await expect(operation).rejects.toMatchObject({ kind: "scope_changed", dispatched: true })
  expect(calls).toHaveLength(1)
})
