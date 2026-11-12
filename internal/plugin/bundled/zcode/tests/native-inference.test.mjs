import { afterEach, expect, test } from "bun:test"
import { ZCodeAuthPlugin, _internal } from "../index.mjs"
import { accountFixture } from "./account-fixtures.mjs"

const original = globalThis.fetch
const state = { version: 1, site: "zai", kind: "individual", uid: "user", zcodeUserId: "user", device: "11111111-2222-4333-8444-555555555555", base: "https://api.z.ai/api/anthropic", business: "business-jwt", jwt: "zcode-jwt", loc: { organizationId: "o", projectId: "p", apiKeyId: "k" } }
const body = JSON.stringify({ model: "GLM-5.3", max_tokens: 4096, thinking: { type: "enabled", budget_tokens: 8192, display: "summarized" }, output_config: { effort: "max" }, messages: [{ role: "user", content: "hello" }] })
const response = () => Response.json({ type: "message", content: [{ type: "text", text: "ok" }] })
const ok = (data) => Response.json({ code: 0, data })
afterEach(() => { globalThis.fetch = original; _internal.config(undefined); _internal.routes.clear() })

async function setup(answer) {
  const slot = accountFixture({ type: "oauth", uid: state.uid, access: state.jwt, refresh: JSON.stringify(state), expires: 0 })
  const seen = []
  globalThis.fetch = async (input, init = {}) => {
    const u = new URL(String(input))
    seen.push({ url: u.href, path: u.pathname, headers: new Headers(init.headers), body: init.body, magpie: init.magpie })
    if (u.pathname.endsWith("/access_tokens")) return ok({ accessToken: "current-pat", tokenType: "Bearer", expiresIn: 600, expiresAt: Date.now() / 1000 + 600 })
    if (u.pathname.endsWith("/billing/balance")) return ok({ plans: [], balances: [] })
    if (u.pathname.endsWith("/messages")) return answer ? answer(u, init) : response()
    throw new Error(`unexpected offline request ${u.pathname}`)
  }
  const hooks = await ZCodeAuthPlugin({ client: slot.client }, { freeFirst: false })
  const context = (await hooks.auth.account(slot.getAuth, { id: "zcode" }, { operation: "context" })).context
  const loader = await hooks.auth.loader(slot.getAuth, { id: "zcode" })
  return { slot, hooks, context, loader, seen }
}

test("paid inference uses a PAT and normalizes GLM effort without forwarding host metadata", async () => {
  const { loader, seen } = await setup()
  const res = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body, magpie: { model: "GLM-5.3", session: "task" } })
  expect(res.status).toBe(200)
  const sent = seen.find((r) => r.path.endsWith("/messages"))
  expect(sent.headers.get("x-api-key")).toBe("current-pat")
  expect(sent.headers.get("authorization")).toBe("Bearer current-pat")
  expect(sent.magpie).toBeUndefined()
  expect(JSON.parse(sent.body)).toMatchObject({ thinking: { type: "enabled" }, output_config: { effort: "max" } })
  expect(JSON.parse(sent.body).thinking.budget_tokens).toBeUndefined()
})

test("business failures in HTTP 200 are exposed and terminal concurrency is not replayed", async () => {
  const { loader, seen } = await setup(() => Response.json({ success: false, code: 3008, msg: "account concurrency reached", request_id: "vendor-id" }))
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body })
  expect(result.status).toBe(429)
  expect(await result.json()).toMatchObject({ code: 3008, msg: "account concurrency reached", request_id: "vendor-id" })
  expect(seen.filter((r) => r.path.endsWith("/messages"))).toHaveLength(1)
  expect(seen.filter((r) => r.path.endsWith("/access_tokens"))).toHaveLength(1)
})

test("an inference HTTP 401 rejects only its PAT generation and retries once", async () => {
  let messages = 0
  let cancelled = 0
  const { loader, seen } = await setup(() => ++messages === 1 ? new Response(new ReadableStream({
    start(controller) { controller.enqueue(new TextEncoder().encode("expired PAT")) },
    cancel() { cancelled++ },
  }), { status: 401 }) : response())
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body })
  expect(result.status).toBe(200)
  expect(cancelled).toBe(1)
  expect(seen.filter((r) => r.path.endsWith("/messages"))).toHaveLength(2)
  expect(seen.filter((r) => r.path.endsWith("/access_tokens"))).toHaveLength(2)
  expect(seen.filter((r) => r.path.endsWith("/auth/z/login"))).toHaveLength(0)
})

