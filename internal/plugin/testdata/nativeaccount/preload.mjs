// Loaded only by the test Bun wrapper. Keep the real embedded plugin and
// route every network request to the fixture, never to an external vendor.
const originalFetch = globalThis.fetch
const fixture = new URL(process.env.ZCODE_FIXTURE_URL)
globalThis.fetch = async (input, init = {}) => {
  const request = input instanceof Request ? input : null
  const url = new URL(request ? request.url : String(input))
  if (url.origin === fixture.origin) return originalFetch(input, init)
  if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("unsupported fixture URL")
  const target = new URL(url.pathname + url.search, fixture)
  const headers = new Headers(init.headers ?? request?.headers)
  headers.set("x-zcode-fixture-origin", url.origin)
  headers.set("x-fake-run", process.env.FAKE_RUN)
  headers.delete("host")
  const { proxy: _proxy, ...options } = init
  if (request) {
    options.method ??= request.method
    options.signal ??= request.signal
    options.redirect ??= request.redirect
    if (!Object.hasOwn(options, "body") && request.body) options.body = await request.arrayBuffer()
  }
  return originalFetch(target, { ...options, headers })
}
