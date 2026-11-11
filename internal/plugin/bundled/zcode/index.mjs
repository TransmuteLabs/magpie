// ZCode's GLM Coding Plan, as an OpenCode provider plugin.
// Community plugin attribution and MIT terms are in LICENSE.MIT and NOTICE.
// Magpie adaptations follow zai-org/ZCode 3.18.0; Copyright 2026 Z.AI Co., Ltd.,
// Apache-2.0. See LICENSE.ZCode and NOTICE.ZCode.
//
// ZCode signs in to a Z.ai or BigModel account through polling or a
// state-checked callback. Paid account requests use short-lived project
// tokens; Start uses the separate ZCode session. Legacy API keys remain
// supported without pretending they are account login credentials.

import { createDecipheriv, createHash } from "node:crypto"
import { existsSync, readFileSync, readdirSync } from "node:fs"
import { arch, homedir, release, userInfo } from "node:os"
import { isAbsolute, join } from "node:path"
import PROMPT from "./prompt.mjs"
import { accountScope, createAccountRuntime } from "./account.mjs"
import { ZCODE_VERSION } from "./project-token.mjs"
import { readNativeUsage, nativeUsageOperation, nativeUsageOperations, startBucketModels, startBalanceNow } from "./usage.mjs"
import { resetOperation, resetOperations } from "./reset.mjs"
import { nativeLaneOperation, nativeLaneOperations, runNativeLane, routeCodingPlanGateway, trustedOfficialMcpURL } from "./lanes.mjs"

const PROVIDER = "zcode"
// FREE_FIRST is magpie's switch for free first in the provider's editor
const FREE_FIRST = "free_first"
const ZCODE = "https://zcode.z.ai"
const ZAI_API = "https://api.z.ai"
const BIGMODEL_API = "https://bigmodel.cn" // BigModel's business API, where ZCode asks it
const ZAI_BASE = "https://api.z.ai/api/anthropic"
const BIGMODEL_BASE = "https://open.bigmodel.cn/api/anthropic"
const START_BASE = ZCODE + "/api/v1/zcode-plan/anthropic"
// APP_VERSION is the ZCode this signs in and asks as, on every call. ZCode
// gates by it, including Start Plan and marketing requests.
const APP_VERSION = ZCODE_VERSION
const UA = "ZCode/" + APP_VERSION

// ZCode's models before its config is read; the Start Plan has all but GLM-5.3.
const MODELS = [
  { id: "GLM-5.3", context: 1_000_000, output: 128_000, efforts: ["low", "high", "max"] },
  { id: "GLM-5.3-Flash", context: 1_000_000, output: 128_000, image: true, video: true, pdf: true, efforts: ["low", "high", "max"] },
  { id: "GLM-5.2", context: 1_000_000, output: 128_000, efforts: ["none", "high", "max"] },
  { id: "GLM-5-Turbo", context: 200_000, output: 64_000, efforts: ["none", "high"] },
]
const START_MODELS = MODELS.filter((m) => m.id !== "GLM-5.3")
const CODING_MODELS = MODELS.filter((m) => m.id === "GLM-5.3" || m.id === "GLM-5.3-Flash")

const SITES = {
  zai: { name: "Z.ai", api: ZAI_API, base: ZAI_BASE, subscribe: "z.ai/subscribe" },
  bigmodel: { name: "BigModel", api: BIGMODEL_API, base: BIGMODEL_BASE, subscribe: "bigmodel.cn/glm-coding" },
}
const siteOf = (s) => (s === "bigmodel" ? "bigmodel" : "zai")

const platform = () => `${process.platform}-${process.arch}` // darwin-arm64, win32-x64, linux-x64, as ZCode names them
const uuid = () => crypto.randomUUID()
const anyDevice = uuid() // X-Device-Mid for calls made signed out (the model list)
// uuidOf is a version 4 shaped UUID made from a seed: the same seed gives
// the same UUID every time
const uuidOf = (seed) => {
  const h = createHash("sha256").update(seed).digest("hex")
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-4${h.slice(13, 16)}-${((parseInt(h[16], 16) & 3) | 8).toString(16)}${h.slice(17, 20)}-${h.slice(20, 32)}`
}
// keyDevice is a device id for an account known only by its key: the same
// key gives the same id on every start, as a sign-in's saved device does
const keyDevice = (key) => uuidOf("zcode-device\0" + key)
const first = (...ss) => ss.map((s) => (typeof s === "string" ? s.trim() : "")).find((s) => s) ?? ""

// STATUS_TEXT is Go's http.StatusText, which magpie's built-in said a
// refusal with no message in.
const STATUS_TEXT = { 400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed",
  408: "Request Timeout", 429: "Too Many Requests", 500: "Internal Server Error", 502: "Bad Gateway", 503: "Service Unavailable",
  504: "Gateway Timeout" }

class ZError extends Error {
  constructor(message, status) {
    super(message)
    this.status = status
  }
}

async function untilAbort(promise, signal) {
  signal.throwIfAborted()
  let stop
  const aborted = new Promise((_, reject) => {
    stop = () => reject(signal.reason ?? new DOMException("Aborted", "AbortError"))
    signal.addEventListener("abort", stop, { once: true })
  })
  try { return await Promise.race([promise, aborted]) }
  finally { signal.removeEventListener("abort", stop) }
}

async function readResponseText(res, signal) {
  if (!res.body) return ""
  const deadline = AbortSignal.timeout(15_000)
  const bounded = signal ? AbortSignal.any([signal, deadline]) : deadline
  bounded.throwIfAborted()
  const reader = res.body.getReader(), decoder = new TextDecoder()
  let text = "", size = 0
  try {
    for (;;) {
      const part = await untilAbort(reader.read(), bounded)
      if (part.done) break
      size += part.value.byteLength
      if (size > 4 * 1024 * 1024) throw new Error("ZCode response body is too large")
      text += decoder.decode(part.value, { stream: true })
    }
    return text + decoder.decode()
  } catch (error) {
    void reader.cancel(error).catch(() => {})
    throw error
  } finally {
    reader.releaseLock()
  }
}

// call asks one of Z.ai's JSON endpoints, which wrap what they say in
// {code, msg, data}: a code of 0 or 200 is a success. zcode.z.ai is told
// the machine's id (X-Device-Mid), as ZCode tells it.
async function call(method, url, { auth, body, headers, device, signal, withHeaders = false } = {}) {
  const h = { "Content-Type": "application/json", Accept: "application/json", "User-Agent": UA }
  if (auth) h.Authorization = auth
  if (url.startsWith(ZCODE + "/")) h["X-Device-Mid"] = device || anyDevice
  Object.assign(h, headers ?? {})
  const deadline = AbortSignal.timeout(15_000)
  const bounded = signal ? AbortSignal.any([signal, deadline]) : deadline
  bounded.throwIfAborted()
  const res = await untilAbort(fetch(url, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body), signal: bounded, redirect: "error" }), bounded)
  const text = await untilAbort(res.text(), bounded)
  let env
  try {
    env = JSON.parse(text)
  } catch {}
  const code = env?.code == null ? "" : String(env.code)
  if (!res.ok) {
    const said = env?.msg ? (code && code !== "0" ? `${env.msg} (${res.status}, code ${code})` : `${env.msg} (${res.status})`) : (STATUS_TEXT[res.status] ?? `${res.status} ${res.statusText}`.trim())
    const err = new ZError(said, res.status)
    err.code = Number(code) || 0 // 3002 and the like: so a caller can tell a rate limit from a failure
    err.retryAfterMs = retryAfterMs(res) // 0 unless the reply carried a Retry-After
    throw err
  }
  if (!env || typeof env !== "object") throw new ZError("not JSON", res.status)
  if (env.success === false || code && code !== "0" && code !== "200") {
    const err = new ZError(env.msg || env.message || `error ${code}`, res.status)
    err.code = Number(code) || 0
    throw err
  }
  return withHeaders ? { data: env.data ?? null, headers: res.headers } : env.data ?? null
}

// ---- the account's plan ----------------------------------------------------------

const rootOf = (base) => {
  try {
    const u = new URL(base)
    return `${u.protocol}//${u.host}`
  } catch {
    return ZAI_API
  }
}
const isTeam = (s) => !!(s.org && s.project)

// plan names the coding plan a key has, "" for none.
async function plan(s, ctx) {
  await ctx?.assertCurrent()
  const subs = (await call("GET", rootOf(s.base) + "/api/biz/subscription/list", { auth: s.key, signal: ctx?.signal })) ?? []
  await ctx?.assertCurrent()
  return (Array.isArray(subs) ? subs : []).find((x) => String(x.status).toUpperCase() === "VALID")?.productName ?? ""
}

function jwtExpiry(jwt) {
  const p = String(jwt ?? "").split(".")
  if (p.length !== 3) return 0
  try {
    const claims = JSON.parse(Buffer.from(p[1], "base64url").toString("utf8"))
    return Number(claims.exp) > 0 ? Number(claims.exp) * 1000 : 0
  } catch {
    return 0
  }
}
const jwtExpired = (jwt) => {
  const t = jwtExpiry(jwt)
  return t > 0 && Date.now() > t
}
// EXPIRED is the built-in's words (errZCodeExpired) for a Start Plan
// sign-in past its end.
const EXPIRED = "ZCode's sign-in has expired; sign in to ZCode again (or add the account again in magpie)"

const num = (v) => {
  const n = typeof v === "string" ? parseFloat(v) : v
  return typeof n === "number" && Number.isFinite(n) ? n : undefined
}

// the alias is the catalog spelling of the model plus -trial, so the picker
// reads GLM-5.3-Trial while the loader strips it back to the raw name
const giftID = (m) => (MODELS.find((x) => x.id.toLowerCase() === String(m).toLowerCase())?.id ?? m) + "-Trial"

const giftNames = (g) => g?.allModels ?? g?.models ?? []
// wireModel is a gift model as ZCode sends it: the buckets name it lowercased
// ("glm-5.3-flash"), the app's request carries its registry id
// ("GLM-5.3-Flash"; normalizeOfficialGlmModelId's spelling). Names ZCode
// does not know go as the bucket wrote them.
const wireModel = (m) => MODELS.find((x) => x.id.toLowerCase() === String(m).toLowerCase())?.id ?? m
const giftModel = (g, id) => giftNames(g).find((m) => giftID(m).toLowerCase() === id.toLowerCase())
const bucketModels = startBucketModels

// Both the card and the request route use the same test for a spent bucket.
// Older balances may omit remaining_units but still tell used and total.
const giftSpent = (x) => {
  const left = num(x.remaining_units)
  if (left !== undefined) return left <= 0
  const total = num(x.total_units), used = num(x.used_units)
  return total !== undefined && used !== undefined && used >= total
}

// startPlan is the gift an account can spend now ({models}), as ZCode reads
// its balance: several plans can be active at once, and a model routes to
// the gift when any of them has a live bucket serving it — a bucket past
// its plan's own end, or one whose remaining_units says none, is skipped
// rather than asked and refused. models are those live buckets' names.
//
// coming (sign-in only) also takes a plan that has not started yet, as
// ZCode's own check does (resolveStartPlanBalanceAvailability's "pending"
// counts as entitled): {name, models: [], coming: true}. Requests and the
// card never ask for it: a plan not started serves nothing yet.
async function startPlan(jwt, device, coming = false, ctx) {
  if (!jwt) throw new Error("not signed in to ZCode")
  if (jwtExpired(jwt)) throw new Error(EXPIRED)
  await ctx?.assertCurrent()
  const response = await call("GET", `${ZCODE}/api/v1/zcode-plan/billing/balance?app_version=${APP_VERSION}`, { auth: "Bearer " + jwt, device, withHeaders: true, signal: ctx?.signal })
  await ctx?.assertCurrent()
  const b = response.data ?? {}
  const now = startBalanceNow(b, response.headers.get("date")) / 1000
  const same = (x, p) => (x.user_plan_id && p.user_plan_id ? x.user_plan_id === p.user_plan_id : String(x.plan_id ?? "") === String(p.plan_id ?? ""))
  const plans = Array.isArray(b.plans) ? b.plans : []
  const balances = Array.isArray(b.balances) ? b.balances : []
  const ms = new Set(), all = new Map()
  let ttl = 600_000
  let any = false
  let name = ""
  for (const p of plans) {
    const st = String(p?.status ?? "").trim().toLowerCase()
    const end = num(p?.ends_at)
    if (st !== "active" || (end > 0 && end <= now) || notStarted(p, now)) continue
    any = true
    if (end > 0) ttl = Math.min(ttl, (end - now) * 1000)
    if (!name) name = String(p?.name ?? "").trim()
    for (const x of balances) {
      if (!same(x, p)) continue
      const exp = num(x.expires_at)
      if (exp > 0 && exp <= now) continue
      if (exp > 0) ttl = Math.min(ttl, (exp - now) * 1000)
      // Spent buckets still declare models; only live ones can spend now.
      for (const m of bucketModels(x)) {
        if (!all.has(m.toLowerCase())) all.set(m.toLowerCase(), m)
        if (!giftSpent(x)) ms.add(m)
      }
    }
  }
  if (any) return { name: name || "Start Plan", models: [...ms], allModels: [...all.values()], ttl }
  if (!coming) return null
  const next = plans.filter((p) => notStarted(p, now) && !(num(p?.ends_at) > 0 && num(p?.ends_at) <= now))
    .sort((a, b) => startNum(a.starts_at) - startNum(b.starts_at))[0]
  return next ? { name: String(next.name ?? "").trim() || "Start Plan", models: [], allModels: [], ttl, coming: true } : null
}

// plansOf says where an account's requests go and what more it can spend:
// {start, gift}. start is what onStart said: an account with no coding plan
// sends every request to the gift and reads no balance to do it; a
// sign-in with no ZCode token and a key whose plan-list read found
// no gift stay on the coding plan. gift is what a coding account also holds:
// the gift plans ({models}), and its requests spend the gift for a model it
// serves, until it says it is spent (see blocked). Asked again after 10
// minutes (a minute when unsure — a plan-list read that failed is retried
// sooner; a gift read that failed simply leaves gift null until the next
// ask).
const routes = new Map()
function accountBoundaryFailure(error, ctx) {
  return ctx?.signal?.aborted || error?.name === "AbortError" ||
    ["account_scope_changed", "account_generation_changed"].includes(error?.kind)
}

async function plansOf(s, ctx) {
  await ctx?.assertCurrent()
  if (!s.jwt) return { start: false, gift: null }
  if (s.kind === "start") return { start: true, gift: null }
  const id = s.key + "\0" + s.jwt
  const r = routes.get(id)
  if (r && Date.now() - r.at < r.ttl) return r
  let start = false
  let gift = null
  let sure = false
  try {
    // A team seat stays on its team even when its personal plan list is
    // empty. Its ZCode session still holds the person's gift plans.
    if (s.kind === "legacy-key" && !isTeam(s) && !(await plan(s, ctx))) start = true
    else {
      // a gift read that failed is unknown, not "no gift": asked again in
      // a minute, so its trial entries come back with the next list (an
      // expired session is known: the card says to sign in again)
      let failed = false
      gift = await startPlan(s.jwt, s.device, false, ctx).catch((error) => {
        if (accountBoundaryFailure(error, ctx)) throw error
        failed = !jwtExpired(s.jwt)
        return null
      })
      sure = !failed
    }
    // a key whose plan list read no plan, where the last read found one,
    // is asked again in a minute rather than sent to the Start Plan for
    // ten: one list without the plan (a renewal's boundary, a hiccup)
    // turned a GLM Coding Plan account's every request into the Start
    // Plan's 405 until the route was read again (#369)
    if (start) sure = !(s.key && r && !r.start)
  } catch (error) {
    if (accountBoundaryFailure(error, ctx)) throw error
    // the plan list could not be read: a key account takes the dual route
    // (start stays false, the gift is kept, a minute's ttl), so a gift
    // refusal still replays to the coding plan rather than blacking out
    try {
      gift = await startPlan(s.jwt, s.device, false, ctx)
      sure = false
    } catch (error) { if (accountBoundaryFailure(error, ctx)) throw error }
  }
  await ctx?.assertCurrent()
  const out = { start, gift, at: Date.now(), ttl: Math.min(sure ? 600_000 : 60_000, gift?.ttl ?? 600_000) }
  routes.set(id, out)
  return out
}

// blocked remembers the models a gift said it has none left for: a key+jwt+
// model for a minute, so one turn that is refused does not ask again.
const blocked = new Map()
// sidelined remembers, the same way, the models whose gift turned free
// first away for another reason (a 405/3012, a block page): free first
// leaves them on the plan for the minute, but a -Trial entry, the user's
// own pick of the gift, still asks ZCode and gets its real answer. Once
// one rest served both, and a refusal under free first answered the
// Trial entry locally as a quota "exhausted" it never was (Can4ry_ on X).
const sidelined = new Map()
const rest = (map, k) => {
  for (const [x, until] of map) if (Date.now() >= until) map.delete(x)
  map.set(k, Date.now() + 60_000)
}

