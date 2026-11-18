import { validateRequestObservation, type Call, type Manifest, type RequestObservation, type Method } from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "observer", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 12 }, capabilities: ["request-observer"] },
  requestObserver: { timeoutMs: 250 },
};
const method: Method = "request.observe";
function parse(call: Call): RequestObservation {
  return validateRequestObservation(call.params);
}
void [manifest, method, parse];
