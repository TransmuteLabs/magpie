// Magpie usage transport, adapted from zai-org/ZCode 3.18.0.
// Copyright 2026 Z.AI Co., Ltd. Licensed under Apache-2.0; see LICENSE.ZCode.
import { ZCODE_VERSION } from "./project-token.mjs"

export const nativeUsageOperations = ["usage.model", "usage.tool", "usage.activity", "usage.detail", "usage.health", "usage.mcp", "usage.balance", "entitlement"]

const APP_VERSION = ZCODE_VERSION
const MODEL_IDS = ["GLM-5.3", "GLM-5.3-Flash", "GLM-5V-Turbo", "GLM-5.2", "GLM-5.1", "GLM-5.1-Highspeed", "GLM-5", "GLM-5-Turbo", "GLM-4.7", "GLM-4.7-FlashX", "GLM-4.7-Flash", "GLM-4.6", "GLM-4.5-Air", "GLM-4.5", "GLM-4.6V", "GLM-4.6V-Flash", "GLM-4.6V-FlashX", "GLM-4.1V-Thinking-FlashX", "GLM-4.1V-Thinking-Flash", "GLM-4-FlashX-250414", "GLM-4-Flash-250414", "GLM-4V-Flash"]
const canonicalModels = new Map(MODEL_IDS.map((id) => [id.toLowerCase(), id]))
// These are the execution aliases published by the provider's model hook.
// Their ticket/card readiness, rather than ordinary usage windows, owns
// applicability. This does not infer how the Highspeed server charges.
const laneAliases = ["glm-5.3-offpeak", "glm-5.3-highspeed", "glm-5.3-flash-offpeak", "glm-5.3-flash-highspeed"]
const laneAliasSet = new Set(laneAliases)
const object = (value) => value !== null && typeof value === "object" && !Array.isArray(value)
const finite = (value) => typeof value === "number" && Number.isFinite(value)
const numeric = (value) => {
  if (finite(value)) return value
  if (typeof value === "string" && value.trim() && Number.isFinite(Number(value))) return Number(value)
  return null
}
const text = (value) => typeof value === "string" ? value.trim() : ""
const iso = (ms) => {
  if (!finite(ms) || ms <= 0) return undefined
  const date = new Date(ms)
  return Number.isFinite(date.getTime()) ? date.toISOString() : undefined
}
const secondsISO = (value) => {
  const seconds = numeric(value)
  return seconds === null ? undefined : iso(seconds * 1000)
}
const compact = (value) => Number.isInteger(value) ? String(value) : value.toFixed(2).replace(/0+$/, "").replace(/\.$/, "")

// Used by both the native card and the retained Start routing code.
export function startBucketModels(bucket) {
  const capabilities = Array.isArray(bucket?.capabilities) ? bucket.capabilities : []
  const fromCapabilities = capabilities.flatMap((value) => {
    const capability = text(value)
    return capability.toLowerCase().startsWith("model:") ? [capability.slice(6).trim()] : []
  }).filter(Boolean)
  const candidates = fromCapabilities.length ? fromCapabilities : [text(bucket?.show_name)]
  const seen = new Set()
  return candidates.flatMap((candidate) => {
    const id = canonicalModels.get(candidate.toLowerCase()) ?? candidate
    const key = id.toLowerCase()
    if (!id || seen.has(key)) return []
    seen.add(key)
    return [id]
  })
}

export function startBalanceNow(data, httpDate, localNowMs = Date.now()) {
  const date = typeof httpDate === "string" ? Date.parse(httpDate) : NaN
  if (Number.isFinite(date)) return date
  return finite(data?.server_time) && data.server_time >= 0 ? data.server_time * 1000 : localNowMs
}

function usageError(kind, response) {
  const error = new Error(`zcode_usage_${kind}`)
  error.kind = kind
  error.retryable = kind === "timeout" || kind === "transport" || (response?.status >= 500)
  error.throttled = response?.status === 429
  error.dispatched = !!response
  if (Number.isInteger(response?.status)) error.httpStatus = response.status
  const code = response?.data?.code
  if (Number.isSafeInteger(code)) {
    error.code = code
    if (code === 2007) error.retryable = true
    if (code === 3002) error.throttled = true
  }
  const requestId = response?.headers?.get?.("x-request-id")?.trim()
  if (requestId && /^[A-Za-z0-9._:-]{1,128}$/.test(requestId)) error.requestId = requestId
  return error
}

