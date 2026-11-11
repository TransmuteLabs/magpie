import { AsyncLocalStorage } from "node:async_hooks"

// An isolated model of the host's expected-auth compare-and-set for tests.
export function accountFixture(initial) {
  let current = structuredClone(initial)
  const getAuth = async () => current == null ? null : structuredClone(current)
  const client = { auth: { set: async ({ expected, body }) => {
    if (JSON.stringify(current) !== JSON.stringify(expected)) return { data: { saved: false, conflict: true } }
    current = structuredClone(body)
    return { data: { saved: true, conflict: false } }
  } } }
  return {
    client, getAuth,
    replace(value) { current = value == null ? null : structuredClone(value) },
    current: getAuth,
  }
}

// Give existing behavioral fixtures the host's selected-account scope and
// live getter, including fetches that happen after loader() has returned.
export async function hostedPlugin(Plugin, input = {}, options = {}) {
  const active = new AsyncLocalStorage()
  const accounts = new WeakMap()
  const client = { ...input.client, auth: { ...input.client?.auth, set: async (request) => {
    const slot = active.getStore()
    if (!slot) throw new Error("test host has no selected account")
    return slot.client.auth.set(request)
  } } }
  const hooks = await Plugin({ ...input, client }, options)
  const find = async (getAuth) => {
    const value = await getAuth()
    if (!value || typeof value !== "object") return accountFixture(value)
    let slot = accounts.get(value)
    if (!slot) accounts.set(value, slot = accountFixture(value))
    return slot
  }
  const loader = hooks.auth.loader.bind(hooks.auth)
  hooks.auth.loader = async (getAuth, provider) => {
    const slot = await find(getAuth)
    return active.run(slot, async () => {
      const result = await loader(slot.getAuth, provider)
      if (!result.fetch) return result
      const fetch = result.fetch
      return { ...result, fetch: (...args) => active.run(slot, () => fetch(...args)) }
    })
  }
  for (const name of ["usage", "checkin"]) {
    if (!hooks.auth[name]) continue
    const operation = hooks.auth[name].bind(hooks.auth)
    hooks.auth[name] = async (getAuth, ...args) => {
      const slot = await find(getAuth)
      return active.run(slot, () => operation(slot.getAuth, ...args))
    }
  }
  return hooks
}