// giftServes: the gift's buckets name their models lowercased ("glm-5.3-
// flash"); what is asked for arrives as the plan lists it ("GLM-5.3-Flash").
const giftServes = (gift, m) => (gift.models ?? []).some((x) => x.toLowerCase() === m.toLowerCase())
const giftRested = (s, m) => Date.now() < (blocked.get(s.key + "\0" + s.jwt + "\0" + m.toLowerCase()) ?? 0)
const giftUsable = (s, gift, m) => !jwtExpired(s.jwt) && giftServes(gift, m) &&
  Date.now() >= (blocked.get(s.key + "\0" + s.jwt + "\0" + m.toLowerCase()) ?? 0)
const giftSidelined = (s, m) => Date.now() < (sidelined.get(s.key + "\0" + s.jwt + "\0" + m.toLowerCase()) ?? 0)

// spentUp is the gift saying it has nothing left to spend: its own no-
// resource-package code 1113, its "exceed quota limit" 1005 — a spent gift
// can answer a 200 with a JSON body carrying one of those codes. Every other
// 429 counts as spent, Z.ai's 1302/1303 rate limits among them; the plan's
// own in-flight cap (3008/3009) does not, since it is a turn not yet come.
function spentUp(text, status) {
  if (busy(status, text)) return false
  if (status === 429) return true
  let b
  try { b = JSON.parse(text) } catch { return false }
  if (!b || typeof b !== "object") return false
  const code = (v) => String(v ?? "")
  const c1 = code(b.code), c2 = code(b.error?.code)
  return c1 === "1113" || c1 === "1005" || c2 === "1113" || c2 === "1005"
}

const MODEL_RATE_CODES = new Set(["1302", "1303", "1304", "1305", "1308", "1310", "1313", "3002", "3008", "3009", "3010"])
const MODEL_TRANSIENT_CODES = new Set(["500", "1120", "1230", "1234", "1312", "2007"])

// Some gateways encode a failed business envelope as an SSE data frame rather
// than an Anthropic error event. Convert only unmistakable failures; forward
// all other frames unchanged, including anything already emitted.
function streamBusinessFailures(res) {
  if (!res.body || !(res.headers.get("content-type") ?? "").toLowerCase().includes("text/event-stream")) return res
  const decoder = new TextDecoder(), encoder = new TextEncoder()
  let pending = ""
  const frame = (source) => {
    const data = source.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n")
    let value
    try { value = JSON.parse(data) } catch { return source }
    if (!value || typeof value !== "object" || value.type === "error") return source
    const reason = value.rawFinishReason ?? value.finish_reason ?? value.finishReason ?? value.providerMetadata?.rawFinishReason
    const code = String(value.providerCode ?? value.code ?? value.error?.code ?? "")
    if (value.success !== false && reason !== "provider_success_false" &&
        !(value.type == null && code && code !== "0" && code !== "200")) return source
    const message = String(value.error?.message ?? value.msg ?? value.message ?? "Provider returned a business error.")
    const type = MODEL_RATE_CODES.has(code) || code === "1005" || code === "1113" ? "rate_limit_error" :
      MODEL_TRANSIENT_CODES.has(code) ? "overloaded_error" : "api_error"
    return `event: error\ndata: ${JSON.stringify({ type: "error", error: { type, message, ...(code ? { code } : {}) }, provider_error: value })}`
  }
  const body = res.body.pipeThrough(new TransformStream({
    transform(chunk, controller) {
      pending += decoder.decode(chunk, { stream: true })
      for (;;) {
        const boundary = /\r?\n\r?\n/.exec(pending)
        if (!boundary) break
        controller.enqueue(encoder.encode(frame(pending.slice(0, boundary.index)) + boundary[0]))
        pending = pending.slice(boundary.index + boundary[0].length)
      }
      // Do not hold an unbounded or incomplete frame in the proxy.
      if (pending.length > 1024 * 1024) {
        controller.enqueue(encoder.encode(pending))
        pending = ""
      }
    },
    flush(controller) {
      pending += decoder.decode()
      if (pending) controller.enqueue(encoder.encode(frame(pending)))
    },
  }))
  const headers = new Headers(res.headers)
  headers.delete("content-length")
  headers.delete("content-encoding")
  return new Response(body, { status: res.status, statusText: res.statusText, headers })
}

// A platform envelope may reject a model request inside HTTP 200. Preserve
// its diagnostics, but never report it as a successful model response.
// Streaming bodies remain streaming and are never replayed after output.
async function inferenceReply(res, signal) {
  if (res.status >= 400) return res
  if ((res.headers.get("content-type") ?? "").toLowerCase().includes("text/event-stream")) return streamBusinessFailures(res)
  if (!(res.headers.get("content-type") ?? "").includes("application/json")) return res
  const text = await readResponseText(res, signal)
  let body
  try { body = JSON.parse(text) } catch {}
  const code = String(body?.code ?? body?.error?.code ?? "")
  const failed = body && typeof body === "object" && body.type !== "message" &&
    (body.success === false || body.type === "error" || !!body.error || code && code !== "0" && code !== "200")
  let status = res.status
  if (failed) {
    status = spentUp(text, res.status) ? 429 : code === "1006" ? 401 : code === "3006" ? 404 :
      MODEL_RATE_CODES.has(code) ? 429 : MODEL_TRANSIENT_CODES.has(code) ? 503 : 400
  }
  const headers = new Headers(res.headers)
  headers.delete("content-length")
  headers.delete("content-encoding")
  return new Response(text, { status, ...(status === res.status ? { statusText: res.statusText } : {}), headers })
}

const isNum = (v) => typeof v === "number" && Number.isFinite(v)

// NO_PLAN_KEY is a key with no GLM Coding Plan behind it. ZCode's gift
// plans (its Start Plan, ZCode 1024 Dev Day) are served by zcode.z.ai
// to ZCode's own session token, which a sign-in with ZCode (the app's,
// or Z.ai's or BigModel's in the browser) carries and a key never does:
// with a key, a gift-only account has nothing to spend, and Z.ai answers
// each request 1113 (rtbeilin on X, 10-24).
const NO_PLAN_KEY =
  "this key's account has no GLM Coding Plan; ZCode's gift plans (the Start Plan, ZCode 1024 Dev Day) are spent only with ZCode's sign-in, never with a key — add the account with \"ZCode app's sign-in\" or a ZCode sign-in in the browser"

// noPlanKey is a coding plan's 1113 refusal (no plan, no resource
// package) to a sign-in with no ZCode session, its message followed by
// NO_PLAN_KEY; anything else goes on as it came. Read only once the
// answer has failed.
function noPlanKey(res, text) {
  let v
  try {
    v = JSON.parse(text)
  } catch {
    return null
  }
  // Z.ai names the code in error.code ("1113", as magpie's
  // zhipu_coding_test has it) or error.type (its 401's "1000"), BigModel's
  // envelope in code
  if (![v?.error?.code, v?.error?.type, v?.code].some((c) => String(c ?? "") === "1113")) return null
  const said = first(v?.error?.message, v?.msg, v?.message)
  const out = v?.error && typeof v.error === "object" ? { ...v, error: { ...v.error, message: (said ? said + " — " : "") + NO_PLAN_KEY } } : { ...v, msg: (said ? said + " — " : "") + NO_PLAN_KEY }
  const h = new Headers(res.headers)
  h.set("content-type", "application/json")
  return new Response(JSON.stringify(out), { status: res.status, statusText: res.statusText, headers: h })
}

// startNum reads a number the balance gives as a number or a string.
function startNum(v) {
  if (isNum(v)) return v
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v.trim())
    if (!Number.isNaN(n)) return n
  }
  return undefined
}

// notStarted is a plan whose starts_at is still to come (a Start Plan
// that takes effect at 23:00 today, windy on Discord): it serves nothing
// yet, so no request is routed to it and its buckets don't count, but the
// card names it with its start. ZCode's balance gives every plan a
// starts_at in Unix seconds; which status a plan not started yet carries
// is not known, so any but "expired" is taken.
function notStarted(p, now) {
  const at = startNum(p?.starts_at)
  return at > now && String(p?.status ?? "").trim().toLowerCase() !== "expired"
}

// ---- account and marketing observations -------------------------------------------

// The Start in-flight gate needs an account-specific fallback identity.
const holdKey = (s) => s.key || s.jwt
// retryAfterMs is a Retry-After header, in seconds or as an HTTP date, in
// ms; 0 when there is none or it doesn't read.
function retryAfterMs(res) {
  const v = first(res?.headers?.get?.("retry-after"))
  if (!v) return 0
  const secs = Number(v)
  if (Number.isFinite(secs) && secs > 0) return secs * 1000
  const at = Date.parse(v)
  return Number.isFinite(at) ? Math.max(0, at - Date.now()) : 0
}
// claimNeedsCaptcha says whether ZCode's claims want its security check
// now, from what client/configs says: its captcha (Aliyun's, which the
// ZCode app passes before every claim, invisibly as a rule, and sends as
// X-Aliyun-Captcha-Verify-Param, 3.17.2's requestVerificationClaimHeaders)
// switched on. No captcha in the configs is no check, as the app reads it
// (readRequestVerificationConfig gives null, and the claim goes without
// the header); one that isn't an object is unknown, null.
// skip_model_request is the same check for Start Plan requests, which it
// spares (true on 11-07); it is not read here.
function claimNeedsCaptcha(c) {
  const k = c?.configs?.captcha
  if (k == null) return false
  if (typeof k !== "object" || Array.isArray(k)) return null
  return k.enabled !== false
}
// The marketing feed and the claim are gated on the client's version:
// under an older version's header the feed answers empty (11-03, #678:
// the day's banner showed for 3.17.2 and for no older one), so they go,
// as every call does, as the current client (APP_VERSION) sends them.
const marketingHeaders = () => ({ "X-ZCode-App-Version": APP_VERSION, "X-Platform": platform(), "X-Client-Language": clientLanguage() })

// TOUCH is ZCode's marketing feed: the ZCode app's claim banners come
// from it. A banner plan ("ZCODE WEEKEND BUILD", 10-30) is served only
// while the app shows its banner, and the app claims it through the same
// /billing/claim (#503's capture of 10-30, kongbai01203: GET
// /api/v1/marketing/touch?seq=<n, counting up from 0> with the session,
// X-Device-Mid, X-Client-Language and X-ZCode-App-Version, answered
// data.deliveries[].banner.buttons[].action = {"type":"claim_zcode_plan",
// "args":{"plan_id":…}} and the plan's name in
// banner.background.args.zcode_plan.name). The v3.17 client reads this
// feed alone: the old preview polling is gone, and the rewards center's
// gifts are the rewards website's, claimed in the client.
const TOUCH = "/api/v1/marketing/touch"
const CLAIM_ACTION = "claim_zcode_plan"
// touchSeq is the feed's seq: ZCode's own count of its feed requests
// since the process started, from 0, one GET per refresh. It is not a
// page: "仅供服务端日志定位，不改变投放判断" (docs/marketing-touch-client.md,
// marketingTouchService.ts's bootRequestSeq, zai-org/ZCode@a9fe401), so a
// check-in asks once, never seq 0..8 in a row (plugins#676), and a banner
// seen at one seq and not another is a delivery arriving later.
const touchSeqKey = Symbol.for("magpie.zcode.marketingSeq")
const nextTouchSeq = () => {
  const n = Number.isSafeInteger(globalThis[touchSeqKey]) ? globalThis[touchSeqKey] : 0
  globalThis[touchSeqKey] = n + 1
  return n
}
// clientLanguage is the language ZCode names to marketing/touch: zh-CN or
// en-US, its two UI languages, never another (marketingTouchService's
// localeSchema), so a computer set to zh-Hans-CN or en-GB asks as ZCode does.
const clientLanguage = () => {
  let l = ""
  try {
    l = Intl.DateTimeFormat().resolvedOptions().locale || ""
  } catch {}
  return /^zh\b/i.test(l) ? "zh-CN" : "en-US"
}

// touchPlans is the plans marketing/touch's deliveries offer to claim:
// [{plan_id, name}], [] for a feed with no deliveries, null for one that
// doesn't read. Each delivery is searched for its claim_zcode_plan
// actions and its zcode_plan's name, wherever the banner keeps them, so
// a button moved inside the banner is still found.
function touchPlans(d) {
  if (d == null) return []
  if (typeof d !== "object" || Array.isArray(d)) return null
  if (d.deliveries == null) return []
  if (!Array.isArray(d.deliveries)) return null
  const out = []
  for (const dv of d.deliveries) {
    const ids = [], names = []
    const walk = (v, depth) => {
      if (!v || typeof v !== "object" || depth > 12) return
      if (Array.isArray(v)) return v.forEach((x) => walk(x, depth + 1))
      if (v.type === CLAIM_ACTION && typeof v.args?.plan_id === "string" && v.args.plan_id.trim()) ids.push(v.args.plan_id.trim())
      if (v.zcode_plan && typeof v.zcode_plan === "object" && typeof v.zcode_plan.name === "string") names.push(v.zcode_plan.name)
      for (const k of Object.keys(v)) walk(v[k], depth + 1)
    }
    walk(dv, 0)
    for (const id of ids) out.push({ plan_id: id, name: first(names[0] ?? "", id) })
  }
  return out
}

// ---- signing in ------------------------------------------------------------------

// Z.ai exchanges the upstream OAuth token for a distinct raw business JWT.


async function selectionCandidates(ctx, { allowIncompleteTeams = false } = {}) {
  const state = ctx.state
  const identity = { family: state.site, userId: state.zcodeUserId }
  const material = await ctx.loginMaterial({ business: "optional", zcode: "optional" })
  const selections = []
  const accepted = (r) => r.ok && r.data && r.data.success !== false &&
    (r.data.code == null || [0, 200, "0", "200"].includes(r.data.code))
  let start = "unknown"
  if (material.zcode) {
    try {
      const r = await ctx.request(`${state.origins.zcode}/api/v1/zcode-plan/billing/balance?app_version=${APP_VERSION}`,
        { headers: { Authorization: `Bearer ${material.zcode}`, "X-Device-Mid": state.device } })
      const data = r.data?.data
      if (accepted(r) && Array.isArray(data?.plans)) {
        const now = startBalanceNow(data, r.headers.get("date")) / 1000
        start = data.plans.some((p) => String(p.status ?? "").toLowerCase() === "active" &&
          !(num(p.ends_at) > 0 && num(p.ends_at) <= now)) ? "available" : "unavailable"
      }
    } catch (error) {
      if (ctx.signal?.aborted || ["account_scope_changed", "account_generation_changed"].includes(error?.kind)) throw error
    }
  }
  selections.push({ ...identity, kind: "start", name: "Start Plan", availability: start })

  let individual = "unknown", name = "GLM Coding Plan"
  if (material.business && state.uid && state.kind === "individual") {
    try {
      const { token } = await ctx.projectToken()
      const r = await ctx.request(`${state.origins.business}/api/biz/subscription/list`, { headers: { Authorization: token } })
      const list = r.data?.data
      if (accepted(r) && Array.isArray(list)) {
        const coding = list.filter((x) => [x?.productId, x?.productName].some((v) => typeof v === "string" && v.toLowerCase().includes("coding")))
        const active = coding.find((x) => x.status === "VALID" && x.inCurrentPeriod === true)
        individual = active ? "available" : coding.some((x) => typeof x.status !== "string" || typeof x.inCurrentPeriod !== "boolean") ? "unknown" : "unavailable"
        if (active) name = first(active.productName, name)
      }
    } catch (error) {
      if (ctx.signal?.aborted || ["account_scope_changed", "account_generation_changed"].includes(error?.kind)) throw error
    }
  }
  selections.push({ ...identity, kind: "individual", name, availability: individual })

  if (!material.business && !allowIncompleteTeams) throw new Error("account_selection_unavailable")
  if (material.business) {
    try {
      const headers = { Authorization: material.business, "Content-Type": "application/json" }
      const result = await ctx.request(`${state.origins.business}/api/biz/customer/getCustomerInfo`, { headers })
      const info = result.data?.data
      if (!accepted(result) || !Array.isArray(info?.organizations)) throw new Error("account_selection_unavailable")
      const projects = info.organizations.flatMap((o) => (Array.isArray(o.projects) ? o.projects : [])
        .filter((p) => String(p.projectType) === "2" && first(o.organizationId) && first(p.projectId))
        .map((p) => ({ organization: o.organizationId, project: p.projectId })))
      for (const p of projects) {
        const auth = state.site === "zai" ? `Bearer ${material.business}` : material.business
        const r = await ctx.request(`${state.origins.business}/api/biz/team/subscribe/product/querySubscribeDetail`, {
          headers: { Authorization: auth, "bigmodel-organization": p.organization, "bigmodel-project": p.project },
        })
        const d = r.data?.data
        if (!accepted(r) || !d || typeof d !== "object") throw new Error("account_selection_unavailable")
        const product = first(d.productId)
        if (d.hasSubscription === false && !product) continue
        if (!product) throw new Error("account_selection_unavailable")
        const availability = d.hasSubscription === false ? "unavailable" :
          d.hasSubscription !== true ? "unknown" :
            d.status === "EFFECTIVE" && d.memberGrantStatus === "VALID" ? "available" :
              d.status === "EXPIRED" || (d.status === "EFFECTIVE" && d.memberGrantStatus === "UNASSIGNED") ? "unavailable" : "unknown"
        selections.push({ ...identity, kind: "team", ...p, product, name: first(d.productName, "GLM Coding Team"), availability })
      }
    } catch (error) {
      if (!allowIncompleteTeams || ctx.signal?.aborted || ["account_scope_changed", "account_generation_changed"].includes(error?.kind)) throw error
    }
  }
  await ctx.assertCurrent()
  return selections
}

