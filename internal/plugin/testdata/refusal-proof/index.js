async function refuse(saved) {
  if (!saved) return
  if (Object.hasOwn(saved, "_magpieAuthEpoch")) throw new Error("the hook received local sign-in metadata")
  await fetch(process.env.REFUSAL_PROOF_URL, { headers: { "x-fake-run": process.env.FAKE_RUN } })
  throw Object.assign(new Error("the vendor turned the sign-in away"), { signIn: "expired" })
}

export default async () => ({
  config: async (cfg) => {
    cfg.provider ??= {}
    cfg.provider["refusal-proof"] = {
      name: "Refusal Proof",
      npm: "@ai-sdk/openai-compatible",
      models: { fixture: { name: "Fixture", limit: { context: 1000, output: 100 } } },
    }
  },
  auth: {
    provider: "refusal-proof",
    methods: [{ type: "api", label: "API key" }],
    async loader(getAuth) {
      if (process.env.REFUSAL_PROOF_PHASE === "loader") await refuse(await getAuth())
      return { fetch: async () => new Response("ok") }
    },
  },
  provider: {
    id: "refusal-proof",
    async models(provider, { auth }) {
      if (process.env.REFUSAL_PROOF_PHASE === "models") await refuse(auth)
      return provider.models
    },
  },
})
