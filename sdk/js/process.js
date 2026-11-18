import { Buffer } from "node:buffer";
import { SDKError, decodeBytes, MAX_CHUNK_BYTES } from "./bytes.js";
import { object, validID, actionOwner } from "./composite.js";

export const PROCESS_HOST_METHODS = Object.freeze([
  "process.open", "process.read", "process.write", "process.stdinEOF", "process.close",
]);
export const MAX_PROCESS_ARGV = 128;
export const MAX_PROCESS_ARGV_BYTES = 65536;
const fail = message => { throw new SDKError("invalid_params", message); };
const token = value => typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= 256
  && value.isWellFormed() && !/[\s\p{Cc}]/u.test(value);
const sequence = value => typeof value === "string" && /^[1-9][0-9]{0,19}$/.test(value)
  && BigInt(value) < 18446744073709551615n;
const fields = (value, names) => object(value) && Object.keys(value).every(key => names.includes(key));

export function validateAgentProcessRecipe(recipe) {
  if (!fields(recipe, ["id", "agent", "argv"]) || !validID(recipe.id) || !validID(recipe.agent)
    || !Array.isArray(recipe.argv) || !recipe.argv.length || recipe.argv.length > MAX_PROCESS_ARGV) {
    fail("Process recipe requires a declared agent and bounded literal arguments");
  }
  let bytes = 0;
  for (const argument of recipe.argv) {
    if (typeof argument !== "string" || !argument.isWellFormed() || argument.includes("\0")
      || (bytes += Buffer.byteLength(argument) + 1) > MAX_PROCESS_ARGV_BYTES) {
      fail("Process recipe arguments exceed their byte contract");
    }
  }
}

export function validateAgentProcesses(manifest) {
  const has = capability => manifest.contract.capabilities.includes(capability);
  const recipes = manifest.processes ?? [];
  if (!Array.isArray(recipes) || recipes.length > 16) fail("Invalid owned process recipes");
  if (!recipes.length) {
    if (has("agent-process")) fail("agent-process requires a declared foreground action recipe");
    return;
  }
  if (manifest.contract.version.minor < 10 || !has("agent-process") || !has("agent-wiring")
    || !has("module-rpc") || manifest.component != null) {
    fail("Owned processes require SDK API 1.10 and root module actions and agent wiring");
  }
  const seen = new Set();
  for (const recipe of recipes) {
    validateAgentProcessRecipe(recipe);
    if (seen.has(recipe.id)) fail("Duplicate process action recipe");
    seen.add(recipe.id);
    const action = manifest.actions?.find(action => action.id === recipe.id);
    if (action?.effect !== "write" || actionOwner(action) !== "primary") {
      fail("Process recipe must select its own primary write action");
    }
    if (manifest.providers?.some(provider => provider.checkin?.action === recipe.id
      || provider.accountTasks?.some(task => task.action === recipe.id))) {
      fail("Foreground process action cannot also be an account maintenance action");
    }
    if (!manifest.agents?.some(agent => agent.id === recipe.agent && agent.launch != null)) {
      fail("Process recipe requires a launch-capable agent from its own package");
    }
  }
}

export function processHostLane(method, params) {
  if (method === "process.open" && fields(params, ["recipeId"]) && validID(params.recipeId)) return 3;
  if (method === "process.read" && fields(params, ["handle", "pipe", "seq", "maxBytes"])
    && token(params.handle) && sequence(params.seq) && Number.isInteger(params.maxBytes)
    && params.maxBytes > 0 && params.maxBytes <= MAX_CHUNK_BYTES) {
    if (params.pipe === "stdout") return 0;
    if (params.pipe === "stderr") return 1;
  }
  if (method === "process.write" && fields(params, ["handle", "writeId", "seq", "bytes"])
    && token(params.handle) && token(params.writeId) && sequence(params.seq)) {
    let bytes;
    try { bytes = decodeBytes(params.bytes); }
    catch { fail("Invalid process input bytes"); }
    if (bytes.length > 0 && bytes.length <= MAX_CHUNK_BYTES) return 2;
  }
  if (["process.stdinEOF", "process.close"].includes(method)
    && fields(params, ["handle"]) && token(params.handle)) return 3;
  fail("Invalid bounded process callback");
}
