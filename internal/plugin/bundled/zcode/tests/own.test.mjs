// Application import uses isolated ZCode fixtures and never mutates the app's store.
import { afterEach, beforeAll, expect, test } from "bun:test"
import { createCipheriv, createHash, randomBytes } from "node:crypto"
import { mkdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs"
import { homedir, tmpdir } from "node:os"
import { join } from "node:path"
import { accountFixture } from "./account-fixtures.mjs"

let ZCodeAuthPlugin, _internal
const realFetch = globalThis.fetch
const dir = () => join(homedir(), ".zcode", "v2")
beforeAll(async () => {
  if (![tmpdir(), realpathSync(tmpdir())].some((t) => homedir().startsWith(t))) throw new Error("run with an isolated HOME")
  process.env.ZCODE_CREDENTIAL_SECRET = "test-seed"
  ;({ ZCodeAuthPlugin, _internal } = await import("../index.mjs"))
})
afterEach(() => { globalThis.fetch = realFetch; rmSync(dir(), { recursive: true, force: true }); _internal.routes.clear() })

function enc(text) {
  const key = createHash("sha256").update("test-seed").digest(), iv = randomBytes(12)
  const cipher = createCipheriv("aes-256-gcm", key, iv)
  const bytes = Buffer.concat([cipher.update(text), cipher.final()])
  return "enc:v1:" + [iv, cipher.getAuthTag(), bytes].map((b) => b.toString("base64url")).join(".")
}
function store(entries, settings = {}, plaintext = false) {
  mkdirSync(dir(), { recursive: true })
  writeFileSync(join(dir(), "credentials.json"), JSON.stringify(Object.fromEntries(Object.entries(entries).map(([k, v]) => [k, plaintext ? v : enc(v)]))))
  writeFileSync(join(dir(), "setting.json"), JSON.stringify(settings))
}
const locKey = (family, user) => "project-key-location:v1:" + encodeURIComponent(JSON.stringify([family === "zai" ? "https://api.z.ai" : "https://bigmodel.cn", family, user, null, 1, "non-team"]))
const location = JSON.stringify({ organizationId: "org", projectId: "project", apiKeyId: "key-id" })
async function importApp() {
  const hooks = await ZCodeAuthPlugin({ client: {} })
  const method = hooks.auth.methods.find((m) => m.label === "ZCode app's sign-in")
  return (await method.authorize()).callback()
}

test("same-name normalized profiles never make an account follow another UID", async () => {
  const info = (id) => JSON.stringify({ id, username: "Ada", displayName: "Ada" })
  store({ "oauth:active_provider": "bigmodel", "oauth:bigmodel:user_info": info("a"), "oauth:bigmodel:access_token": "business-a", zcodejwttoken: "zcode-a" })
  const signed = await importApp()
  expect(signed.uid).toBe("a")
  const slot = accountFixture({ ...signed, type: "oauth" })
  const hooks = await ZCodeAuthPlugin({ client: slot.client })
  const first = (await hooks.auth.account(slot.getAuth, {}, { operation: "context" })).context
  store({ "oauth:active_provider": "bigmodel", "oauth:bigmodel:user_info": info("b"), "oauth:bigmodel:access_token": "business-b", zcodejwttoken: "zcode-b" })
  const stillA = (await hooks.auth.account(slot.getAuth, {}, { operation: "context" })).context
  expect(stillA.scope).toBe(first.scope)
  expect(stillA.generation).toBe(first.generation)
  expect(JSON.parse((await slot.current()).refresh)).toMatchObject({ business: "business-a", jwt: "zcode-a" })
  store({ "oauth:active_provider": "bigmodel", "oauth:bigmodel:user_info": info("a"), "oauth:bigmodel:access_token": "business-a2", zcodejwttoken: "zcode-a2" })
  const updated = (await hooks.auth.account(slot.getAuth, {}, { operation: "context" })).context
  expect(updated.scope).toBe(first.scope)
  expect(updated.generation).not.toBe(first.generation)
  expect(JSON.parse((await slot.current()).refresh)).toMatchObject({ business: "business-a2", jwt: "zcode-a2" })
})

test("unknown user identities never follow another live app account", async () => {
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": JSON.stringify({ name: "Ada" }), zcodejwttoken: "old-jwt",
    "account-provider:coding-plan:account:zai-individual-coding-plan:account:other:api-key": "other.secret" })
  const signed = await importApp()
  expect(JSON.parse(signed.refresh)).toMatchObject({ kind: "start", jwt: "old-jwt" })
  expect(JSON.parse(signed.refresh).key).toBeFalsy()
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": JSON.stringify({ name: "Ada" }), zcodejwttoken: "other-jwt" })
  expect(_internal.stateOf({ ...signed, type: "oauth" }).jwt).toBe("old-jwt")
})