test("a business error after partial stream output is surfaced without replay", async () => {
  const wire = 'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}\n\n' +
    'data: {"success":false,"code":1302,"msg":"vendor limited","request_id":"r1"}\n\n'
  const { loader, seen } = await setup(() => new Response(wire, { headers: { "content-type": "text/event-stream" } }))
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body })
  const text = await result.text()
  expect(text).toContain('"text":"partial"')
  expect(text).toContain("event: error")
  expect(text).toContain('"code":"1302"')
  expect(text).toContain("vendor limited")
  expect(seen.filter((r) => r.path.endsWith("/messages"))).toHaveLength(1)
})

test("the ordinary GLM-5.1-Highspeed model does not require a native execution binding", async () => {
  const { loader, seen } = await setup()
  const requestBody = JSON.stringify({ model: "GLM-5.1-Highspeed", messages: [{ role: "user", content: "hello" }] })
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body: requestBody,
    magpie: { model: "GLM-5.1-Highspeed", session: "task" } })
  expect(result.status).toBe(200)
  expect(seen.find((r) => r.path.endsWith("/messages")).path).toBe("/api/anthropic/v1/messages")
})

test("native GLM reasoning preserves model-specific off behavior and fits effort levels", async () => {
  const { loader, seen } = await setup()
  for (const example of [
    { model: "GLM-5.2", thinking: { type: "disabled", extra: "kept" }, output_config: { effort: "none", format: "keep" }, want: { thinking: { type: "disabled", extra: "kept" }, output_config: { format: "keep" } } },
    { model: "GLM-5.3", thinking: { type: "disabled" }, want: { thinking: { type: "enabled" }, output_config: { effort: "low" } } },
    { model: "GLM-5.3", thinking: { type: "adaptive", budget_tokens: 8192 }, want: { thinking: { type: "enabled" }, output_config: { effort: "high" } } },
    { model: "GLM-5.2", output_config: { effort: "low" }, want: { thinking: { type: "enabled" }, output_config: { effort: "high" } } },
  ]) {
    const { want, ...request } = example
    await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body: JSON.stringify({ ...request, messages: [] }) })
    const sent = seen.filter((r) => r.path.endsWith("/messages")).at(-1)
    const actual = JSON.parse(sent.body)
    expect(actual.thinking).toEqual(want.thinking)
    expect(actual.output_config).toEqual(want.output_config)
  }
})

test("native aliases retain raw model ID and require a matching execution", async () => {
  const { hooks, slot, loader, context } = await setup()
  _internal.config({ config: {
    providerConfigRules: { templateRules: [], providerRules: [{ providerId: "account:zai-individual-coding-plan", config: { builtinModelIds: ["GLM-5.3"] } }] },
    modelConfigRules: { modelRules: [{ modelMatch: ".*", config: { enabled: true } }], modelApiRules: [], providerSiteRules: [],
      templateModelRules: [], builtinProviderModelRules: [] },
  } })
  const listed = await hooks.provider.models({ models: {} }, { getAuth: slot.getAuth })
  expect(listed["GLM-5.3-OffPeak"].api.id).toBe("GLM-5.3")
  expect(listed["GLM-5.3-Highspeed"].api.id).toBe("GLM-5.3")
  await expect(loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-OffPeak", session: "task" } })).rejects.toThrow("admitted execution")
  await expect(loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST",
    body: JSON.stringify({ model: "GLM-5.3-OffPeak", messages: [] }) })).rejects.toThrow("host-bound")
  await expect(loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-OffPeak", session: "task", execution: { lane: "highspeed", scope: context.scope, generation: context.generation, taskId: "task", rawModel: "GLM-5.3" } } })).rejects.toThrow("does not match")
})

test("Off-Peak binds model, account and original session while using distinct credentials", async () => {
  const { loader, context, seen } = await setup()
  const execution = { lane: "offpeak", scope: context.scope, generation: context.generation, taskId: "task-1", rawModel: "GLM-5.3", ticketId: "ticket" }
  const res = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-OffPeak", session: "task-1", execution } })
  expect(res.status).toBe(200)
  const sent = seen.find((r) => r.path.includes("/off-peak/anthropic/"))
  expect(sent.headers.get("authorization")).toBe("Bearer zcode-jwt")
  expect(sent.headers.get("x-coding-plan-api-key")).toBe("current-pat")
  expect(sent.headers.get("x-off-peak-ticket-id")).toBe("ticket")
  expect(JSON.parse(sent.body).model).toBe("GLM-5.3")
  expect(sent.magpie).toBeUndefined()
})

test("an expired highspeed card requests ordinary admission without sending paid inference", async () => {
  const { loader, context, seen } = await setup()
  const execution = { lane: "highspeed", scope: context.scope, generation: context.generation, taskId: "task-2", rawModel: "GLM-5.3", cardId: "old", expiresAt: Date.now() - 1 }
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-Highspeed", session: "task-2", execution } })
  expect(result.status).toBe(409)
  expect(result.headers.get("x-magpie-zcode-fallback")).toBe("ordinary")
  expect(result.headers.get("x-magpie-zcode-fallback-model")).toBe("GLM-5.3")
  for (const name of ["authorization", "x-api-key", "x-coding-plan-api-key"]) expect(result.headers.has(name)).toBe(false)
  expect(seen.some((r) => r.path === "/api/anthropic/v1/messages")).toBe(false)
  expect(seen.some((r) => r.path.includes("/highspeed/anthropic/"))).toBe(false)
  expect(seen.some((r) => r.path.endsWith("/access_tokens"))).toBe(false)
})

