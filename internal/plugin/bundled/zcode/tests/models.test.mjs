// Run with HOME=$(mktemp -d) bun test. All vendor responses are local fixtures.
import { test, expect, beforeAll, afterEach, afterAll } from "bun:test"
import { homedir, tmpdir } from "node:os"
import { mkdirSync, realpathSync, rmSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { accountFixture } from "./account-fixtures.mjs"

let ZCodeAuthPlugin, _internal, resetClientConfigs
const fetched = globalThis.fetch
beforeAll(async () => {
  if (![tmpdir(), realpathSync(tmpdir())].some((t) => homedir().startsWith(t))) throw new Error("run with HOME=$(mktemp -d) bun test")
  ;({ ZCodeAuthPlugin, _internal, resetClientConfigs } = await import("../index.mjs"))
})
afterEach(() => {
  globalThis.fetch = fetched
  _internal.config(undefined)
  _internal.routes.clear()
  resetClientConfigs()
  rmSync(join(homedir(), ".zcode", "v2", "runtime", "provider", "magpie-catalog-test"), { recursive: true, force: true })
})
afterAll(() => { globalThis.fetch = fetched })

const ok = (data) => Response.json({ code: 0, data })
const jwt = (exp) => `e30.${Buffer.from(JSON.stringify({ exp })).toString("base64url")}.sig`
const stateAuth = (state) => ({ type: "oauth", uid: state.uid, access: state.jwt, refresh: JSON.stringify(state), expires: 0 })
const startState = (token = "session") => ({ site: "bigmodel", kind: "start", uid: "user", zcodeUserId: "user", jwt: token, device: "device" })
const providerRule = (providerId, builtinModelIds, rest = {}) => {
  const site = providerId.startsWith("account:bigmodel-") ? "bigmodel" : "zai"
  const start = providerId.endsWith("-start-plan"), team = providerId.includes("-team-")
  const baseUrl = start ? "https://zcode.z.ai/api/v1/zcode-plan/anthropic" :
    site === "bigmodel" ? "https://open.bigmodel.cn/api/anthropic" : "https://api.z.ai/api/anthropic"
  return { providerId, ...rest, config: { group: `${site}-family`, builtinModelIds,
    access: { type: "zhipu-account", accountType: site, mode: start ? "start-plan" : team ? "team-coding-plan" : "individual-coding-plan" },
    api: { type: "anthropic-messages", baseUrl } } }
}
const config = (providerRules, modelConfigRules = {}) => ({ schemaVersion: 1, revision: 1, config: {
  providerConfigRules: { templateRules: [{ templateId: "coding", templateNameMap: { "en-US": "Coding" },
    config: { api: { type: "anthropic-messages", baseUrl: "https://api.z.ai/api/anthropic" } } }], providerRules },
  modelConfigRules: { modelRules: [{ modelMatch: ".*", config: { enabled: true } }], modelApiRules: [],
    providerSiteRules: [], templateModelRules: [], builtinProviderModelRules: [], ...modelConfigRules },
} })
function localConfigFixture(value) {
  const dir = join(homedir(), ".zcode", "v2", "runtime", "provider", "magpie-catalog-test", "release", "data")
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, "zcode-builtin.json"), JSON.stringify({ ...value, revision: Number.MAX_SAFE_INTEGER }))
}
const noLocalConfig = () => localConfigFixture({ schemaVersion: 1, config: null })
async function hook(auth) {
  const account = accountFixture(auth)
  const hooks = await ZCodeAuthPlugin({ client: account.client })
  return { account, hooks, list: (options = {}) => hooks.provider.models({ models: {} }, { getAuth: account.getAuth, ...options }) }
}

test("unavailable configuration returns a fallback rather than an authoritative list", async () => {
  noLocalConfig()
  globalThis.fetch = async () => { throw new Error("connection refused") }
  const { hooks, list } = await hook({ type: "api", key: "k" })
  await hooks.config({ provider: { zcode: { models: { "user-model": {} } } } })
  const ms = await list()
  expect(Object.keys(ms).length).toBeGreaterThan(0)
  expect(ms["user-model"]).toBeDefined()
  expect(ms[Symbol.for("magpie.fellBack")]).toBe(true)
  expect(Object.keys(ms).every((k) => ms[k].id === k)).toBe(true)
})