// Bound credential acquisition as well as reading a complete response body.
// ctx.request combines this signal with the caller's captured signal.
async function within(ctx, timeout, run) {
  const controller = new AbortController()
  const deadline = Date.now() + timeout
  let timer
  let abortCaller
  const stopped = new Promise((_, reject) => {
    timer = setTimeout(() => { controller.abort(); reject(usageError("timeout")) }, timeout)
    abortCaller = () => { controller.abort(); reject(usageError("cancelled")) }
    if (ctx.signal?.aborted) abortCaller()
    else ctx.signal?.addEventListener("abort", abortCaller, { once: true })
  })
  try {
    return await Promise.race([run(controller.signal, () => Math.max(1, deadline - Date.now())), stopped])
  } finally {
    clearTimeout(timer)
    ctx.signal?.removeEventListener("abort", abortCaller)
  }
}

function checkSignal(signal) {
  if (signal.aborted) throw usageError("cancelled")
}

function successful(response, requireData = true) {
  if (!response.ok) throw usageError("http", response)
  const env = response.data
  if (!object(env) || (env.success !== undefined && typeof env.success !== "boolean")) throw usageError("invalid_response", response)
  if (env.success === false || (env.code !== undefined && env.code !== null && env.code !== 0 && env.code !== 200))
    throw usageError("business", response)
  if (env.data === undefined || env.data === null) {
    if (!requireData && (env.code === 0 || env.code === 200 || env.success === true)) return null
    throw usageError("invalid_response", response)
  }
  return env.data
}

const team = (ctx) => ctx.state.kind === "team"
// Older saved Team sign-ins contain a project key and org/project metadata,
// but lack the product identity needed for a native Team selection.
const legacyTeam = (ctx) => ctx.state.kind === "legacy-key" && !!text(ctx.state.org) && !!text(ctx.state.project)
function requireCompleteLegacyScope(ctx) {
  if (ctx.state.kind === "legacy-key" && !!text(ctx.state.org) !== !!text(ctx.state.project))
    throw usageError("unavailable")
}
function teamHeaders(ctx) {
  if (!team(ctx)) return {}
  if (!text(ctx.state.org) || !text(ctx.state.project)) throw usageError("unavailable")
  return { "bigmodel-organization": ctx.state.org, "bigmodel-project": ctx.state.project }
}
function cardTeamHeaders(ctx) {
  if (!legacyTeam(ctx)) return teamHeaders(ctx)
  return {
    "bigmodel-organization": ctx.state.org,
    "bigmodel-project": ctx.state.project,
    "set-language": String(ctx.state.base ?? "").includes("bigmodel.cn") ? "zh" : "en",
    "accept-language": "en-US,en",
  }
}
function businessOrigin(ctx) {
  // A pasted API key keeps the plugin's existing personal BigModel endpoint.
  if (ctx.state.kind === "legacy-key" && ctx.state.base) return new URL(ctx.state.base).origin
  return ctx.state.origins.business
}
const endpoint = (ctx, path) => new URL(path, businessOrigin(ctx))
function cardEndpoint(ctx, path) {
  if (!legacyTeam(ctx)) return endpoint(ctx, path)
  const base = new URL(ctx.state.base)
  const origin = base.hostname.toLowerCase().endsWith("bigmodel.cn") ? ctx.state.origins.business : base.origin
  return new URL(path, origin)
}

async function quotaToken(ctx, pending, signal) {
  const result = await pending
  checkSignal(signal)
  if (!text(result?.token)) throw usageError("unavailable")
  return result.token
}

async function request(ctx, url, headers, signal, remaining) {
  checkSignal(signal)
  return ctx.request(url.toString(), { method: "GET", headers, signal }, { timeout: remaining() })
}

