// Magpie adaptation of zai-org/ZCode 3.18.0 (b95ebaff).
// Copyright 2026 Z.AI Co., Ltd. Licensed under the Apache License, Version 2.0.
// See LICENSE.ZCode and NOTICE.ZCode for the license and upstream notices.
import { createHmac, randomBytes, randomUUID } from "node:crypto"
import { release } from "node:os"
import { ZCODE_VERSION } from "./project-token.mjs"

export const nativeLaneOperations = Object.freeze([
  "billing.preview", "billing.status", "billing.claim.raw", "marketing.feed", "marketing.action",
  "offpeak.availability", "offpeak.ticket", "offpeak.status", "offpeak.settle",
  "highspeed.draw", "highspeed.healthy", "mcp.headers",
])

const APP_VERSION = ZCODE_VERSION
const PLATFORM = `${process.platform}-${process.arch}`
const MARKETING_SEQ = Symbol.for("magpie.zcode.marketingSeq")
const scopeKey = randomBytes(32)
const MAX_ERROR_BYTES = 64 * 1024
const STATES = new Set(["queued", "ready", "active", "expired", "settled", "not_found"])
const MODELS = new Set(["GLM-5.3", "GLM-5.3-Flash"])
const object = (x) => x !== null && typeof x === "object" && !Array.isArray(x)
const nonempty = (x) => typeof x === "string" && x.trim().length > 0
const finite = (x) => typeof x === "number" && Number.isFinite(x)
const whole = (x) => Number.isSafeInteger(x) && x >= 0
const optional = (x, predicate) => x === undefined || predicate(x)
const id = (x) => typeof x === "string" && x.trim().length > 0 && x.trim().length <= 128
const codeOf = (x) => typeof x === "number" && Number.isSafeInteger(x) ? x : typeof x === "string" && /^\d+$/.test(x) && Number.isSafeInteger(Number(x)) ? Number(x) : undefined

function failure(kind, message, extra = {}) {
  return Object.assign(new Error(message), { kind, ...extra })
}
function malformed(operation) { return failure("malformed", `${operation}: invalid vendor response`, { dispatched: true }) }
function invalid(message) { return failure("invalid", message, { dispatched: false }) }
function requestId(headers) {
  const value = headers?.get?.("x-request-id")?.trim()
  return value && /^[\w.:-]{1,128}$/.test(value) ? value : undefined
}
function httpFailure(operation, response, code) {
  const retry = response.headers?.get?.("retry-after")?.trim()
  const retryAfter = retry && /^\d+(?:\.\d+)?$/.test(retry) ? Number(retry) :
    retry && Number.isFinite(Date.parse(retry)) ? Math.max(0, (Date.parse(retry) - Date.now()) / 1000) : undefined
  return failure("http", `${operation}: HTTP ${response.status}${code === undefined ? "" : `, code ${code}`}`, {
    httpStatus: response.status, ...(code === undefined ? {} : { code }),
    retryable: response.status >= 500, throttled: response.status === 429,
    dispatched: true, ...(requestId(response.headers) ? { requestId: requestId(response.headers) } : {}),
    ...(finite(retryAfter) && retryAfter >= 0 ? { retryAfter } : {}),
  })
}
function businessFailure(operation, code, response) {
  const nextDrawAt = response.data?.data?.next_draw_at
  return failure("business", `${operation}: vendor code ${code}`, {
    code, httpStatus: response.status, retryable: false, throttled: code === 3002,
    dispatched: true, ...(requestId(response.headers) ? { requestId: requestId(response.headers) } : {}),
    ...(whole(nextDrawAt) && nextDrawAt > 0 ? { nextDrawAt } : {}),
  })
}
function originURL(value, path) {
  let origin
  try { origin = new URL(value) } catch { throw failure("unavailable", "configured ZCode origin is unavailable") }
  if (origin.username || origin.password || !["https:", "http:"].includes(origin.protocol)) throw failure("unavailable", "configured ZCode origin is invalid")
  // HTTP is useful only for independently configured loopback fixtures/development.
  if (origin.protocol === "http:" && !["localhost", "127.0.0.1", "[::1]"].includes(origin.hostname)) throw failure("unavailable", "configured ZCode origin is not secure")
  return new URL(path, origin.origin)
}
function zcodeURL(ctx, path) { return originURL(ctx.state?.origins?.zcode, path) }
function paidState(ctx) {
  const state = ctx.state ?? {}
  if (!["zai", "bigmodel"].includes(state.site) || !["individual", "team"].includes(state.kind)) {
    throw failure("unavailable", "this operation requires a selected Coding Plan account")
  }
  if (!nonempty(state.zcodeUserId)) throw failure("unavailable", "the selected ZCode user identity is unavailable")
  if (state.kind === "team" && (!nonempty(state.org) || !nonempty(state.project))) {
    throw failure("unavailable", "the selected team organization and project are required")
  }
  return state
}
function teamHeaders(state, target = true) {
  return state.kind === "team"
    ? { ...(target ? { "Bigmodel-Target-Type": "TEAM" } : {}), "Bigmodel-Organization": state.org, "Bigmodel-Project": state.project }
    : target ? { "Bigmodel-Target-Type": "PERSONAL" } : {}
}
async function offpeakHeaders(ctx, rejectedFingerprint) {
  const state = paidState(ctx)
  const { zcode } = await ctx.loginMaterial({ business: "none", zcode: "required" })
  const pat = await ctx.projectToken({ rejectedFingerprint })
  if (!nonempty(pat?.token)) throw failure("unavailable", "project access token unavailable; sign in again")
  await ctx.assertCurrent()
  return { fingerprint: pat.fingerprint, headers: {
    Authorization: `Bearer ${zcode}`, "X-Coding-Plan-Api-Key": pat.token,
    ...(state.site === "bigmodel" && state.kind === "team" ? teamHeaders(state, false) : {}),
  } }
}
async function highspeedHeaders(ctx) {
  const state = paidState(ctx)
  const { business, zcode } = await ctx.loginMaterial({ business: "required", zcode: "required" })
  await ctx.assertCurrent()
  return { Authorization: `Bearer ${zcode}`, "X-Bigmodel-Authorization": `Bearer ${business}`, ...teamHeaders(state) }
}
async function beforeDispatch(resolve) {
  try { return await resolve() } catch (error) {
    // Credential refresh can itself use the network; it cannot have taken a
    // ticket or drawn a card, which are the mutations being coordinated.
    if (error && typeof error === "object") error.dispatched = false
    throw error
  }
}
function sourceHeaders(ctx) {
  let locale = "unknown", timeZone = "unknown"
  try {
    const resolved = Intl.DateTimeFormat().resolvedOptions()
    locale = resolved.locale || locale
    timeZone = resolved.timeZone || timeZone
  } catch {}
  return {
    "User-Agent": `ZCode/${APP_VERSION}`, "HTTP-Referer": ctx.state.origins.zcode,
    "X-ZCode-App-Version": APP_VERSION, "X-Title": "Z Code@electron", "X-Platform": PLATFORM,
    "X-Release-Channel": "production", "X-Client-Language": locale, "X-Client-Timezone": timeZone,
    "X-Os-Category": process.platform === "darwin" ? "macos" : process.platform === "win32" ? "windows" : "linux",
    "X-Os-Version": release(), "X-Request-Id": randomUUID(),
    ...(nonempty(ctx.state.device) ? { "X-Device-Mid": ctx.state.device } : {}),
  }
}
async function request(ctx, path, { method = "GET", headers = {}, body, timeout = 15000 } = {}) {
  return ctx.request(zcodeURL(ctx, path).href, {
    method, headers: { ...sourceHeaders(ctx), ...headers, ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }), redirect: "error",
  }, { timeout })
}
function requireHttp(operation, res) {
  if (!res.ok) throw httpFailure(operation, res, codeOf(res.data?.code))
  if (!object(res.data)) throw malformed(operation)
  return res.data
}

