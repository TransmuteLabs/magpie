// Browser author entry. The existing page bridge owns transport and lifetime.
export { createAccountWorkflow } from "./account-selection.js";
function bridge() {
  const value = globalThis.window?.magpie;
  if (!value?.backend?.call || !value?.config?.get || !value?.config?.cas) {
    throw new Error("Magpie module page bridge is unavailable");
  }
  return value;
}

export const ui = Object.freeze({
  get ready() { return bridge().ready; },
  get service() { return bridge().service ?? ""; },
  get theme() { return bridge().theme ?? null; },
  get context() { return bridge().context ?? {}; },
  get visible() { return bridge().visible !== false; },
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