// Resolve a complete account selection without copying a permanent key.
async function signedIn(site, token, jwt, device, uid, refreshToken, onTeam = false, signal) {
  const s = { version: 1, site, uid, zcodeUserId: uid, device, kind: "individual", base: SITES[site].base,
    jwt, ...(site === "zai" ? { oauthToken: token } : { business: token.replace(/^Bearer\s+/i, "") }),
    ...(refreshToken ? { refreshToken } : {}) }
  // A provisional login has the same runtime and CAS semantics as a saved one,
  // but remains in memory until the host accepts the sign-in.
  let draft = { type: "oauth", refresh: JSON.stringify(s), access: jwt, uid }
  const provisional = createAccountRuntime({
    providerId: PROVIDER, stateOf, claimVerification: async () => null,
    client: { auth: { set: async ({ body, expected }) => {
      if (JSON.stringify(draft) !== JSON.stringify(expected)) return { data: { saved: false, conflict: true } }
      draft = body
      return { data: { saved: true, conflict: false } }
    } } },
  })
  const ctx = await provisional.capture(async () => draft, { signal })
  const candidates = await selectionCandidates(ctx, { allowIncompleteTeams: !onTeam })
  let chosen
  if (onTeam) {
    const teams = candidates.filter((x) => x.kind === "team" && x.availability === "available")
    if (teams.length !== 1) throw new Error(teams.length ? "multiple team plans are available; sign in to the account and choose a team explicitly" : "this account has no available team plan")
    chosen = teams[0]
  } else {
    chosen = candidates.find((x) => x.kind === "individual" && x.availability === "available") ??
      candidates.find((x) => x.kind === "start" && x.availability === "available") ??
      candidates.find((x) => x.kind === "individual" && x.availability === "unknown")
    if (!chosen) throw new Error("this account has no available GLM Coding or Start Plan")
  }
  const current = JSON.parse(draft.refresh)
  return { ...current, kind: chosen.kind, org: chosen.organization || "", project: chosen.project || "",
    product: chosen.product || "", plan: chosen.name || "", ...(chosen.kind !== "individual" ? { loc: undefined } : {}) }
}

// TEAM marks a team seat's account name beside the account's own.
const TEAM = "team"
const who = (u) => first(String(u?.email ?? "").replace(/@phone\.local$/, ""), u?.displayName, u?.username, u?.name, u?.user_id, u?.id, "ZCode")
// uidOf is the account's user id, kept beside its name: magpie tells two
// accounts of one name apart by it, a name being no one's own (wakkana on
// Discord, WorkBuddy's)
const uidOf = (u) => first(u?.user_id, u?.id)
// ZCode's stored profile is either the raw session user or a complete
// normalized OAuthUserProfile. An upstream business-only ID is not proof of
// the ZCode entitlement owner.
const zcodeProfileId = (u) => first(u?.user_id,
  typeof u?.username === "string" && typeof u?.displayName === "string" && u.id !== "unknown" ? u.id : "")
const sleep = (ms, signal) => new Promise((resolve, reject) => {
  if (signal?.aborted) return reject(signal.reason ?? new DOMException("Aborted", "AbortError"))
  const timer = setTimeout(() => { signal?.removeEventListener("abort", abort); resolve() }, ms)
  const abort = () => { clearTimeout(timer); reject(signal.reason ?? new DOMException("Aborted", "AbortError")) }
  signal?.addEventListener("abort", abort, { once: true })
})
const scopeOfState = (s) => accountScope({ ...s, origins: { business: SITES[s.site].api, zcode: ZCODE, zaiOAuth: "https://chat.z.ai" } })
const callbackRedirect = () => `${ZCODE}/app/oauth/login?` + new URLSearchParams({ redirect: "zcode://oauth/callback", app_version: APP_VERSION })

async function oauthRequest(method, url, { auth, body, signal } = {}) {
  const deadline = AbortSignal.timeout(15_000)
  const bounded = signal ? AbortSignal.any([signal, deadline]) : deadline
  bounded.throwIfAborted()
  const res = await untilAbort(fetch(url, { method, headers: { "Content-Type": "application/json", ...(auth ? { Authorization: auth } : {}) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }), signal: bounded, redirect: "error" }), bounded)
  let env
  try { env = await untilAbort(res.json(), bounded) } catch (error) { if (bounded.aborted) throw error }
  if (!res.ok) {
    const error = new Error(`ZCode sign-in HTTP ${res.status}`)
    error.status = res.status
    throw error
  }
  return env
}

async function completeLogin(site, data, device, onTeam, signal) {
  const token = site === "bigmodel" ? first(data?.bigmodel?.access_token, data?.bigmodel?.accessToken, data?.access_token, data?.accessToken) : first(data?.zai?.access_token)
  const jwt = first(data?.token)
  const uid = first(typeof data?.user?.user_id === "string" ? data.user.user_id : "")
  if (!token || !jwt || !uid) throw new Error("ZCode sign-in: incomplete credentials or user identity")
  const refreshToken = site === "bigmodel" ? first(data?.bigmodel?.refresh_token, data?.bigmodel?.refreshToken) : ""
  const s = await signedIn(site, token, jwt, device, uid, refreshToken, onTeam, signal)
  return { type: "success", refresh: JSON.stringify(s), access: jwt, expires: 0,
    accountId: onTeam ? `${who(data.user)} (${TEAM})` : who(data.user), uid, accountScope: scopeOfState(s) }
}

// authorize opens ZCode's sign-in flow on site; its callback polls it.
// onTeam signs in to the account's seat on a team's plan, named apart from
// the account's own plan (the host keeps one entry per account name).
async function authorize(site, log, onTeam = false) {
  const poll = "Bearer " + Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString("hex")
  const device = uuid()
  let envelope
  try {
    envelope = await oauthRequest("POST", ZCODE + "/api/v1/oauth/cli/init", { auth: poll, body: { provider: site }, signal: AbortSignal.timeout(15_000) })
  } catch (e) {
    throw new Error(`ZCode sign-in: ${e.message}`)
  }
  const flow = envelope?.data
  let u
  try {
    u = new URL(flow?.authorize_url)
  } catch {}
  const interval = typeof flow?.poll_interval_sec === "number" ? flow.poll_interval_sec * 1000 : NaN
  const serverDeadline = typeof flow?.expires_at === "number" ? flow.expires_at * 1000 : NaN
  if (envelope?.code !== 0 || !first(flow?.flow_id) || u?.protocol !== "https:" || !first(u.searchParams.get("state")) ||
      !Number.isFinite(serverDeadline) || !Number.isFinite(interval) || serverDeadline <= Date.now() ||
      interval < 1000 || interval >= serverDeadline - Date.now()) throw new Error("ZCode sign-in: invalid flow")
  u.searchParams.set(site === "bigmodel" ? "redirect" : "redirect_uri", callbackRedirect())
  const deadline = Math.min(serverDeadline, Date.now() + 5 * 60_000)
  let completion
  return {
    url: u.toString(),
    instructions: `Sign in to ${SITES[site].name} in the browser; this finishes on its own.`,
    method: "auto",
    callback(_code, { signal } = {}) {
      if (completion) return completion
      completion = (async () => {
      const fail = (msg) => (log(msg), { type: "failed", error: msg })
      for (;;) {
        if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
        if (Date.now() >= deadline) return fail("the sign-in expired; start again")
        let answer
        try {
          answer = await oauthRequest("GET", `${ZCODE}/api/v1/oauth/cli/poll/${encodeURIComponent(flow.flow_id)}`, {
            auth: poll, signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(Math.min(15_000, Math.max(1, deadline - Date.now())))]) : AbortSignal.timeout(Math.min(15_000, Math.max(1, deadline - Date.now()))),
          })
        } catch (e) {
          if (signal?.aborted) throw signal.reason ?? e
          if (e.status >= 400 && e.status < 500 && e.status !== 408 && e.status !== 429) return fail("ZCode sign-in: " + e.message)
          await sleep(Math.min(interval, Math.max(0, deadline - Date.now())), signal)
          continue
        }
        const got = answer?.data
        if (answer?.code !== 0 || !got || typeof got !== "object") return fail("ZCode sign-in: invalid polling response")
        if (got.status === "pending") { await sleep(Math.min(interval, Math.max(0, deadline - Date.now())), signal); continue }
        if (got.status === "failed") return fail(`the sign-in was declined on ${SITES[site].name}`)
        if (got.status !== "ready" || !first(got.user?.user_id)) return fail("ZCode sign-in: invalid ready response")
        try {
          return await completeLogin(site, got, device, onTeam, signal)
        } catch (e) {
          if (signal?.aborted) throw signal.reason ?? e
          return fail(e.message)
        }
      }
      })()
      return completion
    },
  }
}

function authorizeCallback(site, log) {
  const state = Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString("hex")
  const redirect = callbackRedirect()
  const u = new URL(site === "zai" ? "https://chat.z.ai/api/oauth/authorize" : "https://bigmodel.cn/login")
  const params = site === "zai"
    ? { redirect_uri: redirect, response_type: "code", client_id: "client_P8X5CMWmlaRO9gyO-KSqtg", state }
    : { redirect, appId: "zcode", state }
  for (const [k, v] of Object.entries(params)) u.searchParams.set(k, v)
  const deadline = Date.now() + 5 * 60_000
  let completion
  return {
    url: u.toString(), method: "code",
    instructions: "Sign in in the browser, then paste the complete callback URL.",
    callback(value, { signal } = {}) {
      if (completion) return completion
      completion = (async () => {
        try {
          const callback = new URL(value), expected = new URL(redirect)
          if (Date.now() >= deadline || callback.origin !== expected.origin || callback.pathname !== expected.pathname ||
              callback.username || callback.password || callback.searchParams.get("state") !== state ||
              [...expected.searchParams].some(([key, val]) => callback.searchParams.get(key) !== val))
            throw new Error("ZCode sign-in: callback URL or state does not match")
          const code = site === "bigmodel" ? first(callback.searchParams.get("authCode"), callback.searchParams.get("code")) :
            first(callback.searchParams.get("code"), callback.searchParams.get("authCode"))
          if (!code) throw new Error("ZCode sign-in: callback has no authorization code")
          const env = await oauthRequest("POST", `${ZCODE}/api/v1/oauth/token`, {
            body: { provider: site, code, redirect_uri: redirect, state },
            signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(15_000)]) : AbortSignal.timeout(15_000),
          })
          if ((site === "zai" && env?.code !== 0) || (site === "bigmodel" && env?.code !== undefined && env.code !== 0))
            throw new Error("ZCode sign-in: authorization exchange failed")
          const data = env?.data
          if (!first(data?.user?.user_id)) {
            const jwt = first(data?.token)
            if (!jwt) throw new Error("ZCode sign-in: missing session")
            const profile = await oauthRequest("GET", `${ZCODE}/api/v1/oauth/profile`, { auth: `Bearer ${jwt}`, signal })
            if (profile?.code !== 0 || !first(profile?.data?.user?.user_id)) throw new Error("ZCode sign-in: missing session user identity")
            data.user = profile.data.user
          }
          return await completeLogin(site, data, uuid(), false, signal)
        } catch (e) {
          if (signal?.aborted) throw signal.reason ?? e
          log(e.message)
          return { type: "failed", error: e.message }
        }
      })()
      return completion
    },
  }
}

// ---- ZCode's own sign-in ------------------------------------------------------------
// ZCode keeps its sign-in in ~/.zcode/v2/credentials.json, each value
// encrypted (AES-256-GCM, "enc:v1:<iv>.<tag>.<data>") with a key made from
// the machine's user; ~/.zcode/v2/setting.json says whether it is on a
// team's plan. An account signed in "as ZCode" reads them for every
// request, so it follows ZCode's own sign-in, its renewed session and its
// switches between plans, as magpie's built-in did (zcode.go zcodeOwn).
//
// ZCode 3.14 can move its data (数据存储路径, "data storage path"): it copies
// ~/.zcode/v2 to <folder>/.zcode/v2, all but setting.json, and from its next
// start reads credentials.json and runtime/provider from there. The choice
// stays in the home folder's setting.json as dataBaseDir, which ZCode's main
// process reads before anything else (readBootstrapDataBaseDirFromDisk);
// setting.json itself, and the team plan it names, stay in the home folder.

// settingDir is where ZCode keeps setting.json, moved or not.
const settingDir = () => join(homedir(), ".zcode", "v2")

// dataIn is the folder ZCode keeps its .zcode data in: the home folder,
// unless dataBaseDir moved it. A value that isn't an absolute path is
// ignored, as magpie's built-in does (internal/appdir ZCodeDataIn).
function dataIn() {
  const d = readJSON(join(settingDir(), "setting.json"))?.dataBaseDir
  const t = typeof d === "string" ? d.trim() : ""
  return t && isAbsolute(t) ? t : homedir()
}

// zcodeDir is where ZCode keeps credentials.json and runtime/provider.
const zcodeDir = () => join(dataIn(), ".zcode", "v2")

// secret is the key ZCode encrypts its credentials with.
function secret() {
  let seed = process.env.ZCODE_CREDENTIAL_SECRET
  if (!seed) {
    let name = ""
    try {
      name = userInfo().username
    } catch {}
    name = name.slice(name.lastIndexOf("\\") + 1) // DOMAIN\user
    seed = `zcode-credential-fallback:${process.platform}:${homedir()}:${name}`
  }
  return createHash("sha256").update(seed).digest()
}

function decrypt(key, v) {
  if (typeof v !== "string") return null
  if (!v.startsWith("enc:")) return v
  if (!v.startsWith("enc:v1:")) return null
  const parts = v.slice(7).split(".")
  if (parts.length !== 3) return null
  try {
    const [iv, tag, data] = parts.map((p) => Buffer.from(p.replace(/=+$/, ""), "base64url"))
    const d = createDecipheriv("aes-256-gcm", key, iv)
    d.setAuthTag(tag)
    return Buffer.concat([d.update(data), d.final()]).toString("utf8")
  } catch {
    return null
  }
}

function readJSON(path) {
  try {
    return JSON.parse(readFileSync(path, "utf8"))
  } catch {
    return null
  }
}

// ownTeam is the team plan ZCode switched to, when it did.
function ownSelection(set, site) {
  const selection = set?.providerFamilyConnectionSelections?.[site]
  if (selection?.kind === "start-plan") return { kind: "start" }
  if (selection?.kind === "individual-coding-plan") return { kind: "individual" }
  if (selection?.kind === "team-coding-plan") return { kind: "team", product: first(selection.productId),
    org: first(selection.organizationId), project: first(selection.projectId) }
  const legacy = first(set?.modelProviderFamilySelectedKeys?.[site])
  const prefix = `team-plan:builtin:${site}-coding-plan:`
  if (legacy.startsWith(prefix)) {
    try {
      const parts = legacy.slice(prefix.length).split(":").map(decodeURIComponent)
      if (parts.length === 3) return { kind: "team", product: parts[0], org: parts[1], project: parts[2] }
      if (parts.length === 2) return { kind: "team", product: parts[0], project: parts[1], org: "" }
    } catch {}
    return { kind: "invalid" }
  }
  return null
}