test("an explicitly signed-out app never imports stale family credentials", () => {
  store({ "oauth:active_provider": "", "oauth:zai:user_info": JSON.stringify({ id: "u1" }),
    "oauth:zai:access_token": "stale-business", zcodejwttoken: "stale-zcode" }, {}, true)
  expect(_internal.ownSignIn()).toBeNull()
})

test("a ZCode session cannot be imported as Z.ai business material", async () => {
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": JSON.stringify({ user_id: "u1" }),
    "oauth:zai:access_token": "same-session", zcodejwttoken: "same-session" }, {}, true)
  const signed = await importApp()
  const state = JSON.parse(signed.refresh)
  expect(state.kind).toBe("start")
  expect(state.business).toBeFalsy()
  expect(state.jwt).toBe("same-session")
})

test("plaintext import uses the active family and the raw profile's exact location to mint a PAT", async () => {
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": JSON.stringify({ user_id: "u1", name: "User" }),
    "oauth:zai:access_token": "zai-business", zcodejwttoken: "zcode-jwt", [locKey("zai", "u1")]: location,
    [locKey("zai", "u2")]: JSON.stringify({ organizationId: "wrong", projectId: "wrong", apiKeyId: "wrong" }),
    "oauth:bigmodel:user_info": JSON.stringify({ id: "u9" }), "oauth:bigmodel:access_token": "stale-business" },
  { providerFamilyConnectionSelections: { zai: { kind: "individual-coding-plan" } } }, true)
  const before = readFileSync(join(dir(), "credentials.json"), "utf8")
  const signed = await importApp()
  const state = JSON.parse(signed.refresh)
  expect(state).toMatchObject({ site: "zai", kind: "individual", uid: "u1", business: "zai-business", loc: { apiKeyId: "key-id" } })
  expect(state.oauthToken).toBeUndefined()
  const requests = []
  globalThis.fetch = async (input, init = {}) => {
    const u = new URL(String(input)); requests.push({ path: u.pathname, headers: new Headers(init.headers) })
    if (u.pathname.endsWith("/access_tokens")) return Response.json({ code: 0, data: { accessToken: "short-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: Date.now() / 1000 + 600 } })
    if (u.pathname.endsWith("/billing/balance")) return Response.json({ code: 0, data: { plans: [], balances: [] } })
    if (u.pathname.endsWith("/messages")) return Response.json({ type: "message", content: [] })
    throw new Error(`unexpected offline request ${u.pathname}`)
  }
  const slot = accountFixture({ ...signed, type: "oauth" })
  const hooks = await ZCodeAuthPlugin({ client: slot.client }, { freeFirst: false })
  const loader = await hooks.auth.loader(slot.getAuth, {})
  expect((await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body: JSON.stringify({ model: "GLM-5.3", messages: [] }) })).status).toBe(200)
  expect(requests.find((r) => r.path.endsWith("/messages")).headers.get("x-api-key")).toBe("short-pat")
  expect(requests.find((r) => r.path.endsWith("/access_tokens")).headers.get("authorization")).toBe("Bearer zai-business")
  expect(requests.some((r) => r.path === "/api/auth/z/login")).toBe(false)
  expect(JSON.stringify(requests.map((r) => r.path))).not.toContain("/copy/")
  expect(JSON.stringify(requests.map((r) => r.path))).not.toContain("wrong")
  expect(readFileSync(join(dir(), "credentials.json"), "utf8")).toBe(before)
})

test("correcting an older app import's Z.ai business role preserves its account generation", async () => {
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": JSON.stringify({ user_id: "u1", name: "User" }),
    "oauth:zai:access_token": "zai-business", "oauth:session_revision": "session-1", zcodejwttoken: "zcode-jwt" },
  { providerFamilyConnectionSelections: { zai: { kind: "individual-coding-plan" } } }, true)
  const signed = await importApp()
  const legacy = JSON.parse(signed.refresh)
  legacy.oauthToken = legacy.business
  delete legacy.business
  const slot = accountFixture({ ...signed, type: "oauth", refresh: JSON.stringify(legacy), accountEpoch: "same-login" })
  const hooks = await ZCodeAuthPlugin({ client: slot.client })
  const before = await slot.current()
  const account = await hooks.auth.account(slot.getAuth, {}, { operation: "context" })
  const after = await slot.current(), state = JSON.parse(after.refresh)
  expect(after.accountScope).toBe(before.accountScope)
  expect(after.accountEpoch).toBe("same-login")
  expect(account.context.generation).toBe("same-login")
  expect(state.business).toBe("zai-business")
  expect(state.oauthToken).toBeFalsy()
})

