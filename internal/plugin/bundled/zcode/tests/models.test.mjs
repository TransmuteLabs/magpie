// Run with HOME=$(mktemp -d) bun test: a plan's models when ZCode's config
// can't be had stand in from the plugin's table, said to be a fallback,
// so magpie keeps the list it was told last, as the built-in keeps the one
// it fetched last.
import { test, expect, beforeAll, afterAll, afterEach } from "bun:test"
import { homedir, tmpdir } from "node:os"
import { realpathSync } from "node:fs"
import { accountFixture } from "./account-fixtures.mjs"

let ZCodeAuthPlugin, _internal
const fetched = globalThis.fetch
beforeAll(async () => {
  if (![tmpdir(), realpathSync(tmpdir())].some((t) => homedir().startsWith(t))) throw new Error("run with HOME=$(mktemp -d) bun test")
  ;({ ZCodeAuthPlugin, _internal } = await import("../index.mjs"))
})
afterEach(() => _internal.config(undefined))
afterAll(() => {
  globalThis.fetch = fetched
})

test("ZCode's config out of reach, the table stands in, said to be a fallback", async () => {
  _internal.config(null)
  globalThis.fetch = async () => {
    throw new Error("connection refused")
  }
  const account = accountFixture({ type: "api", key: "k" })
  const hooks = await ZCodeAuthPlugin({ client: account.client })
  const ms = await hooks.provider.models({ models: {} }, { getAuth: account.getAuth })
  expect(Object.keys(ms).length).toBeGreaterThan(0)
  expect(ms[Symbol.for("magpie.fellBack")]).toBe(true)
  // A consumer enumerating the list sees only model entries.
  expect(Object.keys(ms).every((k) => ms[k].id === k)).toBe(true)
})

test("the selected BigModel Start provider projects its own capabilities and efforts", async () => {
  _internal.config({ config: {
    providerConfigRules: { providerRules: [
      { providerId: "account:zai-start-plan", builtinModelIds: ["other"] },
      { providerId: "account:bigmodel-start-plan", builtinModelIds: ["GLM-5.3-Flash"] },
    ] },
    modelConfigRules: { modelRules: [{ modelMatch: ".*GLM-5\\.3.*", config: {
      properties: { contextWindow: 1_000_000, inputFormat: { supportsImage: true, supportsVideo: true, supportsPdf: true } },
      optionSpecs: { maxOutputTokens: { max: 128_000 }, reasoningLevel: { values: ["low", "high", "max"] } },
    } }] },
  } })
  globalThis.fetch = async (url) => {
    if (new URL(String(url)).pathname.endsWith("/billing/balance")) return Response.json({ code: 0, data: { plans: [], balances: [] } })
    throw new Error("unexpected offline request")
  }
  const state = { site: "bigmodel", kind: "start", uid: "user", zcodeUserId: "user", jwt: "session", device: "device" }
  const account = accountFixture({ type: "oauth", uid: "user", access: "session", refresh: JSON.stringify(state) })
  const hooks = await ZCodeAuthPlugin({ client: account.client })
  const models = await hooks.provider.models({}, { getAuth: account.getAuth })
  expect(Object.keys(models)).toEqual(["GLM-5.3-Flash"])
  expect(models["GLM-5.3-Flash"].capabilities.input).toMatchObject({ text: true, image: true, video: true, pdf: true })
  expect(models["GLM-5.3-Flash"].limit).toEqual({ context: 1_000_000, output: 128_000 })
  expect(Object.keys(models["GLM-5.3-Flash"].variants)).toEqual(["low", "high", "max"])
})
