// Start Plan requests use the in-flight gate and return terminal
// concurrency refusals without replay. The local fixture counts requests;
// nothing reaches z.ai.
import { afterAll, afterEach, beforeAll, beforeEach, expect, test } from "bun:test"
import { homedir, tmpdir } from "node:os"
import { realpathSync } from "node:fs"
import { ZCodeAuthPlugin as providerPlugin, _internal } from "../index.mjs"
import { hostedPlugin } from "./account-fixtures.mjs"

const ZCodeAuthPlugin = (...args) => hostedPlugin(providerPlugin, ...args)

const { startGate, busyHold } = _internal
const GATE = { ...startGate, lanes: undefined }

let server
const got = [] // every request the fake took: when, whose
let answer = null // (req) => Response, what the fake answers
const now = { flight: new Map(), peak: new Map() } // in flight per account
beforeAll(() => {
  if (![tmpdir(), realpathSync(tmpdir())].some((t) => homedir().startsWith(t))) throw new Error("run with HOME=$(mktemp -d) bun test")
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      const who = req.headers.get("authorization") ?? ""
      const path = new URL(req.url).pathname
      const n = (now.flight.get(who) ?? 0) + 1
      now.flight.set(who, n)
      now.peak.set(who, Math.max(now.peak.get(who) ?? 0, n))
      got.push({ at: Date.now(), who, path, flight: n })
      try {
        await req.text()
        return answer ? await answer({ who, flight: n, path }) : Response.json({ type: "message", content: [] })
      } finally {
        now.flight.set(who, now.flight.get(who) - 1)
      }
    },
  })
})
afterAll(() => server?.stop(true))

const real = globalThis.fetch
const HOSTS = ["https://zcode.z.ai", "https://api.z.ai", "https://open.bigmodel.cn"]
beforeEach(() => {
  globalThis.fetch = (input, init) => {
    const url = input instanceof Request ? input.url : String(input)
    const host = HOSTS.find((h) => url.startsWith(h + "/"))
    if (!host) throw new Error("no network in tests: " + url)
    return real(`http://127.0.0.1:${server.port}/${new URL(host).host}${url.slice(host.length)}`, init)
  }
  got.length = 0
  answer = null
  now.flight.clear()
  now.peak.clear()
  _internal.routes.clear()
  _internal.blocked.clear()
})
afterEach(() => {
  globalThis.fetch = real
  startGate.inflight = GATE.inflight
  startGate.wait = GATE.wait
  startGate.lanes.clear()
  busyHold.until = 0
  busyHold.hits.length = 0
  busyHold.now = () => Date.now()
  _internal.edge.until = 0
})

const DEVICE = "11111111-2222-4333-8444-555555555555"
const jwtOf = (n) => ["{}", JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, n })].map((s) => Buffer.from(s).toString("base64url")).join(".") + ".sig"
const startAuth = (n = 1) => {
  const jwt = jwtOf(n)
  return { type: "oauth", access: jwt, refresh: JSON.stringify({ site: "zai", device: DEVICE, jwt, plan: "Start Plan" }), expires: 0 }
}
const planAuth = () => ({ type: "api", key: "one.secret", metadata: { site: "zai" } })
const BODY = JSON.stringify({ model: "GLM-5.3", max_tokens: 16, messages: [{ role: "user", content: "hi" }] })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const CAP = () => Response.json({ code: 3008, msg: "user concurrency limit exceeded" }, { status: 429 })
const OK = () => Response.json({ type: "message", content: [] })
const messages = () => got.filter((r) => r.path.endsWith("/messages"))

async function loader(auth) {
  const hooks = await ZCodeAuthPlugin({ client: { auth: { set: async () => {} } } })
  return hooks.auth.loader(async () => auth, { id: "zcode" })
}

// one sends a request through opts and reads its answer to the end, as an
// agent's client does
async function one(opts, signal) {
  const res = await opts.fetch("https://api.z.ai/api/anthropic/v1/messages", {
    method: "POST",
    headers: { "content-type": "application/json", "x-api-key": "zcode", "anthropic-version": "2023-06-01" },
    body: BODY,
    signal,
  })
  return { status: res.status, text: await res.text(), retryAfter: res.headers.get("retry-after") }
}

test("a terminal cap goes back to magpie without replay", async () => {
  answer = CAP
  const opts = await loader(startAuth())
  const res = await one(opts)
  expect(res.status).toBe(429)
  expect(messages().length).toBe(1)
  expect(JSON.parse(res.text).code).toBe(3008)
})

test("3010 is a cap too, as ZCode's client reads it", async () => {
  let n = 0
  answer = () => (++n === 1 ? Response.json({ code: "3010", msg: "busy" }, { status: 429 }) : OK())
  const res = await one(await loader(startAuth()))
  expect(res.status).toBe(429)
  expect(messages().length).toBe(1)
})

test("twelve Start Plan requests at once: three of an account in flight, the rest wait their turn", async () => {
  answer = async () => {
    await sleep(60)
    return OK()
  }
  const opts = await loader(startAuth())
  const rs = await Promise.all(Array.from({ length: 12 }, () => one(opts)))
  expect(rs.every((r) => r.status === 200)).toBe(true)
  // every request sent once, never more than three at once (0.1.55: 12)
  expect(messages().length).toBe(12)
  expect(Math.max(...now.peak.values())).toBe(3)
})

