const model = { id: "pi-model" }

export function createEventBus() {}

export async function discoverAndLoadExtensions() {
  return { runtime: { pendingProviderRegistrations: [{ name: "pi-guard", config: {} }] } }
}

export const ModelRuntime = {
  async create({ credentials }) {
    return {
      registerProvider() {},
      getProvider: () => ({ name: "Pi Guard", auth: {} }),
      getModel: (_provider, id) => id === model.id && model,
      getModels: () => [model],
      async *streamSimple() {
        const current = await credentials.modify("pi-guard", async (saved) => {
          if (Object.hasOwn(saved, "_magpieAuthEpoch")) throw new Error("Pi received local sign-in metadata")
          const response = await fetch(process.env.PI_GUARD_VENDOR, {
            method: "POST",
            headers: { "x-fake-run": process.env.FAKE_RUN },
            body: saved.access,
          })
          return await response.json()
        })
        if (Object.hasOwn(current, "_magpieAuthEpoch")) throw new Error("Pi received local sign-in metadata")
        yield { type: "done", reason: "stop", message: { content: [{ type: "text", text: current.access }] } }
      },
    }
  },
}
