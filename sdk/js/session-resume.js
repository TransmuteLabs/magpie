import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const fail = message => { throw new SDKError("invalid_params", message); };
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const id = value => typeof value === "string" && /^[a-z0-9][a-z0-9._-]{0,127}$/.test(value);

export function validateSessionResume(manifest, owner, mode, recipe) {
  if (mode !== "agent") {
    if (recipe != null || !["none", "native"].includes(mode)) fail("Invalid session resume mode or unexpected agent recipe");
    return;
  }
  if (manifest.contract.version.minor < 10 || !manifest.contract.capabilities.includes("agent-wiring")
    || !object(recipe) || Object.keys(recipe).some(key => !["agent", "args"].includes(key))
    || !id(recipe.agent) || !Array.isArray(recipe.args) || recipe.args.length < 1 || recipe.args.length > 32) {
    fail("Agent-backed session resume requires API 1.10 and an owned launch recipe");
  }
  if (!manifest.agents?.some(agent => agent.id === recipe.agent && agent.launch != null
    && (agent.id === owner || agent.profile?.target === owner))) {
    fail("Session resume agent must belong to the source's own package and native owner");
  }
  let placeholders = 0;
  for (const argument of recipe.args) {
    if (argument === "{sessionId}") placeholders++;
    else if (typeof argument !== "string" || argument.length === 0 || Buffer.byteLength(argument) > 1024
      || /[{}\p{Cc}]/u.test(argument)) fail("Session resume accepts bounded literal argv and one session identity placeholder");
  }
  if (placeholders !== 1) fail("Session resume requires exactly one session identity argument");
}