function stringFields(value, fields) {
  const out = {}
  for (const name of fields) if (typeof value?.[name] === "string") out[name] = value[name]
  return out
}
function numberFields(value, fields) {
  const out = {}
  for (const name of fields) if (finite(value?.[name])) out[name] = value[name]
  return out
}
function capabilities(value) {
  return Array.isArray(value) && value.every((item) => typeof item === "string") ? { capabilities: value } : {}
}
function previewEntitlement(value) {
  return { entitlement_id: value.entitlement_id.trim(),
    ...stringFields(value, ["show_name", "meter", "unit_type", "period"]),
    ...numberFields(value, ["grant_units", "priority", "effective_at"]), ...capabilities(value.capabilities) }
}
function previewPlan(value) {
  if (!optional(value.entitlements, Array.isArray)) throw malformed("billing.preview")
  return { plan_id: value.plan_id.trim(), ...stringFields(value, ["name", "description"]),
    ...numberFields(value, ["priority"]),
    entitlements: (value.entitlements ?? []).filter((e) => object(e) && nonempty(e.entitlement_id)).map(previewEntitlement) }
}
function parsePreview(data) {
  if (!object(data) || !optional(data.plans, Array.isArray)) throw malformed("billing.preview")
  if (data.server_time !== undefined && (!finite(data.server_time) || data.server_time < 0)) throw malformed("billing.preview")
  const plans = (data.plans ?? []).filter((p) => object(p) && nonempty(p.plan_id)).map(previewPlan)
  return { ...numberFields(data, ["server_time"]), plans }
}
function claimData(data, requestedPlan) {
  const plan = data.plan
  if (!nonempty(plan.plan_id) || plan.plan_id.trim() !== requestedPlan || !optional(plan.entitlements, Array.isArray)) throw malformed("billing.claim")
  const entitlements = (plan.entitlements ?? []).filter((e) => object(e) && nonempty(e.entitlement_id)).map((e) => ({
    entitlement_id: e.entitlement_id.trim(), ...stringFields(e, ["show_name"]), ...numberFields(e, ["effective_at"]),
  }))
  return { ...numberFields(data, ["server_time"]), plan: {
    ...stringFields(plan, ["user_plan_id", "status"]), plan_id: plan.plan_id.trim(),
    ...numberFields(plan, ["starts_at", "ends_at"]), entitlements,
  } }
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i
function localeOf(value) {
  if (value === undefined) {
    let locale = ""
    try { locale = Intl.DateTimeFormat().resolvedOptions().locale } catch {}
    return locale.toLowerCase().startsWith("zh") ? "zh-CN" : "en-US"
  }
  if (!["zh-CN", "en-US"].includes(value)) throw invalid("invalid marketing locale")
  return value
}
function marketingScope(ctx, token) {
  // The process-local keyed scope changes on credential changes without exposing a token fingerprint.
  const hex = createHmac("sha256", scopeKey).update(JSON.stringify([ctx.scope, ctx.generation, token])).digest("hex")
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-4${hex.slice(13, 16)}-8${hex.slice(17, 20)}-${hex.slice(20, 32)}`
}
async function marketingIdentity(ctx, locale) {
  const { zcode = "" } = await ctx.loginMaterial({ business: "none", zcode: "optional" })
  if (!UUID.test(ctx.state?.device ?? "")) throw failure("unavailable", "marketing device identity is unavailable")
  return {
    token: zcode, scope: marketingScope(ctx, zcode),
    headers: { "X-Device-Mid": ctx.state.device, "X-Client-Language": locale, "X-ZCode-App-Version": APP_VERSION, ...(zcode ? { Authorization: `Bearer ${zcode}` } : {}) },
  }
}
function asset(value) {
  if (!object(value) || typeof value.src !== "string" || value.src.length > 4096 || !/^[a-f0-9]{64}$/.test(value.sha256 ?? "")) return false
  try { const u = new URL(value.src); return ["http:", "https:"].includes(u.protocol) && !u.username && !u.password } catch { return false }
}
function text(value, banner = false) {
  return object(value) && ["plaintext", "html", "markdown", ...(banner ? [""] : [])].includes(value.format) && typeof value.content === "string" && value.content.length <= 20000
}
function visual(v) {
  if (!object(v) || (v.args !== undefined && (!object(v.args) || JSON.stringify(v.args).length > 64000))) return false
  if (v.type === "image") return object(v.image) && asset(v.image.default) && (v.image.dark == null || asset(v.image.dark))
  if (v.type === "video") return object(v.video) && asset(v.video.src) && asset(v.video.fallback)
  if (v.type === "bundle") return object(v.bundle) && asset(v.bundle.bundle) && typeof v.bundle.entry === "string" && v.bundle.entry.length <= 240 && !v.bundle.entry.includes("\\") && v.bundle.entry.split("/").every((part) => part && part !== "." && part !== "..") && (v.bundle.fallback == null || asset(v.bundle.fallback))
  return false
}
function action(a) {
  if (!object(a)) return false
  if (a.type === "close") return true
  if (!object(a.args)) return false
  if (a.type === "claim_zcode_plan") return id(a.args.plan_id)
  if (a.type === "copy_text") return typeof a.args.text === "string" && a.args.text.length <= 20000 && !!a.args.text.trim() && Object.keys(a.args).every((k) => k === "text")
  if (a.type === "open_url") {
    if (typeof a.args.url !== "string" || a.args.url.length > 4096) return false
    try { const u = new URL(a.args.url); return ["http:", "https:"].includes(u.protocol) && !u.username && !u.password } catch { return false }
  }
  if (a.type !== "navigate") return false
  const n = a.args
  if (["upgrade", "rewards"].includes(n.page)) return Object.keys(n).every((k) => k === "page")
  if (n.page === "plugin_marketplace") return Object.keys(n).every((k) => ["page", "plugin_id"].includes(k)) && (n.plugin_id === undefined || (id(n.plugin_id) && /^[^@\s]+@[^@\s]+$/.test(n.plugin_id)))
  if (n.page !== "settings" || !Object.keys(n).every((k) => ["page", "section", "provider_id"].includes(k))) return false
  return optional(n.section, (s) => ["general", "appearance", "models", "browser", "computer_use", "memory", "subagents", "plugins", "mcp", "skills", "commands", "hooks", "usage"].includes(s)) && (n.provider_id === undefined || (n.section === "models" && id(n.provider_id)))
}
function button(b, banner) {
  if (!object(b) || !text(b.text, banner) || !action(b.action)) return false
  if (banner || b.theme == null) return true
  return object(b.theme) && optional(b.theme.variant, (v) => ["", "default", "outline", "secondary", "ghost", "destructive", "warning", "link"].includes(v)) && optional(b.theme.class, (v) => typeof v === "string" && v.length <= 512) && optional(b.theme.style, (v) => typeof v === "string" && v.length <= 512)
}
function popup(p) {
  return object(p) && optional(p.layout, (v) => ["", "v1"].includes(v)) && text(p.title) && text(p.description) && (p.hero == null || visual(p.hero)) && Array.isArray(p.buttons) && p.buttons.length <= 4 && p.buttons.every((b) => button(b, false))
}
function delivery(d) {
  if (!object(d) || !id(d.campaign_id) || !Number.isInteger(d.priority) || d.priority < 0 || d.priority > 100) return false
  if (d.resource_position === "popup") return popup(d.popup)
  if (d.resource_position !== "banner" || !object(d.banner)) return false
  const b = d.banner
  return optional(b.layout, (v) => ["", "v1"].includes(v)) && visual(b.background) && Array.isArray(b.buttons) && b.buttons.length <= 2 && b.buttons.every((x) => button(x, true)) && b.buttons.filter((x) => x.action.type === "close").length <= 1 && b.buttons.filter((x) => x.action.type !== "close").length <= 1 && (b.success_popup == null || popup(b.success_popup))
}
const normalizedAsset = (a) => ({ src: a.src, sha256: a.sha256 })
function normalizedVisual(v) {
  const args = v.args === undefined ? {} : { args: v.args }
  if (v.type === "image") return { type: v.type, image: { default: normalizedAsset(v.image.default), ...(v.image.dark === undefined ? {} : { dark: v.image.dark === null ? null : normalizedAsset(v.image.dark) }) }, ...args }
  if (v.type === "video") return { type: v.type, video: { src: normalizedAsset(v.video.src), fallback: normalizedAsset(v.video.fallback) }, ...args }
  return { type: v.type, bundle: { bundle: normalizedAsset(v.bundle.bundle), entry: v.bundle.entry, ...(v.bundle.fallback === undefined ? {} : { fallback: v.bundle.fallback === null ? null : normalizedAsset(v.bundle.fallback) }) }, ...args }
}
function normalizedAction(a) {
  if (a.type === "close") return { type: a.type }
  if (a.type === "open_url") return { type: a.type, args: { url: a.args.url } }
  if (a.type === "claim_zcode_plan") return { type: a.type, args: { plan_id: a.args.plan_id.trim() } }
  if (a.type === "copy_text") return { type: a.type, args: { text: a.args.text } }
  const n = a.args
  return { type: a.type, args: { page: n.page, ...(n.section === undefined ? {} : { section: n.section }), ...(n.provider_id === undefined ? {} : { provider_id: n.provider_id.trim() }), ...(n.plugin_id === undefined ? {} : { plugin_id: n.plugin_id.trim() }) } }
}
function normalizedButton(b, banner) {
  const result = { text: { format: b.text.format || "plaintext", content: b.text.content }, action: normalizedAction(b.action) }
  if (!banner && b.theme !== undefined) result.theme = b.theme === null ? null : { variant: b.theme.variant || "default", ...(b.theme.class === undefined ? {} : { class: b.theme.class }), ...(b.theme.style === undefined ? {} : { style: b.theme.style }) }
  return result
}
function normalizedPopup(p) {
  return { ...(p.layout === undefined ? {} : { layout: p.layout }), title: { format: p.title.format, content: p.title.content }, description: { format: p.description.format, content: p.description.content },
    ...(p.hero === undefined ? {} : { hero: p.hero === null ? null : normalizedVisual(p.hero) }), buttons: p.buttons.map((b) => normalizedButton(b, false)) }
}
function normalizedDelivery(d) {
  const base = { campaign_id: d.campaign_id.trim(), priority: d.priority, resource_position: d.resource_position }
  if (d.resource_position === "popup") return { ...base, popup: normalizedPopup(d.popup) }
  const b = d.banner
  return { ...base, banner: { ...(b.layout === undefined ? {} : { layout: b.layout }), background: normalizedVisual(b.background),
    buttons: b.buttons.map((item) => normalizedButton(item, true)), ...(b.success_popup === undefined ? {} : { success_popup: b.success_popup === null ? null : normalizedPopup(b.success_popup) }) } }
}
function marketingSnapshot(envelope, scope) {
  const d = envelope?.data
  if (envelope?.code !== 0 || !object(d) || !finite(d.server_time) || !["zh-CN", "en-US"].includes(d.language) || !Array.isArray(d.deliveries) || d.deliveries.length > 32) throw malformed("marketing.feed")
  const deliveries = [], positions = new Set()
  let rejectedCount = 0
  for (const item of d.deliveries) {
    if (!delivery(item) || positions.has(item.resource_position)) { rejectedCount++; continue }
    positions.add(item.resource_position)
    deliveries.push(normalizedDelivery(item))
  }
  return { serverTime: d.server_time * 1000, language: d.language, deliveries, rejectedCount, scope }
}

function validTicket(t, take = false) {
  return object(t) && nonempty(t.ticket_id) && STATES.has(t.state) && optional(t.task_id, (x) => typeof x === "string") && (t.position == null || whole(t.position)) && optional(t.active_deadline, finite) && (!take || (optional(t.accepted, (x) => typeof x === "boolean") && optional(t.next_poll_after, (x) => finite(x) && x >= 0) && optional(t.queued_at, finite) && optional(t.ready_deadline, finite)))
}
function selectedFields(value, names) {
  const out = {}
  for (const name of names) if (Object.hasOwn(value, name)) out[name] = value[name]
  return out
}
function offpeakData(operation, res) {
  if (!res.ok) {
    const code = codeOf(res.data?.code)
    const error = httpFailure(operation, res, code)
    const next = res.data?.next_take_at ?? res.data?.data?.next_take_at
    if (whole(next) && next > 0) error.nextTakeAt = next
    throw error
  }
  if (operation === "offpeak.settle" && res.emptyBody === true) return {}
  if (!object(res.data)) throw malformed(operation)
  if (res.data.code !== undefined && codeOf(res.data.code) !== 0) throw businessFailure(operation, codeOf(res.data.code) ?? -1, res)
  return res.data.code === 0 && Object.hasOwn(res.data, "data") ? res.data.data : res.data
}
function highspeedData(operation, res) {
  const payload = requireHttp(operation, res)
  const code = codeOf(payload.code)
  if (code === undefined) throw malformed(operation)
  if (code !== 0) throw businessFailure(operation, code, res)
  const d = payload.data
  const common = typeof payload.msg === "string" && object(d)
  const draw = operation === "highspeed.draw"
  const validCard = (card) => object(card) && [card.card_id, card.task_id, card.provider, card.model].every(nonempty) && whole(card.issued_at) && whole(card.expires_at)
  const valid = common && (draw ? (d.card === null || validCard(d.card)) && whole(d.next_draw_at) : nonempty(d.card_id) && whole(d.prompt_tokens) && whole(d.completion_tokens) && whole(d.duration_seconds))
  if (!valid) throw malformed(operation)
  return d
}

export function trustedOfficialMcpURL(value, origins) {
  let url
  try { url = new URL(value) } catch { return false }
  if (url.username || url.password) return false
  const configured = typeof origins === "string" ? origins : origins?.zcode
  if (url.protocol === "https:") {
    try { const expected = new URL(configured); return expected.protocol === "https:" && !expected.username && !expected.password && url.origin === expected.origin } catch { return false }
  }
  if (url.protocol !== "http:" || !["localhost", "127.0.0.1", "[::1]"].includes(url.hostname)) return false
  const allowed = Array.isArray(origins?.mcpDev) ? origins.mcpDev : []
  return allowed.some((candidate) => {
    try { const c = new URL(candidate); return c.protocol === "http:" && !c.username && !c.password && ["localhost", "127.0.0.1", "[::1]"].includes(c.hostname) && c.origin === url.origin } catch { return false }
  })
}

export async function nativeLaneOperation(ctx, operation, params = {}) {
  if (!nativeLaneOperations.includes(operation) || !object(params)) throw invalid("unsupported native account operation")
  if (operation === "mcp.headers") {
    if (params.stdio === true && params.url !== undefined) throw failure("untrusted_url", "official MCP destination is ambiguous")
    const target = params.stdio === true ? ctx.state?.origins?.zcode : params.url
    if (!trustedOfficialMcpURL(target, ctx.state?.origins)) throw failure("untrusted_url", "official MCP destination is untrusted")
    const state = ctx.state ?? {}
    const paid = ["individual", "team"].includes(state.kind)
    if (paid) paidState(ctx)
    const { business, zcode } = await ctx.loginMaterial({ business: paid ? "required" : "none", zcode: "required" })
    await ctx.assertCurrent()
    return { headers: {
      Authorization: `Bearer ${zcode}`,
      ...(paid ? { "X-Bigmodel-Authorization": `Bearer ${business}` } : {}),
      ...(paid && (state.kind === "individual" || state.site === "bigmodel") ? teamHeaders(state) : {}),
    } }
  }
  if (operation === "billing.preview") {
    const { zcode } = await ctx.loginMaterial({ business: "none", zcode: "optional" })
    const u = zcodeURL(ctx, "/api/v1/zcode-plan/billing/preview")
    u.searchParams.set("app_version", APP_VERSION); u.searchParams.set("platform", PLATFORM)
    const res = await ctx.request(u.href, { method: "GET", headers: { ...sourceHeaders(ctx), ...(zcode ? { Authorization: `Bearer ${zcode}` } : {}) }, redirect: "error" }, { timeout: 15000 })
    const env = requireHttp(operation, res)
    if (env.code !== undefined && env.code !== 0) throw businessFailure(operation, codeOf(env.code) ?? -1, res)
    return parsePreview(env.data)
  }
  if (operation === "billing.status") {
    const { zcode } = await ctx.loginMaterial({ business: "none", zcode: "required" })
    const u = zcodeURL(ctx, "/api/v1/zcode-plan/billing/balance")
    u.searchParams.set("app_version", APP_VERSION)
    const res = await ctx.request(u.href, { headers: { ...sourceHeaders(ctx), Authorization: `Bearer ${zcode}` } })
    const env = requireHttp(operation, res)
    const code = codeOf(env.code)
    if (code === undefined) throw malformed(operation)
    if (code !== 0) throw businessFailure(operation, code, res)
    if (!object(env.data) || !Array.isArray(env.data.plans)) throw malformed(operation)
    const plans = env.data.plans.map((plan) => {
      if (!object(plan) || !nonempty(plan.plan_id) || !nonempty(plan.user_plan_id) || typeof plan.status !== "string" ||
          !optional(plan.starts_at, finite) || !optional(plan.ends_at, finite)) throw malformed(operation)
      return { plan_id: plan.plan_id.trim(), user_plan_id: plan.user_plan_id.trim(), status: plan.status,
        ...numberFields(plan, ["starts_at", "ends_at"]) }
    })
    return { plans }
  }
  if (operation === "billing.claim.raw") {
    if (!nonempty(params.plan_id)) throw invalid("billing.claim requires plan_id")
    if (!nonempty(ctx.state?.zcodeUserId)) throw failure("unavailable", "the selected ZCode user identity is unavailable", { dispatched: false })
    let verification
    try { verification = await ctx.claimVerification() } catch (error) {
      if (aborted(ctx.signal, error)) throw error
      await ctx.assertCurrent()
      throw failure("verification_unavailable", "claim verification policy is unavailable", { dispatched: false })
    }
    if (verification !== false) throw failure("verification_unavailable", verification === true ? "claim requires unavailable verification; use the ZCode app" : "claim verification policy is unavailable", { dispatched: false })
    const { zcode } = await beforeDispatch(() => ctx.loginMaterial({ business: "none", zcode: "required" }))
    const res = await request(ctx, "/api/v1/zcode-plan/billing/claim", { method: "POST", headers: { Authorization: `Bearer ${zcode}`, "X-ZCode-App-Version": APP_VERSION, "X-Platform": PLATFORM }, body: { plan_id: params.plan_id } })
    const env = requireHttp(operation, res), code = codeOf(env.code)
    if (code === undefined) throw malformed(operation)
    if (code !== 0) throw businessFailure(operation, code, res)
    if (!object(env.data) || !object(env.data.plan)) throw malformed(operation)
    return claimData(env.data, params.plan_id)
  }
  if (operation === "marketing.feed" || operation === "marketing.action") {
    const locale = localeOf(params.locale)
    const identity = await marketingIdentity(ctx, locale)
    if (operation === "marketing.feed") {
      const seq = Number.isSafeInteger(globalThis[MARKETING_SEQ]) ? globalThis[MARKETING_SEQ] : 0
      globalThis[MARKETING_SEQ] = seq + 1
      const res = await request(ctx, `/api/v1/marketing/touch?seq=${seq}`, { headers: identity.headers })
      const env = requireHttp(operation, res)
      const latest = await marketingIdentity(ctx, locale)
      await ctx.assertCurrent()
      if (latest.scope !== identity.scope) throw failure("stale", "marketing identity changed")
      return marketingSnapshot(env, identity.scope)
    }
    if (params.scope !== identity.scope) throw failure("stale", "marketing identity changed")
    if (!id(params.campaign_id) || !["confirm", "cancel"].includes(params.action_type)) throw invalid("invalid marketing action")
    const res = await request(ctx, "/api/v1/marketing/touch/action", { method: "POST", headers: identity.headers, body: { campaign_id: params.campaign_id.trim(), action_type: params.action_type } })
    const code = codeOf(requireHttp(operation, res).code)
    if (code === undefined) throw malformed(operation)
    if (code !== 0) throw businessFailure(operation, code, res)
    return {}
  }
  if (operation.startsWith("offpeak.")) {
    paidState(ctx)
    let path, method = "GET", body
    if (operation === "offpeak.availability") path = "/ticket/availability"
    else if (operation === "offpeak.ticket") {
      if (!nonempty(params.task_id)) throw invalid("offpeak.ticket requires task_id")
      path = "/ticket"; method = "POST"; body = { task_id: params.task_id }
    } else if (operation === "offpeak.status") {
      if (!Array.isArray(params.ticket_ids) || params.ticket_ids.length > 100 || !params.ticket_ids.every(nonempty)) throw invalid("offpeak.status requires at most 100 ticket_ids")
      if (!params.ticket_ids.length) return { tickets: [] }
      path = "/ticket/status"; method = "POST"; body = { ticket_ids: params.ticket_ids }
    } else {
      if (!nonempty(params.ticket_id)) throw invalid("offpeak.settle requires ticket_id")
      path = `/ticket/${encodeURIComponent(params.ticket_id)}/settle`; method = "POST"
    }
    const { headers } = await beforeDispatch(() => offpeakHeaders(ctx))
    const res = await request(ctx, `/api/v1/off-peak${path}`, { method, body, headers, timeout: 10000 })
    const data = offpeakData(operation, res)
    if (!object(data)) throw malformed(operation)
    if (operation === "offpeak.availability") {
      if (typeof data.can_take_number !== "boolean" || !optional(data.next_take_at, (n) => whole(n) && n > 0) || (!data.can_take_number && data.next_take_at === undefined)) throw malformed(operation)
      return selectedFields(data, ["can_take_number", "next_take_at"])
    } else if (operation === "offpeak.ticket") {
      if (!validTicket(data, true) || (data.task_id !== undefined && data.task_id !== params.task_id)) throw malformed(operation)
      return selectedFields(data, ["ticket_id", "task_id", "state", "accepted", "position", "next_poll_after", "queued_at", "ready_deadline"])
    } else if (operation === "offpeak.status") {
      if (!optional(data.next_poll_after, (x) => finite(x) && x >= 0) || !optional(data.tickets, (v) => Array.isArray(v) && v.every((t) => validTicket(t))) || (data.tickets ?? []).some((t) => !params.ticket_ids.includes(t.ticket_id))) throw malformed(operation)
      return { ...selectedFields(data, ["next_poll_after"]), tickets: (data.tickets ?? []).map((t) => selectedFields(t, ["ticket_id", "task_id", "state", "position", "active_deadline"])) }
    } else if (!optional(data.ticket_id, nonempty) || !optional(data.task_id, (x) => typeof x === "string") || !optional(data.state, (x) => typeof x === "string") || !optional(data.settled_at, finite) || (data.ticket_id !== undefined && data.ticket_id !== params.ticket_id)) throw malformed(operation)
    return selectedFields(data, ["ticket_id", "task_id", "state", "settled_at"])
  }
  if (operation === "highspeed.draw") {
    const state = paidState(ctx)
    if (!nonempty(params.task_id) || !MODELS.has(params.model)) throw invalid("highspeed.draw requires a task_id and an eligible model")
    const provider = state.site === "zai" ? "builtin:zai-coding-plan" : "builtin:bigmodel-coding-plan"
    const headers = await beforeDispatch(() => highspeedHeaders(ctx))
    const res = await request(ctx, "/api/v1/highspeed/draw", { method: "POST", headers, body: { task_id: params.task_id, provider, model: params.model }, timeout: 20000 })
    const data = highspeedData(operation, res)
    if (data.card && (data.card.task_id !== params.task_id || data.card.provider !== provider || data.card.model !== params.model)) throw malformed(operation)
    return { card: data.card && selectedFields(data.card, ["card_id", "task_id", "provider", "model", "issued_at", "expires_at"]), next_draw_at: data.next_draw_at }
  }
  if (!nonempty(params.card_id)) throw invalid("highspeed.healthy requires card_id")
  paidState(ctx)
  const { zcode } = await ctx.loginMaterial({ business: "none", zcode: "required" })
  const res = await request(ctx, `/api/v1/highspeed/${encodeURIComponent(params.card_id)}/healthy`, { headers: { Authorization: `Bearer ${zcode}` }, timeout: 20000 })
  const data = highspeedData(operation, res)
  if (data.card_id !== params.card_id) throw malformed(operation)
  return selectedFields(data, ["card_id", "prompt_tokens", "completion_tokens", "duration_seconds"])
}

export function routeCodingPlanGateway(input, headers, { enabled = false, zcodeOrigin } = {}) {
  if (!enabled) return { url: input, headers }
  let url
  try { url = new URL(input) } catch { return { url: input, headers } }
  if (url.protocol !== "https:" || url.username || url.password || url.port) return { url: input, headers }
  const path = url.pathname.replace(/\/+$/, "") || "/"
  let target
  if (url.hostname === "open.bigmodel.cn" && path === "/api/anthropic/v1/messages") target = "/api/v1/ultra/anthropic/v1/messages"
  else if (url.hostname === "api.z.ai" && path === "/api/anthropic/v1/messages") target = "/api/v1/ultra-zai/anthropic/v1/messages"
  else return { url: input, headers }
  const routed = originURL(zcodeOrigin, target)
  routed.search = url.search
  const resultHeaders = new Headers(headers)
  resultHeaders.delete("host")
  return { url: routed.href, headers: resultHeaders }
}

function aborted(signal, error) { return signal?.aborted || error?.name === "AbortError" }
function identityChanged(error) { return ["stale", "account_scope_changed", "account_generation_changed"].includes(error?.kind) }
function contextExceeded(value) {
  let text
  try { text = (typeof value === "string" ? value : JSON.stringify(value ?? "")).toLowerCase() } catch { return false }
  return /context_(length|window)_exceeded|model_context(_window)?_exceeded|prompt_too_long/.test(text) || (text.includes("context") && text.includes("exceed")) || text.includes("prompt is too long") || text.includes("range of input length should be") || (text.includes("input token count") && text.includes("exceed") && text.includes("maximum number of tokens allowed")) || (text.includes("total message token length") && text.includes("exceed") && text.includes("model limit")) || (text.includes("maximum context length") && text.includes("tokens") && (text.includes("requested") || text.includes("resulted")))
}
function errorContextExceeded(error) {
  const seen = new Set()
  while (error && typeof error === "object" && !seen.has(error)) {
    seen.add(error)
    if ([error.message, error.code, error.providerCode, error.responseBody].some(contextExceeded)) return true
    error = error.cause
  }
  return contextExceeded(error)
}
function errorCode(value) {
  return codeOf(value?.providerCode) ?? codeOf(value?.error?.providerCode) ??
    codeOf(value?.provider_code) ?? codeOf(value?.error?.provider_code) ??
    codeOf(value?.error_code) ?? codeOf(value?.error?.error_code) ??
    codeOf(value?.code) ?? codeOf(value?.error?.code)
}
function acceptedOutput(value) {
  return object(value) && ((Array.isArray(value.content) && value.content.length > 0) || (Array.isArray(value.choices) && value.choices.length > 0) || (Array.isArray(value.output) && value.output.length > 0))
}
function errorFrame(value) {
  if (!object(value) || acceptedOutput(value)) return false
  const code = errorCode(value)
  return value.type === "error" || object(value.error) || value.success === false || (code !== undefined && code !== 0)
}
function harmlessPrelude(value) {
  return value?.type === "ping"
}
function retryAfter(headers) {
  const raw = headers.get("retry-after")?.trim()
  let ms = 60000
  if (raw && /^\d+(?:\.\d+)?$/.test(raw)) ms = Number(raw) * 1000
  else if (raw && Number.isFinite(Date.parse(raw))) ms = Math.max(0, Date.parse(raw) - Date.now())
  return Math.min(ms, 300000)
}
function delay(ms, signal) {
  if (signal?.aborted) return Promise.reject(signal.reason ?? new DOMException("Aborted", "AbortError"))
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { signal?.removeEventListener("abort", stop); resolve() }, ms)
    const stop = () => { clearTimeout(timer); reject(signal.reason ?? new DOMException("Aborted", "AbortError")) }
    signal?.addEventListener("abort", stop, { once: true })
  })
}
async function readBeforeBoundary(reader, signal, timeout) {
  let timer, stop
  const interrupted = new Promise((_, reject) => {
    if (signal?.aborted) reject(signal.reason ?? new DOMException("Aborted", "AbortError"))
    else if (signal) {
      stop = () => reject(signal.reason ?? new DOMException("Aborted", "AbortError"))
      signal.addEventListener("abort", stop, { once: true })
    }
    if (timeout) timer = setTimeout(() => reject(new DOMException("Error body timed out", "TimeoutError")), timeout)
  })
  try { return await Promise.race([reader.read(), interrupted]) }
  finally { if (timer) clearTimeout(timer); if (stop) signal.removeEventListener("abort", stop) }
}
async function peekFailure(response, signal) {
  if (!response.body) return { response, failure: response.ok ? null : { message: "" } }
  const contentType = (response.headers.get("content-type") ?? "").toLowerCase()
  const sse = contentType.includes("text/event-stream")
  if (response.ok && !sse && !contentType.includes("json")) return { response, failure: null }
  const length = Number(response.headers.get("content-length"))
  if (!sse && Number.isFinite(length) && length > MAX_ERROR_BYTES) return { response, failure: null }
  const reader = response.body.getReader(), chunks = []
  let size = 0, text = "", parsed = null
  const decoder = new TextDecoder()
  try {
    while (size <= MAX_ERROR_BYTES) {
      if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
      const { value, done } = await readBeforeBoundary(reader, signal, sse ? 60000 : 10000)
      if (done) break
      chunks.push(value); size += value.byteLength
      if (size > MAX_ERROR_BYTES) break
      text += decoder.decode(value, { stream: true })
      if (!sse) continue
      const frames = text.split(/\r?\n\r?\n/)
      if (frames.length < 2) continue
      for (const frame of frames.slice(0, -1)) {
        const data = frame.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n")
        if (!data || data === "[DONE]") continue
        let value
        try { value = JSON.parse(data) } catch { return { response: rebuild(response, reader, chunks), failure: null } }
        if (errorFrame(value)) return { response: rebuild(response, reader, chunks), failure: value }
        if (!harmlessPrelude(value)) return { response: rebuild(response, reader, chunks), failure: null }
      }
      text = frames.at(-1)
    }
    if (!sse && size <= MAX_ERROR_BYTES) {
      const body = text + decoder.decode()
      try { parsed = JSON.parse(body) } catch {}
      if (!response.ok && !object(parsed)) parsed = { message: body }
    }
    return { response: rebuild(response, reader, chunks), failure: parsed }
  } catch (error) {
    reader.cancel().catch(() => {})
    if (error && typeof error === "object") error.preacceptSafe = chunks.length === 0 && error.name !== "TimeoutError"
    throw error
  }
}
function rebuild(original, reader, chunks) {
  let index = 0
  const body = new ReadableStream({
    async pull(controller) {
      if (index < chunks.length) { controller.enqueue(chunks[index++]); return }
      try { const next = await reader.read(); if (next.done) controller.close(); else controller.enqueue(next.value) } catch (error) { controller.error(error) }
    },
    cancel(reason) { return reader.cancel(reason) },
  })
  return new Response(body, { status: original.status, statusText: original.statusText, headers: original.headers })
}
function expiredResponse(response, code) {
  const headers = new Headers(response.headers)
  headers.set("x-magpie-offpeak-expired", String(code))
  if (response.status >= 400) return new Response(response.body, { status: response.status, statusText: response.statusText, headers })
  response.body?.cancel().catch(() => {})
  headers.set("content-type", "application/json")
  headers.delete("content-length")
  headers.delete("content-encoding")
  return new Response(JSON.stringify({ type: "error", error: { type: "invalid_request_error", code, message: `off-peak-ticket-expired: ${code}` } }), { status: 400, headers })
}

export async function runNativeLane(ctx, { url, init = {}, model, sessionId, execution, send, fallback }) {
  if (!object(execution) || !["offpeak", "highspeed"].includes(execution.lane) || !nonempty(sessionId) || execution.taskId !== sessionId || !nonempty(ctx.scope) || execution.scope !== ctx.scope || !nonempty(ctx.generation) || execution.generation !== ctx.generation || !MODELS.has(model) || execution.rawModel !== model) throw invalid("native execution binding does not match account, session, or model")
  if (typeof send !== "function") throw invalid("native execution sender is unavailable")
  paidState(ctx)
  const highspeed = execution.lane === "highspeed"
  if (highspeed ? !nonempty(execution.cardId) : !nonempty(execution.ticketId)) throw invalid("native execution has no ticket or card")
  const signal = init.signal && ctx.signal && init.signal !== ctx.signal ? AbortSignal.any([init.signal, ctx.signal]) : init.signal ?? ctx.signal
  const ordinary = async () => {
    if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
    await ctx.assertCurrent()
    return fallback()
  }
  await ctx.assertCurrent()
  if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
  let body
  try {
    const raw = typeof init.body === "string" ? init.body :
      init.body instanceof ArrayBuffer || ArrayBuffer.isView(init.body) ? new TextDecoder().decode(init.body) : ""
    body = JSON.parse(raw)
  } catch {}
  if (!object(body) || body.model !== model) throw invalid("native request body does not match the admitted model")
  if (highspeed && (!whole(execution.expiresAt) || execution.expiresAt === 0)) throw invalid("highspeed card has no valid expiry")
  if (highspeed && execution.expiresAt <= Date.now()) {
    if (typeof fallback !== "function") throw failure("unavailable", "highspeed card expired")
    return ordinary()
  }
  const upstream = zcodeURL(ctx, `/api/v1/${highspeed ? "highspeed" : "off-peak"}/anthropic/v1/messages`)
  try { upstream.search = new URL(url).search } catch { throw invalid("invalid inference URL") }
  let rejectedFingerprint, recovered = false
  for (;;) {
    let credentials, fingerprint
    try {
      if (highspeed) credentials = await highspeedHeaders(ctx)
      else ({ headers: credentials, fingerprint } = await offpeakHeaders(ctx, rejectedFingerprint))
      rejectedFingerprint = undefined
    } catch (error) {
      if (!highspeed || aborted(signal, error) || errorContextExceeded(error) || identityChanged(error) || typeof fallback !== "function") throw error
      return ordinary()
    }
    const headers = new Headers(init.headers)
    for (const name of ["host", "authorization", "x-api-key", "x-coding-plan-api-key", "x-off-peak-ticket-id", "x-highspeed-card-id", "x-bigmodel-authorization", "bigmodel-target-type", "bigmodel-organization", "bigmodel-project"]) headers.delete(name)
    for (const [key, value] of Object.entries(credentials)) headers.set(key, value)
    headers.set("x-api-key", credentials.Authorization.slice(7))
    headers.set(highspeed ? "X-Highspeed-Card-ID" : "X-Off-Peak-Ticket-ID", highspeed ? execution.cardId : execution.ticketId)
    let res
    try {
      await ctx.assertCurrent()
      res = await send(upstream.href, { ...init, headers, ...(signal ? { signal } : {}) })
    } catch (error) {
      if (!highspeed || aborted(signal, error) || errorContextExceeded(error) || identityChanged(error) || typeof fallback !== "function") throw error
      return ordinary()
    }
    let observed
    try { observed = await peekFailure(res, signal) } catch (error) {
      if (!highspeed || !error?.preacceptSafe || aborted(signal, error) || errorContextExceeded(error) || identityChanged(error) || typeof fallback !== "function") throw error
      return ordinary()
    }
    const code = errorCode(observed.failure)
    if (!highspeed && !recovered && res.status === 401 && fingerprint && !acceptedOutput(observed.failure)) {
      recovered = true
      rejectedFingerprint = fingerprint
      observed.response.body?.cancel().catch(() => {})
      await ctx.assertCurrent()
      continue
    }
    if (!highspeed && errorFrame(observed.failure) && [3001, 3102, 3106].includes(code)) return expiredResponse(observed.response, code)
    if (!highspeed && !acceptedOutput(observed.failure) && ((errorFrame(observed.failure) && code === 3105) || (res.status === 429 && observed.failure))) {
      // Only a pre-acceptance rejection can be retried, and only with the same ticket.
      if (init.body != null && typeof init.body !== "string" && !(init.body instanceof Uint8Array) && !(init.body instanceof ArrayBuffer)) return observed.response
      observed.response.body?.cancel().catch(() => {})
      await delay(retryAfter(res.headers), signal)
      await ctx.assertCurrent()
      continue
    }
    if (highspeed && (!res.ok || errorFrame(observed.failure)) && !acceptedOutput(observed.failure) && !contextExceeded(observed.failure) && observed.failure && typeof fallback === "function") {
      observed.response.body?.cancel().catch(() => {})
      return ordinary()
    }
    return observed.response
  }
}
