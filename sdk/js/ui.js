// Browser author entry. The existing page bridge owns transport and lifetime.
export { createAccountWorkflow } from "./account-selection.js";

// A sandboxed page cannot open an approval popup. Keep the active flow's
// address selectable for manual copying without granting navigation authority.
export function createAuthLinkPresentation({ container, input, select }) {
  let disposed = false;
  const clear = () => { input.value = ""; container.hidden = true; };
  const choose = () => {
    if (disposed || container.hidden) return;
    input.focus({ preventScroll: true });
    input.select();
  };
  input.readOnly = true;
  clear();
  select.addEventListener("click", choose);
  return Object.freeze({
    update(flow) {
      clear();
      if (disposed || !flow || !["waiting", "pending"].includes(flow.state) ||
        typeof flow.url !== "string" || /[\x00-\x20\x7f]/u.test(flow.url)) return;
      try {
        const url = new URL(flow.url);
        if (url.protocol !== "https:" || url.username || url.password) return;
        input.value = url.href;
        container.hidden = false;
      } catch { /* Invalid display URLs stay unavailable. */ }
    },
    dispose() {
      disposed = true;
      clear();
      select.removeEventListener("click", choose);
    },
  });
}

function bridge() {
  const value = globalThis.window?.magpie;
  if (!value?.backend?.call || !value?.config?.get || !value?.config?.cas) {
    throw new Error("Magpie module page bridge is unavailable");
  }
  return value;
}

async function history(method, ...args) {
  const value = bridge().history;
  if (!value || !await value.ready) throw new Error("Session history is unsupported by this host");
  return value[method](...args);
}

export const ui = Object.freeze({
  get ready() { return bridge().ready; },
  get service() { return bridge().service ?? ""; },
  get theme() { return bridge().theme ?? null; },
  get context() { return bridge().context ?? {}; },
  get visible() { return bridge().visible !== false; },
  history: Object.freeze({
    get ready() { return bridge().history?.ready ?? Promise.resolve(false); },
    inspect(source) { return history("inspect", source); },
    requestAccess(selection) { return history("requestAccess", selection); },
    release(selection) { return history("release", selection); },
    list(selection) { return history("list", selection); },
    read(selection) { return history("read", selection); },
    media(selection) { return history("media", selection); },
  }),
  onTheme(listener) { return bridge().onTheme(listener); },
  onVisibility(listener) { return bridge().onVisibility(listener); },
  onAction(listener) { return bridge().onAction(listener); },
  read(name, params = null) { return bridge().read(name, params); },
  image: Object.freeze({
    copy(png) { return bridge().image.copy(png); },
    save(png, name) { return bridge().image.save(png, name); },
  }),
  backend: Object.freeze({
    call(binding, input = null, options) {
      return options === undefined ? bridge().backend.call(binding, input) : bridge().backend.call(binding, input, options);
    },
    accounts(binding) { return bridge().backend.accounts(binding); },
  }),
  auth: Object.freeze({
    start(binding, options) { return bridge().auth.start(binding, options); },
    finish(binding, flow) { return bridge().auth.finish(binding, flow); },
    cancel(binding, flow) { return bridge().auth.cancel(binding, flow); },
  }),
  config: Object.freeze({
    get() { return bridge().config.get(); },
    cas(expected, values) { return bridge().config.cas(expected, values); },
  }),
});