function windowSpan(unit, number) {
  if (unit === 6) return 7 * 86400
  const count = Math.max(1, Math.trunc(number || (unit === 3 ? 5 : 1)))
  return ({ 1: 60, 3: 3600, 4: 86400, 5: 30 * 86400 }[unit] ?? 0) * count
}
function windowName(span) {
  if (!span) return "Credits"
  if (span < 86400) return `${Math.trunc(span / 3600)} hours`
  if (span === 7 * 86400) return "Weekly"
  if (span >= 28 * 86400) return "Monthly"
  return `${Math.trunc(span / 86400)} days`
}
function quotaWindows(data) {
  if (!object(data) || (data.limits !== undefined && (!Array.isArray(data.limits) || !data.limits.every(object)))) throw usageError("invalid_response")
  return (data.limits ?? []).map((limit) => {
    const span = windowSpan(limit.unit, limit.number)
    const window = { name: windowName(span) }
    const type = String(limit.type ?? "").toUpperCase()
    if (["", "TOKENS_LIMIT", "CREDIT_LIMIT"].includes(type) && (span === 5 * 3600 || span === 7 * 86400))
      window.notModels = laneAliases
    if (type === "TIME_LIMIT") Object.assign(window, { name: "MCP · Month", aside: true })
    if (finite(limit.percentage)) window.used = limit.percentage
    if (finite(limit.usage) && limit.usage === 0) {
      // Zero quota is an uncapped informational window, not an exhausted plan.
      Object.assign(window, { used: 0, aside: true })
    } else if (finite(limit.usage) && limit.usage > 0) {
      const used = finite(limit.remaining) ? limit.usage - limit.remaining : finite(limit.currentValue) ? limit.currentValue : null
      if (used !== null) {
        if (finite(limit.remaining) || !finite(limit.percentage)) window.used = 100 * used / limit.usage
        Object.assign(window, { amount: used, limit: limit.usage, display: `${compact(used)} / ${compact(limit.usage)}` })
      }
    }
    const reset = iso(limit.nextResetTime)
    if (reset) window.resetsAt = reset
    if (span) window.span = span
    return window
  })
}

async function fetchQuota(ctx, pending, signal, remaining) {
  const token = await quotaToken(ctx, pending, signal)
  const url = cardEndpoint(ctx, "/api/monitor/usage/quota/limit")
  if (team(ctx) || legacyTeam(ctx)) url.searchParams.set("type", "2")
  const response = await request(ctx, url, { authorization: token, ...cardTeamHeaders(ctx) }, signal, remaining)
  const data = successful(response)
  const windows = quotaWindows(data)
  const level = text(data.level)
  return { windows, ...(level ? { plan: `GLM Coding ${level[0].toUpperCase()}${level.slice(1)}` } : {}) }
}

function businessTime(value) {
  const n = numeric(value)
  if (n !== null && n > 0) return iso(n > 1e12 ? n : n * 1000)
  if (typeof value !== "string") return undefined
  const date = value.trim().replace(" ", "T")
  const ms = Date.parse(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d$/.test(date) ? `${date}+08:00` : date)
  return iso(ms)
}

// The older customer-package list is only a read-only compatibility view
// for explicit key-only credentials; it is not the ZCode reset service.
async function fetchLegacyResetList(ctx, pending, signal, remaining) {
  const isTeam = legacyTeam(ctx)
  let authorization
  if (isTeam) {
    const material = await ctx.loginMaterial({ business: "optional", zcode: "none" })
    checkSignal(signal)
    if (!text(material?.business)) return null
    authorization = ctx.state.site === "zai" ? `Bearer ${material.business}` : material.business
  } else authorization = await quotaToken(ctx, pending, signal)
  const url = cardEndpoint(ctx, "/api/biz/customer-package-reset/list")
  url.searchParams.set("targetType", isTeam ? "TEAM" : "PERSONAL")
  const response = await request(ctx, url, { authorization, ...cardTeamHeaders(ctx) }, signal, remaining)
  const data = successful(response)
  if (!object(data) || !Array.isArray(data.fiveHourResets) || !Array.isArray(data.weekResets) ||
      ![...data.fiveHourResets, ...data.weekResets].every((entry) => object(entry) && typeof entry.available === "boolean")) throw usageError("invalid_response", response)
  const each = []
  const collect = (items, window) => {
    for (const item of items) {
      if (!item.available) continue
      const until = businessTime(item.expireTime)
      each.push(until ? { window, until } : { window })
    }
    return items.filter((item) => item.available).length
  }
  const fiveHour = collect(data.fiveHourResets, "fiveHour"), weekly = collect(data.weekResets, "weekly")
  each.sort((a, b) => (a.until ? Date.parse(a.until) : Infinity) - (b.until ? Date.parse(b.until) : Infinity))
  return { source: "legacy-list", count: fiveHour + weekly, byWindow: true, fiveHour, weekly, each,
    ...(isTeam ? { team: true } : {}), ...(each[0]?.until ? { until: each[0].until } : {}) }
}

