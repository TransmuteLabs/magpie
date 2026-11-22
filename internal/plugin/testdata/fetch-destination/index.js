const id = "fetch-destination"
let heldLoader = false

export const Destination = async (input) => ({
  config(cfg) {
    cfg.provider ??= {}
    cfg.provider[id] = {
      name: "Destination Fixture",
      npm: "@ai-sdk/openai-compatible",
      api: process.env.DESTINATION_FALLBACK,
      models: {
        chat: { name: "Chat", limit: { context: 8192, output: 1024 } },
        claude: {
          name: "Claude",
          provider: { npm: "@ai-sdk/anthropic", api: new URL("/model/v1/", process.env.DESTINATION_FALLBACK).href },
          limit: { context: 8192, output: 1024 },
        },
        roots: {
          name: "Claude at root",
          provider: { npm: "@ai-sdk/anthropic", api: new URL("/", process.env.DESTINATION_FALLBACK).href },
          limit: { context: 8192, output: 1024 },
        },
        gem: {
          name: "Gemini",
          provider: { npm: "@ai-sdk/google", api: new URL("/v1beta", process.env.DESTINATION_FALLBACK).href },
          limit: { context: 8192, output: 1024 },
        },
      },
    }
  },
  auth: {
    provider: id,
    options: [{ key: "new-route", label: "New route" }],
    methods: [{
      type: "api",
      label: "Fixture access",
      keyless: process.env.DESTINATION_PUBLIC === "yes",
      authorize: async () => ({ type: "success", key: "public" }),
    }],
    async loader(getAuth, provider) {
      const { metadata, key } = await getAuth()
      const baseURL = metadata?.useModelsBase ? provider.models.chat.api.url : metadata?.baseURL
      const newRoute = input.magpie.option(id, "new-route")
      if (!newRoute && !heldLoader && process.env.DESTINATION_LOADER_WAIT) {
        heldLoader = true
        await fetch(process.env.DESTINATION_LOADER_WAIT, { signal: AbortSignal.timeout(15000) })
      }
      return {
        baseURL,
        headers: { "x-loader-route": newRoute ? "new" : "old", "x-loader-model": provider.models.chat.name },
        fetch(url, init) {
          const alternate = process.env.DESTINATION_ALTERNATE
          const origin = new URL(url).origin
          const allowed = alternate
            ? [process.env.DESTINATION_FALLBACK, alternate]
            : [metadata?.baseURL || process.env.DESTINATION_FALLBACK]
          const credentials = [
            init.headers.get("authorization") === `Bearer ${key}`,
            init.headers.get("x-api-key") === key,
            init.headers.get("x-goog-api-key") === key,
          ]
          if (!allowed.some((base) => origin === new URL(base).origin) || !credentials.some(Boolean)) {
            throw new Error("fixture refused a request for another loader destination or credential")
          }
          const useAlternate = alternate && origin === new URL(alternate).origin
          init.headers.set("x-fake-run", useAlternate ? process.env.DESTINATION_ALTERNATE_RUN : process.env.FAKE_RUN)
          init.headers.set("x-loader-host", String(process.pid))
          return fetch(url, init)
        },
      }
    },
  },
  provider: {
    id,
    models(provider, { auth }) {
      const base = auth?.metadata?.baseURL
      if (base) provider.models.chat.api.url = base
      const route = input.magpie.option(id, "new-route") ? "new" : "old"
      provider.models.chat.name = route === "new" ? "New route chat" : "Old route chat"
      if (process.env.DESTINATION_ALTERNATE) {
        const origin = route === "new" ? process.env.DESTINATION_ALTERNATE : process.env.DESTINATION_FALLBACK
        provider.models.chat.api.url = new URL(`/tenant-${route}/v1`, origin).href
        provider.models.chat.api.id = `chat-${route}`
        provider.models.gem.api.url = new URL(`/tenant-${route}/v1beta`, origin).href
        provider.models.gem.api.id = `models/fixture/gem ${route}`
        if (route === "new" && process.env.DESTINATION_NEW_NPM) provider.models.chat.api.npm = process.env.DESTINATION_NEW_NPM
        if (route === "new" && process.env.DESTINATION_REMOVE_MODEL === "yes") delete provider.models.gem
        if (route === "new" && process.env.DESTINATION_REMOVE_MODEL === "fallback") {
          delete provider.models.gem
          provider.models[Symbol.for("magpie.fellBack")] = true
        }
      }
      return provider.models
    },
  },
  async "chat.headers"(request, output) {
    if (request.sessionID === "held-headers") {
      const response = await fetch(process.env.DESTINATION_HEADERS, { signal: AbortSignal.timeout(15000) })
      output.headers["x-prepared"] = await response.text()
    }
    output.headers["x-header-route"] = input.magpie.option(id, "new-route") ? "new" : "old"
  },
})