// configKey is the coding plan key ZCode's config.json gives its built-in
// coding plan provider, Z.ai's first, as ZCode lists them; ZCode reads it
// as it is, and one written encrypted is read too.
function configKey(key, site) {
  const providers = readJSON(join(zcodeDir(), "config.json"))?.provider
  if (!providers || typeof providers !== "object") return null
  for (const [ids, base] of [
    [["builtin:zai-coding-plan", "account:zai-individual-coding-plan"], ZAI_BASE],
    [["builtin:bigmodel-coding-plan", "account:bigmodel-individual-coding-plan"], BIGMODEL_BASE],
  ]) {
    if ((base === BIGMODEL_BASE ? "bigmodel" : "zai") !== site) continue
    for (const id of ids) {
      let k = providers[id]?.options?.apiKey
      if (typeof k !== "string") continue
      k = k.trim()
      if (k.startsWith("enc:")) k = first(decrypt(key, k))
      if (k.includes(".") && !/[\s{$]/.test(k)) return { key: k, base }
    }
  }
  return null
}

// Import only the active family's exact UID and selected project location.
const LOCATION = "project-key-location:v1:"
function ownLocation(store, key, site, uid, selection) {
  if (!uid) return null
  for (const [name, v] of Object.entries(store)) {
    if (!name.startsWith(LOCATION)) continue
    let at
    try {
      at = JSON.parse(decodeURIComponent(name.slice(LOCATION.length)))
    } catch {
      continue
    }
    if (!Array.isArray(at) || at[1] !== site || String(at[2] ?? "") !== uid || String(at[4]) !== "1") continue
    if (selection?.kind === "team") {
      if (at[3]?.organizationId !== selection.org || at[3]?.projectId !== selection.project) continue
    } else if (at[3] !== null || (at[5] && at[5] !== "non-team")) continue
    try { if (new URL(at[0]).origin !== SITES[site].api) continue } catch { continue }
    let loc
    try {
      loc = JSON.parse(decrypt(key, v) ?? "")
    } catch {}
    const org = first(loc?.organizationId), project = first(loc?.projectId), apiKeyId = first(loc?.apiKeyId)
    if (!org || !project || !apiKeyId) continue
    return { origin: SITES[site].api, organizationId: org, projectId: project, apiKeyId }
  }
  return null
}

// ownSignIn is what ZCode is signed in to now: the account, its coding
// plan key, ZCode's session token (the Start Plan's key) and the team plan
// it is switched to; null when it has none of them.
function ownSignIn() {
  const store = readJSON(join(zcodeDir(), "credentials.json"))
  if (!store || typeof store !== "object") return null
  const key = secret()
  const set = readJSON(join(settingDir(), "setting.json")) ?? {}
  const active = first(decrypt(key, store["oauth:active_provider"]))
  if (Object.hasOwn(store, "oauth:active_provider")) {
    if (!["zai", "bigmodel", "zai_cn"].includes(active)) return null
    let info = {}, linked = null
    try { info = JSON.parse(decrypt(key, store[`oauth:${active}:user_info`]) ?? "{}") } catch {}
    const uid = uidOf(info)
    const zcodeUserId = zcodeProfileId(info)
    const site = ["zai", "bigmodel"].includes(set.providerFamilyDomain) ? set.providerFamilyDomain :
      active === "zai" ? "zai" : "bigmodel"
    const selected = ownSelection(set, site)
    if (selected?.kind === "invalid") return null
    let accountIdentity = uid
    let token = active === site ? first(decrypt(key, store[`oauth:${active}:access_token`])) : ""
    try { linked = JSON.parse(decrypt(key, store["oauth:linked_bigmodel"]) ?? "null") } catch {}
    if (site === "bigmodel" && zcodeUserId && linked?.userId === zcodeUserId && first(linked.customerId) && first(linked.accessToken)) {
      token = first(linked.accessToken)
      accountIdentity = first(linked.customerId)
    }
    const jwt = first(decrypt(key, store.zcodejwttoken)).replace(/^Bearer\s+/i, "")
    token = token.replace(/^Bearer\s+/i, "")
    if (site === "bigmodel" && token === jwt) token = ""
    const legacyKey = !token && selected?.kind !== "start" && selected?.kind !== "team" ? configKey(key, site)?.key ?? "" : ""
    const kind = selected?.kind === "individual" && legacyKey ? "legacy-key" :
      selected?.kind ?? (token && uid ? "individual" : legacyKey ? "legacy-key" : "start")
    if (!token && !jwt && !legacyKey) return null
    const loc = ownLocation(store, key, site, accountIdentity, selected)
    const loginGeneration = first(decrypt(key, store["oauth:session_revision"]))
    const refreshToken = first(decrypt(key, store[`oauth:${active}:refresh_token`]))
    return { version: 1, source: "zcode", site, uid, ...(zcodeUserId ? { zcodeUserId } : {}), accountIdentity, user: who(info), loginProvider: active,
      base: SITES[site].base, device: anyDevice, kind, jwt,
      ...(site === "zai" ? { oauthToken: token } : { business: token }),
      ...(loginGeneration ? { loginGeneration } : {}), ...(refreshToken ? { refreshToken } : {}),
      ...(kind === "legacy-key" ? { key: legacyKey } : {}),
      ...(loc ? { loc } : {}), ...(selected ? { org: selected.org, project: selected.project, product: selected.product } : {}) }
  }
  const available = ["zai", "bigmodel"].filter((site) => decrypt(key, store[`oauth:${site}:user_info`]) ||
    decrypt(key, store[`oauth:${site}:access_token`]) ||
    Object.keys(store).some((name) => name.includes(`:${site}-individual-coding-plan:`) && name.endsWith(":api-key")) ||
    configKey(key, site)?.key)
  const site = active === "zai" || active === "bigmodel" ? active :
    (set.providerFamilyDomain === "zai" || set.providerFamilyDomain === "bigmodel") ? set.providerFamilyDomain : available.length === 1 ? available[0] : ""
  if (!site) return null
  let info = {}
  try { info = JSON.parse(decrypt(key, store[`oauth:${site}:user_info`]) ?? "{}") } catch {}
  const uid = uidOf(info)
  const zcodeUserId = zcodeProfileId(info)
  const selected = ownSelection(set, site)
  if (selected?.kind === "invalid") return null
  const jwt = first(decrypt(key, store.zcodejwttoken)).replace(/^Bearer\s+/i, "")
  let business = first(decrypt(key, store[`oauth:${site}:access_token`])).replace(/^Bearer\s+/i, "")
  if (site === "bigmodel" && business === jwt) business = ""
  const refreshToken = first(decrypt(key, store[`oauth:${site}:refresh_token`]))
  const loginGeneration = first(decrypt(key, store["oauth:session_revision"]))
  let legacyKey = ""
  for (const [name, v] of Object.entries(store)) {
    if (!name.includes(`:${site}-individual-coding-plan:`) || !name.endsWith(":api-key")) continue
    if (!uid || !name.includes(`:account:${uid}:`)) continue
    const k = decrypt(key, v)
    if (!k || !k.includes(".")) continue
    legacyKey = k
    break
  }
  if (!legacyKey) legacyKey = configKey(key, site)?.key ?? ""
  const kind = selected?.kind === "start" || selected?.kind === "team" ? selected.kind :
    business && uid ? "individual" : legacyKey ? "legacy-key" : "start"
  const loc = ownLocation(store, key, site, uid, selected)
  if (!jwt && !business && !legacyKey) return null
  return { version: 1, source: "zcode", site, uid, ...(zcodeUserId ? { zcodeUserId } : {}), user: who(info), base: SITES[site].base, device: anyDevice,
    kind, jwt, ...(site === "zai" ? { oauthToken: business } : { business }),
    ...(loginGeneration ? { loginGeneration } : {}), ...(refreshToken ? { refreshToken } : {}), ...(loc ? { loc } : {}),
    ...(selected ? { org: selected.org, project: selected.project, product: selected.product } : {}),
    ...(kind === "legacy-key" ? { key: legacyKey } : {}) }
}

// asZCode is the sign-in "as ZCode": what it reads is ZCode's own.
function asZCode() {
  return {
    url: "",
    instructions: "Uses the account the ZCode app is signed in to, and follows it while ZCode stays signed in to that account.",
    method: "auto",
    async callback(_code, { signal } = {}) {
      const own = ownSignIn()
      if (!own) return { type: "failed", error: "ZCode isn't signed in on this computer; sign in to ZCode, or sign in to Z.ai or BigModel here" }
      if (own.kind === "team" && !own.org && own.project && own.product && (own.business || own.oauthToken) && own.uid) {
        try {
          const deadline = signal ? AbortSignal.any([signal, AbortSignal.timeout(15_000)]) : AbortSignal.timeout(15_000)
          const login = own.site === "zai" ? await call("POST", `${SITES.zai.api}/api/auth/z/login`, {
            body: { token: own.oauthToken }, headers: { "User-Agent": UA }, signal: deadline,
          }) : null
          const business = own.site === "zai" ? first(login?.access_token, login?.accessToken) : own.business
          if (!business) throw new Error("ZCode business credential unavailable")
          const data = await call("GET", `${SITES[own.site].api}/api/biz/customer/getCustomerInfo`, { auth: business, signal: deadline })
          const now = ownSignIn()
          if (!now || now.uid !== own.uid || now.site !== own.site || now.business !== own.business ||
              now.oauthToken !== own.oauthToken || now.loginGeneration !== own.loginGeneration ||
              now.jwt !== own.jwt || now.project !== own.project || now.product !== own.product)
            throw new Error("ZCode account changed while resolving team identity")
          const matches = (data?.organizations ?? []).filter((o) => o.projects?.some((p) => p.projectId === own.project)).map((o) => first(o.organizationId)).filter(Boolean)
          if (new Set(matches).size === 1) own.org = matches[0]
        } catch (error) { if (signal?.aborted) throw signal.reason ?? error }
      }
      if (own.kind === "team" && (!own.org || !own.project || !own.product))
        return { type: "failed", error: "ZCode's selected team identity could not be resolved uniquely" }
      const { user, uid, ...s } = own
      const state = { ...s, uid }
      return { type: "success", refresh: JSON.stringify(state), access: s.key || s.jwt || "", expires: 0,
        accountId: user, ...(uid ? { uid } : {}), accountScope: scopeOfState(state) }
    },
  }
}

// stateOf is a stored sign-in as the requests need it.
function stateOf(auth, { follow = true } = {}) {
  if (auth?.type === "oauth") {
    let s = {}
    try {
      s = JSON.parse(auth.refresh)
    } catch {}
    if (follow && s.source === "zcode") {
      // ZCode's own, read now, while ZCode is signed in to this same
      // account. Signed out, or signed in to another account since, it is
      // what it was when this was signed in: another account's key would
      // send this account's requests as that one, and an expired session
      // says sign in again (EXPIRED) rather than borrowing another's.
      const own = ownSignIn()
      // (by user id where both have one: another account may share the name)
      const unbound = !first(s.uid, auth.uid, s.key, s.jwt, s.business, s.token, s.oauthToken)
      if (own && own.uid && (unbound || (own.uid === first(s.uid, auth.uid) && own.site === siteOf(s.site) &&
          own.kind === s.kind && (own.kind !== "team" ||
            (own.org === s.org && own.project === s.project && own.product === s.product))))) {
        const { user, ...now } = own
        s = { ...s, ...now, device: s.device }
      }
    }
    const site = siteOf(s.site)
    const key = s.key ?? (s.jwt ? "" : auth.access)
    const kind = s.kind || (key ? "legacy-key" : s.org && s.project ? "team" : "start")
    return { ...s, site, kind, uid: first(s.uid, auth.uid), base: s.base || SITES[site].base, key, device: s.device || (key ? keyDevice(key) : anyDevice) }
  }
  if (auth?.type === "api") {
    const site = siteOf(auth.metadata?.site)
    const key = String(auth.key ?? "").trim()
    return { site, kind: "legacy-key", uid: first(auth.uid), base: SITES[site].base, key, device: key ? keyDevice(key) : anyDevice }
  }
  return null
}

// ---- models ----------------------------------------------------------------------

// The provider's family and selected plan determine its own config rules.
const planID = (base, site = "zai", kind = "individual") =>
  base.includes("/zcode-plan/")
    ? `account:${site}-start-plan`
    : `account:${base.includes("bigmodel.cn") ? "bigmodel" : "zai"}-${kind === "team" ? "team" : "individual"}-coding-plan`

const efforts = (vs) => [...new Set(vs.map((v) => (v === "disabled" ? "none" : v === "enabled" ? "high" : v)))]

// modelsOf is a plan's models in ZCode's provider config.
function modelsOf(cfg, plan) {
  const c = cfg?.config ?? {}
  const ids = []
  const on = new Set()
  const add = (id) => {
    if (!on.has(id.toLowerCase())) on.add(id.toLowerCase()), ids.push(id)
  }
  for (const r of c.providerConfigRules?.providerRules ?? []) if (r.providerId === plan) (r.builtinModelIds ?? []).forEach(add)
  const off = new Set()
  for (const r of c.modelConfigRules?.builtinProviderModelRules ?? []) {
    if (r.providerId !== plan) continue
    if (r.config?.enabled === false) off.add(String(r.modelId).toLowerCase())
    else add(r.modelId)
  }
  const out = []
  for (const id of ids) {
    if (off.has(id.toLowerCase())) continue
    const m = { id, context: 0, output: 0, efforts: [] }
    for (const r of c.modelConfigRules?.modelRules ?? []) {
      let re
      try {
        re = new RegExp(`^(?:${r.modelMatch})$`, "i")
      } catch {
        continue
      }
      if (!re.test(id)) continue
      const x = r.config ?? {}
      if (x.properties?.contextWindow > 0) m.context = x.properties.contextWindow
      if (x.optionSpecs?.maxOutputTokens?.max > 0) m.output = x.optionSpecs.maxOutputTokens.max
      if (typeof x.properties?.inputFormat?.supportsImage === "boolean") m.image = x.properties.inputFormat.supportsImage
      if (typeof x.properties?.inputFormat?.supportsVideo === "boolean") m.video = x.properties.inputFormat.supportsVideo
      if (typeof x.properties?.inputFormat?.supportsPdf === "boolean") m.pdf = x.properties.inputFormat.supportsPdf
      if (typeof x.properties?.inputFormat?.supportsAudio === "boolean") m.audio = x.properties.inputFormat.supportsAudio
      if (x.optionSpecs?.reasoningLevel?.values?.length) m.efforts = efforts(x.optionSpecs.reasoningLevel.values)
    }
    out.push(m)
  }
  return out
}

// localConfig is the latest provider config an installed ZCode has.
function localConfig() {
  const files = []
  const rt = join(zcodeDir(), "runtime", "provider")
  const walk = (dir, depth) => {
    let es = []
    try {
      es = readdirSync(dir, { withFileTypes: true })
    } catch {
      return
    }
    for (const e of es) {
      if (depth < 3 && e.isDirectory()) walk(join(dir, e.name), depth + 1)
      else if (depth === 3 && e.name === "zcode-builtin.json") files.push(join(dir, e.name))
    }
  }
  walk(rt, 0)
  if (process.platform === "darwin") files.push("/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json")
  let best = null
  for (const f of files) {
    try {
      if (!existsSync(f)) continue
      const b = JSON.parse(readFileSync(f, "utf8"))
      if (!best || (b.revision ?? 0) > (best.revision ?? 0)) best = b
    } catch {}
  }
  return best
}

// remoteConfig is the release of ZCode's provider config zcode.z.ai names now.
async function remoteConfig() {
  const signal = AbortSignal.timeout(8000)
  const c = await clientConfigs()
  const url = c?.configs?.builtin_provider_config_json
  if (!String(url ?? "").startsWith("https://")) throw new Error("ZCode's configs name no provider config")
  const res = await fetch(url, { headers: { "User-Agent": UA }, signal })
  if (!res.ok) throw new Error(`ZCode's provider config: ${res.status}`)
  return res.json()
}

// clientConfigs is what zcode.z.ai tells this client version
// (/api/v1/client/configs): the provider config's address, and the
// captcha its claims and requests need (claimNeedsCaptcha). A read that
// came through is kept for 10 minutes; a failed one is not kept, and
// throws.
let clientCache = null
async function clientConfigs() {
  if (clientCache && Date.now() - clientCache.at < 600_000) return clientCache.c
  const c = await call("GET", `${ZCODE}/api/v1/client/configs?app_version=${APP_VERSION}&platform=${platform()}`, { signal: AbortSignal.timeout(8000) })
  clientCache = { c, at: Date.now() }
  return c
}
// resetClientConfigs is for tests: the next read asks zcode.z.ai again.
export function resetClientConfigs() {
  clientCache = null
}

// config is ZCode's provider config, the later of zcode.z.ai's and an
// installed ZCode's, kept for 10 minutes.
let cfgCache = null
async function zcodeConfig() {
  if (cfgCache && Date.now() - cfgCache.at < 600_000) return cfgCache.cfg
  let cfg = null
  try {
    cfg = await remoteConfig()
  } catch {}
  const local = localConfig()
  if (local && (!cfg || (local.revision ?? 0) > (cfg.revision ?? 0))) cfg = local
  cfgCache = { cfg, at: Date.now() }
  return cfg
}

// variants are a model's reasoning efforts as @ai-sdk/anthropic options.
const BUDGET = { minimal: 1024, low: 4096, medium: 8192, high: 16_000, max: 31_999 }
function variants(es, id) {
  const out = {}
  const glm = /^glm-5\.[23](?:$|-)/i.test(id)
  for (const e of es) out[e] = e === "none" ? { thinking: { type: "disabled" } } :
    glm ? { thinking: { type: "enabled" }, effort: e } :
      { thinking: { type: "enabled", budgetTokens: BUDGET[e] ?? 16_000 } }
  return out
}

// entry is a model as a config hook declares it.
const entry = (m) => ({
  id: m.id,
  name: m.id,
  reasoning: m.efforts.some((e) => e !== "none"),
  tool_call: true,
  attachment: !!(m.image || m.video || m.pdf || m.audio),
  modalities: { input: ["text", ...(m.image ? ["image"] : []), ...(m.video ? ["video"] : []),
    ...(m.pdf ? ["pdf"] : []), ...(m.audio ? ["audio"] : [])], output: ["text"] },
  // what ZCode's config names, else 0 (unknown), as magpie's built-in
  limit: { context: m.context || 0, output: m.output || 0 },
  cost: { input: 0, output: 0, cache_read: 0, cache_write: 0 },
  variants: variants(m.efforts, m.id),
})

function nativeBody(text) {
  if (typeof text !== "string") return text
  let body
  try { body = JSON.parse(text) } catch { return text }
  const model = String(body?.model ?? "")
  if (!/^glm-5\.[23](?:$|-)/i.test(model)) return text
  if ((body.thinking != null && (typeof body.thinking !== "object" || Array.isArray(body.thinking))) ||
      (body.output_config != null && (typeof body.output_config !== "object" || Array.isArray(body.output_config)))) return text
  const version = /^glm-5\.2(?:$|-)/i.test(model) ? 2 : 3
  const requested = typeof body.output_config?.effort === "string" ? body.output_config.effort.trim().toLowerCase() : ""
  const kind = body.thinking?.type
  const off = kind === "disabled" || kind === "between_tools" || requested === "none"
  if (!off && !["enabled", "adaptive"].includes(kind) && !(body.thinking == null && requested)) return text
  const thinking = { ...(body.thinking ?? {}) }
  const budget = Number(thinking.budget_tokens)
  delete thinking.budget_tokens
  delete thinking.display
  const output = body.output_config == null ? undefined : { ...body.output_config }
  if (off && version === 2) {
    thinking.type = "disabled"
    if (output) delete output.effort
  } else {
    thinking.type = "enabled"
    let effort = off ? "low" : requested
    if (!effort && budget > 0) effort = budget <= 4096 ? "low" : budget <= 24000 ? "high" : "max"
    if (["max", "xhigh", "ultra"].includes(effort)) effort = "max"
    else if (version === 3 && ["low", "minimal"].includes(effort)) effort = "low"
    else effort = "high"
    body.output_config = { ...(output ?? {}), effort }
  }
  body.thinking = thinking
  if (off && version === 2 && output) body.output_config = output
  return JSON.stringify(body)
}

// model is a model as OpenCode's provider hands it to provider.models.
// modelOf reads the model a Messages body asks for.
function modelOf(text) {
  try {
    const m = JSON.parse(text)?.model
    return typeof m === "string" ? m : ""
  } catch {
    return ""
  }
}

// kept passes a response on as it came, the account kept: the built-in
// never marked a ZCode account lapsed nor cleared one.
function kept(res, text) {
  const h = new Headers(res.headers)
  h.delete("content-length")
  h.delete("content-encoding")
  // Only this plugin may create the internal fallback signal. An upstream
  // response must not be able to request an ordinary paid model transition.
  h.delete("X-Magpie-ZCode-Fallback")
  h.delete("X-Magpie-ZCode-Fallback-Model")
  h.set("X-Magpie-Sign-In", "kept")
  return new Response(text !== undefined ? text : res.body, { status: res.status, statusText: res.statusText, headers: h })
}

// ---- the plan's concurrency ------------------------------------------------------
// The Start Plan gate limits this process's concurrent requests. A vendor
// concurrency refusal (3008/3009/3010) is terminal in ZCode 3.18 and is
// passed back without another model request.
//
// The coding plan keeps no gate of the plugin's: its cap depends on its tier
// (Z.ai serves Max more at once than Pro, and Lite fewer) and is higher
// off-peak (https://docs.z.ai/devpack/usage-policy), so a number written here
// would hold a Max account to a Lite one's. magpie's own queue spaces it by
// the number declared below (DEFAULT_CONCURRENCY, on the auth hook and in
// package.json), which a user raises on the account.
const DEFAULT_CONCURRENCY = 3
// DEFAULT_ACCOUNTS is how many of a user's ZCode accounts magpie sends
// requests to at the same time (maxAccounts, on the auth hook and in
// package.json): the first in the user's list, the next taking over once
// one runs out or fails. 01huadalang on Discord: three accounts used at
// once from one address got the address refused with a 405, two didn't. A
// user sets their own on the provider (Accounts at once), 0 for no limit.
const DEFAULT_ACCOUNTS = 2
const BUSY_CODES = new Set(["3008", "3009", "3010"])
// The hint names neither "plan" nor "limit": after the vendor's "…limit
// exceeded" either would read to magpie as a plan used up.
const BUSY_HINT =
  "ZCode's account or model concurrency limit was reached; run fewer agents or subagents at once, or try again later"

// startGate is the Start Plan's in-flight count per account. PR #395 measured
// one Z.ai gift account at 3 of 12 parallel requests served in one run and 4
// of 12 in another, so three are in flight at once, what magpie's queue sends
// by DEFAULT_CONCURRENCY too. The gate holds them there when that queue
// doesn't: a user who raised the account's number for the coding plan, two
// magpie providers signed in to one account, a host that doesn't read
// maxConcurrency. A cap the server still says now and then (a slot it lets
// go of late, the ZCode app on the same account) the 1s re-ask gets past.
// A request past them waits here, first come first served, until one is
// answered to the end, up to startGate.wait; an agent that gives up leaves
// the queue. A wait that runs out is answered startQueueAnswer's 429,
// nothing sent. The gate lives in this process's memory alone, by account
// (gateKey).
const startGate = { inflight: 3, wait: 120_000, lanes: new Map() }

// startFull says the account's Start Plan slots are all taken, or others wait.
const startFull = (key) => {
  const l = startGate.lanes.get(key)
  return !!l && (l.busy >= startGate.inflight || l.queue.length > 0)
}

// startSlot waits for one of the account's Start Plan slots: { release },
// null when startGate.wait ran out first, or the agent's abort thrown.
function startSlot(key, signal) {
  const lanes = startGate.lanes
  let l = lanes.get(key)
  if (!l) lanes.set(key, (l = { busy: 0, queue: [] }))
  const next = () => {
    while (l.busy < startGate.inflight && l.queue.length) l.queue.shift()()
    if (!l.busy && !l.queue.length && lanes.get(key) === l) lanes.delete(key)
  }
  const take = () => {
    l.busy++
    let gone = false
    return {
      release() {
        if (gone) return
        gone = true
        l.busy--
        next()
      },
    }
  }
  if (signal?.aborted) return Promise.reject(signal.reason ?? new DOMException("The operation was aborted.", "AbortError"))
  if (l.busy < startGate.inflight && !l.queue.length) return Promise.resolve(take())
  return new Promise((resolve, reject) => {
    const leave = () => {
      clearTimeout(timer)
      signal?.removeEventListener?.("abort", aborted)
      const i = l.queue.indexOf(go)
      if (i >= 0) l.queue.splice(i, 1)
    }
    const go = () => {
      leave()
      resolve(take())
    }
    const aborted = () => {
      leave()
      next()
      reject(signal.reason ?? new DOMException("The operation was aborted.", "AbortError"))
    }
    const timer = setTimeout(() => {
      leave()
      next()
      resolve(null)
    }, startGate.wait)
    signal?.addEventListener?.("abort", aborted, { once: true })
    l.queue.push(go)
  })
}

// released hands res on and lets its slot go once ZCode's answer has come
// to its end, failed, or been given up on (the agent's abort, the reader's
// cancel) — the request is in flight at ZCode until then. The answer is read
// as it comes, whether or not the agent reads it yet, so a reply read late,
// or dropped unread, does not keep its slot past ZCode's own end of it.
function released(res, slot, signal) {
  if (!slot) return res
  if (!res.body) {
    slot.release()
    return res
  }
  const reader = res.body.getReader()
  const stop = (why) => {
    slot.release()
    reader.cancel(why).catch(() => {})
  }
  signal?.addEventListener?.("abort", () => slot.release(), { once: true })
  const pump = async (c) => {
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        c.enqueue(value)
      }
      slot.release()
      c.close()
    } catch (e) {
      slot.release()
      try {
        c.error(e)
      } catch {}
    }
  }
  // started, not awaited: the agent reads each chunk as it comes
  const body = new ReadableStream({ start: (c) => void pump(c), cancel: stop })
  return new Response(body, { status: res.status, statusText: res.statusText, headers: res.headers })
}

