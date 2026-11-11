// Magpie reset transport, adapted from zai-org/ZCode 3.18.0.
// Copyright 2026 Z.AI Co., Ltd. Licensed under Apache-2.0; see LICENSE.ZCode.
// The reset service is transport only. The host owns idempotency, scheduling,
// and confirmation against a later status read.
export const resetOperations = ["reset.status", "reset.opportunity", "reset.use", "reset.history.read"]

const object = (value) => value !== null && typeof value === "object" && !Array.isArray(value)
const positive = (value) => typeof value === "number" && Number.isFinite(value) && value > 0
const requestIdOf = (headers) => {
  const id = headers?.get?.("x-request-id")?.trim()
  return id && /^[A-Za-z0-9._:-]{1,128}$/.test(id) ? id : undefined
}

function failure(kind, { code, httpStatus, retryable = false, throttled = false, dispatched = false, requestId } = {}) {
  const error = new Error(`coding_plan_reset_${kind}`)
  Object.assign(error, { kind, retryable, throttled, dispatched })
  if (Number.isSafeInteger(code)) error.code = code
  if (Number.isInteger(httpStatus)) error.httpStatus = httpStatus
  if (requestId) error.requestId = requestId
  return error
}

function statusData(data) {
  if (!object(data)) return false
  const opportunities = (items) => Array.isArray(items) && items.every((item) => object(item) && positive(item.expire_at))
  const history = (item) => item === null || (object(item) && positive(item.used_at))
  return opportunities(data.available_five_hour_resets) && opportunities(data.available_week_resets) &&
    history(data.latest_five_hour_reset_history) && history(data.latest_week_reset_history) &&
    typeof data.has_unread_history === "boolean"
}

export async function resetOperation(ctx, operation, params = {}) {
  if (!resetOperations.includes(operation) || !object(params)) throw failure("invalid_request")
  const isOpportunity = operation === "reset.opportunity"
  const isUse = operation === "reset.use"
  let body
  if (isOpportunity || isUse) {
    const key = typeof params.idempotency_key === "string" ? params.idempotency_key.trim() : ""
    if (!key || key.length > 64) throw failure("invalid_idempotency_key")
    if (isUse && params.reset_type !== "FIVE_HOUR" && params.reset_type !== "WEEK") throw failure("invalid_reset_type")
    body = JSON.stringify(isUse ? { idempotency_key: key, reset_type: params.reset_type } : { idempotency_key: key })
  }
  const state = ctx.state
  if (state.kind !== "individual" && state.kind !== "team") throw failure("unavailable")
  if (typeof state.zcodeUserId !== "string" || !state.zcodeUserId.trim()) throw failure("unavailable")
  if (state.kind === "team" && (!state.org || !state.project)) throw failure("unavailable")
  let material
  try {
    material = await ctx.loginMaterial({ business: "required", zcode: "required" })
  } catch (error) {
    if (error?.kind === "cancelled" || ctx.signal?.aborted) throw failure("cancelled")
    throw failure("unavailable")
  }
  if (!material?.business || !material?.zcode) throw failure("unavailable")
  const headers = { Authorization: `Bearer ${material.zcode}`, "X-Bigmodel-Authorization": material.business }
  if (operation !== "reset.history.read") {
    headers["Bigmodel-Target-Type"] = state.kind === "team" ? "TEAM" : "PERSONAL"
    if (state.kind === "team") {
      headers["Bigmodel-Organization"] = state.org
      headers["Bigmodel-Project"] = state.project
    }
  }
  if (body !== undefined) headers["Content-Type"] = "application/json"
  const path = operation === "reset.history.read" ? "history/read" : operation.slice("reset.".length)
  const url = new URL(`/api/v1/coding-plan/reset/${path}`, state.origins.zcode).toString()
  let response
  try {
    response = await ctx.request(url, { method: operation === "reset.status" ? "GET" : "POST", headers, ...(body === undefined ? {} : { body }) }, { timeout: 15_000 })
  } catch (error) {
    const dispatched = typeof error?.dispatched === "boolean" ? error.dispatched : true
    if (ctx.signal?.aborted || error?.kind === "cancelled") throw failure("cancelled", { dispatched })
    if (error?.message === "account_scope_changed" || error?.message === "account_generation_changed")
      throw failure("scope_changed", { dispatched })
    const timeout = error?.kind === "timeout" || error?.name === "TimeoutError"
    throw failure(timeout ? "timeout" : "transport", { retryable: true, dispatched })
  }
  const env = response.data
  const requestId = requestIdOf(response.headers)
  const details = { httpStatus: response.status, requestId, dispatched: true }
  const validEnvelope = object(env) && Number.isSafeInteger(env.code) && (env.msg === undefined || typeof env.msg === "string")
  if (validEnvelope && env.code !== 0) {
    if (isOpportunity && env.code === 3301) {
      if (!object(env.data) || env.data.granted !== false || !Number.isSafeInteger(env.data.next_try_at) || env.data.next_try_at <= 0)
        throw failure("invalid_response", { ...details, code: env.code })
      return { granted: false, next_try_at: env.data.next_try_at }
    }
    // 2007 is the reset coordinator's known transient business failure. Do
    // not assign an inferred reset-specific meaning to other codes.
    throw failure("business", { ...details, code: env.code, retryable: env.code === 2007,
      throttled: isOpportunity && env.code === 429 })
  }
  if (!response.ok) {
    const throttled = isOpportunity && response.status === 429
    throw failure(throttled ? "throttled" : "http", { ...details, throttled, retryable: response.status >= 500 })
  }
  if (!validEnvelope) throw failure("invalid_response", details)
  if (operation === "reset.history.read") return null
  if (operation === "reset.status") {
    if (!statusData(env.data)) throw failure("invalid_response", details)
    const data = env.data
    return {
      available_five_hour_resets: data.available_five_hour_resets.map(({ expire_at }) => ({ expire_at })),
      available_week_resets: data.available_week_resets.map(({ expire_at }) => ({ expire_at })),
      latest_five_hour_reset_history: data.latest_five_hour_reset_history === null ? null : { used_at: data.latest_five_hour_reset_history.used_at },
      latest_week_reset_history: data.latest_week_reset_history === null ? null : { used_at: data.latest_week_reset_history.used_at },
      has_unread_history: data.has_unread_history,
    }
  }
  if (isOpportunity && object(env.data) && env.data.granted === true) return { granted: true }
  if (isUse && object(env.data) && env.data.used === true) return { used: true }
  throw failure("invalid_response", details)
}