test("membership and matching API, site, template and model rules determine the published capabilities", async () => {
  const zai = "account:zai-individual-coding-plan", bigmodel = "account:bigmodel-individual-coding-plan"
  const provider = providerRule(zai, ["GLM-5.3", "GLM-5.3-Flash"], { templateId: "coding" })
  _internal.config(config([provider, providerRule(bigmodel, ["GLM-5.3"])], {
    modelRules: [{ modelMatch: ".*", config: { enabled: true, properties: {
      contextWindow: 1_000_000, inputFormat: { supportsImage: false, supportsVideo: false, supportsPdf: false, supportsAudio: false },
    }, optionSpecs: { maxOutputTokens: { max: 128_000 }, reasoningLevel: { values: ["low", "high"] } } } }],
    modelApiRules: [
      { modelMatch: ".*glm-5\\.3.*", apiTypeMatch: "anthropic-messages",
        config: { properties: { contextWindow: 800_000, inputFormat: { supportsAudio: true } },
          optionSpecs: { reasoningLevel: { values: ["low", "max"] } } } },
      { modelMatch: ".*", apiTypeMatch: "openai-chat-completions", config: { properties: { inputFormat: { supportsAudio: false } } } },
    ],
    providerSiteRules: [
      { modelMatch: ".*", baseUrlMatch: "https://api\\.z\\.ai/api/anthropic/?",
        config: { properties: { contextWindow: 900_000, inputFormat: { supportsImage: true, supportsVideo: true, supportsPdf: true } },
          optionSpecs: { reasoningLevel: { values: ["high"] } } } },
      { modelMatch: "glm-5\\.3", apiTypeMatch: "anthropic-messages", baseUrlMatch: "https://api\\.z\\.ai/api/anthropic/?",
        config: { properties: { inputFormat: { supportsVideo: false } } } },
      { modelMatch: ".*", apiTypeMatch: "openai-chat-completions", baseUrlMatch: "https://api\\.z\\.ai/api/anthropic/?",
        config: { properties: { inputFormat: { supportsPdf: false } } } },
      { modelMatch: ".*", baseUrlMatch: "https://other\\.example/api/anthropic", config: { properties: { inputFormat: { supportsImage: false } } } },
    ],
    templateModelRules: [{ templateId: "coding", modelId: "GLM-5.3", config: { properties: { contextWindow: 950_000 },
      optionSpecs: { maxOutputTokens: { max: 90_000 }, reasoningLevel: { values: ["low", "max"] } } } }],
    builtinProviderModelRules: [
      { providerId: zai, modelId: "GLM-5.3", config: { properties: { contextWindow: 980_000 },
        optionSpecs: { maxOutputTokens: { max: 64_000 }, reasoningLevel: { values: ["high", "max"] } } } },
      { providerId: zai, modelId: "GLM-5.2", config: { enabled: true } },
      { providerId: zai, modelId: "GLM-5-Turbo", config: { enabled: true } },
    ],
  }))
  globalThis.fetch = async () => { throw new Error("unexpected offline request") }
  const { hooks, list } = await hook({ type: "api", key: "k" })
  await hooks.config({ provider: { zcode: { models: { "custom-personal": {}, "user-model": {} } } } })
  const models = await list()
  expect(Object.keys(models)).toEqual(expect.arrayContaining(["GLM-5.3", "GLM-5.3-Flash", "custom-personal", "user-model"]))
  expect(models["GLM-5.2"]).toBeUndefined()
  expect(models["GLM-5-Turbo"]).toBeUndefined()
  expect(models["GLM-5.3"].capabilities.input).toMatchObject({ image: true, video: false, pdf: true, audio: true })
  expect(models["GLM-5.3-Flash"].capabilities.input).toMatchObject({ image: true, video: true, pdf: true, audio: true })
  expect(models["GLM-5.3"].limit).toEqual({ context: 980_000, output: 64_000 })
  expect(models["GLM-5.3-Flash"].limit.context).toBe(900_000)
  expect(Object.keys(models["GLM-5.3"].variants)).toEqual(["high", "max"])
  const other = await hook({ type: "api", key: "k", metadata: { site: "bigmodel" } })
  const big = await other.list()
  expect(big["GLM-5.3"].capabilities.input).toMatchObject({ image: false, video: false, pdf: false, audio: true })
  expect(big["GLM-5.3"].limit).toEqual({ context: 800_000, output: 128_000 })
})