// startQueueAnswer is a request that waited its whole turn behind the
// account's others and was never sent: a 429 magpie reads as a rate limit,
// so it tries the next account, and a short Retry-After.
function startQueueAnswer() {
  const error = {
    type: "rate_limit_error",
    message: `ZCode's free Start Plan serves this account ${startGate.inflight} requests at a time; this one waited ${Math.round(startGate.wait / 1000)}s behind the others and was not sent. Run fewer agents or subagents on ZCode at once, or try again`,
  }
  return new Response(JSON.stringify({ type: "error", error }), {
    status: 429, statusText: STATUS_TEXT[429], headers: { "content-type": "application/json", "retry-after": "5" },
  })
}

// busyHold is the firewall's countdown. Two days of logs (11-05 23:06–23:15
// and 11-06 06:23–06:27) show the same shape: sustained ~18 requests/min on
// the Start Plan, up to ~10 of them in flight against a cap of about three,
// brings code-3008 caps in a run, and 3–7.5 minutes later the edge firewall
// (errors.aliyun.com) refuses the whole IP with 405 for 1–2 hours. Re-asks
// absorbed ~90% of those caps, so neither magpie nor the agent saw that the
// address was saturated and nobody slowed the arrival rate down. startGate
// now keeps this computer's own requests under the cap, so a cap is ZCode
// busy or another client on the account; a request refused on its first ask
// and again a second later (busyCap) is that, sustained, and BUSY_N of them
// in one minute hold the Start Plan (zcode.z.ai only, process-wide, every
// account) off BUSY_HOLD. A cap the 1s re-ask got past counts for nothing:
// on 0.1.55 every capped request counted, a lone late slot as much as a
// saturated address, and busy afternoons held users for nothing. Replay of
// both days: N=5, H=3min froze 2–7 minutes before the first 405 and spared
// 37–52 requests. A held request answers 429 with Retry-After, so magpie
// still reads a rate limit, rests the account and fails over; it is not the
// 405/BlockedHint of an IP already refused. The coding plan, on another
// host, is never held. The hold lives in this process's memory alone: magpie
// restarting forgets it. Only model requests are held — balance reads, the
// usage card and check-in still reach zcode.z.ai during it (the check-in
// holds its own reads per account on a 429).
const BUSY_N = 5
const BUSY_WINDOW = 60_000
const BUSY_HOLD = 3 * 60_000
const busyHold = { hits: [], until: 0, now: () => Date.now() }
// busyHold.now is the clock, injectable for tests: a hold's window, its
// length and its expiry all read through it.
const busyHeld = () => busyHold.now() < busyHold.until

// busyCap records one sustained concurrency cap of a Start Plan request.
// Only a run arms a hold; the window is cleared on arming, and a cap that
// lands while the hold is on counts toward nothing — the hold's own 429s and
// the first caps after it cannot keep re-arming it.
function busyCap() {
  if (busyHeld()) return
  const now = busyHold.now()
  busyHold.hits.push(now)
  while (busyHold.hits.length && now - busyHold.hits[0] > BUSY_WINDOW) busyHold.hits.shift()
  if (busyHold.hits.length >= BUSY_N) {
    busyHold.until = now + BUSY_HOLD
    busyHold.hits.length = 0
  }
}

// BUSY_HOLD_MARK marks an answer the hold made up: a request the gate or the
// hold stopped locally never reached ZCode, and it says nothing about the
// gift's quota. The gift paths (a -Trial pick, free first) read the mark
// before they judge the answer spent, so a hold's 429 passes through as the
// rate limit it is and arms no minute's rest (11-08: the hold's answer
// through a -Trial entry read as "over its quota" with half the free quota
// left).
const BUSY_HOLD_MARK = "x-magpie-busy-hold"

function busyHeldAnswer() {
  const wait = Math.max(1, Math.ceil((busyHold.until - busyHold.now()) / 1000))
  const error = {
    type: "rate_limit_error",
    message: `ZCode turned away ${BUSY_N} requests in the last minute as too many requests at once, each again when asked a second later. So that ZCode's firewall does not block this network, magpie sends no free Start Plan requests for the next ${wait}s. Wait, run fewer agents or subagents on ZCode at once, or use a GLM Coding Plan model`,
  }
  return new Response(JSON.stringify({ type: "error", error }), {
    status: 429, statusText: STATUS_TEXT[429],
    headers: { "content-type": "application/json", "retry-after": String(wait), [BUSY_HOLD_MARK]: "1" },
  })
}

// busy says a 429 is that in-flight cap: its own code (3008, 3009, 3010), or its
// own words when the envelope carries no code.
function busy(status, text) {
  if (status !== 429) return false
  let b
  try {
    b = JSON.parse(text)
  } catch {
    return false
  }
  if (!b || typeof b !== "object") return false
  const say = (v) => (v == null || typeof v === "object" ? "" : String(v))
  if (BUSY_CODES.has(say(b.code) || say(b.error?.code))) return true
  return /concurrency limit exceeded/i.test(say(b.msg) + " " + say(b.message) + " " + say(b.error?.message) + " " + say(b.error))
}

// busyAnswer is the vendor's cap refusal as the agent reads it: its words,
// then what they are and what to do, as noPlanKey says why a key has no plan.
function busyAnswer(res, text) {
  const h = new Headers(res.headers)
  h.set("content-type", "application/json")
  h.delete("content-length")
  h.delete("content-encoding")
  let v
  try {
    v = JSON.parse(text)
  } catch {}
  if (v && typeof v === "object") {
    const said = first(v.msg, v.message, v.error?.message, typeof v.error === "string" ? v.error : "", `${res.status}`)
    const code = v.code ?? v.error?.code
    const head = code != null && !said.includes(String(code)) ? `${said} (${res.status}, code ${code})` : said
    const message = `${head} — ${BUSY_HINT}`
    const out = v.error && typeof v.error === "object" ? { ...v, error: { ...v.error, message } } : { ...v, msg: message }
    return new Response(JSON.stringify(out), { status: res.status, statusText: res.statusText, headers: h })
  }
  return new Response(JSON.stringify({ type: "error", error: { type: "rate_limit_error", message: `${res.status} ${STATUS_TEXT[res.status] ?? res.statusText} — ${BUSY_HINT}`.trim() } }), {
    status: res.status,
    statusText: res.statusText,
    headers: h,
  })
}

// 3.18 classifies concurrency codes 3008/3009/3010 as terminal, not
// automatic retries. The response is consumed only for an HTTP 429.
async function askBusy(u, o, hs, onCap) {
  const res = await fetch(u, { ...o, headers: hs, redirect: "error" })
  if (res.status !== 429) return res
  let text
  try { text = await readResponseText(res, o.signal) }
  catch (error) {
    if (error && typeof error === "object") error.responseStarted = true
    throw error
  }
  if (busy(429, text)) {
    onCap?.()
    return busyAnswer(res, text)
  }
  const headers = new Headers(res.headers)
  headers.delete("content-length")
  headers.delete("content-encoding")
  return new Response(text, { status: res.status, statusText: res.statusText, headers })
}

function model(m, url) {
  const e = entry(m)
  return {
    id: m.id,
    providerID: PROVIDER,
    name: m.id,
    api: { id: m.id, url, npm: "@ai-sdk/anthropic" },
    status: "active",
    cost: { input: 0, output: 0, cache: { read: 0, write: 0 } },
    limit: e.limit,
    options: {},
    headers: {},
    capabilities: {
      temperature: true,
      reasoning: e.reasoning,
      attachment: e.attachment,
      toolcall: true,
      input: { text: true, image: !!m.image, audio: !!m.audio, video: !!m.video, pdf: !!m.pdf },
      output: { text: true, image: false, audio: false, video: false, pdf: false },
      interleaved: false,
    },
    release_date: "",
    variants: e.variants,
  }
}

// ---- the Start Plan's requests ----------------------------------------------------
// zcode.z.ai answers 405 "request has been blocked due to unusual
// activity" (code 3012) to a request that doesn't look like ZCode's own
// (#343: the agent's system prompt as a fourth block), and to every
// request of an account its risk control blocked, ZCode's own app's too
// (plugins#348). A Start Plan request goes as the plugin
// the plan serves (ARNO's "Freeflow", provider zcode-start) sends it: its
// headers (startHeaders) and its body (dress). The texts are prompt.mjs.

// START_RESTRICTED is what ZCode's 3012, "request has been blocked due to
// unusual activity", means: ZCode's risk control blocked the account
// (ZCode's support says so, and ZCode's own app gets the same 405 on such
// an account, plugins#348), not that the request was sent wrong. Word for
// word magpie's provider.ZCodeRestrictedHint: the gateway reads it as the
// account restricted, rests it and asks the group's next member, and the
// Routing page says it in the user's language.
const START_RESTRICTED = "ZCode's risk control blocked this account for unusual activity (code 3012); it rests, and the group's other providers are used. If the ZCode app fails on this account too, only ZCode support can lift the block"
// START_EDGE is what a block page from ZCode's edge firewall (Aliyun's)
// means: this network's address, not the account. Word for word magpie's
// provider.BlockedHint.
const START_EDGE = "the provider's network firewall blocked requests from this IP; wait a while, or switch to another network or proxy"
const EDGE_PAGE = /errors\.aliyun\.com|<html/i
const START_REFUSED = /unusual activity|request has been blocked|"code"\s*:\s*"?3012\b/i

// startRefused says a Start Plan answer is a refusal: a 405, a block page
// or code 3012.
const startRefused = (status, text) => status >= 400 && (status === 405 || START_REFUSED.test(text))