test("a same-session Z.ai app business update preserves scope and account generation", async () => {
  const info = JSON.stringify({ user_id: "u1", name: "User" })
  const settings = { providerFamilyConnectionSelections: { zai: { kind: "individual-coding-plan" } } }
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": info, "oauth:zai:access_token": "business-a",
    "oauth:session_revision": "revision-a", zcodejwttoken: "same-zcode-jwt" }, settings, true)
  const signed = await importApp()
  const slot = accountFixture({ ...signed, type: "oauth" })
  const hooks = await ZCodeAuthPlugin({ client: slot.client })
  const first = (await hooks.auth.account(slot.getAuth, {}, { operation: "context" })).context
  const current = await slot.current(), state = JSON.parse(current.refresh)
  Object.assign(state, { businessExpiresAt: 1_900_000_000_000, businessGeneration: "old-material",
    businessRejectedFingerprint: "old-rejection", businessRejectedGeneration: "old-material" })
  slot.replace({ ...current, refresh: JSON.stringify(state) })
  store({ "oauth:active_provider": "zai", "oauth:zai:user_info": info, "oauth:zai:access_token": "business-b",
    "oauth:session_revision": "revision-b", zcodejwttoken: "same-zcode-jwt" }, settings, true)
  const second = (await hooks.auth.account(slot.getAuth, {}, { operation: "context" })).context
  const updated = JSON.parse((await slot.current()).refresh)
  expect(second.scope).toBe(first.scope)
  expect(second.generation).toBe(first.generation)
  expect(updated.business).toBe("business-b")
  expect(updated.businessExpiresAt).toBeUndefined()
  expect(updated.businessGeneration).toBeUndefined()
  expect(updated.businessRejectedFingerprint).toBeUndefined()
})

test("China login uses only the current user's linked BigModel account and revision", async () => {
  const entries = { "oauth:active_provider": "zai_cn", "oauth:zai_cn:user_info": JSON.stringify({ user_id: "cn-user" }),
    "oauth:zai_cn:access_token": "china-oauth", "oauth:session_revision": "revision-1",
    "oauth:linked_bigmodel": JSON.stringify({ userId: "cn-user", customerId: "bm-user", accessToken: "bm-business" }),
    zcodejwttoken: "zcode-jwt", [locKey("bigmodel", "bm-user")]: location }
  const settings = { providerFamilyDomain: "bigmodel", providerFamilyConnectionSelections: { bigmodel: { kind: "individual-coding-plan" } } }
  store(entries, settings, true)
  const signed = await importApp()
  expect(JSON.parse(signed.refresh)).toMatchObject({ site: "bigmodel", uid: "cn-user", accountIdentity: "bm-user",
    business: "bm-business", loginGeneration: "revision-1", loc: { apiKeyId: "key-id" } })
  store({ ...entries, "oauth:linked_bigmodel": JSON.stringify({ userId: "another-user", customerId: "other", accessToken: "other-token" }) }, settings, true)
  const other = await importApp()
  const state = JSON.parse(other.refresh)
  expect(state.business).toBeFalsy()
  expect(state.accountIdentity).toBe("cn-user")
  expect(state.loc).toBeUndefined()
})

test("an app's explicit legacy key remains usable without manufacturing a user identity", async () => {
  store({ "oauth:active_provider": "bigmodel", "oauth:bigmodel:user_info": "{}" }, { providerFamilyDomain: "bigmodel" }, true)
  writeFileSync(join(dir(), "config.json"), JSON.stringify({ provider: {
    "account:bigmodel-individual-coding-plan": { options: { apiKey: "legacy.secret" } },
  } }))
  const signed = await importApp()
  expect(signed.type).toBe("success")
  expect(JSON.parse(signed.refresh)).toMatchObject({ kind: "legacy-key", key: "legacy.secret", uid: "" })
  const slot = accountFixture({ ...signed, type: "oauth" })
  const hooks = await ZCodeAuthPlugin({ client: slot.client })
  const account = await hooks.auth.account(slot.getAuth, {}, { operation: "context" })
  expect(account.context.userId).toBe("")
})

test("legacy project-only team selection resolves only a unique organization", async () => {
  const entries = { "oauth:active_provider": "bigmodel", "oauth:bigmodel:user_info": JSON.stringify({ id: "u1", username: "Ada" }), "oauth:bigmodel:access_token": "business", zcodejwttoken: "zcode" }
  const settings = { modelProviderFamilySelectedKeys: { bigmodel: "team-plan:builtin:bigmodel-coding-plan:product:project" } }
  store(entries, settings, true)
  let organizations = [{ organizationId: "team-a", projects: [{ projectId: "project" }] }]
  globalThis.fetch = async () => Response.json({ code: 0, data: { organizations } })
  const signed = await importApp()
  expect(JSON.parse(signed.refresh)).toMatchObject({ kind: "team", org: "team-a", project: "project", product: "product" })
  organizations = [...organizations, { organizationId: "team-b", projects: [{ projectId: "project" }] }]
  const ambiguous = await importApp()
  expect(ambiguous.type).toBe("failed")
  expect(ambiguous.error).toContain("uniquely")
})
