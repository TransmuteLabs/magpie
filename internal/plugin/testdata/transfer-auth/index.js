export const TransferPlugin = async ({ client }) => {
  const provider = "transferco"
  const mode = process.env.TRANSFER_METHOD
  const rotate = async () => {
    const response = await fetch(process.env.FAKE_BASE + "/renew", {
      headers: { "x-fake-run": process.env.FAKE_RUN },
    })
    if (!response.ok) throw new Error("fixture did not rotate the token")
    return response.json()
  }
  const save = async (getAuth) => {
    const before = await getAuth()
    const replacement = await rotate()
    await client.auth.set({ path: { id: provider }, body: { ...before, ...replacement } })
  }
  return {
    config(cfg) {
      cfg.provider ??= {}
      cfg.provider[provider] = {
        name: "Transfer Fixture",
        models: { one: { name: "One" } },
      }
    },
    auth: {
      provider,
      methods: [{ type: "api", label: "API key" }],
      ...(mode === "refresh" && { refresh: rotate }),
      async loader(getAuth) {
        if (mode === "loader") await save(getAuth)
        return {
          async fetch() {
            if (mode === "fetch") await save(getAuth)
            return new Response("done")
          },
        }
      },
      async usage(getAuth) {
        if (mode === "usage") await save(getAuth)
        return { plan: "test" }
      },
    },
  }
}
