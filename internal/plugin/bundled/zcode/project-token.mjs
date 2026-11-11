// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Z.AI Co., Ltd
// Modified for Magpie's embedded fetch and account-lifetime boundaries.
// ZCode project access tokens, following zai-org/ZCode v3.18.0 (b95ebaff):
// shared/project-access-token{,-client}.ts. No credentials are written to disk.
export const ZCODE_VERSION = "3.18.0"
const TIMEOUT = 15_000
const LEAD = 120_000
const JITTER = 60_000
const BACKOFF = 5_000
const SCENE = 1 // API_KEY_USAGE_SCENE.CODING_PLAN
const success = (code) => code === 0 || code === 200 || code === "0" || code === "200"
const text = (value) => typeof value === "string" ? value.trim() : ""
const positive = (value) => typeof value === "number" && Number.isFinite(value) && value > 0

export class ProjectTokenError extends Error {
  constructor(code, { transient = false, status, retryAfter, businessRejected = false } = {}) {
    super(code)
    this.name = "ProjectTokenError"
    this.code = code
    this.transient = transient
    if (status !== undefined) this.status = status
    if (retryAfter !== undefined) this.retryAfter = retryAfter
    if (businessRejected) this.businessRejected = true
  }
}

function dataOf(raw) {
  if (!raw || typeof raw !== "object" || !success(raw.code) || !("data" in raw))
    throw new ProjectTokenError("project_token_invalid_response")
  return raw.data
}

function retryAfter(value, now) {
  const s = text(value)
  if (!s) return undefined
  if (/^\d+(?:\.\d+)?$/.test(s)) {
    const ms = Number(s) * 1000
    return Number.isFinite(ms) ? ms : undefined
  }
  const at = Date.parse(s)
  return Number.isFinite(at) ? Math.max(0, at - now) : undefined
}

async function fingerprint(token) {
  const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(token))
  return Array.from(new Uint8Array(bytes), (b) => b.toString(16).padStart(2, "0")).join("")
}

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

// One caller's cancellation must not abort a shared issuance for another.
function waitFor(pending, signal) {
  if (!signal) return pending
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const abort = () => { signal.removeEventListener("abort", abort); reject(signal.reason) }
    signal.addEventListener("abort", abort, { once: true })
    pending.then(
      (value) => { signal.removeEventListener("abort", abort); resolve(value) },
      (error) => { signal.removeEventListener("abort", abort); reject(error) },
    )
  })
}

// One instance per auth owner. Calling clear invalidates pending work too.
export class ProjectTokenClient {
  #sessions = new Map()
  #owner
  #fetch
  #now
  #random

  constructor({ fetch = globalThis.fetch, now = Date.now, random = Math.random } = {}) {
    this.#fetch = fetch
    this.#now = now
    this.#random = random
  }

