import { afterEach, expect, test } from "bun:test"
import { createAccountRuntime } from "../account.mjs"
import { accountFixture } from "./account-fixtures.mjs"

const realFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = realFetch })
const base = { version: 1, kind: "individual", site: "zai", uid: "user-1", device: "11111111-2222-4333-8444-555555555555", base: "https://api.z.ai/api/anthropic", business: "biz", jwt: "session" }
const saved = (state) => ({ type: "oauth", uid: state.uid, access: state.jwt, refresh: JSON.stringify(state), expires: 0 })
const ok = (data) => Response.json({ code: 0, data })
const jwt = (expiresAt) => `e30.${Buffer.from(JSON.stringify({ exp: Math.floor(expiresAt / 1000) })).toString("base64url")}.sig`
function setup(state = base, clock = () => Date.now()) {
  const slot = accountFixture(saved(state))
  const runtime = createAccountRuntime({ client: slot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh), claimVerification: async () => false, now: clock, random: () => 0 })
  return { slot, runtime }
}
function vendor({ now, team = false, family = "zai", mint } = {}) {
  const requests = []
  let calls = 0
  globalThis.fetch = async (input, init = {}) => {
    const u = new URL(String(input)), headers = new Headers(init.headers)
    requests.push({ path: u.pathname, method: init.method ?? "GET", authorization: headers.get("authorization"), org: headers.get("bigmodel-organization"), project: headers.get("bigmodel-project"), body: init.body ? JSON.parse(init.body) : null })
    if (u.pathname.endsWith("/getCustomerInfo")) return ok({ organizations: [
      { organizationId: "other", projects: [{ projectId: "other", projectType: 1 }] },
      { organizationId: "org", organizationName: "默认机构", projects: [{ projectId: "project", projectName: "默认项目", projectType: team ? 2 : 1 }] },
    ] })
    if (u.pathname.endsWith("/api_keys") && (init.method ?? "GET") === "GET") return ok([])
    if (u.pathname.endsWith("/api_keys") && init.method === "POST") return ok({ apiKey: "key-id", name: team ? "zcode-team-api-key" : "zcode-api-key", ...(team ? { keyType: 2 } : {}) })
    if (u.pathname.endsWith("/access_tokens")) {
      calls++
      if (mint) return mint(calls)
      return ok({ enable: true, accessToken: `pat-${calls}`, tokenType: "Bearer", expiresIn: 600, expiresAt: (now() + 600_000) / 1000 })
    }
    throw new Error(`unexpected offline request ${u.pathname} (${family})`)
  }
  return { requests, calls: () => calls }
}

test("personal and Team discovery mint the official PAT without copying a secret", async () => {
  for (const [site, team] of [["zai", false], ["bigmodel", false], ["zai", true], ["bigmodel", true]]) {
    const clock = () => Date.now()
    const state = { ...base, site, ...(site === "bigmodel" ? { base: "https://open.bigmodel.cn/api/anthropic" } : {}), ...(team ? { kind: "team", org: "org", project: "project", product: "product" } : {}) }
    const fake = vendor({ now: clock, team, family: site })
    const { slot, runtime } = setup(state, clock)
    const ctx = await runtime.capture(slot.getAuth)
    const epoch = ctx.generation
    expect((await ctx.projectToken()).token).toBe("pat-1")
    expect((await slot.current()).accountEpoch).toBe(epoch)
    const location = JSON.parse((await slot.current()).refresh).loc
    expect(location).toMatchObject({ organizationId: "org", projectId: "project", apiKeyId: "key-id" })
    expect(JSON.stringify(await slot.current())).not.toContain("pat-1")
    expect(fake.requests.map((r) => r.path).some((p) => p.includes("/copy/"))).toBe(false)
    expect(fake.requests[0].authorization).toBe("biz")
    const listed = fake.requests.find((r) => r.path.endsWith("/api_keys") && r.method === "GET")
    expect(listed.authorization).toBe(team || site === "bigmodel" ? "biz" : "Bearer biz")
    if (team) expect([listed.org, listed.project]).toEqual(["org", "project"])
    const create = fake.requests.find((r) => r.path.endsWith("/api_keys") && r.method === "POST")
    expect(create.body).toEqual({ name: team ? "zcode-team-api-key" : "zcode-api-key", ...(team ? { keyType: 2 } : {}), usageScene: 1 })
    expect(fake.requests.at(-1)).toMatchObject({ method: "POST", authorization: "Bearer biz", body: { clientType: "zcode", clientVersion: expect.any(String) } })
  }
})