test("each account has slots of its own", async () => {
  answer = async () => {
    await sleep(60)
    return OK()
  }
  const [a, b] = await Promise.all([loader(startAuth(1)), loader(startAuth(2))])
  const rs = await Promise.all([...Array.from({ length: 5 }, () => one(a)), ...Array.from({ length: 5 }, () => one(b))])
  expect(rs.every((r) => r.status === 200)).toBe(true)
  expect([...now.peak.values()]).toEqual([3, 3])
})

test("a refused request releases its slot without a replay", async () => {
  const opts = await loader(startAuth())
  let n = 0
  answer = () => (++n <= 3 ? CAP() : OK())
  const rs = await Promise.all(Array.from({ length: 4 }, () => one(opts)))
  expect(rs.filter((r) => r.status === 429)).toHaveLength(3)
  expect(rs.filter((r) => r.status === 200)).toHaveLength(1)
  expect(Math.max(...now.peak.values())).toBeLessThanOrEqual(3)
  const asks = messages()
  expect(asks.length).toBe(4)
})

test("a wait that runs out is answered here, plainly, and nothing is sent", async () => {
  startGate.wait = 150
  let free = () => {}
  const held = new Promise((r) => (free = r))
  answer = async () => {
    await held
    return OK()
  }
  const opts = await loader(startAuth())
  const first = Array.from({ length: 3 }, () => one(opts))
  while (messages().length < 3) await sleep(5)
  const t0 = Date.now()
  const late = await one(opts)
  expect(Date.now() - t0).toBeGreaterThanOrEqual(140)
  expect(late.status).toBe(429)
  expect(late.retryAfter).toBe("5")
  const e = JSON.parse(late.text).error
  expect(e.type).toBe("rate_limit_error")
  expect(e.message).toContain("serves this account 3 requests at a time")
  expect(e.message).toContain("was not sent")
  expect(messages().length).toBe(3)
  free()
  expect((await Promise.all(first)).every((r) => r.status === 200)).toBe(true)
  // the slots are back: the next request goes out at once
  expect((await one(opts)).status).toBe(200)
  expect(messages().length).toBe(4)
})

test("an agent that gives up while waiting leaves the queue", async () => {
  let free = () => {}
  const held = new Promise((r) => (free = r))
  answer = async () => {
    await held
    return OK()
  }
  const opts = await loader(startAuth())
  const first = Array.from({ length: 3 }, () => one(opts))
  while (messages().length < 3) await sleep(5)
  const ctl = new AbortController()
  const gone = one(opts, ctl.signal)
  const next = one(opts)
  await sleep(20)
  ctl.abort()
  await expect(gone).rejects.toThrow()
  free()
  await Promise.all(first)
  expect((await next).status).toBe(200)
  // the request that gave up was never sent; the one behind it was
  expect(messages().length).toBe(4)
  expect(startGate.lanes.size).toBe(0)
})

test("a stream the agent stops reading lets its slot go", async () => {
  answer = () =>
    new Response(
      new ReadableStream({
        pull: async (c) => {
          await sleep(20)
          c.enqueue(new TextEncoder().encode("event: ping\ndata: {}\n\n"))
        },
      }),
      { headers: { "content-type": "text/event-stream" } },
    )
  const opts = await loader(startAuth())
  const send = () => opts.fetch("https://api.z.ai/api/anthropic/v1/messages", { method: "POST", headers: { "content-type": "application/json" }, body: BODY })
  const streams = await Promise.all([send(), send(), send()])
  const fourth = send()
  await sleep(50)
  expect(messages().length).toBe(3)
  const r = streams[0].body.getReader()
  await r.read()
  await r.cancel()
  const res = await fourth
  expect(res.status).toBe(200)
  expect(messages().length).toBe(4)
  for (const s of [res, ...streams.slice(1)]) await s.body.cancel()
})

test("the coding plan keeps no gate of the plugin's", async () => {
  answer = async () => {
    await sleep(60)
    return OK()
  }
  const opts = await loader(planAuth())
  const rs = await Promise.all(Array.from({ length: 8 }, () => one(opts)))
  expect(rs.every((r) => r.status === 200)).toBe(true)
  expect(Math.max(...now.peak.values())).toBe(8)
  expect(startGate.lanes.size).toBe(0)
})

test("isolated caps return without an automatic retry", async () => {
  const opts = await loader(startAuth())
  let n = 0
  answer = () => (n++ % 2 === 0 ? CAP() : OK())
  for (let i = 0; i < 4; i++) expect((await one(opts)).status).toBe(i % 2 === 0 ? 429 : 200)
  expect(busyHold.until).toBe(0)
  expect(messages().length).toBe(4)
})

test("five separate terminal caps hold the Start Plan in plain words", async () => {
  const opts = await loader(startAuth())
  answer = CAP
  for (let i = 0; i < 4; i++) expect((await one(opts)).status).toBe(429)
  expect(busyHold.until).toBe(0)
  const fifth = await one(opts)
  expect(fifth.status).toBe(429)
  expect(messages().length).toBe(5)
  expect(busyHold.until).toBeGreaterThan(Date.now())
  const sent = messages().length
  const held = await one(opts)
  expect(held.status).toBe(429)
  expect(messages().length).toBe(sent)
  const said = JSON.parse(held.text).error.message
  expect(said).toContain("ZCode turned away 5 requests in the last minute as too many requests at once")
  expect(said).toContain("so that ZCode's firewall does not block this network".replace(/^s/, "S"))
  expect(said).toContain("Wait, run fewer agents or subagents on ZCode at once, or use a GLM Coding Plan model")
})

