interface HybridBridge {
  ready: Promise<unknown>;
  backend: { call(binding: string, input: unknown): Promise<unknown> };
}

declare const magpie: HybridBridge;

const output = document.querySelector("pre")!;
async function show(binding: string, input: unknown): Promise<void> {
  try {
    await magpie.ready;
    output.textContent = JSON.stringify(await magpie.backend.call(binding, input), null, 2);
  } catch (error) {
    output.textContent = error instanceof Error ? error.message : String(error);
  }
}

document.querySelector("#primary")!.addEventListener("click", () => void show("page.pid", {}));
document.querySelector("#backend")!.addEventListener("click", () => void show("page.backend", {}));
document.querySelector("#forward")!.addEventListener("click", () => {
  const text = (document.querySelector("#text") as HTMLInputElement).value;
  void show("page.forward", { text });
});
