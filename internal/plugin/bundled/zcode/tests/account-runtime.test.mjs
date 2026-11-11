import { afterEach, expect, test } from "bun:test"
import { createAccountRuntime } from "../account.mjs"
import { accountFixture } from "./account-fixtures.mjs"

const decode = (auth) => auth.type === "api"
  ? { site: "zai", kind: "legacy-key", key: auth.key, uid: auth.uid || "", base: "https://api.z.ai/api/anthropic" }
  : JSON.parse(auth.refresh)
const auth = (state) => ({ type: "oauth", uid: state.uid, access: state.jwt, refresh: JSON.stringify(state), expires: 0 })
const jwt = (exp) => `e30.${Buffer.from(JSON.stringify({ exp })).toString("base64url")}.sig`
const originalFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = originalFetch })

function make(initial, stateOf = decode) {
  const slot = accountFixture(initial)
  const runtime = createAccountRuntime({ client: slot.client, providerId: "zcode", stateOf, claimVerification: async () => false })
  return { slot, runtime }
}

test("login material is family-specific, per-kind and absent from public context", async () => {
  const expired = jwt(Math.floor(Date.now() / 1000) - 60)
  const { slot, runtime } = make(auth({ version: 1, site: "bigmodel", kind: "individual", uid: "u1", device: "d", base: "https://open.bigmodel.cn/api/anthropic", business: "business-secret", jwt: expired, refreshToken: "actual-refresh" }))
  const ctx = await runtime.capture(slot.getAuth)
  expect(await ctx.loginMaterial({ business: "required", zcode: "optional" })).toEqual({ business: "business-secret", zcode: "" })
  expect(await ctx.loginMaterial({ business: "none", zcode: "none" })).toEqual({ business: "", zcode: "" })
  await expect(ctx.loginMaterial()).rejects.toThrow("account_zcode_relogin_required")
  expect(JSON.stringify(ctx.public())).not.toContain("secret")
  expect(JSON.stringify(ctx.public())).not.toContain("refresh")
  const stored = JSON.parse((await slot.current()).refresh)
  expect(stored.refreshToken).toBe("actual-refresh")
  expect(stored.business).toBe("business-secret")
})

test("legacy keys remain usable as keys, never as business JWTs", async () => {
  const { slot, runtime } = make({ type: "api", key: "legacy.secret" })
  const ctx = await runtime.capture(slot.getAuth)
  expect((await ctx.projectToken()).kind).toBe("legacy-key")
  expect((await ctx.projectToken()).token).toBe("legacy.secret")
  expect((await ctx.loginMaterial({ business: "optional", zcode: "none" })).business).toBe("")
  await expect(ctx.loginMaterial({ business: "required", zcode: "none" })).rejects.toThrow("relogin")
  expect(ctx.public().scope).toBeTruthy()
  expect(ctx.public().generation).toBeTruthy()
  expect(ctx.public().userId).toBe("")
})

test("a migrated family UID becomes a billing identity only after a matching ZCode profile", async () => {
  const state = { site: "bigmodel", kind: "individual", uid: "family-user", jwt: "session-jwt",
    business: "business", base: "https://open.bigmodel.cn/api/anthropic" }
  const { slot, runtime } = make(auth(state))
  const requests = []
  let returned = "other-user"
  globalThis.fetch = async (input, init) => {
    requests.push({ url: String(input), authorization: new Headers(init.headers).get("authorization") })
    return Response.json({ code: 0, data: { user: { user_id: returned } } })
  }
  const unverified = await runtime.capture(slot.getAuth, { verifyIdentity: true })
  expect(unverified.public().userId).toBe("")
  returned = "family-user"
  const verified = await runtime.capture(slot.getAuth, { verifyIdentity: true })
  expect(verified.public().userId).toBe("family-user")
  expect(verified.scope).toBe(unverified.scope)
  expect(verified.generation).toBe(unverified.generation)
  expect(requests.every((request) => request.url.endsWith("/api/v1/oauth/profile") && request.authorization === "Bearer session-jwt")).toBe(true)
})

