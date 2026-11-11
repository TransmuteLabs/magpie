import { afterEach, expect, test } from "bun:test"
import { ZCodeAuthPlugin } from "../index.mjs"

const originalFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = originalFetch })
const ok = (data, code = 0) => Response.json({ code, data })
const init = () => ({ flow_id: "flow", authorize_url: "https://login.example/authorize?state=expected-state", expires_at: Date.now() / 1000 + 300, poll_interval_sec: 1 })
function fixture(site, { initData = init(), pollData, tokenData, profileData, initCode = 0, pollStatuses = [], customerUnavailable = false } = {}) {
  const calls = []
  globalThis.fetch = async (input, options = {}) => {
    const u = new URL(String(input)), payload = options.body ? JSON.parse(options.body) : null
    calls.push({ url: u.href, method: options.method ?? "GET", payload, authorization: new Headers(options.headers).get("authorization") })
    if (u.pathname === "/api/v1/oauth/cli/init") return ok(initData, initCode)
    if (u.pathname === "/api/v1/oauth/cli/poll/flow") {
      if (pollStatuses.length) return new Response("{}", { status: pollStatuses.shift() })
      return ok(pollData ?? { status: "ready", token: "zcode", user: { user_id: "u1", name: "Ada" }, [site]: { access_token: "oauth", ...(site === "bigmodel" ? { refresh_token: "refresh" } : {}) } })
    }
    if (u.pathname === "/api/v1/oauth/token") return ok(tokenData ?? { token: "zcode", user: { user_id: "u1", name: "Ada" }, [site]: { access_token: "oauth" } })
    if (u.pathname === "/api/v1/oauth/profile") return ok(profileData ?? { user: { user_id: "u1", name: "Ada" } })
    if (u.pathname === "/api/auth/z/login") return ok({ accessToken: "business" })
    if (u.pathname.endsWith("/billing/balance")) return ok({ plans: [], balances: [] })
    if (u.pathname.endsWith("/getCustomerInfo")) return customerUnavailable ? new Response("unavailable", { status: 503 }) : ok({ organizations: [{ organizationId: "o", projects: [{ projectId: "p", projectType: 1 }] }] })
    if (u.pathname.endsWith("/api_keys")) return ok([{ apiKey: "key", name: "zcode-api-key" }])
    if (u.pathname.endsWith("/access_tokens")) return ok({ accessToken: "pat", tokenType: "Bearer", expiresIn: 600, expiresAt: Date.now() / 1000 + 600 })
    if (u.pathname.endsWith("/subscription/list")) return ok([{ productName: "GLM Coding Pro", status: "VALID", inCurrentPeriod: true }])
    throw new Error(`unexpected offline request ${u.pathname}`)
  }
  return calls
}
async function method(label) {
  const hooks = await ZCodeAuthPlugin({ client: {} })
  return hooks.auth.methods.find((m) => m.label === label)
}

test("CLI polling retains distinct OAuth materials for both families", async () => {
  for (const [site, label] of [["zai", "ZCode: Z.ai GLM Coding Plan"], ["bigmodel", "ZCode: BigModel (智谱) GLM Coding Plan"]]) {
    const calls = fixture(site)
    const flow = await (await method(label)).authorize()
    const [a, b] = await Promise.all([flow.callback(), flow.callback()])
    expect(a).toEqual(b)
    expect(a.type).toBe("success")
    expect(a.uid).toBe("u1")
    expect(a.accountScope).toBeTruthy()
    const state = JSON.parse(a.refresh)
    expect(state).toMatchObject({ site, kind: "individual", uid: "u1", zcodeUserId: "u1", jwt: "zcode",
      ...(site === "zai" ? { oauthToken: "oauth" } : { business: "oauth" }) })
    if (site === "bigmodel") expect(state.refreshToken).toBe("refresh")
    expect(calls.filter((c) => c.url.includes("/oauth/cli/poll/")).length).toBe(1)
    expect(calls.some((c) => c.url.includes("/copy/"))).toBe(false)
  }
})