test("all-disabled and explicitly empty provider memberships are authoritative", async () => {
  const plan = "account:zai-individual-coding-plan"
  const { list } = await hook({ type: "api", key: "k" })
  for (const [ids, rules] of [
    [["GLM-5.3", "GLM-5.3-Flash"], [
      { providerId: plan, modelId: "GLM-5.3", config: { enabled: false } },
      { providerId: plan, modelId: "GLM-5.3-Flash", config: { enabled: false } },
    ]],
    [[], []],
  ]) {
    _internal.config(config([providerRule(plan, ids)], { builtinProviderModelRules: rules }))
    const models = await list()
    expect(Object.keys(models)).toEqual([])
    expect(models[Symbol.for("magpie.fellBack")]).toBeUndefined()
  }
  _internal.config(config([providerRule(plan, ["GLM-5.3", "GLM-5.3-Flash"])], {
    modelRules: [{ modelMatch: ".*", config: { enabled: false } }],
    builtinProviderModelRules: [{ providerId: plan, modelId: "GLM-5.3", config: { enabled: true } }],
  }))
  expect(Object.keys(await list())).toEqual(["GLM-5.3"])
  _internal.config(config([providerRule("account:bigmodel-start-plan", [])]))
  expect((await list())[Symbol.for("magpie.fellBack")]).toBe(true)
})

test("sparse model rules inherit the baseline enabled value without inventing one", async () => {
  const plan = "account:zai-individual-coding-plan"
  const { list } = await hook({ type: "api", key: "k" })
  const sparse = { modelMatch: ".*", config: { properties: { inputFormat: { supportsImage: true } } } }
  _internal.config(config([providerRule(plan, ["GLM-5.3"])], {
    modelRules: [{ modelMatch: ".*", config: { enabled: true } }, sparse],
  }))
  expect((await list())["GLM-5.3"].capabilities.input.image).toBe(true)
  _internal.config(config([providerRule(plan, ["GLM-5.3"])], { modelRules: [sparse] }))
  expect(Object.keys(await list())).toEqual([])
})

test("Start balance names its own models, empty is authoritative, failure is unknown", async () => {
  _internal.config(config([
    providerRule("account:zai-start-plan", ["other"]),
    providerRule("account:bigmodel-start-plan", ["GLM-5.3-Flash"]),
  ], { modelRules: [{ modelMatch: ".*GLM-5\\.3.*", config: {
    enabled: true, properties: { contextWindow: 1_000_000, inputFormat: { supportsImage: true, supportsVideo: true, supportsPdf: true } },
    optionSpecs: { maxOutputTokens: { max: 128_000 }, reasoningLevel: { values: ["low", "high", "max"] } },
  } }] }))
  const now = Math.floor(Date.now() / 1000)
  let balance = { plans: [{ plan_id: "start", status: "active", ends_at: now + 3600 }],
    balances: [{ plan_id: "start", capabilities: ["model:GLM-5.3-Flash"], remaining_units: 10 }] }
  globalThis.fetch = async (url) => {
    if (!new URL(String(url)).pathname.endsWith("/billing/balance")) throw new Error("unexpected offline request")
    if (balance === null) throw new Error("connection refused")
    return ok(balance)
  }
  const { list } = await hook(stateAuth(startState()))
  let models = await list()
  expect(Object.keys(models)).toEqual(["GLM-5.3-Flash", "GLM-5.3-Flash-Trial"])
  expect(models["GLM-5.3-Flash"].capabilities.input).toMatchObject({ text: true, image: true, video: true, pdf: true })
  expect(models["GLM-5.3-Flash"].limit).toEqual({ context: 1_000_000, output: 128_000 })
  expect(Object.keys(models["GLM-5.3-Flash"].variants)).toEqual(["low", "high", "max"])
  balance = { plans: [], balances: [] }
  models = await list()
  expect(Object.keys(models)).toEqual([])
  expect(models[Symbol.for("magpie.fellBack")]).toBeUndefined()
  noLocalConfig()
  _internal.config(undefined)
  models = await list({ refresh: true })
  expect(Object.keys(models)).toEqual([])
  expect(models[Symbol.for("magpie.fellBack")]).toBeUndefined()
  expect(models[Symbol.for("magpie.listError")]).toContain("config")
  _internal.config(config([providerRule("account:bigmodel-start-plan", ["GLM-5.3-Flash"])]))
  balance = null
  expect((await list())[Symbol.for("magpie.fellBack")]).toBe(true)
})