test("a migrated legacy Team context is bound to organization, project and optional product", async () => {
  const state = { site: "bigmodel", kind: "legacy-key", uid: "u1", key: "team.secret", token: "business",
    org: "organization-a", project: "project-a", base: "https://open.bigmodel.cn/api/anthropic" }
  for (const change of [{ org: "organization-b" }, { project: "project-b" }, { product: "product-b" }]) {
    const { slot, runtime } = make(auth(state))
    const captured = await runtime.capture(slot.getAuth)
    expect(captured.public()).toMatchObject({ kind: "legacy-key", organization: "organization-a", project: "project-a" })
    const saved = await slot.current()
    slot.replace({ ...saved, refresh: JSON.stringify({ ...JSON.parse(saved.refresh), ...change }) })
    expect((await slot.current()).accountEpoch).toBe(captured.generation)
    await expect(captured.assertCurrent()).rejects.toMatchObject({ kind: "account_scope_changed" })

    const other = make(auth({ ...state, ...change }))
    expect((await other.runtime.capture(other.slot.getAuth)).scope).not.toBe(captured.scope)
  }
})

test("a partially populated legacy Team selection can still use its explicit key", async () => {
  for (const selection of [{ org: "organization-a" }, { project: "project-a" }]) {
    const { slot, runtime } = make(auth({ site: "bigmodel", kind: "legacy-key", key: "team.secret",
      token: "business", base: "https://open.bigmodel.cn/api/anthropic", ...selection }))
    const ctx = await runtime.capture(slot.getAuth)
    expect(ctx.state.kind).toBe("legacy-key")
    expect(await ctx.projectToken()).toMatchObject({ kind: "legacy-key", token: "team.secret" })
  }
})

test("epoch is durable across capture and rejects stale account contexts", async () => {
  const { slot, runtime } = make(auth({ version: 1, site: "zai", kind: "start", uid: "user-a", jwt: "session-a", base: "https://api.z.ai/api/anthropic" }))
  const a = await runtime.capture(slot.getAuth)
  const b = await runtime.capture(slot.getAuth, { expectedScope: a.scope, expectedGeneration: a.generation })
  expect(b.generation).toBe(a.generation)
  const current = await slot.current()
  slot.replace({ ...current, accountEpoch: "different-login" })
  await expect(a.assertCurrent()).rejects.toMatchObject({ kind: "account_generation_changed" })
  await expect(runtime.capture(slot.getAuth, { expectedGeneration: a.generation })).rejects.toMatchObject({ kind: "account_generation_changed" })
})

test("following a positively identified app session changes epoch without changing scope", async () => {
  const original = { version: 1, source: "zcode", site: "zai", kind: "individual", uid: "u1", jwt: "old-jwt", business: "old-business", base: "https://api.z.ai/api/anthropic" }
  let live = { ...original }
  const stateOf = (a, options) => options?.follow ? live : decode(a)
  const { slot, runtime } = make(auth(original), stateOf)
  const first = await runtime.capture(slot.getAuth)
  live = { ...original, jwt: "new-jwt", business: "new-business" }
  const second = await runtime.capture(slot.getAuth)
  expect(second.scope).toBe(first.scope)
  expect(second.generation).not.toBe(first.generation)
  expect(await second.loginMaterial()).toEqual({ business: "new-business", zcode: "new-jwt" })
  await expect(first.assertCurrent()).rejects.toMatchObject({ kind: "account_generation_changed" })
})

test("context request deadline includes a stalled response body", async () => {
  const { slot, runtime } = make(auth({ site: "zai", kind: "start", uid: "u1", jwt: "zcode", base: "https://api.z.ai/api/anthropic" }))
  const ctx = await runtime.capture(slot.getAuth)
  globalThis.fetch = async (_url, { signal }) => new Response(new ReadableStream({
    start(controller) { signal.addEventListener("abort", () => controller.error(signal.reason), { once: true }) },
  }))
  await expect(ctx.request("https://zcode.z.ai/test", {}, { timeout: 20 })).rejects.toMatchObject({ kind: "timeout", retryable: true })
})
