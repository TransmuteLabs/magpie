import { type Manifest, type Handler, type MiddlewareCall, type MiddlewareResult } from "../index.js";

const manifest: Manifest = { schema: 1, id: "middleware.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["gateway-middleware"] },
  middleware: { request: true, account: true, provider: true, event: true, response: true, origins: ["local"] } };
const result: MiddlewareResult = { state: { seen: true }, reject: { status: 403, message: "denied" } };
const handler: Handler = async (_context, call) => {
  const input = call.params as unknown as MiddlewareCall;
  return { body: input.body, state: result.state };
};
void [manifest, handler];
