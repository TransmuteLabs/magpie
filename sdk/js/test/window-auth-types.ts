import { ui, type WindowAuthFlowView } from "../ui.js";
import type { Manifest, WindowAuthBindingDescriptor } from "../index.js";

const grant: WindowAuthBindingDescriptor = {
  id: "signin", from: "window:page", provider: "vendor",
  methods: [{ id: "browser", mode: "poll" }],
};
const manifest: Partial<Manifest> = {
  authBindings: [grant], contract: { version: { major: 1, minor: 5 }, capabilities: ["account-auth"] },
};
const view: Promise<WindowAuthFlowView> = ui.auth.start("signin", {
  method: "browser", input: { site: "fixture" },
  account: { id: "chosen", expectedGeneration: "9007199254740993" },
});
void manifest;
void view.then(value => ui.auth.finish("signin", value.flow));
void ui.auth.cancel("signin", "host-flow");
// @ts-expect-error account authority belongs outside business input and has a string generation
void ui.auth.start("signin", { method: "device", account: { id: "chosen", expectedGeneration: 1 } });
// @ts-expect-error callback support is not in the polling contract
grant.methods.push({ id: "browser", mode: "callback" });