const pickFields = (value, keys) => Object.fromEntries(keys.filter((key) => value[key] !== undefined).map((key) => [key, value[key]]))
function personalEntitlement(data) {
  if (!Array.isArray(data)) return { kind: "unknown" }
  let malformed = false
  for (const item of data) {
    if (!object(item)) continue
    const relevant = [item.productId, item.productName].some((value) => typeof value === "string" && value.toLowerCase().includes("coding"))
    if (!relevant) continue
    const valid = typeof item.status === "string" && typeof item.inCurrentPeriod === "boolean" &&
      (item.productId === undefined || typeof item.productId === "string") &&
      (item.productName === undefined || typeof item.productName === "string") &&
      (item.autoRenew === undefined || typeof item.autoRenew === "number" || typeof item.autoRenew === "boolean") &&
      ["nextRenewTime", "valid", "billingCycle"].every((key) => item[key] === undefined || typeof item[key] === "string")
    if (!valid) { malformed = true; continue }
    if (item.status === "VALID" && item.inCurrentPeriod) return { kind: "available",
      subscription: pickFields(item, ["productId", "productName", "status", "inCurrentPeriod", "autoRenew", "nextRenewTime", "valid", "billingCycle"]) }
  }
  return { kind: malformed ? "unknown" : "unavailable" }
}
function teamEntitlement(data) {
  if (!object(data) || typeof data.hasSubscription !== "boolean" ||
      ["status", "memberGrantStatus", "productId", "productName", "subscribeEndTime", "subscribePeriod"].some((key) => data[key] != null && typeof data[key] !== "string")) return { kind: "unknown" }
  if (!data.hasSubscription) return { kind: "unavailable" }
  if (data.status === "EXPIRED") return { kind: "unavailable", reason: "expired" }
  if (data.status === "EFFECTIVE" && data.memberGrantStatus === "UNASSIGNED") return { kind: "unavailable", reason: "unassigned" }
  if (data.status !== "EFFECTIVE" || data.memberGrantStatus !== "VALID") return { kind: "unknown" }
  return { kind: "available", subscription: pickFields(data, ["hasSubscription", "status", "memberGrantStatus", "productId", "productName", "subscribeEndTime", "subscribePeriod"]) }
}
async function fetchEntitlement(ctx, pending, signal, remaining) {
  if (ctx.state.kind === "start") return { kind: "unknown" }
  const isLegacyTeam = legacyTeam(ctx)
  const isTeam = team(ctx) || isLegacyTeam
  let authorization
  if (isTeam) {
    const material = await ctx.loginMaterial({ business: "optional", zcode: "none" })
    checkSignal(signal)
    if (!text(material?.business)) return { kind: "unknown" }
    authorization = ctx.state.site === "zai" ? `Bearer ${material.business}` : material.business
  } else authorization = await quotaToken(ctx, pending, signal)
  const path = isTeam ? "/api/biz/team/subscribe/product/querySubscribeDetail" : "/api/biz/subscription/list"
  const url = isLegacyTeam ? cardEndpoint(ctx, path) : endpoint(ctx, path)
  const response = await request(ctx, url, { Authorization: authorization, ...(isLegacyTeam ? cardTeamHeaders(ctx) : teamHeaders(ctx)) }, signal, remaining)
  let data
  try { data = successful(response) } catch { return { kind: "unknown" } }
  return isTeam ? teamEntitlement(data) : personalEntitlement(data)
}

function samePlan(bucket, plan) {
  if (text(bucket.user_plan_id) && text(plan.user_plan_id)) return bucket.user_plan_id === plan.user_plan_id
  return !!text(bucket.plan_id) && bucket.plan_id === plan.plan_id
}
function livePlan(plan, now) {
  if (!object(plan) || text(plan.status).toLowerCase() !== "active") return false
  const end = numeric(plan.ends_at), start = numeric(plan.starts_at)
  return !(end !== null && end > 0 && end * 1000 <= now) && !(start !== null && start > 0 && start * 1000 > now)
}
function periodSpan(bucket, plan) {
  const a = numeric(bucket.period_start), b = numeric(bucket.period_end)
  if (a !== null && b !== null && b > a) return Math.trunc(b - a)
  const entitlement = Array.isArray(plan.entitlements) ? plan.entitlements.find((entry) => entry?.entitlement_id === bucket.entitlement_id) : null
  return ({ hourly: 3600, daily: 86400, weekly: 7 * 86400, monthly: 30 * 86400 })[text(entitlement?.period).toLowerCase()] ?? 0
}