  clear() { this.#sessions.clear(); this.#owner = undefined }

  async #current(session, key, input) {
    if (this.#sessions.get(key) !== session || (input.isCurrent && !(await input.isCurrent())))
      throw new ProjectTokenError("project_token_scope_invalidated")
    if (this.#sessions.get(key) !== session)
      throw new ProjectTokenError("project_token_scope_invalidated")
  }

  async #request(session, key, input, url, init) {
    await this.#current(session, key, input)
    const timeout = AbortSignal.timeout(TIMEOUT)
    const signal = timeout
    let res
    try {
      res = await waitFor(this.#fetch(url, { ...init, signal, redirect: "error" }), signal)
      if (!res.ok) {
        await res.body?.cancel().catch(() => {})
        await this.#current(session, key, input)
        if (res.status === 404) return { code: 404 }
        throw new ProjectTokenError(`project_token_http_${res.status}`, {
          status: res.status, transient: res.status === 429 || res.status >= 500 && res.status < 600,
          businessRejected: res.status === 401 && new URL(url).pathname.startsWith("/api/biz/") &&
            new Headers(init.headers).get("Authorization") === "Bearer " + session.token,
          ...(res.status === 429 ? { retryAfter: retryAfter(res.headers.get("retry-after"), this.#now()) } : {}),
        })
      }
      let raw
      try { raw = await waitFor(res.json(), signal) }
      catch (error) {
        if (signal.aborted) throw error
        throw new ProjectTokenError("project_token_invalid_response")
      }
      await this.#current(session, key, input)
      return raw
    } catch (error) {
      await this.#current(session, key, input)
      if (error instanceof ProjectTokenError) throw error
      if (timeout.aborted || error instanceof DOMException && (error.name === "TimeoutError" || error.name === "AbortError"))
        throw new ProjectTokenError("project_token_timeout", { transient: true })
      if (error instanceof TypeError)
        throw new ProjectTokenError("project_token_network_error", { transient: true })
      throw new ProjectTokenError("project_token_request_failed")
    }
  }

  #keys(origin, location) {
    return `${origin}/api/biz/v1/organization/${encodeURIComponent(location.organizationId)}/projects/${encodeURIComponent(location.projectId)}/api_keys`
  }

  async #location(session, key, input, origin, token) {
    const rawHeaders = { Authorization: token, "Content-Type": "application/json" }
    const customer = input.customerInfo ?? dataOf(await this.#request(session, key, input, `${origin}/api/biz/customer/getCustomerInfo`, { method: "GET", headers: rawHeaders }))
    if (!customer || typeof customer !== "object" || Array.isArray(customer) || customer.organizations !== undefined && !Array.isArray(customer.organizations))
      throw new ProjectTokenError("project_token_invalid_response")
    const organizations = customer.organizations ?? []
    for (const org of organizations) {
      if (!org || typeof org.organizationId !== "string" || org.organizationName != null && typeof org.organizationName !== "string" || org.projects !== undefined && !Array.isArray(org.projects))
        throw new ProjectTokenError("project_token_invalid_response")
      for (const project of org.projects ?? []) {
        if (!project || typeof project.projectId !== "string" || project.projectName != null && typeof project.projectName !== "string" || project.projectType != null && !["string", "number"].includes(typeof project.projectType))
          throw new ProjectTokenError("project_token_invalid_response")
      }
    }
    let location = input.team
    if (location) {
      if (!text(location.organizationId) || !text(location.projectId) || !organizations.some((org) => text(org.organizationId) === text(location.organizationId) && (org.projects ?? []).some((p) => text(p.projectId) === text(location.projectId))))
        throw new ProjectTokenError("project_token_project_unavailable")
    } else {
      const personal = input.personalProjectSelection === "default" ? organizations : organizations
        .map((org) => ({ ...org, projects: (org.projects ?? []).filter((p) => String(p.projectType ?? "").trim() !== "2") }))
        .filter((org) => org.organizationId && org.projects.length)
      const org = personal.find((o) => o.organizationName?.includes("默认机构")) ?? personal[0]
      const project = (org?.projects ?? []).find((p) => p.projectName?.includes("默认项目")) ?? org?.projects?.[0]
      if (!org?.organizationId || !project?.projectId) throw new ProjectTokenError("project_token_project_unavailable")
      location = { organizationId: org.organizationId, projectId: project.projectId }
    }
    const headers = { ...rawHeaders }
    if (input.team) {
      headers["bigmodel-organization"] = location.organizationId
      headers["bigmodel-project"] = location.projectId
    } else if (input.family === "zai") headers.Authorization = "Bearer " + token
    const url = this.#keys(origin, location)
    const list = dataOf(await this.#request(session, key, input, url, { method: "GET", headers }))
    if (!Array.isArray(list)) throw new ProjectTokenError("project_token_invalid_response")
    const name = input.team ? "zcode-team-api-key" : "zcode-api-key"
    const valid = (item) => item && text(item.apiKey) && (item.name == null || typeof item.name === "string") && (item.keyType == null || typeof item.keyType === "number")
    const candidate = list.find((item) => item && item.name === name && (!input.team || valid(item) && item.keyType === 2))
    const found = candidate ?? dataOf(await this.#request(session, key, input, url, {
      method: "POST", headers,
      body: JSON.stringify({ name, ...(input.team ? { keyType: 2 } : {}), usageScene: SCENE }),
    }))
    if (!valid(found) || input.team && (found.name !== name || found.keyType !== 2))
      throw new ProjectTokenError("project_token_invalid_response")
    return { organizationId: location.organizationId, projectId: location.projectId, apiKeyId: text(found.apiKey) }
  }

  resolve(input) {
    input.signal?.throwIfAborted()
    const { signal, ...shared } = input
    return waitFor(this.#resolve(shared), signal)
  }

  async #resolve(input) {
    let origin
    try {
      const parsed = new URL(input.origin)
      if (parsed.protocol !== "https:" && parsed.protocol !== "http:") throw new Error()
      origin = parsed.origin
    } catch { throw new ProjectTokenError("project_token_invalid_origin") }
    if (input.family !== "zai" && input.family !== "bigmodel") throw new ProjectTokenError("project_token_invalid_family")
    const token = text(input.loginToken).replace(/^Bearer\s+/i, "")
    const accountId = text(input.accountId)
    if (!token || !accountId) throw new ProjectTokenError("project_token_login_required")
    const key = JSON.stringify([origin, input.family, accountId])
    if (this.#owner !== key) {
      this.#sessions.clear()
      this.#owner = key
    }
    let session = this.#sessions.get(key)
    if (!session || session.token !== token || session.generation !== input.loginGeneration) {
      session = { token, generation: input.loginGeneration, locations: new Map(), entries: new Map(), rejectedLocations: new Set() }
      this.#sessions.set(key, session)
    }
    await this.#current(session, key, input)
    const selection = JSON.stringify([input.team ?? null, input.personalProjectSelection ?? "non-team"])
    let pending = session.locations.get(selection)
    if (!pending) {
      const saved = input.knownLocation
      const known = saved && text(saved.organizationId) && text(saved.projectId) && text(saved.apiKeyId) &&
        (!input.team || saved.organizationId === input.team.organizationId && saved.projectId === input.team.projectId)
      pending = known && !session.rejectedLocations.has(selection)
        ? Promise.resolve({ organizationId: saved.organizationId, projectId: saved.projectId, apiKeyId: saved.apiKeyId })
        : this.#location(session, key, input, origin, token)
      session.locations.set(selection, pending)
      void pending.catch(() => { if (session.locations.get(selection) === pending) session.locations.delete(selection) })
    }
    const location = await pending
    await this.#current(session, key, input)
    const cacheKey = JSON.stringify([location.organizationId, location.projectId, location.apiKeyId, SCENE])
    let entry = session.entries.get(cacheKey)
    if (input.rejectedFingerprint && entry?.fingerprint === input.rejectedFingerprint) {
      entry.value = undefined
      entry.refreshAt = undefined
    }
    if (entry?.pending) return entry.pending
    if (entry?.value && this.#now() < Math.min(entry.refreshAt, entry.value.expiresAt)) return { ...location, ...entry.value, fingerprint: entry.fingerprint }
    if (!entry) { entry = {}; session.entries.set(cacheKey, entry) }
    const current = entry
    const started = this.#now()
    const validFallback = () => session.entries.get(cacheKey) === current && current.value && this.#now() < current.value.expiresAt
    const issuing = Promise.resolve().then(async () => {
      await this.#current(session, key, input)
      let raw
      for (let attempt = 1; ; attempt++) {
        try {
          raw = await this.#request(session, key, input, `${this.#keys(origin, location)}/${encodeURIComponent(location.apiKeyId)}/access_tokens`, {
            method: "POST", headers: { Authorization: "Bearer " + token, "Content-Type": "application/json" },
            body: JSON.stringify({ clientType: "zcode", clientVersion: ZCODE_VERSION }),
          })
          break
        } catch (error) {
          if (!error?.transient || attempt >= 2 || validFallback()) throw error
          if (error.status === 429) {
            if (error.retryAfter !== undefined && error.retryAfter > 1000) throw error
            const min = Math.max(250, error.retryAfter ?? 0)
            const random = Math.max(0, Math.min(1, this.#random()))
            await wait(min + Math.floor((1000 - min) * random))
            await this.#current(session, key, input)
          }
        }
      }
      if (raw?.code === 404) {
        session.locations.delete(selection)
        session.rejectedLocations.add(selection)
        throw new ProjectTokenError("project_token_invalid_response", { status: 404 })
      }
      const data = dataOf(raw)
      if (!data || typeof data !== "object" || data.enable !== undefined && typeof data.enable !== "boolean")
        throw new ProjectTokenError("project_token_invalid_response")
      if (data.enable === false) throw new ProjectTokenError("project_token_disabled")
      if (!text(data.accessToken) || data.tokenType !== "Bearer" || !positive(data.expiresIn) || !positive(data.expiresAt))
        throw new ProjectTokenError("project_token_invalid_response")
      const expiresAt = Math.min(data.expiresAt * 1000, started + data.expiresIn * 1000)
      if (!Number.isFinite(expiresAt) || expiresAt <= this.#now()) throw new ProjectTokenError("project_token_invalid_response")
      const mark = await fingerprint(text(data.accessToken))
      await this.#current(session, key, input)
      current.fingerprint = mark
      current.value = { token: text(data.accessToken), expiresAt }
      current.refreshAt = expiresAt - LEAD - Math.max(0, Math.min(1, this.#random())) * JITTER
      return { ...location, ...current.value, fingerprint: mark }
    }).catch(async (error) => {
      await this.#current(session, key, input)
      if (error?.transient && validFallback()) {
        current.refreshAt = Math.min(this.#now() + BACKOFF, current.value.expiresAt)
        return { ...location, ...current.value, fingerprint: current.fingerprint }
      }
      if (session.entries.get(cacheKey) === current) session.entries.delete(cacheKey)
      if (error instanceof ProjectTokenError) throw error
      throw new ProjectTokenError("project_token_request_failed")
    }).finally(() => { if (current.pending === issuing) current.pending = undefined })
    current.pending = issuing
    return issuing
  }
}

// The CLI OAuth token and the business JWT have different lifetimes. This
// is the official CLI adapter's memory-only cache, owned by the same account.
export class ZaiBusinessTokenCache {
  #session
  #rejected = new WeakSet()
  #now

  constructor({ now = Date.now } = {}) { this.#now = now }

  clear() { this.#session = undefined }

  invalidate(value) {
    this.#rejected.add(value)
    if (this.#session?.value === value) this.#session.value = undefined
  }

  wasRejected(value) { return this.#rejected.has(value) }

  resolve(oauth, exchange, signal) {
    signal?.throwIfAborted()
    return waitFor(this.#resolve(oauth, exchange), signal)
  }

  #resolve(oauth, exchange) {
    if (this.#session?.oauth !== oauth) this.#session = { oauth }
    const session = this.#session
    if (session.pending) return session.pending
    if (session.value && this.#now() < Math.min(session.retryAt ?? session.value.refreshAt, session.value.expiresAt ?? Infinity))
      return Promise.resolve(session.value)
    const started = this.#now()
    const pending = Promise.resolve().then(() => {
      if (this.#session !== session) throw new ProjectTokenError("project_token_scope_invalidated")
      return exchange()
    }).then((raw) => {
      if (this.#session !== session) throw new ProjectTokenError("project_token_scope_invalidated")
      if (!raw || raw.code != null && !success(raw.code)) throw new ProjectTokenError("project_token_login_failed")
      const data = raw.data
      const token = text(data?.access_token) || text(data?.accessToken)
      if (!token) throw new ProjectTokenError("project_token_login_failed")
      let jwtExpiry
      try {
        const exp = JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8")).exp
        if (typeof exp === "number" && Number.isFinite(exp)) jwtExpiry = exp * 1000
      } catch {}
      const ttl = data.expires_in ?? data.expiresIn
      const deadlines = [jwtExpiry, typeof ttl === "number" && Number.isFinite(ttl) ? started + ttl * 1000 : undefined].filter((x) => x !== undefined)
      const expiresAt = deadlines.length ? Math.min(...deadlines) : undefined
      const cacheUntil = expiresAt ?? started + 300_000
      if (cacheUntil <= this.#now()) throw new ProjectTokenError("project_token_login_failed")
      const value = Object.freeze({ token, expiresAt, refreshAt: cacheUntil - LEAD })
      session.value = value
      session.retryAt = undefined
      return value
    }).catch((error) => {
      if (this.#session !== session) throw new ProjectTokenError("project_token_scope_invalidated")
      const value = session.value
      if (error instanceof ProjectTokenError && error.transient && value && !this.wasRejected(value) && value.expiresAt !== undefined && this.#now() < value.expiresAt) {
        session.retryAt = Math.min(this.#now() + BACKOFF, value.expiresAt)
        return value
      }
      session.value = undefined
      session.retryAt = undefined
      if (error instanceof ProjectTokenError) throw error
      throw new ProjectTokenError("project_token_login_failed")
    }).finally(() => { if (session.pending === pending) session.pending = undefined })
    session.pending = pending
    return pending
  }
}
