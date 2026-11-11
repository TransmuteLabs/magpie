import { afterEach, expect, test } from "bun:test"
import { createAccountRuntime } from "../account.mjs"
import { accountFixture } from "./account-fixtures.mjs"

const realFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = realFetch })
const base = { version: 1, kind: "individual", site: "zai", uid: "user-1", device: "11111111-2222-4333-8444-555555555555", base: "https://api.z.ai/api/anthropic", business: "biz", jwt: "session" }
const saved = (state) => ({ type: "oauth", uid: state.uid, access: state.jwt, refresh: JSON.stringify(state), expires: 0 })
const ok = (data) => Response.json({ code: 0, data })
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