// edge is the hold a block page puts on this computer's address: zcode.z.ai
// (the Start Plan, a gift) is not asked again until edge.until, and its
// requests are answered here with the same 405 and START_EDGE, so magpie
// still reads them as the network's (provider.EdgeBlocked) and asks the
// group's next provider. Every account of this process goes out from the
// one address, so the hold is the process's, not an account's. Asking on
// while the address is blocked, from every account and every session, only
// knocks again on a firewall that is counting (plugins#649: two accounts,
// three sessions and their subagents, and ZCode's own app blocked too; a
// block in #369 lasted about seven minutes). EDGE_HOLD is short enough that
// a user who switched network or proxy waits little for it.
const EDGE_HOLD = 3 * 60_000
const edge = { until: 0 }
const edgeHeld = () => Date.now() < edge.until

function hhmm(t) {
  const d = new Date(t)
  return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0") + ":" + String(d.getSeconds()).padStart(2, "0")
}

// edgeHeldAnswer is a held request's answer: the block page's status and
// meaning, and until when this computer holds off.
function edgeHeldAnswer() {
  const error = { type: "permission_error", message: `405 ${STATUS_TEXT[405]} — ${START_EDGE} (ZCode's Start Plan is not asked again from here until ${hhmm(edge.until)}, plugins#649)` }
  return new Response(JSON.stringify({ type: "error", error }), { status: 405, statusText: STATUS_TEXT[405], headers: { "content-type": "application/json" } })
}

// startBlocked is the refusal's answer: what the vendor said (its status
// for a block page), then what it means. A block page is the network's;
// anything else, ZCode's JSON 3012 or a bare 405, the account's.
function startBlocked(res, text) {
  const page = EDGE_PAGE.test(text)
  let said = ""
  if (!page) {
    try {
      const v = JSON.parse(text)
      said = first(v?.error?.message, typeof v?.error === "string" ? v.error : "", v?.message, v?.msg)
      const code = v?.code ?? v?.error?.code
      if (said && code != null && !String(said).includes(String(code))) said = `${said} (${res.status}, code ${code})`
    } catch {}
  }
  said ||= `${res.status} ${STATUS_TEXT[res.status] ?? res.statusText}`.trim()
  const h = new Headers(res.headers)
  h.set("content-type", "application/json")
  const error = { type: "permission_error", message: `${said} — ${page ? START_EDGE : START_RESTRICTED}` }
  return new Response(JSON.stringify({ type: "error", error }), { status: res.status, statusText: res.statusText, headers: h })
}

const osVersion = () => `${process.platform} ${release()} ${arch()}`
const osCategory = () => (process.platform === "darwin" ? "macos" : process.platform === "win32" ? "windows" : "linux")

function intl() {
  try {
    return Intl.DateTimeFormat().resolvedOptions()
  } catch {
    return {}
  }
}

// startHeaders is a Start Plan request's headers, jwt its key: ZCode's
// own (3.16.3's Frn and Vdt in glm/zcode.cjs) and the body's content-type,
// and nothing else. The agent's headers never go on: Claude Code's
// anthropic-beta, x-app, x-stainless-*, anthropic-dangerous-direct-browser-
// access and x-claude-code-session-id reach the plugin through magpie's
// gateway, and ZCode sends none of them (no anthropic-beta at all on the
// Start Plan: its tool search beta is for coding plans only). Accept and
// accept-encoding are left to the HTTP client, as ZCode leaves them to
// Node's.
function startHeaders(h, jwt) {
  const out = new Headers()
  out.set("content-type", h.get("content-type") || "application/json")
  out.set("Authorization", "Bearer " + jwt)
  out.set("anthropic-version", "2023-06-01")
  out.set("HTTP-Referer", ZCODE)
  out.set("User-Agent", UA + " ai-sdk/anthropic/3.0.81")
  out.set("X-ZCode-App-Version", APP_VERSION)
  // the desktop app's agent names itself electron: 3.17.2's
  // zcode-protocol-entrypoint passes sourceTitle "electron", and a bare
  // run under app-server says the same (Mvc in glm/zcode.cjs); "cli" is
  // the zcode command line's alone, which sends no X-Device-Mid on the
  // app's calls and no desktop prompt texts, as this plugin does
  out.set("X-Title", "Z Code@electron")
  out.set("X-Release-Channel", "production")
  const o = intl()
  out.set("X-Client-Language", o.locale || "unknown")
  out.set("X-Client-Timezone", o.timeZone || "unknown")
  out.set("X-ZCode-Agent", "glm")
  out.set("X-Platform", platform())
  out.set("X-Os-Category", osCategory())
  // os.release() alone, as ZCode's buildZCodeSourceHeadersFromContext
  // has it; the prompt's OS line is osVersion()
  out.set("X-Os-Version", release())
  out.set("x-request-id", uuid())
  out.set("x-zcode-session-type", "main")
  out.set("x-zcode-trace-id", uuid())
  // 3.16's createModelRequestAttributionHeaders: what asked the model
  out.set("x-zcode-query-source", "main_turn")
  return out
}

// queryOf is the request's x-query-id, as 3.17.2's
// createModelRequestAttributionHeaders (zft in glm/zcode.cjs) sends it:
// the user's query, "query_" + a UUID with the prefix cut, the same for
// every request of one turn (the tool calls the model makes on the way)
// and a new one for the next thing the user says. The agent's turns are
// counted: a user message that carries text, not tool results alone,
// starts a query. A body that doesn't read gives none, and none is sent.
function queryOf(text, session) {
  let b
  try {
    b = JSON.parse(text)
  } catch {
    return ""
  }
  if (!Array.isArray(b?.messages)) return ""
  let n = 0
  for (const m of b.messages) {
    if (m?.role !== "user") continue
    const c = m.content
    if (typeof c === "string" ? c.trim() : Array.isArray(c) && c.some((x) => x && x.type !== "tool_result")) n++
  }
  return n ? uuidOf("zcode-query\0" + session + "\0" + n) : ""
}

// Claude Code's tool search, as ZCode's plans take it. Claude Code
// (2.1.310) offers a tool named ToolSearch, names its deferred tools in a
// system turn, and sends a deferred tool's schema with defer_loading only
// once a search loaded it (a tool_reference in a ToolSearch result), with
// a DeferredToolPlaceholder that keeps the search on. api.z.ai's
// Anthropic endpoint, given a tool named exactly ToolSearch, shows the
// model none of the other tools, Bash, Read and Write among them, but
// those a tool_reference loaded: a subagent, which hasn't searched,
// reports only the deferred tools, and the main turn has to search for
// Bash before it can run it (小八 on Discord, 10-24; the beta header
// changes nothing, the name alone does). ZCode itself sends no ToolSearch
// and no defer_loading. So the search goes as SEARCH_ALIAS, in the tools
// and in the turns that called it, the tools Claude Code sent with their
// schemas go as ZCode's do, without defer_loading, and the placeholder is
// left out; the model's calls to SEARCH_ALIAS come back as ToolSearch
// (searchNamed).
const SEARCH = "ToolSearch"
const SEARCH_ALIAS = "ToolSearchTool"
const PLACEHOLDER = "DeferredToolPlaceholder"

// searchless is a Messages request body without Claude Code's ToolSearch
// name, and whether it had the tool; anything else goes as it is.
function searchless(text) {
  if (typeof text !== "string" || !text.includes(`"${SEARCH}"`)) return { text, renamed: false }
  let b
  try {
    b = JSON.parse(text)
  } catch {
    return { text, renamed: false }
  }
  if (!b || !Array.isArray(b.tools) || !b.tools.some((t) => t?.name === SEARCH)) return { text, renamed: false }
  // a tool of the alias's own name keeps it; the search can't then be renamed
  if (b.tools.some((t) => t?.name === SEARCH_ALIAS)) return { text, renamed: false }
  b.tools = b.tools
    .filter((t) => t?.name !== PLACEHOLDER)
    .map((t) => {
      if (!t || typeof t !== "object") return t
      const { defer_loading, ...rest } = t
      return t.name === SEARCH ? { ...rest, name: SEARCH_ALIAS } : rest
    })
  if (Array.isArray(b.messages))
    for (const m of b.messages)
      if (m?.role === "assistant" && Array.isArray(m.content))
        for (const x of m.content) if (x?.type === "tool_use" && x.name === SEARCH) x.name = SEARCH_ALIAS
  if (b.tool_choice?.type === "tool" && b.tool_choice.name === SEARCH) b.tool_choice.name = SEARCH_ALIAS
  return { text: JSON.stringify(b), renamed: true }
}

// searchNamed is an answer to a request searchless renamed, the model's
// calls to SEARCH_ALIAS named ToolSearch again, as Claude Code runs them:
// a stream's content_block_start events, or a whole message's content.
function searchNamed(res, signal) {
  if (!res.body) return res
  const type = res.headers.get("content-type") ?? ""
  const named = (block) => {
    if (block?.type === "tool_use" && block.name === SEARCH_ALIAS) {
      block.name = SEARCH
      return true
    }
    return false
  }
  const line = (l) => {
    if (!l.startsWith("data:") || !l.includes(`"${SEARCH_ALIAS}"`)) return l
    try {
      const d = JSON.parse(l.slice(5))
      return named(d?.content_block) ? "data: " + JSON.stringify(d) : l
    } catch {
      return l
    }
  }
  if (type.includes("text/event-stream")) {
    const dec = new TextDecoder()
    const enc = new TextEncoder()
    let held = ""
    const body = res.body.pipeThrough(
      new TransformStream({
        transform(chunk, ctl) {
          held += dec.decode(chunk, { stream: true })
          const i = held.lastIndexOf("\n")
          if (i < 0) return
          ctl.enqueue(enc.encode(held.slice(0, i + 1).split("\n").map(line).join("\n")))
          held = held.slice(i + 1)
        },
        flush(ctl) {
          held += dec.decode()
          if (held) ctl.enqueue(enc.encode(line(held)))
        },
      }),
    )
    return new Response(body, { status: res.status, statusText: res.statusText, headers: res.headers })
  }
  if (!type.includes("json")) return res
  return readResponseText(res, signal).then((text) => {
    let out = text
    if (text.includes(`"${SEARCH_ALIAS}"`)) {
      try {
        const v = JSON.parse(text)
        if (Array.isArray(v?.content) && v.content.map(named).some(Boolean)) out = JSON.stringify(v)
      } catch {}
    }
    return new Response(out, { status: res.status, statusText: res.statusText, headers: res.headers })
  })
}

// startURL is a Start Plan request's URL without the query the agent put
// on it: Claude Code asks /v1/messages?beta=true, which magpie passes on,
// and ZCode asks /v1/messages alone.
function startURL(url) {
  const i = url.indexOf("?")
  return i < 0 ? url : url.slice(0, i)
}

// WORK_DIR and WORK_GIT are where magpie tells a plugin's fetch the agent
// that asked works: the folder, percent-encoded, and "yes" or "no" for a
// git repository, each left off when magpie doesn't know it (magpie's
// provider.WorkDirHeader and WorkGitHeader). They are for the plugin, not
// the vendor.
const WORK_DIR = "x-magpie-work-dir"
const WORK_GIT = "x-magpie-work-git"

// workPlace takes WORK_DIR and WORK_GIT off h: {dir?, git?}, what magpie
// said of where the agent works.
function workPlace(h) {
  const place = {}
  const dir = h.get(WORK_DIR)
  if (dir) {
    try {
      place.dir = decodeURIComponent(dir)
    } catch {}
  }
  const git = h.get(WORK_GIT)
  if (place.dir && (git === "yes" || git === "no")) place.git = git
  h.delete(WORK_DIR)
  h.delete(WORK_GIT)
  return place
}

// environment is the 3.18 runtime environment announcement. The host runs in magpie,
// not in the agent's folder: the working directory and whether it is a git
// repository are the agent's, as magpie tells them (place), and each is
// left out when it isn't told rather than said as magpie's own (#86). The
// agent's shell isn't known here either, so it isn't said; the platform
// and OS version are this computer's, as ZCode's X-Os-* headers say them.
function environment(place = {}) {
  const e = PROMPT.environment
  const lines = [e.heading, e.invokedLine]
  if (place.dir) {
    lines.push(` - ${e.cwdLabel}: ${place.dir}`)
    if (place.git) lines.push(` - ${e.gitLabel}: ${place.git === "yes" ? e.gitYes : e.gitNo}`)
  }
  lines.push(` - ${e.platformLabel}: ${process.platform}`, ` - ${e.osVersionLabel}: ${osVersion()}`)
  lines.push(` - ${e.untrustedNote}`)
  return lines.join("\n")
}