test("a valid installed all-disabled config remains authoritative when the CDN fails", async () => {
  const plan = "account:zai-individual-coding-plan"
  localConfigFixture(config([providerRule(plan, ["GLM-5.3", "GLM-5.3-Flash"])], {
    builtinProviderModelRules: [
      { providerId: plan, modelId: "GLM-5.3", config: { enabled: false } },
      { providerId: plan, modelId: "GLM-5.3-Flash", config: { enabled: false } },
    ],
  }))
  globalThis.fetch = async () => { throw new Error("CDN offline") }
  const { list } = await hook({ type: "api", key: "k" })
  const models = await list({ refresh: true })
  expect(Object.keys(models)).toEqual([])
  expect(models[Symbol.for("magpie.fellBack")]).toBeUndefined()
  expect(models[Symbol.for("magpie.listError")]).toContain("config")
})

test("a fresh nonempty Start balance removes old models even when config is unavailable", async () => {
  noLocalConfig()
  const plan = "account:bigmodel-start-plan"
  _internal.config(config([providerRule(plan, ["GLM-5.3", "GLM-5.3-Flash"])]))
  const now = Math.floor(Date.now() / 1000)
  let names = ["GLM-5.3", "GLM-5.3-Flash"]
  globalThis.fetch = async (input) => {
    if (!new URL(String(input)).pathname.endsWith("/billing/balance")) throw new Error("CDN offline")
    return ok({ plans: [{ plan_id: "start", status: "active", ends_at: now + 3600 }],
      balances: names.map((name) => ({ plan_id: "start", capabilities: [`model:${name}`], remaining_units: 10 })) })
  }
  const { list } = await hook(stateAuth(startState()))
  let models = await list()
  expect(models["GLM-5.3"]).toBeDefined()
  expect(models["GLM-5.3-Flash"]).toBeDefined()
  names = ["GLM-5.3"]
  models = await list({ refresh: true })
  expect(Object.keys(models)).toEqual(["GLM-5.3", "GLM-5.3-Trial"])
  expect(models[Symbol.for("magpie.fellBack")]).toBeUndefined()
  expect(models[Symbol.for("magpie.listError")]).toContain("config")
})

test("a separately evidenced gift adds a Trial without expanding paid membership", async () => {
  _internal.config(config([
    providerRule("account:bigmodel-individual-coding-plan", ["GLM-5.3-Flash"]),
    providerRule("account:bigmodel-start-plan", ["GLM-5.3-Flash"]),
  ]))
  const now = Math.floor(Date.now() / 1000)
  const state = { ...startState("session"), kind: "individual", business: "business",
    base: "https://open.bigmodel.cn/api/anthropic", loc: { organizationId: "o", projectId: "p", apiKeyId: "k" } }
  globalThis.fetch = async (input) => {
    const path = new URL(String(input)).pathname
    if (path.endsWith("/access_tokens")) return ok({ accessToken: "pat", tokenType: "Bearer", expiresIn: 600, expiresAt: now + 600 })
    if (path.endsWith("/billing/balance")) return ok({ plans: [{ plan_id: "gift", status: "active", ends_at: now + 3600 }],
      balances: [{ plan_id: "gift", capabilities: ["model:GLM-5.3"], remaining_units: 10 }] })
    throw new Error(`unexpected request ${path}`)
  }
  const { list } = await hook(stateAuth(state))
  const models = await list()
  expect(models["GLM-5.3-Flash"]).toBeDefined()
  expect(models["GLM-5.3"]).toBeUndefined()
  expect(models["GLM-5.3-Trial"].free).toBe(true)
  expect(models[Symbol.for("magpie.fellBack")]).toBeUndefined()
})