function startProjection(data, date) {
  if (!object(data) || (data.plans !== undefined && !Array.isArray(data.plans)) || (data.balances !== undefined && !Array.isArray(data.balances))) throw usageError("invalid_response")
  const now = startBalanceNow(data, date)
  const plans = (data.plans ?? []).filter((plan) => livePlan(plan, now))
  const pending = (data.plans ?? []).filter((plan) => {
    if (!object(plan) || text(plan.status).toLowerCase() === "expired") return false
    const start = numeric(plan.starts_at), end = numeric(plan.ends_at)
    return start !== null && start * 1000 > now && !(end !== null && end > 0 && end * 1000 <= now)
  }).sort((a, b) => numeric(a.starts_at) - numeric(b.starts_at))
  const observations = [], buckets = []
  for (const bucket of data.balances ?? []) {
    if (!object(bucket)) continue
    const plan = plans.find((item) => samePlan(bucket, item))
    if (!plan) continue
    const expiry = numeric(bucket.expires_at)
    if (expiry !== null && expiry > 0 && expiry * 1000 <= now) continue
    const total = numeric(bucket.total_units), actualUsed = numeric(bucket.used_units), left = numeric(bucket.remaining_units)
    if (total === null && actualUsed === null && left === null) continue
    const used = actualUsed ?? (total !== null && left !== null ? total - left : null)
    const models = startBucketModels(bucket)
    const name = text(bucket.show_name) || models.join(", ") || "Credits"
    const identity = {}
    for (const [source, target] of [["user_plan_id", "userPlanId"], ["plan_id", "planId"], ["bucket_id", "bucketId"], ["entitlement_id", "entitlementId"]]) {
      const value = text(bucket[source])
      if (value) identity[target] = value
    }
    for (const [source, target] of [["period_start", "periodStart"], ["period_end", "periodEnd"], ["expires_at", "expiresAt"]]) {
      const value = secondsISO(bucket[source])
      if (value) identity[target] = value
    }
    const observation = { kind: "start-balance", name, models, used, limit: total, remaining: left, unit: text(bucket.unit_type), at: iso(now), ...(Object.keys(identity).length ? { identity } : {}) }
    if (identity.expiresAt) observation.nextRefreshAt = identity.expiresAt
    observations.push(observation)
    const window = { name, models, _plan: text(plan.name) || "Start Plan", _spent: left !== null ? left <= 0 : total !== null && used !== null && total > 0 && used >= total }
    // For display, remaining is authoritative, even when reservations make
    // available_units differ or used_units is inconsistent.
    const amount = total !== null && left !== null ? total - left : used
    if (total !== null && total > 0 && amount !== null) Object.assign(window, { used: 100 * amount / total, amount, limit: total, display: `${compact(amount)} / ${compact(total)}` })
    if (identity.expiresAt) window.resetsAt = identity.expiresAt
    const span = periodSpan(bucket, plan)
    if (span) window.span = span
    buckets.push(window)
  }
  const earliest = plans.reduce((best, plan) => {
    const end = numeric(plan.ends_at)
    return !best || (end !== null && end > 0 && (!(numeric(best.ends_at) > 0) || end < numeric(best.ends_at))) ? plan : best
  }, null)
  const chosen = earliest ?? pending[0]
  return { observations, buckets, pending, plan: chosen ? text(chosen.name) || "Start Plan" : "", until: earliest ? secondsISO(earliest.ends_at) : undefined }
}

async function fetchStart(ctx, signal, remaining) {
  const material = await ctx.loginMaterial({ business: "none", zcode: "optional" })
  checkSignal(signal)
  if (!text(material?.zcode)) return null
  const url = new URL("/api/v1/zcode-plan/billing/balance", ctx.state.origins.zcode)
  url.searchParams.set("app_version", APP_VERSION)
  const headers = { Authorization: `Bearer ${material.zcode}` }
  if (text(ctx.state.device)) headers["X-Device-Mid"] = ctx.state.device
  const response = await request(ctx, url, headers, signal, remaining)
  return startProjection(successful(response), response.headers?.get?.("date"))
}