// today is the local date, YYYY-MM-DD.
function today(now = new Date()) {
  const p = (n) => String(n).padStart(2, "0")
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`
}

// Environment, model and date are runtime announcements in 3.18, not
// dynamic system sections. The harness wraps each announcement separately.
function contextPrefix(place, provider, model) {
  const wrap = (value) => "<system-reminder>\n" +
    value.replace(/<\/?system-reminder\b[^>]*>/gi, (tag) => "&lt;" + tag.slice(1)) + "\n</system-reminder>"
  const announcements = [environment(place)]
  if (model) announcements.push(PROMPT.poweredBy.replace("{provider}", provider).replace("{model}", model))
  announcements.push(PROMPT.context.currentDateLine.replace("{date}", today()))
  return { role: "user", content: announcements.map((value) => ({ type: "text", text: wrap(value) })) }
}

// dress is a Messages request body as ZCode sends it: ZCode's three cached
// system blocks and no more, the agent's own system text at the end of the
// third (#343); the context
// prefix as the first turn; no cache mark in the turns but on the last
// block of the last; none on the tools; metadata.user_id naming the
// sign-in's device. site names the powered-by line's provider, place where
// the agent works (workPlace). A body that isn't a Messages request goes as
// it is.
function dress(text, site, device, place, session) {
  if (typeof text !== "string" || !text.includes("messages")) return text
  let b
  try {
    b = JSON.parse(text)
  } catch {
    return text
  }
  if (!b || typeof b !== "object" || !Array.isArray(b.messages)) return text
  const cached = (t) => ({ type: "text", text: t, cache_control: { type: "ephemeral" } })
  const provider = site === "bigmodel" ? "account:bigmodel-start-plan" : "account:zai-start-plan"
  const model = typeof b.model === "string" ? b.model : ""
  // Keep the request's own system instructions in the dynamic block;
  // environment and model identity are separate runtime announcements.
  const own = [], otherSystemBlocks = []
  if (typeof b.system === "string") {
    if (b.system.trim()) own.push(b.system)
  } else if (Array.isArray(b.system)) {
    for (const x of b.system) {
      if (x?.type === "text" && typeof x.text === "string") {
        if (x.text.trim()) own.push(x.text)
      } else otherSystemBlocks.push(x)
    }
  }
  const dynamic = [
    PROMPT.dynamicBehavior,
    PROMPT.contextManagement,
    PROMPT.totalTokens,
    ...own,
  ]
  b.system = [{ type: "text", text: PROMPT.cliPrefix }, cached(PROMPT.stableSections.join("\n\n")), cached("\n\n" + dynamic.join("\n\n")), ...otherSystemBlocks]
  b.messages = [contextPrefix(place, provider, model), ...b.messages]
  let last = null
  for (const m of b.messages) {
    if (!m || m.role === "system") continue
    last = m
    if (Array.isArray(m.content)) for (const x of m.content) if (x && typeof x === "object") delete x.cache_control
  }
  if (last && typeof last.content === "string") last.content = [cached(last.content)]
  else if (last && Array.isArray(last.content) && last.content.length) {
    const x = last.content[last.content.length - 1]
    if (x && typeof x === "object" && !x.cache_control) x.cache_control = { type: "ephemeral" }
  }
  if (Array.isArray(b.tools)) for (const t of b.tools) if (t && typeof t === "object") delete t.cache_control
  const meta = b.metadata && typeof b.metadata === "object" && !Array.isArray(b.metadata) ? b.metadata : {}
  b.metadata = { ...meta, user_id: typeof meta.user_id === "string" && meta.user_id.trim() ? meta.user_id : userIdOf(device, session) }
  return JSON.stringify(b)
}

// userIdOf is metadata.user_id as ZCode's client (3.14.3, glm/zcode.cjs
// MLs and vWr) puts it on every Anthropic-format request, a coding plan's
// too: the machine's device id, no account uuid. The plan's endpoint keeps
// a prompt cache on the machine it picks by it, so a request without it
// lands anywhere and its cache is mostly missed (#387). Its session_id is
// the request's x-session-id (sessionOf), as ZCode's is.
const userIdOf = (device, session = "") => JSON.stringify({ device_id: device, account_uuid: "", session_id: session })

// marked is a coding plan's Messages request body with metadata.user_id
// as ZCode sends it, when the agent sent none of its own. Anything else,
// a body that isn't a Messages request among them, goes as it is.
function marked(text, device, session) {
  if (typeof text !== "string" || !device || !text.includes("messages")) return text
  let b
  try {
    b = JSON.parse(text)
  } catch {
    return text
  }
  if (!b || typeof b !== "object" || Array.isArray(b) || !Array.isArray(b.messages)) return text
  const meta = b.metadata && typeof b.metadata === "object" && !Array.isArray(b.metadata) ? b.metadata : {}
  if (typeof meta.user_id === "string" && meta.user_id.trim()) return text
  b.metadata = { ...meta, user_id: userIdOf(device, session) }
  return JSON.stringify(b)
}

// sessionOf is the x-session-id a request goes with, as ZCode's client
// (3.14.3, glm/zcode.cjs xst and Tst) sends it: a bare UUID, the same on
// every turn of a conversation. BigModel keeps a prompt's cache for the
// requests that name the same session and seldom for those that name none
// (#387). The agent's session (chat.headers below) goes as it is when it is
// a UUID, else as a UUID made from it, so the vendor sees only ZCode's
// format, never magpie's or another agent's; with none, a UUID made from
// the device.
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
function sessionOf(id, device) {
  const own = typeof id === "string" ? id.trim() : ""
  if (UUID.test(own)) return own.toLowerCase()
  return uuidOf(own ? "zcode-session\0" + own : "zcode-session-device\0" + (device ?? ""))
}

// ---- the plugin ------------------------------------------------------------------

export async function ZCodeAuthPlugin({ client, magpie } = {}, options = {}) {
  const log = (message) => {
    try {
      client?.app?.log?.({ body: { service: "zcode-auth", level: "error", message } })
    } catch {}
  }
  const origins = {
    zai: { business: ZAI_API, zcode: ZCODE, zaiOAuth: "https://chat.z.ai", mcpDev: options?.mcpDevOrigins },
    bigmodel: { business: BIGMODEL_API, zcode: ZCODE, zaiOAuth: "https://chat.z.ai", mcpDev: options?.mcpDevOrigins },
  }
  const runtime = createAccountRuntime({ client, providerId: PROVIDER, stateOf, origins,
    claimVerification: async () => {
      try { return claimNeedsCaptcha(await clientConfigs()) } catch { return null }
    },
  })
  const accountPreflight = ({ operation, params = {} }) => {
    if (operation !== "mcp.headers") return
    const destination = params.stdio === true && params.url === undefined ? `${ZCODE}/mcp` : params.url
    if ((params.stdio === true && params.url !== undefined) || !trustedOfficialMcpURL(destination, origins.zai)) {
      const error = new Error("untrusted official MCP URL")
      error.kind = "untrusted_url"
      throw error
    }
  }
  // freeFirst reads the option from the provider config's options first,
  // and from the plugin's own (magpie's Plugins › Options) when the
  // provider carries no freeFirst of its own. It is on unless one of them
  // says false: a gift ZCode hands out (each weekend, sgfvdsg86265 on X)
  // lapses unused behind an agent that asks the plain model, and every
  // failure of the gift is answered from the plan. magpie's switch in the
  // provider's editor (FREE_FIRST, auth.options below) turns it off too;
  // it is on by default, so a JSON freeFirst: false a user wrote before
  // the switch existed keeps it off. A key magpie doesn't report (an older
  // magpie, OpenCode) leaves it as the JSON says.
  const freeFirst = (providerOptions) =>
    magpie?.options?.(PROVIDER)?.[FREE_FIRST] !== false &&
    (providerOptions?.freeFirst !== undefined ? providerOptions.freeFirst !== false : options?.freeFirst !== false)
  const oauth = (site) => ({
    type: "oauth",
    label: `ZCode: ${SITES[site].name}${site === "bigmodel" ? " (智谱)" : ""} GLM Coding Plan`,
    authorize: () => authorize(site, log),
  })
  return {
    // the agent's session, which magpie tells the plugins here and not
    // in the request, goes on as x-session-id (sessionOf)
    "chat.headers": async (input, output) => {
      const provider = input?.model?.providerID ?? input?.provider?.info?.id
      if (provider !== PROVIDER) return
      const id = input?.sessionID
      if (typeof id === "string" && id.trim()) output.headers["x-session-id"] = sessionOf(id)
    },
    config: async (cfg) => {
      cfg.provider ??= {}
      const was = cfg.provider[PROVIDER] ?? {}
      cfg.provider[PROVIDER] = {
        name: "ZCode",
        npm: "@ai-sdk/anthropic",
        api: ZAI_BASE + "/v1",
        ...was,
        models: { ...Object.fromEntries(MODELS.map((m) => [m.id, entry(m)])), ...(was.models ?? {}) },
      }
    },
    provider: {
      id: PROVIDER,
      // the account's plan's models, as ZCode's config lists them now
      async models(provider, { getAuth, signal } = {}) {
        const ctx = await runtime.capture(getAuth, { signal })
        const s = stateOf(await getAuth(), { follow: false })
        s.jwt = (await ctx.loginMaterial({ business: "none", zcode: "optional" })).zcode
        if (ctx.state.kind !== "start") s.key = (await ctx.projectToken()).token
        const { start, gift: known } = await plansOf(s, ctx)
        const gift = start ? await startPlan(s.jwt, s.device, false, ctx).catch((error) => {
          if (accountBoundaryFailure(error, ctx)) throw error
          return null
        }) : known
        const base = start ? START_BASE : s.base
        const cfg = await zcodeConfig().catch(() => null)
        let ms = modelsOf(cfg, planID(base, s.site, ctx.state.kind))
        // ZCode's config can't be had: its table stands in, and magpie keeps
        // the list it was told last, as the built-in keeps the one it fetched last
        const fell = !ms.length
        if (fell) ms = start ? START_MODELS : CODING_MODELS
        // a gift-only account serves what its balance names, as ZCode
        // offers it (resolveStartPlanBalanceAvailability): the Start
        // Plan's config models the balance lacks would only be refused
        // (young02036). A balance naming none leaves the config's list.
        if (start && giftNames(gift).length) ms = ms.filter((m) => giftNames(gift).some((x) => x.toLowerCase() === m.id.toLowerCase()))
        const url = s.base + "/v1"
        const out = Object.fromEntries(ms.map((m) => [m.id, model(m, url)]))
        const details = [...ms, ...modelsOf(cfg, planID(START_BASE, s.site)), ...modelsOf(cfg, planID(s.base, s.site)), ...MODELS]
        const about = (raw) => details.find((m) => m.id.toLowerCase() === raw.toLowerCase()) ?? { efforts: [] }
        const has = new Set(Object.keys(out).map((id) => id.toLowerCase()))
        for (const raw of giftNames(gift)) {
          // a gift-only account's models are its gift's, by their own
          // names, as ZCode lists them (its Start Plan's GLM-5.3, which the
          // plan's config leaves out): every request of it goes to the gift
          // anyway, and the card counts them by that name too (#1261)
          if (start && !has.has(raw.toLowerCase())) {
            const plain = about(raw).id ?? raw
            has.add(raw.toLowerCase())
            out[plain] = model({ ...about(raw), id: plain }, url)
          }
          const id = giftID(raw)
          out[id] ??= model({ ...about(raw), id }, url)
          out[id].free = true
        }
        if (ctx.state.kind === "individual" || ctx.state.kind === "team") {
          for (const raw of ["GLM-5.3", "GLM-5.3-Flash"]) {
            if (!out[raw]) continue
            for (const lane of ["OffPeak", "Highspeed"]) {
              const id = `${raw}-${lane}`
              out[id] = model({ ...about(raw), id }, url)
              out[id].api.id = raw
            }
          }
        }
        await ctx.assertCurrent()
        // a fallback tells magpie to keep the list it was told last, which
        // predates the gift read just made: an account whose gift lists
        // trial entries is told this list, so ZCode's config out of reach
        // (a remote box that can't get its CDN) never hides them
        if (fell && !giftNames(gift).length) out[Symbol.for("magpie.fellBack")] = true
        return out
      },
    },
    auth: {
      provider: PROVIDER,
      // a plan serves one account a few requests at a time: magpie's own
      // field, so its queue spaces what the agents ask for before the vendor
      // sees it. For the coding plan this is a default, not a cap the plugin
      // enforces — its cap follows its tier (Max > Pro > Lite) and the time
      // of day, and a user who knows theirs can raise this on the account.
      // The Start Plan has the plugin's own gate below it (startGate)
      maxConcurrency: DEFAULT_CONCURRENCY,
      // how many accounts take requests at once, the others in reserve
      // (DEFAULT_ACCOUNTS); a magpie that doesn't read it uses them all
      maxAccounts: DEFAULT_ACCOUNTS,
      // magpie's own switches (OpenCode never reads auth.options), read at
      // each request through input.magpie
      options: [
        {
          key: FREE_FIRST,
          label: "Free quota first",
          description: "When on, the plain GLM models spend ZCode's free Start Plan quota (the -Trial entries' quota) before the GLM Coding Plan, and go to the plan when it is spent or refused. Off: only the -Trial entries use it. freeFirst: false in the options JSON turns it off too.",
          labels: { zh: "优先用免费额度", "zh-TW": "優先用免費額度", ja: "無料枠を優先", de: "Gratis-Kontingent zuerst" },
          descriptions: {
            zh: "开启后，普通的 GLM 模型先用 ZCode 的免费 Start Plan 额度（即 -Trial 条目的额度），用完或被拒时再用 GLM Coding Plan。关闭后只有 -Trial 条目会用它。选项 JSON 里的 freeFirst: false 也会关闭它。",
            "zh-TW": "開啟後，一般的 GLM 模型先用 ZCode 的免費 Start Plan 額度（即 -Trial 項目的額度），用完或被拒時再用 GLM Coding Plan。關閉後只有 -Trial 項目會用它。選項 JSON 裡的 freeFirst: false 也會關閉它。",
            ja: "オンにすると、通常の GLM モデルは GLM Coding Plan より先に ZCode の無料 Start Plan 枠（-Trial 項目の枠）を使い、使い切るか拒否されるとプランに戻ります。オフでは -Trial 項目だけが使います。オプション JSON の freeFirst: false でもオフになります。",
            de: "Ist dies an, nutzen die normalen GLM-Modelle zuerst ZCodes freies Start-Plan-Kontingent (das der -Trial-Einträge) und erst danach den GLM Coding Plan. Aus: nur die -Trial-Einträge nutzen es. freeFirst: false im Optionen-JSON schaltet es auch aus.",
          },
          default: true,
        },
        { key: "coding_plan_ultra", label: "ZCode ultra gateway",
          description: "Route supported paid Messages requests through ZCode's optional ultra gateway.", default: false },
      ],
      accountOperations: ["context", "selection.list", "selection.select", ...nativeUsageOperations, ...resetOperations, ...nativeLaneOperations],
      accountPreflight,
      async account(getAuth, provider, { operation, params = {}, expectedScope, expectedGeneration, signal } = {}) {
        accountPreflight({ operation, params, provider })
        const ctx = await runtime.capture(getAuth, { signal, expectedScope, expectedGeneration, verifyIdentity: operation === "context" })
        let data
        if (operation === "context") data = {}
        else if (operation === "selection.list") data = { selections: await selectionCandidates(ctx) }
        else if (nativeUsageOperations.includes(operation)) data = await nativeUsageOperation(ctx, operation, params)
        else if (resetOperations.includes(operation)) data = await resetOperation(ctx, operation, params)
        else if (nativeLaneOperations.includes(operation)) data = await nativeLaneOperation(ctx, operation, params)
        else throw new Error("unsupported ZCode account operation")
        await ctx.assertCurrent()
        return { context: ctx.public(), data }
      },
      async select(getAuth, provider, { selection, expectedScope, expectedGeneration, signal } = {}) {
        const ctx = await runtime.capture(getAuth, { signal, expectedScope, expectedGeneration })
        const candidates = await selectionCandidates(ctx)
        const fields = ["kind", "family", "userId", "organization", "project", "product"]
        const matches = candidates.filter((x) => fields.every((field) => String(x[field] ?? "") === String(selection?.[field] ?? "")))
        if (matches.length !== 1 || matches[0].availability === "unavailable") throw new Error("account_selection_unavailable")
        const pick = matches[0]
        await ctx.loginMaterial({ business: pick.kind === "start" ? "optional" : "required", zcode: pick.kind === "start" ? "required" : "optional" })
        await ctx.assertCurrent()
        const auth = await getAuth()
        const state = stateOf(auth, { follow: false })
        const next = { ...state, version: 1, source: "selection", kind: pick.kind, org: pick.organization || "",
          project: pick.project || "", product: pick.product || "", plan: pick.name }
        delete next.loc
        delete next.key
        const selected = { ...auth, type: "oauth", refresh: JSON.stringify(next), access: next.jwt || "", expires: 0,
          accountScope: scopeOfState(next) }
        delete selected.accountEpoch
        await ctx.assertCurrent()
        return selected
      },
      async loader(getAuth, provider) {
        const s0 = stateOf(await getAuth(), { follow: false })
        if (!s0) return {}
        return {
          baseURL: s0.base + "/v1",
          apiKey: s0.key || "zcode", // the fetch below puts the real one on
          async fetch(input, init) {
            // a Start Plan request holds one of its account's slots
            // (startSlot) until its answer has been read to the end
            const own = {}
            try {
              return released(await serve(input, init, own), own.slot, own.signal)
            } catch (e) {
              own.slot?.release()
              throw e
            }
          },
        }
        async function serve(input, init, own) {
            let url = input instanceof Request ? input.url : String(input)
            const opts = input instanceof Request ? { method: input.method, headers: input.headers, body: input.body, signal: input.signal, duplex: "half", ...init } : { ...init }
            const metadata = opts.magpie ?? {}
            delete opts.magpie
            if ((metadata.expectedScope !== undefined || metadata.expectedGeneration !== undefined) &&
                (typeof metadata.expectedScope !== "string" || !metadata.expectedScope ||
                 typeof metadata.expectedGeneration !== "string" || !metadata.expectedGeneration))
              throw new Error("invalid ZCode account binding")
            const ctx = await runtime.capture(getAuth, { signal: opts.signal,
              expectedScope: metadata.expectedScope, expectedGeneration: metadata.expectedGeneration })
            const auth = await getAuth()
            const s = stateOf(auth, { follow: false })
            const zcode = (await ctx.loginMaterial({ business: "none", zcode: "optional" })).zcode
            if (ctx.state.kind === "start" && !zcode && s.jwt && jwtExpired(s.jwt)) throw new Error(EXPIRED)
            s.jwt = zcode
            if (ctx.state.kind !== "legacy-key") s.key = ctx.scope
            // the account a Start Plan slot belongs to: its user id, which a
            // renewed session keeps, before the key or session itself
            const gateKey = String(auth?.uid || auth?.accountId || holdKey(s))
            const requestedModel = first(metadata.model)
            const laneIntent = /^GLM-5\.3(?:-Flash)?-(?:OffPeak|Highspeed)$/i.test(requestedModel)
            if (laneIntent && !metadata.execution) throw new Error("ZCode native lane requires an admitted execution for this session")
            if (laneIntent && !requestedModel.toLowerCase().endsWith("-" + String(metadata.execution.lane ?? "").toLowerCase()))
              throw new Error("ZCode native lane binding does not match the requested model")
            if (!laneIntent && metadata.execution) throw new Error("ZCode native lane binding does not match the requested model")
            if (laneIntent && !["individual", "team"].includes(ctx.state.kind)) throw new Error("ZCode native lanes require a paid account selection")
            const { start: alone, gift: known } = await plansOf(s, ctx)
            let gift = known
            let paidMaterial
            let key = ""
            // a dual account's body is read ahead of sending, so a gift that
            // says it is spent can spend nothing and replay what was asked for
            let start = alone
            let orig, forcedGift = false, freeGift = false
            let asked = ""
            let renamed = false
            const done = (res) => (renamed ? searchNamed(res, opts.signal) : res)
            if (opts.body != null) {
              orig = opts.body = typeof opts.body === "string" ? opts.body : await new Response(opts.body).text()
              delete opts.duplex
              // Claude Code's tool search goes under another name, or
              // ZCode's plans show the model no other tool (searchless)
              const plain = searchless(orig)
              if (plain.renamed) {
                orig = opts.body = plain.text
                renamed = true
              }
              let m = modelOf(orig)
              asked = m
              if (/^GLM-5\.3(?:-Flash)?-(?:OffPeak|Highspeed)$/i.test(m))
                throw new Error("ZCode native lane requires host-bound model and execution metadata")
              if (laneIntent && m.toLowerCase().endsWith("-trial")) throw new Error("ZCode native lane cannot use a Trial model")
              if (m.toLowerCase().endsWith("-trial")) {
                gift ??= await startPlan(s.jwt, s.device, false, ctx).catch((error) => {
                  if (accountBoundaryFailure(error, ctx)) throw error
                  return null
                })
                const raw = giftModel(gift, m)
                const available = raw && giftUsable(s, gift, raw)
                if (!available) {
                  const error = { type: raw ? "rate_limit_error" : "invalid_request_error", message: raw ? `${giftRested(s, raw) ? `ZCode said the last request for ${raw} was over its quota, so it is not asked again for a minute: ` : ""}ZCode's limited quota for ${raw} is exhausted or temporarily unavailable` : `ZCode has no active limited quota for ${m}` }
                  return kept(new Response(JSON.stringify({ type: "error", error }), { status: raw ? 429 : 400, headers: { "content-type": "application/json" } }))
                }
                orig = opts.body = JSON.stringify({ ...JSON.parse(orig), model: wireModel(raw) })
                m = raw
                forcedGift = true
              } else if (!laneIntent && !alone && !edgeHeld() && !busyHeld() && !startFull(gateKey) && freeFirst(provider?.options) && s.jwt && known && giftUsable(s, known, m) && !giftSidelined(s, m)) {
                // free first: the plain model spends the account's own live
                // gift before it spends the coding plan, under the name the
                // gift lists. The gift is the route's cache alone (a failed
                // read cached its why), so a gift that cannot be read keeps
                // the request on the plan with no read per request; a
                // gift-only account is already on the gift and is left as
                // it was. A refusal past this check is replayed on the plan
                // in the same request (replayOnPlan below). A busy hold (or
                // a block page) keeps the plain model off the gift: the
                // coding plan is never held, and free first must not move
                // the request onto a route the hold answers 429 without
                // asking. Nor onto a gift whose slots are all taken
                // (startFull): the plan serves it now, the gift only after
                // the others.
                const raw = giftNames(known).find((x) => x.toLowerCase() === m.toLowerCase())
                orig = opts.body = JSON.stringify({ ...JSON.parse(orig), model: wireModel(raw) })
                m = raw
                freeGift = true
              }
              start = alone || forcedGift || freeGift
            }
            const swap = (base) => {
              for (const b of [START_BASE, ZAI_BASE, BIGMODEL_BASE]) {
                if (url.startsWith(b)) {
                  url = base + url.slice(b.length)
                  break
                }
              }
            }
            let h = new Headers(opts.headers)
            const askedUrl = url
            const place = workPlace(h)
            const sid = sessionOf(h.get("x-session-id"), s.device)
            // restModel is free first's gift turning this model away: the
            // model rests a minute and the route is dropped, so the requests
            // behind it re-read the balance and go straight to the plan. A
            // quota answer rests it in blocked, which a -Trial entry obeys
            // too; any other refusal (spent false) in sidelined, which only
            // free first reads.
            const restModel = (spent) => {
              rest(spent ? blocked : sidelined, s.key + "\0" + s.jwt + "\0" + asked.toLowerCase())
              routes.delete(s.key + "\0" + s.jwt)
            }
            // replayOnPlan is free first's answer to a gift that refused
            // what its own balance read had promised: the model as the
            // agent sent it, the plan's headers, the plan's own patience
            // (askBusy).
            const replayOnPlan = async () => {
              // the gift is done with: its slot goes to the next request
              own.slot?.release()
              let u = askedUrl
              for (const b of [START_BASE, ZAI_BASE, BIGMODEL_BASE]) {
                if (u.startsWith(b)) {
                  u = s.base + u.slice(b.length)
                  break
                }
              }
              const h2 = new Headers(opts.headers)
              workPlace(h2)
              h2.set("x-session-id", sid)
              paidMaterial = await ctx.projectToken()
              key = paidMaterial.token
              h2.delete("authorization")
              h2.set("x-api-key", key)
              h2.set("Authorization", "Bearer " + key)
              h2.delete("content-length")
              opts.body = nativeBody(marked(JSON.stringify({ ...JSON.parse(orig), model: asked }), s.device, sid))
              delete opts.duplex
              const routed = routeCodingPlanGateway(u, h2, { enabled: magpie?.option?.(PROVIDER, "coding_plan_ultra") === true, zcodeOrigin: ctx.state.origins.zcode })
              await ctx.assertCurrent()
              let result = await askBusy(routed.url, opts, routed.headers)
              if (ctx.state.kind !== "legacy-key" && result.status === 401) {
                await result.body?.cancel().catch(() => {})
                paidMaterial = await ctx.projectToken({ rejectedFingerprint: paidMaterial.fingerprint })
                routed.headers.set("x-api-key", paidMaterial.token)
                routed.headers.set("authorization", "Bearer " + paidMaterial.token)
                result = await askBusy(routed.url, opts, routed.headers)
              }
              await ctx.assertCurrent()
              return result
            }
            // a block page held this computer off zcode.z.ai (edge): the
            // Start Plan and a picked gift are answered here; free first
            // already left the gift for the plan. Checked before the busy
            // hold: an IP the firewall already refused keeps that behavior
            // (the 405 magpie reads as the network's), hold or no hold.
            if (start && edgeHeld()) return kept(edgeHeldAnswer())
            // a run of busy caps held this computer off zcode.z.ai before the
            // edge firewall could block the IP: a held request is answered
            // here, with a 429 magpie still reads as a rate limit
            if (start && busyHeld()) return kept(busyHeldAnswer())
            // the account's Start Plan slots (startGate): a request past them
            // waits here for one rather than being sent to be refused
            if (start) {
              own.slot = await startSlot(gateKey, opts.signal)
              own.signal = opts.signal
              if (!own.slot) return kept(startQueueAnswer())
              // a hold may have armed while it waited
              if (edgeHeld()) return kept(edgeHeldAnswer())
              if (busyHeld()) return kept(busyHeldAnswer())
            }
            if (start) {
              if (jwtExpired(s.jwt)) throw new Error(EXPIRED)
              if (!s.jwt) throw new Error("ZCode's sign-in has no key; sign in again")
              swap(START_BASE)
              // the Start Plan is served only to what looks like ZCode's own request
              url = startURL(url)
              const callerQuery = h.get("x-query-id")
              h = startHeaders(h, s.jwt)
              h.set("x-session-id", sid)
              if (opts.body != null) {
                const text = orig ?? (typeof opts.body === "string" ? opts.body : await new Response(opts.body).text())
                opts.body = nativeBody(dress(text, s.site, s.device, place, sid))
                const q = callerQuery || queryOf(text, sid)
                if (q) h.set("x-query-id", q)
                delete opts.duplex
                h.delete("content-length")
              }
            } else {
              if (!laneIntent) {
                paidMaterial = await ctx.projectToken()
                key = paidMaterial.token
              }
              swap(s.base)
              // a Messages request names the device, as ZCode's does (#387);
              // count_tokens and other paths ZCode never sends go as they are
              if (opts.body != null && /\/v1\/messages$/.test(new URL(url).pathname)) {
                const text = typeof opts.body === "string" ? opts.body : await new Response(opts.body).text()
                const next = nativeBody(marked(text, s.device, sid))
                if (next !== text) h.delete("content-length")
                opts.body = next
                delete opts.duplex
              }
              if (renamed) h.delete("content-length")
              h.set("x-session-id", sid)
              h.delete("authorization")
              if (!laneIntent) {
                h.set("x-api-key", key)
                h.set("Authorization", "Bearer " + key)
                const routed = routeCodingPlanGateway(url, h, { enabled: magpie?.option?.(PROVIDER, "coding_plan_ultra") === true, zcodeOrigin: ctx.state.origins.zcode })
                url = routed.url
                h = routed.headers
              }
            }
            // A concurrency refusal is terminal; only a rejected paid PAT
            // permits the once-only account-scoped recovery below.
            let res, fallbackRequested = false
            try {
              // Only Start Plan caps count toward the firewall hold: the
              // coding plan is served by another host and was never refused
              // with the IP's 405.
              await ctx.assertCurrent()
              if (laneIntent) {
                res = await runNativeLane(ctx, { url, init: { ...opts, headers: h }, model: modelOf(opts.body),
                  sessionId: metadata.session, execution: metadata.execution,
                  send: async (u, request) => {
                    await ctx.assertCurrent()
                    const response = await fetch(u, { ...request, redirect: "error" })
                    await ctx.assertCurrent()
                    return response
                  },
                  ...(metadata.execution.lane === "highspeed" ? { fallback: () => {
                    fallbackRequested = true
                    return Response.json({ type: "error", error: {
                      type: "highspeed_unavailable", message: "Highspeed is unavailable; ordinary routing requires authorization.",
                    } }, { status: 409, headers: {
                      "X-Magpie-ZCode-Fallback": "ordinary",
                      "X-Magpie-ZCode-Fallback-Model": metadata.execution.rawModel,
                    } })
                  } } : {}),
                })
              } else {
                res = await askBusy(url, opts, h, start ? busyCap : undefined)
                await ctx.assertCurrent()
                if (!start && ctx.state.kind !== "legacy-key" && res.status === 401) {
                  await res.body?.cancel().catch(() => {})
                  paidMaterial = await ctx.projectToken({ rejectedFingerprint: paidMaterial.fingerprint })
                  h.set("x-api-key", paidMaterial.token)
                  h.set("authorization", "Bearer " + paidMaterial.token)
                  res = await askBusy(url, opts, h)
                  await ctx.assertCurrent()
                }
              }
            } catch (e) {
              // free first moved the request onto the gift, so a gift that
              // cannot be asked at all — the network, the host — is answered
              // from the plan; the route read is dropped, so the next
              // request learns whether the host is back. A caller's abort
              // stays an abort.
              if (!freeGift || accountBoundaryFailure(e, ctx) || e?.responseStarted) throw e
              routes.delete(s.key + "\0" + s.jwt)
              res = await replayOnPlan()
            }
            if (fallbackRequested) return res
            res = await inferenceReply(res, opts.signal)
            // the Start Plan turning the request away says what to do about
            // it: the account blocked, or the network
            if (start && res.status >= 400) {
              const text = await readResponseText(res, opts.signal)
              if (startRefused(res.status, text)) {
                // a key account sent to the Start Plan (its plan list read
                // none) reads its plans again on the next request, so a
                // coding plan that is there after all is used (#369)
                // a block page holds every request off zcode.z.ai a while
                if (EDGE_PAGE.test(text)) edge.until = Date.now() + EDGE_HOLD
                if (freeGift) {
                  restModel(false)
                  return done(kept(await inferenceReply(await replayOnPlan(), opts.signal)))
                }
                if (s.key && !forcedGift) routes.delete(s.key + "\0" + s.jwt)
                return kept(startBlocked(res, text))
              }
              res = new Response(text, { status: res.status, statusText: res.statusText, headers: res.headers })
            }
            // a key with no plan behind it: what Z.ai said, and why a
            // ZCode gift plan can't be had with a key (NO_PLAN_KEY)
            if (!start && !s.jwt && res.status >= 400) {
              const text = await readResponseText(res, opts.signal)
              const why = noPlanKey(res, text)
              if (why) return done(kept(why))
              res = new Response(text, { status: res.status, statusText: res.statusText, headers: res.headers })
            }
            // a trial entry's gift answer that can't serve the request is
            // the agent's answer, not the coding plan's: the user picked the
            // gift, so a spent bucket says 429 (its quota error inside a 200
            // among them) and anything else goes on as it came. A quota
            // answer keeps that model off the gift for a minute, so a rate
            // limit is not knocked twice. A stream's body is never read here.
            if (start && forcedGift) {
              const json = (res.headers.get("content-type") ?? "").includes("application/json")
              if (res.status >= 400 || json) {
                const text = await readResponseText(res, opts.signal)
                // the hold's own answer (BUSY_HOLD_MARK) is a rate limit the
                // gift never said: it passes through as it came and arms
                // no rest
                const held = res.headers.get(BUSY_HOLD_MARK) === "1"
                const spent = !held && spentUp(text, res.status)
                if (!spent && res.status < 400) return done(kept(res, text))
                if (spent) rest(blocked, s.key + "\0" + s.jwt + "\0" + modelOf(orig).toLowerCase())
                return spent && res.status < 400 ? kept(new Response(text, { status: 429, headers: res.headers })) : done(kept(res, text))
              }
            }
            // free first's gift answer: the choice was the plugin's, so the
            // plan answers what the gift would fail — a spent bucket and a
            // refusal like any other 4xx or 5xx among them, the gift's own
            // waited-out busy cap included, since the plan's cap is its own.
            // A spent answer or a block page rests the model a minute and
            // drops the route read; a 401/403 drops the read alone, so the
            // next request learns whether the session is gone. Anything
            // else — a 200 carrying the answer — goes on as it came. A
            // stream's body is never read here.
            else if (start && freeGift) {
              const json = (res.headers.get("content-type") ?? "").includes("application/json")
              if (res.status >= 400 || json) {
                const text = await readResponseText(res, opts.signal)
                const cap = busy(res.status, text)
                // the hold's own answer is no spent answer either (see the
                // -Trial path above)
                const spent = !cap && res.headers.get(BUSY_HOLD_MARK) !== "1" && spentUp(text, res.status)
                if (spent) {
                  restModel(true)
                  return done(kept(await inferenceReply(await replayOnPlan(), opts.signal)))
                }
                if (res.status >= 400) {
                  if (!cap && (res.status === 401 || res.status === 403)) routes.delete(s.key + "\0" + s.jwt)
                  return done(kept(await inferenceReply(await replayOnPlan(), opts.signal)))
                }
                return done(kept(res, text))
              }
            }
            // the plan's answer goes on as it came, the account kept: the
            // built-in never marked a ZCode account lapsed nor cleared one.
            // A spent bucket's 200 says 429 (spentSaid), as a trial
            // entry's does, so magpie rests the model, not the account
            return done(kept(res))
        }
      },
      async usage(getAuth, _provider, { signal } = {}) {
        const began = Date.now()
        try {
          const ctx = await runtime.capture(getAuth, { signal })
          const card = await readNativeUsage(ctx)
          const auth = await getAuth()
          const s = stateOf(auth, { follow: false })
          routes.delete((ctx.state.kind === "legacy-key" ? s.key : ctx.scope) + "\0" + s.jwt)
          if (card.error) return card
          const material = await ctx.loginMaterial({ business: "none", zcode: "optional" })
          if (ctx.state.kind !== "start" && !material.zcode) {
            const why = s.jwt ? "ZCode's sign-in has expired: sign in again" :
              auth.type === "api" ? "not read with an API key: add the account with a ZCode sign-in" : "this sign-in has no ZCode session: sign in again"
            card.windows = [...(card.windows ?? []), { name: "ZCode gift plans (-Trial models) need ZCode's sign-in", used: 0, aside: true, display: why }]
          }
          const remaining = 19_000 - (Date.now() - began)
          if (material.zcode && remaining > 0) {
            const timeout = Math.min(2_000, remaining)
            try {
              const response = await ctx.request(`${ZCODE}${TOUCH}?seq=${nextTouchSeq()}`, {
                headers: { Authorization: `Bearer ${material.zcode}`, "X-Device-Mid": ctx.state.device, ...marketingHeaders() },
                signal: AbortSignal.timeout(timeout),
              }, { timeout })
              if (response.ok && response.data?.code === 0) {
                const plans = touchPlans(response.data.data)
                const unique = [...new Map((plans ?? []).map((p) => [p.plan_id, p])).values()]
                if (unique.length) card.windows = [...(card.windows ?? []), {
                  name: "Gift plans to claim in the ZCode app", used: 0, aside: true,
                  display: `${unique.length} · ${first(...unique.map((p) => p.name), "ZCode gift plan")}`,
                }]
              }
            } catch (e) {
              if (signal?.aborted) throw signal.reason ?? e
            }
          }
          return card
        } catch (e) {
          if (signal?.aborted) throw signal.reason ?? e
          return { error: e?.message ?? "account usage unavailable", signIn: "kept" }
        }
      },
      methods: [
        oauth("zai"),
        oauth("bigmodel"),
        { type: "oauth", label: "ZCode: Z.ai paste callback URL", authorize: async () => authorizeCallback("zai", log) },
        { type: "oauth", label: "ZCode: BigModel paste callback URL", authorize: async () => authorizeCallback("bigmodel", log) },
        // a team's plan beside the account's own (#285): the sign-ins above
        // take the account's own plan when it has one
        {
          type: "oauth",
          label: "ZCode: a team's GLM Coding Plan",
          prompts: [
            {
              type: "select",
              key: "site",
              message: "Where is the team's plan?",
              options: [
                { label: "Z.ai", value: "zai", hint: "z.ai" },
                { label: "BigModel (智谱)", value: "bigmodel", hint: "bigmodel.cn" },
              ],
            },
          ],
          authorize: (inputs) => authorize(inputs?.site === "bigmodel" ? "bigmodel" : "zai", log, true),
        },
        { type: "oauth", label: "ZCode app's sign-in", authorize: async () => asZCode() },
        {
          type: "api",
          label: "GLM Coding Plan API key",
          prompts: [
            {
              type: "select",
              key: "site",
              message: "Where is the key from?",
              options: [
                { label: "Z.ai", value: "zai", hint: "api.z.ai" },
                { label: "BigModel (智谱)", value: "bigmodel", hint: "open.bigmodel.cn" },
              ],
            },
          ],
        },
      ],
    },
  }
}

// for tests
// config sets what zcodeConfig has read, for tests: null is ZCode's config
// out of reach, undefined forgets it
const config = (cfg) => (cfgCache = cfg === undefined ? null : { cfg, at: Date.now() })
export const _internal = { edge, EDGE_HOLD, busyHold, startGate, BUSY_N, BUSY_WINDOW, BUSY_HOLD, busyCap, busyHeld, config, entry, routes, blocked, sidelined, plansOf, spentUp, giftServes, modelOf, ownSignIn, stateOf, dress, queryOf, PROMPT, marked, keyDevice, sessionOf, busy }