test("polling rejects invalid init timing, state, code and incomplete ready responses", async () => {
  for (const change of [{ poll_interval_sec: 0 }, { expires_at: Date.now() / 1000 - 1 }, { authorize_url: "https://login.example/no-state" }]) {
    fixture("zai", { initData: { ...init(), ...change } })
    await expect((await method("ZCode: Z.ai GLM Coding Plan")).authorize()).rejects.toThrow("invalid flow")
  }
  fixture("zai", { initCode: 200 })
  await expect((await method("ZCode: Z.ai GLM Coding Plan")).authorize()).rejects.toThrow("invalid flow")
  fixture("zai", { pollData: { status: "ready", token: "zcode", zai: { access_token: "oauth" } } })
  const flow = await (await method("ZCode: Z.ai GLM Coding Plan")).authorize()
  expect((await flow.callback()).type).toBe("failed")
})

test("polling retries a transient HTTP refusal but terminates on a non-retryable one", async () => {
  let calls = fixture("bigmodel", { pollStatuses: [429] })
  let flow = await (await method("ZCode: BigModel (智谱) GLM Coding Plan")).authorize()
  expect((await flow.callback()).type).toBe("success")
  expect(calls.filter((c) => c.url.includes("/oauth/cli/poll/"))).toHaveLength(2)
  calls = fixture("bigmodel", { pollStatuses: [401] })
  flow = await (await method("ZCode: BigModel (智谱) GLM Coding Plan")).authorize()
  expect((await flow.callback()).type).toBe("failed")
  expect(calls.filter((c) => c.url.includes("/oauth/cli/poll/"))).toHaveLength(1)
})

test("a temporary Team-discovery outage does not discard a completed personal login", async () => {
  fixture("bigmodel", { customerUnavailable: true })
  const flow = await (await method("ZCode: BigModel (智谱) GLM Coding Plan")).authorize()
  const result = await flow.callback()
  expect(result.type).toBe("success")
  expect(JSON.parse(result.refresh)).toMatchObject({ kind: "individual", uid: "u1", business: "oauth", jwt: "zcode" })
})

test("a full callback is state checked and exchanged once at the configured redirect", async () => {
  const calls = fixture("bigmodel")
  const flow = await (await method("ZCode: BigModel paste callback URL")).authorize()
  const authorize = new URL(flow.url)
  const redirect = authorize.searchParams.get("redirect")
  const callback = new URL(redirect)
  callback.searchParams.set("state", authorize.searchParams.get("state"))
  callback.searchParams.set("authCode", "one-time-code")
  const [a, b] = await Promise.all([flow.callback(callback.href), flow.callback(callback.href)])
  expect(a.type).toBe("success")
  expect(b).toEqual(a)
  const exchanges = calls.filter((c) => c.url.endsWith("/oauth/token"))
  expect(exchanges).toHaveLength(1)
  expect(exchanges[0].payload).toEqual({ provider: "bigmodel", code: "one-time-code", redirect_uri: redirect, state: authorize.searchParams.get("state") })
  expect(calls.some((c) => c.url.includes("tokenByAuthCode"))).toBe(false)
})

test("a callback without a session user obtains it from the ZCode JWT profile", async () => {
  const calls = fixture("bigmodel", { tokenData: { token: "zcode", bigmodel: { access_token: "oauth" } },
    profileData: { user: { user_id: "session-user", name: "Ada" } } })
  const flow = await (await method("ZCode: BigModel paste callback URL")).authorize()
  const authorize = new URL(flow.url), callback = new URL(authorize.searchParams.get("redirect"))
  callback.searchParams.set("state", authorize.searchParams.get("state"))
  callback.searchParams.set("authCode", "code")
  const result = await flow.callback(callback.href)
  expect(result.type).toBe("success")
  expect(JSON.parse(result.refresh).zcodeUserId).toBe("session-user")
  const profile = calls.find((call) => call.url.endsWith("/api/v1/oauth/profile"))
  expect(profile.authorization).toBe("Bearer zcode")
})

test("pasted callback rejects a different origin or state before exchange", async () => {
  const calls = fixture("zai")
  const flow = await (await method("ZCode: Z.ai paste callback URL")).authorize()
  const callback = new URL("http://localhost/oauth/callback?code=c&state=wrong")
  expect((await flow.callback(callback.href)).type).toBe("failed")
  expect(calls.filter((c) => c.url.endsWith("/oauth/token"))).toHaveLength(0)
})