test("concurrent token requests coalesce and a late rejection cannot invalidate a newer PAT", async () => {
  let clock = 1_900_000_000_000
  const fake = vendor({ now: () => clock })
  const { slot, runtime } = setup(base, () => clock)
  const a = await runtime.capture(slot.getAuth), b = await runtime.capture(slot.getAuth)
  const [one, same] = await Promise.all([a.projectToken(), b.projectToken()])
  expect(one.token).toBe(same.token)
  expect(fake.calls()).toBe(1)
  const two = await b.projectToken({ rejectedFingerprint: one.fingerprint })
  expect(two.token).toBe("pat-2")
  expect((await a.projectToken({ rejectedFingerprint: one.fingerprint })).token).toBe("pat-2")
  expect(fake.calls()).toBe(2)
})

test("only transient refresh failures reuse an unexpired, unrejected PAT with backoff", async () => {
  let clock = 1_900_000_000_000
  let status = 503
  const fake = vendor({ now: () => clock, mint: (n) => n === 1 ? ok({ accessToken: "first", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 }) : new Response("{}", { status }) })
  const { slot, runtime } = setup(base, () => clock)
  const ctx = await runtime.capture(slot.getAuth)
  const first = await ctx.projectToken()
  clock += 500_000
  expect((await ctx.projectToken()).token).toBe(first.token)
  expect((await ctx.projectToken()).token).toBe(first.token)
  expect(fake.calls()).toBe(2)
  clock += 5_001
  status = 403
  await expect(ctx.projectToken()).rejects.toThrow("project_token_http_403")
})

test("404 invalidates only the saved location; the next call rediscovers it", async () => {
  let clock = 1_900_000_000_000
  const fake = vendor({ now: () => clock, mint: (n) => n === 1 ? new Response("{}", { status: 404 }) : ok({ accessToken: "new", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 }) })
  const location = { organizationId: "org", projectId: "project", apiKeyId: "deleted" }
  const { slot, runtime } = setup({ ...base, loc: location }, () => clock)
  const ctx = await runtime.capture(slot.getAuth)
  await expect(ctx.projectToken()).rejects.toMatchObject({ httpStatus: 404 })
  expect(JSON.parse((await slot.current()).refresh).loc).toBeUndefined()
  expect((await ctx.projectToken()).token).toBe("new")
  expect(fake.requests.some((r) => r.path.endsWith("/getCustomerInfo"))).toBe(true)
})