test("expired or missing mandatory Start credentials signal relogin without a paid catalog", async () => {
  let requests = 0
  globalThis.fetch = async () => { requests++; throw new Error("unexpected request") }
  for (const token of ["", jwt(Math.floor(Date.now() / 1000) - 60)]) {
    const { hooks, account, list } = await hook(stateAuth(startState(token)))
    await expect(list()).rejects.toMatchObject({ signIn: "expired", noModels: true })
    expect(await hooks.auth.usage(account.getAuth, { id: "zcode" })).toMatchObject({ signIn: "expired", error: expect.stringContaining("sign in") })
    const loader = await hooks.auth.loader(account.getAuth, { id: "zcode" })
    await expect(loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body: "{}" })).rejects.toMatchObject({ signIn: "expired" })
  }
  expect(requests).toBe(0)
})

test("expired optional ZCode session leaves paid listing and inference usable", async () => {
  _internal.config(config([providerRule("account:bigmodel-individual-coding-plan", ["GLM-5.3"])]))
  const state = { ...startState(jwt(Math.floor(Date.now() / 1000) - 60)), kind: "individual", business: "business",
    base: "https://open.bigmodel.cn/api/anthropic", loc: { organizationId: "o", projectId: "p", apiKeyId: "k" } }
  const sent = []
  globalThis.fetch = async (input, init = {}) => {
    const u = new URL(String(input))
    if (u.pathname.endsWith("/access_tokens")) return ok({ accessToken: "pat", tokenType: "Bearer", expiresIn: 600, expiresAt: Date.now() / 1000 + 600 })
    if (u.pathname.endsWith("/messages")) { sent.push(new Headers(init.headers)); return Response.json({ type: "message", content: [] }) }
    throw new Error(`unexpected request ${u.pathname}`)
  }
  const { hooks, account, list } = await hook(stateAuth(state))
  expect((await list())["GLM-5.3"]).toBeDefined()
  const loader = await hooks.auth.loader(account.getAuth, { id: "zcode" })
  const response = await loader.fetch("https://open.bigmodel.cn/api/anthropic/v1/messages", {
    method: "POST", body: JSON.stringify({ model: "GLM-5.3", messages: [] }),
  })
  expect(response.status).toBe(200)
  expect(sent[0].get("authorization")).toBe("Bearer pat")
})

test("expired Z.ai bootstrap without business material signals relogin before listing", async () => {
  let requests = 0
  globalThis.fetch = async () => { requests++; throw new Error("unexpected offline request") }
  const now = Math.floor(Date.now() / 1000)
  const state = { site: "zai", kind: "individual", uid: "user", zcodeUserId: "user", jwt: "zcode-session",
    oauthToken: jwt(now - 60), base: "https://api.z.ai/api/anthropic" }
  const { list } = await hook(stateAuth(state))
  await expect(list()).rejects.toMatchObject({ signIn: "expired", noModels: true,
    message: expect.stringContaining("sign in") })
  expect(requests).toBe(0)
})

