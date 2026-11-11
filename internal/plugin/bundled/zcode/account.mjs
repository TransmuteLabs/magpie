// ZCode account runtime, adapted from zai-org/ZCode 3.18.0.
// Copyright 2026 Z.AI Co., Ltd. Licensed under Apache-2.0; see LICENSE.ZCode.
import { createHash, randomUUID } from "node:crypto"
import { ProjectTokenClient, ProjectTokenError, ZaiBusinessTokenCache, ZCODE_VERSION } from "./project-token.mjs"

const hash = (value) => createHash("sha256").update(value).digest("hex")
const text = (value) => typeof value === "string" ? value.trim() : ""
const rawToken = (value) => text(value).replace(/^Bearer\s+/i, "")
const validKind = (kind) => ["individual", "team", "start", "legacy-key"].includes(kind)

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

function unavailable(code, kind = "unavailable") {
  const error = new Error(code)
  error.kind = code === "account_scope_changed" || code === "account_generation_changed" ? code : kind
  return error
}

function expired(token, now = Date.now()) {
  const parts = token.split(".")
  if (parts.length !== 3) return false
  try {
    const exp = Number(JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8")).exp)
    return Number.isFinite(exp) && exp > 0 && exp * 1000 <= now + 30_000
  } catch {
    return false
  }
}

function origin(value) {
  const u = new URL(value)
  if (!(["https:", "http:"].includes(u.protocol)) || u.username || u.password) throw unavailable("account_origin_invalid")
  return u.origin
}

// The public scope is opaque. A legacy credential with no user ID still has
// its own stable scope; its secret or fingerprint is never returned to callers.
export function accountScope(state) {
  const fallback = state.kind === "legacy-key" ? text(state.key) : rawToken(state.jwt) || rawToken(state.business || state.token)
  const uid = text(state.uid) || (fallback ? "unidentified:" + hash(fallback) : "")
  const teamSelection = state.kind === "team" || (state.kind === "legacy-key" && (text(state.org) || text(state.project)))
  return hash(JSON.stringify([origin(state.origins.business), origin(state.origins.zcode), state.site, uid, state.kind,
    teamSelection ? text(state.org) : "", teamSelection ? text(state.project) : "",
    teamSelection ? text(state.product) : ""]))
}

export function createAccountRuntime({ client, providerId, stateOf, origins, claimVerification, now = Date.now, random = Math.random }) {
  const entries = new Map()
  const defaults = {
    zai: { business: "https://api.z.ai", zcode: "https://zcode.z.ai", zaiOAuth: "https://chat.z.ai" },
    bigmodel: { business: "https://bigmodel.cn", zcode: "https://zcode.z.ai", zaiOAuth: "https://chat.z.ai" },
  }
  const stateFor = (auth, follow = false) => {
    const value = stateOf(auth, { follow })
    if (!value) throw unavailable("account_sign_in_required")
    if (value.version !== undefined && value.version !== 1) throw unavailable("account_schema_unsupported")
    const site = value.site === "bigmodel" ? "bigmodel" : "zai"
    const selectedOrigins = { ...defaults[site], ...(origins?.[site] ?? {}) }
    const normalized = {
      business: origin(selectedOrigins.business),
      zcode: origin(selectedOrigins.zcode),
      zaiOAuth: origin(selectedOrigins.zaiOAuth),
      ...(Array.isArray(selectedOrigins.mcpDev) ? { mcpDev: selectedOrigins.mcpDev.map(origin) } : {}),
    }
    const state = { ...value, version: 1, site, origins: normalized, uid: text(value.uid || auth.uid) }
    if (!validKind(state.kind)) throw unavailable("account_selection_unavailable")
    if (state.kind === "team" && (!text(state.org) || !text(state.project) || !text(state.product))) throw unavailable("account_selection_unavailable")
    return state
  }

  async function capture(getAuth, { signal, expectedScope, expectedGeneration, verifyIdentity = false } = {}) {
    if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
    let auth = await getAuth()
    if (!auth) throw unavailable("account_sign_in_required")
    let saved = stateFor(auth)
    // Older app-owned slots contained no identity or credential at all.
    // Bind them only to the app's current authoritative identity and
    // selection, never to their display label.
    if (saved.source === "zcode" && !text(saved.uid) && !text(saved.key) && !rawToken(saved.jwt) &&
        !rawToken(saved.business || saved.token || saved.oauthToken)) {
      const followed = stateFor(auth, true)
      if (!text(followed.uid) || !rawToken(followed.jwt || followed.business || followed.token || followed.oauthToken) && !text(followed.key))
        throw unavailable("account_identity_unavailable")
      const scope = accountScope(followed)
      const next = { ...auth, uid: followed.uid, refresh: JSON.stringify(followed), accountScope: scope, accountEpoch: randomUUID() }
      const result = await client?.auth?.set?.({ path: { id: providerId }, body: next, expected: auth })
      if (!result?.data?.saved && !result?.data?.conflict) throw unavailable("account_persistence_unavailable")
      auth = await getAuth()
      if (!auth) throw unavailable("account_sign_in_required")
      saved = stateFor(auth)
    }
    let scope = accountScope(saved)
    if (auth.accountScope && auth.accountScope !== scope) throw unavailable("account_scope_changed")

    // All writes compare the captured auth. On conflict only the same login
    // generation may be rebased, and no vendor mutation is repeated.
    async function writeDerived(change, { newSession = false } = {}) {
      for (;;) {
        const before = auth
        const oldEpoch = text(before.accountEpoch)
        const current = stateFor(before)
        if (accountScope(current) !== scope) throw unavailable("account_scope_changed")
        const nextState = change(current)
        const next = { ...before, accountScope: scope,
          accountEpoch: newSession ? randomUUID() : oldEpoch || randomUUID(),
          ...(before.type === "oauth" ? { refresh: JSON.stringify(nextState) } : {}) }
        const result = await client?.auth?.set?.({ path: { id: providerId }, body: next, expected: before })
        if (result?.data?.saved === true) {
          const fresh = await getAuth()
          if (!fresh || fresh.accountEpoch !== next.accountEpoch || fresh.accountScope !== scope) throw unavailable("account_scope_changed")
          auth = fresh
          saved = stateFor(fresh)
          return
        }
        if (!result?.data?.conflict) throw unavailable("account_persistence_unavailable")
        const fresh = await getAuth()
        if (!oldEpoch && fresh?.accountEpoch && accountScope(stateFor(fresh)) === scope &&
            JSON.stringify(stateFor(fresh)) === JSON.stringify(current)) {
          auth = fresh
          saved = stateFor(fresh)
          return
        }
        if (!fresh || text(fresh.accountEpoch) !== oldEpoch || accountScope(stateFor(fresh)) !== scope) throw unavailable("account_scope_changed")
        auth = fresh
      }
    }

    if (!auth.accountEpoch || !auth.accountScope) await writeDerived((s) => s)
    // Follow the app only with an affirmative family/UID/full-selection match.
    // A changed app session is a new login, not a derived location update.
    const followed = stateFor(auth, true)
    if (saved.source === "zcode" && saved.uid && followed.uid === saved.uid && followed.site === saved.site &&
        accountScope(followed) === scope &&
        (rawToken(followed.business || followed.token) !== rawToken(saved.business || saved.token) ||
         rawToken(followed.jwt) !== rawToken(saved.jwt) || text(followed.oauthToken) !== text(saved.oauthToken) ||
         text(followed.loginGeneration) !== text(saved.loginGeneration) || text(followed.accountIdentity) !== text(saved.accountIdentity) ||
         (text(followed.zcodeUserId) && text(followed.zcodeUserId) !== text(saved.zcodeUserId)) ||
         text(followed.refreshToken) !== text(saved.refreshToken) ||
         text(followed.key) !== text(saved.key))) {
      await writeDerived(() => ({ ...saved, ...followed, loc: followed.loc }), { newSession: true })
    }
    const generation = text(auth.accountEpoch)
    if (expectedScope && expectedScope !== scope) throw unavailable("account_scope_changed")
    if (expectedGeneration && expectedGeneration !== generation) throw unavailable("account_generation_changed")
    // Older slots may have a family UID but no proof that it is the ZCode
    // entitlement owner. Only a matching profile authenticated by this same
    // ZCode session may promote it; inference never depends on this read.
    if (verifyIdentity && saved.uid && !text(saved.zcodeUserId) && rawToken(saved.jwt) && !expired(rawToken(saved.jwt), now())) {
      const capturedUid = saved.uid, capturedJwt = rawToken(saved.jwt)
      const deadline = AbortSignal.timeout(15_000)
      const bounded = signal ? AbortSignal.any([signal, deadline]) : deadline
      try {
        bounded.throwIfAborted()
        const res = await untilAbort(fetch(`${saved.origins.zcode}/api/v1/oauth/profile`, {
          headers: { Authorization: `Bearer ${capturedJwt}` }, signal: bounded, redirect: "error",
        }), bounded)
        const raw = await untilAbort(res.text(), bounded)
        if (raw.length > 1024 * 1024) throw unavailable("account_identity_unavailable")
        const profile = JSON.parse(raw)
        const verified = res.ok && profile?.code === 0 ? text(profile?.data?.user?.user_id) : ""
        const latest = await getAuth()
        if (!latest || text(latest.accountEpoch) !== generation) throw unavailable("account_generation_changed")
        if (latest.accountScope !== scope || accountScope(stateFor(latest)) !== scope) throw unavailable("account_scope_changed")
        auth = latest
        saved = stateFor(latest)
        if (saved.uid !== capturedUid || rawToken(saved.jwt) !== capturedJwt) throw unavailable("account_generation_changed")
        if (verified && verified === capturedUid) await writeDerived((current) => {
          if (current.uid !== capturedUid || rawToken(current.jwt) !== capturedJwt) throw unavailable("account_generation_changed")
          return { ...current, zcodeUserId: verified }
        })
      } catch (error) {
        if (signal?.aborted) throw signal.reason ?? error
        if (error?.kind === "account_scope_changed" || error?.kind === "account_generation_changed") throw error
      }
    }
    const state = {
      site: saved.site, kind: saved.kind, uid: saved.uid, zcodeUserId: text(saved.zcodeUserId),
      accountIdentity: saved.accountIdentity || saved.uid,
      device: saved.device, base: saved.base,
      org: saved.org || "", project: saved.project || "", product: saved.product || "", origins: saved.origins,
    }

    async function verifyCurrent() {
      const currentAuth = await getAuth()
      if (!currentAuth || currentAuth.accountEpoch !== generation) throw unavailable("account_generation_changed")
      const current = stateFor(currentAuth)
      if (currentAuth.accountScope !== scope || accountScope(current) !== scope) throw unavailable("account_scope_changed")
      if (current.source === "zcode") {
        const app = stateFor(currentAuth, true)
        if (current.uid && app.uid === current.uid && app.site === current.site && accountScope(app) === scope &&
            (rawToken(app.business || app.token) !== rawToken(current.business || current.token) ||
             rawToken(app.jwt) !== rawToken(current.jwt) || text(app.oauthToken) !== text(current.oauthToken) ||
             text(app.loginGeneration) !== text(current.loginGeneration) || text(app.accountIdentity) !== text(current.accountIdentity) ||
             (text(app.zcodeUserId) && text(app.zcodeUserId) !== text(current.zcodeUserId)) ||
             text(app.refreshToken) !== text(current.refreshToken) ||
             text(app.key) !== text(current.key)))
          throw unavailable("account_generation_changed")
      }
      auth = currentAuth
      saved = current
    }

    async function assertCurrent() {
      if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
      await verifyCurrent()
      if (signal?.aborted) throw signal.reason ?? new DOMException("Aborted", "AbortError")
    }

    const entryKey = JSON.stringify([scope, generation])
    let owner = entries.get(entryKey)
    if (!owner) {
      owner = { pat: new ProjectTokenClient({ fetch: (...args) => fetch(...args), now, random }),
        business: new ZaiBusinessTokenCache({ now }) }
      entries.set(entryKey, owner)
    }

    async function resolveBusiness(operationSignal) {
      await assertCurrent()
      const oauth = rawToken(saved.oauthToken)
      if (state.site !== "zai" || !oauth) return { token: rawToken(saved.business || saved.token) }
      return owner.business.resolve(oauth, async () => {
        await verifyCurrent()
        const timeout = AbortSignal.timeout(15_000)
        try {
          const res = await untilAbort(fetch(`${state.origins.business}/api/auth/z/login`, {
            method: "POST", headers: { "Content-Type": "application/json", "User-Agent": `ZCode/${ZCODE_VERSION}` },
            body: JSON.stringify({ token: oauth }), signal: timeout, redirect: "error",
          }), timeout)
          await verifyCurrent()
          if (!res.ok) {
            await res.body?.cancel().catch(() => {})
            throw new ProjectTokenError("project_token_login_failed", { transient: res.status === 429 || res.status >= 500 && res.status < 600 })
          }
          const raw = await untilAbort(res.json(), timeout)
          await verifyCurrent()
          return raw
        } catch (error) {
          if (error instanceof ProjectTokenError || error?.kind === "account_generation_changed" || error?.kind === "account_scope_changed") throw error
          throw new ProjectTokenError("project_token_login_failed", { transient: error instanceof TypeError || timeout.aborted })
        }
      }, operationSignal)
    }

    async function loginMaterial({ business = "required", zcode = "required" } = {}) {
      await assertCurrent()
      const get = (name, requirement, token) => {
        if (!["required", "optional", "none"].includes(requirement)) throw unavailable("account_credential_requirement_invalid")
        if (requirement === "none") return ""
        const value = rawToken(token)
        if (!value || expired(value, now())) {
          if (requirement === "required") {
            const error = unavailable(`account_${name}_relogin_required`)
            error.credentialState = value ? "expired" : "missing"
            throw error
          }
          return ""
        }
        return value
      }
      let businessToken = ""
      if (business !== "none") {
        try { businessToken = (await resolveBusiness(signal)).token }
        catch (error) {
          if (business === "required" || signal?.aborted || error?.kind === "account_generation_changed" || error?.kind === "account_scope_changed") throw error
        }
      }
      await assertCurrent()
      return { business: get("business", business, businessToken), zcode: get("zcode", zcode, saved.jwt) }
    }

    async function request(url, init = {}, { timeout = 15_000 } = {}) {
      const mark = (error, dispatched) => {
        if (error && typeof error === "object") {
          try { error.dispatched = dispatched } catch {}
          return error
        }
        return Object.assign(new Error("account_request_cancelled"), { kind: "cancelled", dispatched })
      }
      try { await assertCurrent() } catch (error) { throw mark(error, false) }
      const controller = new AbortController()
      const signals = [signal, init.signal].filter(Boolean)
      let timedOut = false
      const abort = (s) => controller.abort(s.reason ?? new DOMException("Aborted", "AbortError"))
      const listeners = signals.map((s) => {
        const fn = () => abort(s)
        if (s.aborted) fn()
        else s.addEventListener("abort", fn, { once: true })
        return [s, fn]
      })
      const timer = setTimeout(() => { timedOut = true; controller.abort() }, timeout)
      let dispatched = false
      let rejectAbort
      const aborted = new Promise((_, reject) => { rejectAbort = () => reject(controller.signal.reason ?? new DOMException("Aborted", "AbortError")) })
      controller.signal.addEventListener("abort", rejectAbort, { once: true })
      if (controller.signal.aborted) rejectAbort()
      try {
        const work = (async () => {
          if (controller.signal.aborted) throw controller.signal.reason
          dispatched = true
          const res = await fetch(url, { ...init, signal: controller.signal, redirect: "error" })
          let body = ""
          let size = 0
          if (res.body) {
            const reader = res.body.getReader()
            const decoder = new TextDecoder()
            try {
              for (;;) {
                const part = await reader.read()
                if (part.done) break
                size += part.value.byteLength
                if (size > 4 * 1024 * 1024) {
                  void reader.cancel().catch(() => {})
                  throw unavailable("account_response_too_large", "malformed")
                }
                body += decoder.decode(part.value, { stream: true })
              }
              body += decoder.decode()
            } finally {
              reader.releaseLock()
            }
          }
          let data = null
          try { data = JSON.parse(body) } catch {}
          return { ok: res.ok, status: res.status, headers: res.headers, data, emptyBody: size === 0 }
        })()
        const result = await Promise.race([work, aborted])
        await assertCurrent()
        return result
      } catch (error) {
        if (signal?.aborted || init.signal?.aborted) throw mark(signal?.reason ?? init.signal?.reason ?? error, dispatched)
        if (error?.kind && error.kind !== "network") throw mark(error, dispatched)
        const safe = unavailable(timedOut ? "account_request_timeout" : "account_request_failed", timedOut ? "timeout" : "network")
        safe.retryable = true
        safe.dispatched = dispatched
        throw safe
      } finally {
        clearTimeout(timer)
        controller.signal.removeEventListener("abort", rejectAbort)
        for (const [s, fn] of listeners) s.removeEventListener("abort", fn)
      }
    }

    async function saveLocation(loc, rejectedLocation) {
      await assertCurrent()
      await writeDerived((current) => {
        const next = { ...current }
        if (loc) next.loc = loc
        else if (!rejectedLocation || JSON.stringify(next.loc) === JSON.stringify(rejectedLocation)) delete next.loc
        return next
      })
      await assertCurrent()
    }

    async function projectToken({ rejectedFingerprint, signal: operationSignal } = {}) {
      await assertCurrent()
      const ownSignal = signal && operationSignal ? AbortSignal.any([signal, operationSignal]) : signal || operationSignal
      ownSignal?.throwIfAborted()
      if (state.kind === "legacy-key") {
        const token = text(saved.key)
        if (!token) throw unavailable("account_legacy_key_unavailable")
        return { kind: "legacy-key", token, fingerprint: hash(token), expiresAt: 0 }
      }
      if (state.kind !== "individual" && state.kind !== "team") throw unavailable("project_token_selection_unsupported")
      if (!state.uid) throw unavailable("project_token_identity_required")
      for (let attempt = 0; attempt < 2; attempt++) {
        const business = await resolveBusiness(ownSignal)
        if (!business.token || expired(business.token, now())) throw unavailable("account_business_relogin_required")
        const loc = saved.loc
        try {
          let knownLocation
          try {
            if (loc && (!loc.origin || origin(loc.origin) === state.origins.business)) knownLocation = loc
          } catch {}
          const value = await owner.pat.resolve({
            origin: state.origins.business, family: state.site, accountId: state.accountIdentity,
            loginToken: business.token, loginGeneration: generation,
            ...(state.kind === "team" ? { team: { organizationId: state.org, projectId: state.project } } : {}),
            ...(knownLocation ? { knownLocation } : {}), rejectedFingerprint, signal: ownSignal,
            isCurrent: async () => { await verifyCurrent(); return true },
          })
          ownSignal?.throwIfAborted()
          await assertCurrent()
          const location = { origin: state.origins.business, organizationId: value.organizationId,
            projectId: value.projectId, apiKeyId: value.apiKeyId }
          if (JSON.stringify(saved.loc) !== JSON.stringify(location)) await saveLocation(location)
          ownSignal?.throwIfAborted()
          await assertCurrent()
          return { kind: "project-token", token: value.token, fingerprint: value.fingerprint, expiresAt: value.expiresAt }
        } catch (error) {
          if (error?.status === 404 && loc && !ownSignal?.aborted) await saveLocation(null, loc)
          if (error?.businessRejected && saved.oauthToken && attempt === 0) {
            owner.business.invalidate(business)
            continue
          }
          if (error instanceof ProjectTokenError) {
            error.kind = error.code === "project_token_scope_invalidated" ? "account_generation_changed" : "unavailable"
            error.httpStatus = error.status
            error.retryable = error.transient === true
          }
          throw error
        }
      }
      throw unavailable("account_business_relogin_required")
    }

    return {
      state, scope, generation, signal, assertCurrent, loginMaterial, request, projectToken,
      public: () => ({ scope, generation, origin: state.origins.zcode, family: state.site, userId: state.zcodeUserId, accountIdentity: state.accountIdentity,
        kind: state.kind, organization: state.org, project: state.project, product: state.product }),
      claimVerification: async () => { await assertCurrent(); const result = await claimVerification(); await assertCurrent(); return result },
    }
  }

  return { capture }
}
