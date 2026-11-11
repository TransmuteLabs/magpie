import { afterEach, expect, test } from "bun:test"
import { ZCodeAuthPlugin } from "../index.mjs"
import { accountFixture } from "./account-fixtures.mjs"

const original = globalThis.fetch
afterEach(() => { globalThis.fetch = original })
const ok = (data) => Response.json({ code: 0, data })
const state = { version: 1, site: "bigmodel", kind: "individual", uid: "u1", zcodeUserId: "u1", device: "11111111-2222-4333-8444-555555555555", base: "https://open.bigmodel.cn/api/anthropic", business: "biz", jwt: "jwt", loc: { organizationId: "personal", projectId: "personal-p", apiKeyId: "k" } }
function vendor() {
  globalThis.fetch = async (input, init = {}) => {
    const u = new URL(String(input)), h = new Headers(init.headers)
    if (u.pathname.endsWith("/billing/balance")) return ok({ plans: [{ plan_id: "start", status: "active", name: "Start Plan" }], balances: [] })
    if (u.pathname.endsWith("/access_tokens")) return ok({ accessToken: "pat", tokenType: "Bearer", expiresIn: 600, expiresAt: Date.now() / 1000 + 600 })
    if (u.pathname.endsWith("/subscription/list")) return ok([{ productName: "GLM Coding Pro", status: "VALID", inCurrentPeriod: true }])
    if (u.pathname.endsWith("/getCustomerInfo")) return ok({ organizations: [
      { organizationId: "personal", projects: [{ projectId: "personal-p", projectType: 1 }] },
      { organizationId: "org-a", projects: [{ projectId: "project-a", projectType: 2 }] },
      { organizationId: "org-b", projects: [{ projectId: "project-b", projectType: 2 }] },
    ] })
    if (u.pathname.endsWith("/querySubscribeDetail")) return ok({ hasSubscription: true, status: "EFFECTIVE", memberGrantStatus: "VALID", productId: `product-${h.get("bigmodel-organization")}`, productName: "Team" })
    throw new Error(`unexpected offline request ${u.pathname}`)
  }
}

test("list exposes every full candidate and select returns a distinct internal auth without replacing its source", async () => {
  vendor()
  const slot = accountFixture({ type: "oauth", uid: "u1", access: "jwt", refresh: JSON.stringify(state), expires: 0 })
  const hooks = await ZCodeAuthPlugin({ client: slot.client })
  const context = (await hooks.auth.account(slot.getAuth, {}, { operation: "context" })).context
  const expected = { expectedScope: context.scope, expectedGeneration: context.generation }
  const list = await hooks.auth.account(slot.getAuth, {}, { operation: "selection.list", ...expected })
  const teams = list.data.selections.filter((s) => s.kind === "team")
  expect(teams.map((s) => [s.organization, s.project, s.product])).toEqual([
    ["org-a", "project-a", "product-org-a"], ["org-b", "project-b", "product-org-b"],
  ])
  const original = await slot.current()
  const selected = await hooks.auth.select(slot.getAuth, {}, { selection: teams[1], ...expected })
  expect(await slot.current()).toEqual(original)
  expect(selected.accountScope).not.toBe(context.scope)
  expect(selected.accountEpoch).toBeUndefined()
  expect(JSON.parse(selected.refresh)).toMatchObject({ kind: "team", org: "org-b", project: "project-b", product: "product-org-b", business: "biz", jwt: "jwt" })
  expect(JSON.stringify(list)).not.toContain("biz")
  expect(JSON.stringify(list)).not.toContain("jwt")
})

test("MCP trust is checked before account credentials are read", async () => {
  const hooks = await ZCodeAuthPlugin({ client: {} })
  let reads = 0
  const getAuth = async () => { reads++; throw new Error("must not read credentials") }
  await expect(hooks.auth.account(getAuth, {}, { operation: "mcp.headers", params: { url: "https://evil.example/mcp" } })).rejects.toMatchObject({ kind: "untrusted_url" })
  expect(() => hooks.auth.accountPreflight({ operation: "mcp.headers", params: { stdio: true, url: "" } })).toThrow()
  expect(reads).toBe(0)
})

test("selection listing does not claim a partial list when Team discovery lacks business identity", async () => {
  globalThis.fetch = async () => ok({ plans: [], balances: [] })
  const start = { ...state, kind: "start", business: "" }
  const slot = accountFixture({ type: "oauth", uid: "u1", access: "jwt", refresh: JSON.stringify(start), expires: 0 })
  const hooks = await ZCodeAuthPlugin({ client: slot.client })
  await expect(hooks.auth.account(slot.getAuth, {}, { operation: "selection.list" })).rejects.toThrow("account_selection_unavailable")
})
