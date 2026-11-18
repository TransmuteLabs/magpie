import { createAuthLinkPresentation, type AuthLinkPresentation, type WindowAuthFlowView } from "../ui.js";

const presentation: AuthLinkPresentation = createAuthLinkPresentation({
  container: document.createElement("div"),
  input: document.createElement("input"),
  select: document.createElement("button"),
});
const flow: WindowAuthFlowView = { flow: "opaque", state: "waiting", url: "https://approval.example/" };
presentation.update(flow);
presentation.update(null);
presentation.dispose();
// @ts-expect-error A presenter consumes an auth flow, not an arbitrary link.
presentation.update("https://approval.example/");
// @ts-expect-error This helper grants no navigation authority.
presentation.open(flow.url);