test("cold Z.ai model discovery reuses saved business after bootstrap and optional session expiry", async () => {
  const now = Math.floor(Date.now() / 1000), business = jwt(now + 3600)
  _internal.config(config([providerRule("account:zai-individual-coding-plan", ["GLM-5.3"])]))
  const state = { site: "zai", kind: "individual", uid: "user", zcodeUserId: "user", jwt: jwt(now - 60),
    oauthToken: jwt(now - 60), business, base: "https://api.z.ai/api/anthropic",
    loc: { organizationId: "org", projectId: "project", apiKeyId: "key" } }
  const requests = []
  globalThis.fetch = async (input, init = {}) => {
    const path = new URL(String(input)).pathname
    requests.push({ path, auth: new Headers(init.headers).get("authorization") })
    if (path.endsWith("/access_tokens")) return ok({ accessToken: "pat", tokenType: "Bearer", expiresIn: 600, expiresAt: now + 600 })
    throw new Error(`unexpected offline request ${path}`)
  }
  const { list, account } = await hook(stateAuth(state))
  expect((await list())["GLM-5.3"]).toBeDefined()
  const cold = accountFixture(JSON.parse(JSON.stringify(await account.current())))
  const hooks = await ZCodeAuthPlugin({ client: cold.client })
  expect((await hooks.provider.models({ models: {} }, { getAuth: cold.getAuth }))["GLM-5.3"]).toBeDefined()
  expect(requests.map((request) => request.path)).toEqual([
    "/api/biz/v1/organization/org/projects/project/api_keys/key/access_tokens",
    "/api/biz/v1/organization/org/projects/project/api_keys/key/access_tokens",
  ])
  expect(requests.every((request) => request.auth === `Bearer ${business}`)).toBe(true)
})

test("ordinary recovery is not negative-cached and explicit refresh reloads both config URLs", async () => {
  noLocalConfig()
  let down = true, revision = 1
  const requests = []
  globalThis.fetch = async (input) => {
    const url = new URL(String(input))
    requests.push(url.href)
    if (down) throw new Error("offline")
    if (url.pathname === "/api/v1/client/configs") return ok({ configs: { builtin_provider_config_json: `https://cdn.example/config-${revision}.json` } })
    if (url.hostname === "cdn.example") return Response.json({ ...config([providerRule("account:zai-individual-coding-plan", [revision === 1 ? "old-model" : "new-model"])]),
      revision: 9_007_199_254_740_000 + revision })
    throw new Error("unexpected offline request")
  }
  const { list } = await hook({ type: "api", key: "k" })
  expect((await list())[Symbol.for("magpie.fellBack")]).toBe(true)
  down = false
  expect(Object.keys(await list())).toEqual(["old-model"])
  revision = 2
  expect(Object.keys(await list())).toEqual(["old-model"])
  expect(Object.keys(await list({ refresh: true }))).toEqual(["new-model"])
  expect(requests.filter((url) => url.includes("/api/v1/client/configs"))).toHaveLength(3)
  expect(requests.filter((url) => url.includes("cdn.example"))).toEqual(["https://cdn.example/config-1.json", "https://cdn.example/config-2.json"])
  down = true
  expect((await list({ refresh: true }))[Symbol.for("magpie.fellBack")]).toBe(true)
})

test("missing CDN address and malformed provider config do not become cached successes", async () => {
  noLocalConfig()
  let mode = "missing-url", clients = 0, cdn = 0
  globalThis.fetch = async (input) => {
    const url = new URL(String(input))
    if (url.pathname === "/api/v1/client/configs") {
      clients++
      return ok({ configs: mode === "missing-url" ? {} : { builtin_provider_config_json: "https://cdn.example/config.json" } })
    }
    if (url.hostname === "cdn.example") {
      cdn++
      const release = { ...config([providerRule("account:zai-individual-coding-plan", ["recovered-model"])]),
        revision: 9_007_199_254_740_000 }
      if (mode === "missing-rules") delete release.config.modelConfigRules.modelApiRules
      return Response.json(mode === "malformed" ? {} : release)
    }
    throw new Error("unexpected offline request")
  }
  const { list } = await hook({ type: "api", key: "k" })
  expect((await list())[Symbol.for("magpie.fellBack")]).toBe(true)
  mode = "malformed"
  expect((await list())[Symbol.for("magpie.fellBack")]).toBe(true)
  mode = "missing-rules"
  expect((await list())[Symbol.for("magpie.fellBack")]).toBe(true)
  mode = "good"
  expect(Object.keys(await list())).toEqual(["recovered-model"])
  expect(clients).toBe(2)
  expect(cdn).toBe(3)
})