test("cancelling one waiter does not cancel a shared PAT issuance or publish to that waiter", async () => {
  let release, started
  const began = new Promise((resolve) => { started = resolve })
  const blocked = new Promise((resolve) => { release = resolve })
  const clock = () => Date.now()
  const fake = vendor({ now: clock, mint: async () => {
    started()
    await blocked
    return ok({ accessToken: "shared-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock() + 600_000) / 1000 })
  } })
  const { slot, runtime } = setup(base, clock)
  const first = await runtime.capture(slot.getAuth), second = await runtime.capture(slot.getAuth)
  const controller = new AbortController()
  const cancelled = first.projectToken({ signal: controller.signal }).then(() => "returned", (error) => error.name)
  await began
  const shared = second.projectToken()
  controller.abort()
  release()
  expect(await cancelled).toBe("AbortError")
  expect((await shared).token).toBe("shared-pat")
  expect(fake.calls()).toBe(1)
})

test("restart reuses the persisted location but never a persisted PAT", async () => {
  const clock = () => Date.now()
  const fake = vendor({ now: clock })
  const { slot, runtime } = setup(base, clock)
  await (await runtime.capture(slot.getAuth)).projectToken()
  const restarted = createAccountRuntime({ client: slot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: clock, random: () => 0 })
  const next = await (await restarted.capture(slot.getAuth)).projectToken()
  expect(next.token).toBe("pat-2")
  expect(fake.requests.filter((r) => r.path.endsWith("/getCustomerInfo"))).toHaveLength(1)
  expect(JSON.stringify(await slot.current())).not.toContain("pat-1")
  expect(JSON.stringify(await slot.current())).not.toContain("pat-2")
})

test("a cold runtime reuses scope-bound business material after bootstrap expiry", async () => {
  let clock = 1_900_000_000_000
  const business = jwt(clock + 1_200_000), oauth = jwt(clock + 5_000), expiredSession = jwt(clock - 60_000)
  const state = { ...base, business: undefined, oauthToken: oauth, jwt: expiredSession,
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }
  const requests = []
  globalThis.fetch = async (input, init = {}) => {
    const path = new URL(String(input)).pathname
    requests.push({ path, auth: new Headers(init.headers).get("authorization") })
    if (path === "/api/auth/z/login") return ok({ access_token: business, expires_in: 900 })
    if (path.endsWith("/access_tokens")) return ok({ accessToken: `pat-${requests.length}`, tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 })
    throw new Error(`unexpected offline request ${path}`)
  }
  const { slot, runtime } = setup(state, () => clock)
  const first = await runtime.capture(slot.getAuth)
  const scope = first.scope, generation = first.generation
  expect((await first.projectToken()).kind).toBe("project-token")
  const persisted = await slot.current()
  expect(JSON.parse(persisted.refresh)).toMatchObject({ oauthToken: oauth, business, businessExpiresAt: clock + 900_000 })
  expect(persisted.accountScope).toBe(scope)
  expect(persisted.accountEpoch).toBe(generation)
  expect(JSON.stringify(persisted)).not.toContain("pat-2")
  clock += 10_000
  const coldSlot = accountFixture(JSON.parse(JSON.stringify(persisted)))
  const cold = createAccountRuntime({ client: coldSlot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  const next = await cold.capture(coldSlot.getAuth)
  expect(next.scope).toBe(scope)
  expect(next.generation).toBe(generation)
  expect((await next.loginMaterial({ business: "required", zcode: "optional" })).zcode).toBe("")
  expect((await next.projectToken()).kind).toBe("project-token")
  expect(requests.filter((request) => request.path === "/api/auth/z/login")).toHaveLength(1)
  expect(requests.filter((request) => request.path.endsWith("/access_tokens")).map((request) => request.auth)).toEqual([`Bearer ${business}`, `Bearer ${business}`])
})

test("an expired bootstrap without business material requires relogin without vendor traffic", async () => {
  const clock = 1_900_000_000_000
  let requests = 0
  globalThis.fetch = async () => { requests++; throw new Error("unexpected offline request") }
  for (const material of [{}, { business: jwt(clock - 1_000) },
    { business: jwt(clock + 600_000), businessExpiresAt: clock - 1 }]) {
    const { slot, runtime } = setup({ ...base, business: undefined, oauthToken: jwt(clock - 60_000), ...material }, () => clock)
    const ctx = await runtime.capture(slot.getAuth)
    await expect(ctx.projectToken()).rejects.toMatchObject({ signIn: "expired", noModels: true, credentialState: "expired",
      message: expect.stringContaining("sign in") })
  }
  expect(requests).toBe(0)
})

test("another account's business material cannot repair an expired bootstrap", async () => {
  const clock = 1_900_000_000_000, business = jwt(clock + 600_000)
  let requests = 0
  globalThis.fetch = async (input) => {
    requests++
    expect(new URL(String(input)).pathname.endsWith("/access_tokens")).toBe(true)
    return ok({ accessToken: "other-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 })
  }
  const firstSlot = accountFixture(saved({ ...base, uid: "user-a", business: undefined, oauthToken: jwt(clock - 60_000) }))
  const otherSlot = accountFixture(saved({ ...base, uid: "user-b", business, oauthToken: jwt(clock - 60_000),
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }))
  const client = { auth: { set: (request) => (request.expected.uid === "user-a" ? firstSlot : otherSlot).client.auth.set(request) } }
  const runtime = createAccountRuntime({ client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  const other = await runtime.capture(otherSlot.getAuth)
  expect((await other.projectToken()).token).toBe("other-pat")
  const first = await runtime.capture(firstSlot.getAuth)
  expect(first.scope).not.toBe(other.scope)
  await expect(first.projectToken()).rejects.toMatchObject({ signIn: "expired", noModels: true })
  expect(requests).toBe(1)
})

test("business refresh falls back only for transient failure with known remaining lifetime", async () => {
  let clock = 1_900_000_000_000, status = 503, exchanges = 0
  const business = jwt(clock + 180_000), oauth = jwt(clock + 3_600_000)
  const { slot, runtime } = setup({ ...base, business, businessExpiresAt: clock + 180_000, oauthToken: oauth }, () => clock)
  globalThis.fetch = async (input) => {
    expect(new URL(String(input)).pathname).toBe("/api/auth/z/login")
    exchanges++
    return status === 200 ? Response.json({ code: 0, success: false, data: { access_token: "not-accepted" } }) : new Response("{}", { status })
  }
  const ctx = await runtime.capture(slot.getAuth)
  clock += 70_000
  expect((await ctx.loginMaterial({ business: "required", zcode: "none" })).business).toBe(business)
  expect((await ctx.loginMaterial({ business: "required", zcode: "none" })).business).toBe(business)
  expect(exchanges).toBe(1)
  clock += 5_001
  status = 401
  await expect(ctx.loginMaterial({ business: "required", zcode: "none" })).rejects.toMatchObject({
    code: "project_token_login_failed", status: 401, transient: false,
  })
  expect(exchanges).toBe(2)
  status = 200
  await expect(ctx.loginMaterial({ business: "required", zcode: "none" })).rejects.toMatchObject({
    code: "project_token_login_failed", transient: false,
  })
  expect(exchanges).toBe(3)
})

test("opaque business expiry is never invented as a transient fallback", async () => {
  const clock = 1_900_000_000_000
  let fail = false
  globalThis.fetch = async () => fail ? new Response("{}", { status: 503 }) : ok({ access_token: "opaque-business" })
  const { slot, runtime } = setup({ ...base, business: undefined, oauthToken: jwt(clock + 3_600_000) }, () => clock)
  const ctx = await runtime.capture(slot.getAuth)
  expect((await ctx.loginMaterial({ business: "required", zcode: "none" })).business).toBe("opaque-business")
  const stored = await slot.current()
  expect(JSON.parse(stored.refresh).businessExpiresAt).toBeUndefined()
  const coldSlot = accountFixture(JSON.parse(JSON.stringify(stored)))
  const cold = createAccountRuntime({ client: coldSlot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  fail = true
  await expect((await cold.capture(coldSlot.getAuth)).loginMaterial({ business: "required", zcode: "none" }))
    .rejects.toMatchObject({ code: "project_token_login_failed", status: 503, transient: true })
})

test("a rejected business token is replaced once with a usable bootstrap", async () => {
  const clock = 1_900_000_000_000, oauth = jwt(clock + 3_600_000)
  const oldBusiness = jwt(clock + 600_000), newBusiness = jwt(clock + 1_200_000)
  let exchanges = 0
  const authorizations = []
  globalThis.fetch = async (input, init = {}) => {
    const path = new URL(String(input)).pathname, auth = new Headers(init.headers).get("authorization")
    if (path === "/api/auth/z/login") { exchanges++; return ok({ access_token: newBusiness, expires_in: 1200 }) }
    if (path.endsWith("/access_tokens")) {
      authorizations.push(auth)
      return auth === `Bearer ${oldBusiness}` ? new Response("{}", { status: 401 }) :
        ok({ accessToken: "new-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 })
    }
    throw new Error(`unexpected offline request ${path}`)
  }
  const { slot, runtime } = setup({ ...base, oauthToken: oauth, business: oldBusiness,
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }, () => clock)
  const ctx = await runtime.capture(slot.getAuth)
  expect((await ctx.projectToken()).token).toBe("new-pat")
  expect(exchanges).toBe(1)
  expect(authorizations).toEqual([`Bearer ${oldBusiness}`, `Bearer ${newBusiness}`])
  const stored = await slot.current(), state = JSON.parse(stored.refresh)
  expect(state.business).toBe(newBusiness)
  expect(state.businessRejectedFingerprint).toBeUndefined()
  expect(stored.accountEpoch).toBe(ctx.generation)
  expect(stored.accountScope).toBe(ctx.scope)
})

test("a raw business authorization rejection also permits one scoped exchange", async () => {
  const clock = 1_900_000_000_000, oauth = jwt(clock + 3_600_000)
  const oldBusiness = jwt(clock + 600_000), newBusiness = jwt(clock + 1_200_000)
  const customerAuth = []
  let exchanges = 0
  globalThis.fetch = async (input, init = {}) => {
    const path = new URL(String(input)).pathname, auth = new Headers(init.headers).get("authorization")
    if (path === "/api/auth/z/login") { exchanges++; return ok({ access_token: newBusiness, expires_in: 1200 }) }
    if (path.endsWith("/getCustomerInfo")) {
      customerAuth.push(auth)
      return auth === oldBusiness ? new Response("{}", { status: 401 }) :
        ok({ organizations: [{ organizationId: "org", projects: [{ projectId: "project", projectType: 1 }] }] })
    }
    if (path.endsWith("/api_keys")) return ok([{ name: "zcode-api-key", apiKey: "key-id" }])
    if (path.endsWith("/access_tokens")) return ok({ accessToken: "new-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 })
    throw new Error(`unexpected offline request ${path}`)
  }
  const { slot, runtime } = setup({ ...base, oauthToken: oauth, business: oldBusiness }, () => clock)
  const ctx = await runtime.capture(slot.getAuth)
  expect((await ctx.projectToken()).token).toBe("new-pat")
  expect(customerAuth).toEqual([oldBusiness, newBusiness])
  expect(exchanges).toBe(1)
})

test("a second business rejection stops after one exchange and reports rejection", async () => {
  const clock = 1_900_000_000_000, oauth = jwt(clock + 3_600_000)
  const oldBusiness = jwt(clock + 600_000), newBusiness = jwt(clock + 1_200_000)
  let exchanges = 0, mints = 0
  globalThis.fetch = async (input) => {
    const path = new URL(String(input)).pathname
    if (path === "/api/auth/z/login") { exchanges++; return ok({ access_token: newBusiness, expires_in: 1200 }) }
    if (path.endsWith("/access_tokens")) { mints++; return new Response("{}", { status: 401 }) }
    throw new Error(`unexpected offline request ${path}`)
  }
  const { slot, runtime } = setup({ ...base, oauthToken: oauth, business: oldBusiness,
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }, () => clock)
  await expect((await runtime.capture(slot.getAuth)).projectToken()).rejects.toMatchObject({
    signIn: "expired", noModels: true, credentialState: "rejected", message: expect.stringContaining("rejected"),
  })
  expect(exchanges).toBe(1)
  expect(mints).toBe(2)
})

test("a rejected saved business token with expired bootstrap remains rejected after restart", async () => {
  const clock = 1_900_000_000_000
  const business = jwt(clock + 600_000), oauth = jwt(clock - 60_000)
  let calls = 0
  globalThis.fetch = async (input) => {
    calls++
    expect(new URL(String(input)).pathname.endsWith("/access_tokens")).toBe(true)
    return new Response("{}", { status: 401 })
  }
  const { slot, runtime } = setup({ ...base, oauthToken: oauth, business,
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }, () => clock)
  const first = await runtime.capture(slot.getAuth)
  await expect(first.projectToken()).rejects.toMatchObject({ signIn: "expired", noModels: true })
  const persisted = await slot.current()
  expect(JSON.parse(persisted.refresh).businessRejectedFingerprint).toBeTruthy()
  const coldSlot = accountFixture(JSON.parse(JSON.stringify(persisted)))
  const cold = createAccountRuntime({ client: coldSlot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  await expect((await cold.capture(coldSlot.getAuth)).projectToken()).rejects.toMatchObject({ signIn: "expired", noModels: true })
  expect(calls).toBe(1)
})

test("a late business rejection cannot invalidate a newer same-account credential", async () => {
  const clock = 1_900_000_000_000, oauth = jwt(clock + 3_600_000)
  const oldBusiness = jwt(clock + 600_000), newBusiness = jwt(clock + 1_200_000)
  let release, started, oldRequests = 0, exchanges = 0
  const began = new Promise((resolve) => { started = resolve })
  const blocked = new Promise((resolve) => { release = resolve })
  globalThis.fetch = async (input, init = {}) => {
    const path = new URL(String(input)).pathname, auth = new Headers(init.headers).get("authorization")
    if (path === "/api/auth/z/login") { exchanges++; return ok({ access_token: newBusiness, expires_in: 1200 }) }
    if (path.endsWith("/access_tokens")) {
      if (auth === `Bearer ${oldBusiness}`) {
        if (++oldRequests === 1) { started(); await blocked }
        return new Response("{}", { status: 401 })
      }
      expect(auth).toBe(`Bearer ${newBusiness}`)
      return ok({ accessToken: "new-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 })
    }
    throw new Error(`unexpected offline request ${path}`)
  }
  const { slot, runtime } = setup({ ...base, oauthToken: oauth, business: oldBusiness,
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }, () => clock)
  const first = await runtime.capture(slot.getAuth)
  const otherRuntime = createAccountRuntime({ client: slot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  const second = await otherRuntime.capture(slot.getAuth)
  const late = first.projectToken()
  await began
  expect((await second.projectToken()).token).toBe("new-pat")
  release()
  expect((await late).token).toBe("new-pat")
  const stored = await slot.current(), state = JSON.parse(stored.refresh)
  expect(state.business).toBe(newBusiness)
  expect(state.businessRejectedFingerprint).toBeUndefined()
  expect(stored.accountEpoch).toBe(first.generation)
  expect(stored.accountScope).toBe(first.scope)
  expect(exchanges).toBe(1)
})

test("a late rejection cannot invalidate a newer issuance of identical business bytes", async () => {
  const clock = 1_900_000_000_000, oauth = jwt(clock + 3_600_000), business = jwt(clock + 600_000)
  let release, started, requests = 0, exchanges = 0
  const began = new Promise((resolve) => { started = resolve })
  const blocked = new Promise((resolve) => { release = resolve })
  globalThis.fetch = async (input, init = {}) => {
    const path = new URL(String(input)).pathname
    if (path === "/api/auth/z/login") { exchanges++; return ok({ access_token: business, expires_in: 600 }) }
    if (path.endsWith("/access_tokens")) {
      expect(new Headers(init.headers).get("authorization")).toBe(`Bearer ${business}`)
      const number = ++requests
      if (number === 1) { started(); await blocked; return new Response("{}", { status: 401 }) }
      if (number === 2) return new Response("{}", { status: 401 })
      return ok({ accessToken: "new-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock + 600_000) / 1000 })
    }
    throw new Error(`unexpected offline request ${path}`)
  }
  const { slot, runtime } = setup({ ...base, oauthToken: oauth, business,
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key-id" } }, () => clock)
  const first = await runtime.capture(slot.getAuth)
  const otherRuntime = createAccountRuntime({ client: slot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  const second = await otherRuntime.capture(slot.getAuth)
  const late = first.projectToken()
  await began
  expect((await second.projectToken()).token).toBe("new-pat")
  const renewed = JSON.parse((await slot.current()).refresh)
  expect(renewed.businessGeneration).toBeTruthy()
  release()
  expect((await late).token).toBe("new-pat")
  const stored = JSON.parse((await slot.current()).refresh)
  expect(stored.business).toBe(business)
  expect(stored.businessGeneration).toBe(renewed.businessGeneration)
  expect(stored.businessRejectedFingerprint).toBeUndefined()
  expect(exchanges).toBe(1)
})

test("same-generation business exchanges cannot overwrite a newer CAS winner", async () => {
  const clock = 1_900_000_000_000, oauth = jwt(clock + 3_600_000)
  const older = jwt(clock + 600_000), newer = jwt(clock + 1_200_000)
  let release, started, exchanges = 0
  const began = new Promise((resolve) => { started = resolve })
  const blocked = new Promise((resolve) => { release = resolve })
  globalThis.fetch = async () => {
    if (++exchanges === 1) { started(); await blocked; return ok({ access_token: older, expires_in: 600 }) }
    return ok({ access_token: newer, expires_in: 1200 })
  }
  const { slot, runtime } = setup({ ...base, business: undefined, oauthToken: oauth }, () => clock)
  const first = await runtime.capture(slot.getAuth)
  const otherRuntime = createAccountRuntime({ client: slot.client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  const second = await otherRuntime.capture(slot.getAuth)
  const late = first.loginMaterial({ business: "required", zcode: "none" })
  await began
  expect((await second.loginMaterial({ business: "required", zcode: "none" })).business).toBe(newer)
  release()
  expect((await late).business).toBe(newer)
  const stored = await slot.current()
  expect(JSON.parse(stored.refresh).business).toBe(newer)
  expect(stored.accountEpoch).toBe(first.generation)
  expect(stored.accountScope).toBe(first.scope)
})

test("a failed business CAS never releases an unpersisted credential", async () => {
  const clock = 1_900_000_000_000
  let calls = 0
  globalThis.fetch = async (input) => {
    calls++
    expect(new URL(String(input)).pathname).toBe("/api/auth/z/login")
    return ok({ access_token: jwt(clock + 600_000), expires_in: 600 })
  }
  const { slot, runtime } = setup({ ...base, business: undefined, oauthToken: jwt(clock + 3_600_000) }, () => clock)
  await runtime.capture(slot.getAuth)
  const denied = createAccountRuntime({ client: { auth: { set: async () => ({ data: { saved: false, conflict: false } }) } },
    providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh), claimVerification: async () => false, now: () => clock, random: () => 0 })
  const ctx = await denied.capture(slot.getAuth)
  await expect(ctx.projectToken()).rejects.toMatchObject({ kind: "unavailable", message: "account_persistence_unavailable" })
  expect(JSON.parse((await slot.current()).refresh).business).toBeUndefined()
  expect(calls).toBe(1)
})

test("a login change at business CAS cannot publish to the replacement generation", async () => {
  const clock = 1_900_000_000_000
  const slot = accountFixture(saved({ ...base, business: undefined, oauthToken: jwt(clock + 3_600_000) }))
  let changed = false
  const client = { auth: { set: async (request) => {
    if (!changed && JSON.parse(request.body.refresh).business) {
      changed = true
      slot.replace({ ...await slot.current(), accountEpoch: "replacement-login" })
    }
    return slot.client.auth.set(request)
  } } }
  const runtime = createAccountRuntime({ client, providerId: "zcode", stateOf: (a) => JSON.parse(a.refresh),
    claimVerification: async () => false, now: () => clock, random: () => 0 })
  globalThis.fetch = async () => ok({ access_token: jwt(clock + 600_000), expires_in: 600 })
  const ctx = await runtime.capture(slot.getAuth)
  await expect(ctx.projectToken()).rejects.toMatchObject({ kind: "account_generation_changed" })
  const stored = await slot.current()
  expect(stored.accountEpoch).toBe("replacement-login")
  expect(JSON.parse(stored.refresh).business).toBeUndefined()
  expect(changed).toBe(true)
})

test("a login-generation change during business exchange never publishes stale material", async () => {
  const clock = 1_900_000_000_000
  let release, started
  const began = new Promise((resolve) => { started = resolve })
  const blocked = new Promise((resolve) => { release = resolve })
  globalThis.fetch = async () => { started(); await blocked; return ok({ access_token: jwt(clock + 600_000), expires_in: 600 }) }
  const { slot, runtime } = setup({ ...base, business: undefined, oauthToken: jwt(clock + 3_600_000) }, () => clock)
  const ctx = await runtime.capture(slot.getAuth)
  const pending = ctx.loginMaterial({ business: "required", zcode: "none" })
  await began
  slot.replace({ ...await slot.current(), accountEpoch: "new-login" })
  release()
  await expect(pending).rejects.toMatchObject({ kind: "account_generation_changed" })
  expect(JSON.parse((await slot.current()).refresh).business).toBeUndefined()
})

test("disabled and malformed mint responses never fall back to a permanent key", async () => {
  for (const response of [
    { enable: false },
    { enable: true, accessToken: "broken", tokenType: "Bearer", expiresIn: 0, expiresAt: 1 },
    { accessToken: "broken", tokenType: "not-bearer", expiresIn: 600, expiresAt: 1_900_000_600 },
  ]) {
    const clock = () => 1_900_000_000_000
    const fake = vendor({ now: clock, mint: () => ok(response) })
    const { slot, runtime } = setup(base, clock)
    await expect((await runtime.capture(slot.getAuth)).projectToken()).rejects.toThrow()
    expect(fake.requests.some((r) => r.path.includes("/copy/"))).toBe(false)
  }
})

test("an account switch during mint cannot publish or return the old token", async () => {
  let release, started
  const began = new Promise((resolve) => { started = resolve })
  const blocked = new Promise((resolve) => { release = resolve })
  const clock = () => Date.now()
  vendor({ now: clock, mint: async () => { started(); await blocked; return ok({ accessToken: "old-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: (clock() + 600_000) / 1000 }) } })
  const { slot, runtime } = setup(base, clock)
  const ctx = await runtime.capture(slot.getAuth)
  const pending = ctx.projectToken()
  await began
  const current = await slot.current()
  slot.replace({ ...current, accountEpoch: "different-login" })
  release()
  await expect(pending).rejects.toMatchObject({ kind: "account_generation_changed" })
  expect(JSON.stringify(await slot.current())).not.toContain("old-pat")
})