async function fetchMcp(ctx, signal, remaining) {
  if (ctx.state.kind !== "individual" && ctx.state.kind !== "team") return null
  const material = await ctx.loginMaterial({ business: "optional", zcode: "optional" })
  checkSignal(signal)
  if (!text(material?.business) || !text(material?.zcode)) return null
  const headers = { Authorization: `Bearer ${material.zcode}`, "X-Bigmodel-Authorization": `Bearer ${material.business}` }
  if (!team(ctx)) headers["Bigmodel-Target-Type"] = "PERSONAL"
  else if (ctx.state.site === "bigmodel") Object.assign(headers, { "Bigmodel-Target-Type": "TEAM", ...teamHeaders(ctx) })
  const response = await request(ctx, new URL("/api/v1/mcp/usage", ctx.state.origins.zcode), headers, signal, remaining)
  if (!response.ok) throw usageError("http", response)
  if (!object(response.data) || !Number.isSafeInteger(response.data.code)) throw usageError("invalid_response", response)
  if (response.data.code !== 0) throw usageError("business", response)
  if (!object(response.data.data)) throw usageError("invalid_response", response)
  const data = response.data.data, total = data.total_usage
  if (!finite(data.server_time) || !object(total) || !finite(total.used) || !finite(total.limit) || !finite(total.remaining) || total.limit <= 0 ||
      (data.next_refresh_at !== undefined && !finite(data.next_refresh_at)) || (data.level !== undefined && typeof data.level !== "string")) return null
  return { kind: "server-mcp", name: "Server MCP", models: [], used: Math.max(0, total.used), limit: total.limit,
    remaining: Math.min(total.limit, Math.max(0, total.remaining)), unit: "", at: iso(data.server_time * 1000),
    ...(data.next_refresh_at === undefined ? {} : { nextRefreshAt: iso(data.next_refresh_at * 1000) }) }
}

function startWindows(start, startOnly) {
  if (!start) return []
  const best = new Map()
  start.buckets.forEach((bucket, index) => {
    for (const model of bucket.models) {
      const key = model.toLowerCase(), previous = start.buckets[best.get(key)]
      if (!previous || (previous._spent && !bucket._spent) || (previous._spent === bucket._spent && (bucket.used ?? Infinity) < (previous.used ?? Infinity))) best.set(key, index)
    }
  })
  const several = new Set(start.buckets.map((bucket) => bucket._plan)).size > 1
  const windows = start.buckets.map(({ _plan, _spent, ...window }, index) => {
    const models = window.models.filter((model) => !laneAliasSet.has(model.toLowerCase()) && best.get(model.toLowerCase()) === index)
    const aliases = models.map((model) => `${model}-Trial`)
    if (!startOnly) {
      for (let i = 0; i < models.length; i++) {
        const at = window.name.toLowerCase().indexOf(models[i].toLowerCase())
        if (at >= 0) window.name = window.name.slice(0, at) + aliases[i] + window.name.slice(at + models[i].length)
      }
    }
    if (!startOnly || several) window.name = `${window.name} · ${_plan}`
    if (models.length) window.models = startOnly ? [...models, ...aliases] : aliases
    else { delete window.models; window.aside = true }
    return window
  })
  return [...windows.filter((window) => !window.aside), ...windows.filter((window) => window.aside)]
}

function pendingWindows(start) {
  return (start?.pending ?? []).map((plan) => {
    const date = new Date(numeric(plan.starts_at) * 1000)
    const two = (n) => String(n).padStart(2, "0")
    const local = `${date.getFullYear()}-${two(date.getMonth() + 1)}-${two(date.getDate())} ${two(date.getHours())}:${two(date.getMinutes())}`
    return { name: "ZCode gift plan not started yet", used: 0, aside: true, display: `${text(plan.name) || "Start Plan"} · ${local}` }
  })
}

function entitlementTerm(entitlement) {
  if (entitlement?.kind !== "available") return {}
  const sub = entitlement.subscription
  const auto = sub.autoRenew === true || sub.autoRenew === 1
  let raw = sub.subscribeEndTime || sub.nextRenewTime
  if (!raw && !auto && typeof sub.valid === "string") {
    const dates = sub.valid.match(/\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}:\d{2})?/g) ?? []
    raw = dates.at(-1)
  }
  if (typeof raw !== "string") return {}
  const value = raw.trim().replace(" ", "T")
  const ms = Date.parse(/^\d{4}-\d\d-\d\d$/.test(value) ? `${value}T00:00:00+08:00` :
    /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d$/.test(value) ? `${value}+08:00` : value)
  const until = iso(ms)
  return until ? { until, renew: auto ? "auto" : "off" } : {}
}