test("a rejected highspeed request emits only an admission hint, never an ordinary replay", async () => {
  const { loader, context, seen } = await setup(() => Response.json({ type: "error", error: { message: "unavailable" } }, { status: 503 }))
  const execution = { lane: "highspeed", scope: context.scope, generation: context.generation, taskId: "task-3", rawModel: "GLM-5.3", cardId: "card", expiresAt: Date.now() + 60_000 }
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-Highspeed", session: "task-3", execution } })
  expect(result.status).toBe(409)
  expect(result.headers.get("x-magpie-zcode-fallback")).toBe("ordinary")
  expect(result.headers.get("x-magpie-zcode-fallback-model")).toBe("GLM-5.3")
  expect(seen.some((r) => r.path.includes("/highspeed/anthropic/"))).toBe(true)
  expect(seen.some((r) => r.path === "/api/anthropic/v1/messages")).toBe(false)
})

test("an accepted native response cannot spoof an internal fallback hint", async () => {
  const { loader, context, seen } = await setup(() => Response.json({ type: "message", content: [{ type: "text", text: "accepted" }] }, { headers: {
    "X-Magpie-ZCode-Fallback": "ordinary", "X-Magpie-ZCode-Fallback-Model": "GLM-5.3",
  } }))
  const execution = { lane: "highspeed", scope: context.scope, generation: context.generation, taskId: "task-4", rawModel: "GLM-5.3", cardId: "card", expiresAt: Date.now() + 60_000 }
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-Highspeed", session: "task-4", execution } })
  expect(result.status).toBe(200)
  expect(result.headers.has("x-magpie-zcode-fallback")).toBe(false)
  expect(result.headers.has("x-magpie-zcode-fallback-model")).toBe(false)
  expect((await result.json()).content[0].text).toBe("accepted")
  expect(seen.some((r) => r.path === "/api/anthropic/v1/messages")).toBe(false)
})

test("visible highspeed output is retained instead of requesting fallback", async () => {
  const stream = 'data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"visible"}}\n\ndata: {"type":"error","error":{"message":"late failure"}}\n\n'
  const { loader, context, seen } = await setup(() => new Response(stream, { headers: { "content-type": "text/event-stream" } }))
  const execution = { lane: "highspeed", scope: context.scope, generation: context.generation, taskId: "task-5", rawModel: "GLM-5.3", cardId: "card", expiresAt: Date.now() + 60_000 }
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-Highspeed", session: "task-5", execution } })
  expect(result.status).toBe(200)
  expect(result.headers.has("x-magpie-zcode-fallback")).toBe(false)
  expect(await result.text()).toBe(stream)
  expect(seen.some((r) => r.path === "/api/anthropic/v1/messages")).toBe(false)
})

test("a highspeed message_start followed by an error does not replay an accepted request", async () => {
  const stream = 'data: {"type":"message_start","message":{"content":[]}}\n\ndata: {"type":"error","error":{"message":"late failure"}}\n\n'
  const { loader, context, seen } = await setup(() => new Response(stream, { headers: { "content-type": "text/event-stream" } }))
  const execution = { lane: "highspeed", scope: context.scope, generation: context.generation, taskId: "task-accepted", rawModel: "GLM-5.3", cardId: "card", expiresAt: Date.now() + 60_000 }
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-Highspeed", session: "task-accepted", execution } })
  expect(result.status).toBe(200)
  expect(result.headers.has("x-magpie-zcode-fallback")).toBe(false)
  expect(await result.text()).toBe(stream)
  expect(seen.some((r) => r.path === "/api/anthropic/v1/messages")).toBe(false)
})

test("Off-Peak failures never request ordinary paid fallback", async () => {
  const { loader, context, seen } = await setup(() => Response.json({ type: "error", error: { message: "unavailable" } }, { status: 503 }))
  const execution = { lane: "offpeak", scope: context.scope, generation: context.generation, taskId: "task-6", rawModel: "GLM-5.3", ticketId: "ticket" }
  const result = await loader.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", body,
    magpie: { model: "GLM-5.3-OffPeak", session: "task-6", execution } })
  expect(result.status).toBe(503)
  expect(result.headers.has("x-magpie-zcode-fallback")).toBe(false)
  expect(seen.some((r) => r.path === "/api/anthropic/v1/messages")).toBe(false)
})