export async function readNativeUsage(ctx) {
  if (ctx.signal?.aborted) throw usageError("cancelled")
  requireCompleteLegacyScope(ctx)
  const startOnly = ctx.state.kind === "start"
  let project = null
  const optional = (run) => within(ctx, 2000, run).catch(() => null)
  const primary = startOnly ? Promise.resolve(null) : within(ctx, 15_000, (signal, remaining) => {
    project = Promise.resolve().then(() => ctx.projectToken({ signal }))
    return fetchQuota(ctx, project, signal, remaining)
  }).catch((error) => ({ error }))
  const [quota, entitlement, start, mcp, legacyResets] = await Promise.all([
    primary,
    optional((signal, remaining) => fetchEntitlement(ctx, project, signal, remaining)),
    optional((signal, remaining) => fetchStart(ctx, signal, remaining)),
    optional((signal, remaining) => fetchMcp(ctx, signal, remaining)),
    ctx.state.kind === "legacy-key" ? optional((signal, remaining) => fetchLegacyResetList(ctx, project, signal, remaining)) : null,
  ])
  if (ctx.signal?.aborted) throw usageError("cancelled")
  await within(ctx, 1000, () => ctx.assertCurrent())
  const observations = [...(start?.observations ?? []), ...(mcp ? [mcp] : [])]
  if (startOnly) {
    if (!start) return { signIn: "kept", error: "Start Plan balance unavailable", observations }
    if (!start.plan) return { signIn: "kept", error: "No active Start Plan", observations }
    return { signIn: "kept", plan: start.plan, ...(start.until ? { until: start.until, renew: "off" } : {}), windows: [...startWindows(start, true), ...pendingWindows(start)], observations }
  }
  const paid = quota?.error ? { error: "Coding Plan quota unavailable", windows: [] } : quota
  if (legacyResets) paid.resets = legacyResets
  if (entitlement?.kind === "available") {
    const name = text(entitlement.subscription.productName)
    if (name && !paid.plan) paid.plan = name
    Object.assign(paid, entitlementTerm(entitlement))
  }
  if (start?.buckets.length) {
    const windows = startWindows(start, false)
    const trials = [...new Set(windows.flatMap((window) => window.models ?? []).map((model) => model.toLowerCase()))]
    if (trials.length) paid.windows = paid.windows.map((window) => window.aside ? window : {
      ...window, notModels: [...new Set([...(window.notModels ?? []), ...trials])],
    })
    paid.windows.push(...windows)
  }
  if (start?.pending.length) paid.windows.push(...pendingWindows(start))
  return { signIn: "kept", ...paid, observations }
}

// Projection keeps optional fields absent instead of turning them into zeroes.
const number = (value) => finite(value)
const string = (value) => typeof value === "string"
const nullableMetric = (value) => value === null || string(value) || number(value)
const project = (check, value) => check.project ? check.project(value) : value
const array = (check) => {
  const valid = (value) => Array.isArray(value) && value.every(check)
  valid.project = (value) => value.map((item) => project(check, item))
  return valid
}
const shape = (schema) => {
  const valid = (value) => object(value) && Object.entries(schema).every(([key, check]) => value[key] === undefined || check(value[key]))
  valid.project = (value) => Object.fromEntries(Object.entries(schema).filter(([key]) => value[key] !== undefined).map(([key, check]) => [key, project(check, value[key])]))
  return valid
}
const nullable = (check) => {
  const valid = (value) => value === null || check(value)
  valid.project = (value) => value === null ? null : project(check, value)
  return valid
}
const stringArray = array(string), numberArray = array(number), metricArray = array(nullableMetric)
const modelSummary = shape({ modelName: string, totalTokens: number, sortOrder: number })
const toolSummary = shape({ toolCode: string, toolName: string, totalUsageCount: number, sortOrder: number })
const modelData = shape({ modelName: string, totalTokens: number, tokensUsage: numberArray, sortOrder: number })
const toolData = shape({ toolCode: string, toolName: string, sortOrder: number, usageCount: numberArray, totalUsageCount: number })
const detailModel = shape({ modelCode: string, modelName: string, sortOrder: number, totalTokens: number, totalCredits: number,
  totalCreditsUsage: metricArray, cachedInputCreditsUsage: metricArray, uncachedInputCreditsUsage: metricArray, outputCreditsUsage: metricArray,
  tokensUsage: metricArray, totalTokensUsage: metricArray, cachedInputTokensUsage: metricArray, uncachedInputTokensUsage: metricArray, outputTokensUsage: metricArray })
const detailMcp = shape({ mcpCode: string, mcpName: string, toolCode: string, toolName: string, sortOrder: number, totalCredits: number,
  totalUsageCount: number, creditsUsage: metricArray, mcpCallCount: metricArray, usageCount: metricArray })
const metric = shape({ value: nullableMetric, trend: nullableMetric })
const reportSchemas = {
  "usage.model": shape({ x_time: stringArray, granularity: string, modelCallCount: numberArray, tokensUsage: numberArray,
    totalUsage: shape({ totalModelCallCount: number, totalTokensUsage: number, modelSummaryList: array(modelSummary) }), modelDataList: array(modelData), modelSummaryList: array(modelSummary) }),
  "usage.tool": shape({ x_time: stringArray, granularity: string, networkSearchCount: numberArray, webReadMcpCount: numberArray, zreadMcpCount: numberArray,
    totalUsage: shape({ totalNetworkSearchCount: number, totalWebReadMcpCount: number, totalZreadMcpCount: number, totalSearchMcpCount: number, toolSummaryList: array(toolSummary) }), toolDataList: array(toolData), toolSummaryList: array(toolSummary) }),
  "usage.activity": shape({ summary: nullable(shape({ totalTokens: number, peakDailyTokens: number, peakDailyTokensDate: string, totalUsageDurationMs: number, currentStreakDays: number, longestStreakDays: number })),
    series: array(shape({ date: string, totalTokens: number, modelCallCount: number, mcpCalls: number })) }),
  "usage.detail": shape({ summary: nullable(shape({ cacheHitRate: metric, totalCredits: metric, averageDailyCredits: metric })),
    modelUsage: nullable(shape({ xTime: stringArray, periodTypes: stringArray, modelDataList: array(detailModel) })),
    mcpUsage: nullable(shape({ xTime: stringArray, periodTypes: stringArray, mcpDataList: array(detailMcp) })) }),
  "usage.health": shape({ x_time: stringArray, xTime: stringArray, proMaxDecodeSpeed: numberArray, liteDecodeSpeed: numberArray }),
}

async function fetchReport(ctx, operation, params, signal, remaining) {
  if (!object(params) || !text(params.startTime) || !text(params.endTime)) throw usageError("invalid_request")
  if (operation === "usage.detail" && params.usageType !== "MODEL" && params.usageType !== "MCP") throw usageError("invalid_request")
  const paths = { "usage.model": "/api/monitor/usage/model-usage", "usage.tool": "/api/monitor/usage/tool-usage",
    "usage.activity": "/api/monitor/credit-usage/activity", "usage.detail": "/api/monitor/credit-usage/usage-detail", "usage.health": "/api/monitor/usage/model-performance-day" }
  const token = await quotaToken(ctx, Promise.resolve().then(() => ctx.projectToken({ signal })), signal)
  const url = cardEndpoint(ctx, paths[operation])
  const isTeam = team(ctx) || legacyTeam(ctx)
  if (operation === "usage.model" || operation === "usage.tool") { if (isTeam) url.searchParams.set("type", "2") }
  if (operation === "usage.activity" || operation === "usage.detail") url.searchParams.set("type", isTeam ? "3" : "1")
  url.searchParams.set("startTime", params.startTime)
  url.searchParams.set("endTime", params.endTime)
  if (operation === "usage.detail") url.searchParams.set("usageType", params.usageType)
  const response = await request(ctx, url, { authorization: token, ...cardTeamHeaders(ctx) }, signal, remaining)
  const data = successful(response, false)
  if (data !== null && !reportSchemas[operation](data)) throw usageError("invalid_response", response)
  return data === null ? null : project(reportSchemas[operation], data)
}

export async function nativeUsageOperation(ctx, operation, params = {}) {
  if (!nativeUsageOperations.includes(operation)) throw usageError("invalid_request")
  if (ctx.signal?.aborted) throw usageError("cancelled")
  if (operation !== "usage.mcp" && operation !== "usage.balance") requireCompleteLegacyScope(ctx)
  return within(ctx, 15_000, async (signal, remaining) => {
    let result
    if (operation === "usage.mcp") result = await fetchMcp(ctx, signal, remaining)
    if (operation === "usage.balance") {
      try {
        const start = await fetchStart(ctx, signal, remaining)
        result = start ? { kind: "available", observations: start.observations } : { kind: "unknown" }
      } catch (error) {
        if (signal.aborted) throw error
        result = { kind: "unknown" }
      }
    }
    if (operation === "entitlement") {
      const pending = team(ctx) || legacyTeam(ctx) || ctx.state.kind === "start" ? null : Promise.resolve().then(() => ctx.projectToken({ signal }))
      try { result = await fetchEntitlement(ctx, pending, signal, remaining) } catch (error) {
        if (signal.aborted) throw error
        result = { kind: "unknown" }
      }
    }
    if (operation !== "usage.mcp" && operation !== "usage.balance" && operation !== "entitlement") {
      if (ctx.state.kind === "start") throw usageError("unavailable")
      result = await fetchReport(ctx, operation, params, signal, remaining)
    }
    checkSignal(signal)
    await ctx.assertCurrent()
    return result
  })
}
