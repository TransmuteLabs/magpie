import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const id = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
const flag = value => typeof value === "string" && value.length >= 2 && value.length <= 128 && /^-[a-zA-Z0-9_-]+$/.test(value) && value !== "--";
const text = value => typeof value === "string" && Buffer.byteLength(value) <= 8192 && !/[\p{Cc}]/u.test(value);
const invalid = message => { throw new SDKError("invalid_params", message); };
const mcpFormats = new Set(["alma", "antigravity", "claude", "cline", "codex", "commandcode", "copilot", "crush", "cursor", "devin", "gemini", "goose", "grok", "hermes", "kimi", "omp", "opencode", "zed"]);

export function validateAgentMetadata(manifest, agent, fail, homePath) {
  if (["aliases", "ua", "launch", "profile", "library"].some(key => agent[key] != null)
    && manifest.contract.version.minor < 5) fail("Extended agent metadata requires SDK API 1.5");
  const aliases = agent.aliases ?? [], prefixes = agent.ua ?? [];
  if (!Array.isArray(aliases) || aliases.length > 64 || !Array.isArray(prefixes) || prefixes.length > 64) fail("Invalid agent identity metadata");
  const seen = new Set([agent.id]);
  for (const alias of aliases) {
    if (!id(alias) || seen.has(alias)) fail("Invalid or duplicate agent alias");
    seen.add(alias);
  }
  const agents = new Set();
  for (const prefix of prefixes) {
    if (typeof prefix !== "string" || !prefix || Buffer.byteLength(prefix) > 256 || prefix.trim() !== prefix
      || /[\p{Cc}]/u.test(prefix) || agents.has(prefix.toLowerCase())) fail("Invalid or duplicate agent user-agent prefix");
    agents.add(prefix.toLowerCase());
  }
  if (agent.profile != null && (!object(agent.profile) || Object.keys(agent.profile).some(key => key !== "target")
    || !id(agent.profile.target) || agent.profile.target === agent.id || agent.profile.target === "magpie"
    || agent.launch == null && !agent.configFiles?.length)) {
    fail("Agent profile requires a distinct existing agent target");
  }
  if (agent.launch != null) {
    const launch = agent.launch;
    if (!object(launch) || Object.keys(launch).some(key => !["env", "args", "model", "plain"].includes(key))
      || !agent.bin && agent.profile == null || launch.model != null && launch.model !== "" && !flag(launch.model)) fail("Invalid agent launch declaration");
    if (launch.model && !agent.fields?.some(field => field.key === "model")) fail("Agent launch model flag requires a declared model field");
    const env = launch.env ?? {}, args = launch.args ?? {}, plain = launch.plain ?? [];
    if (!object(env) || Object.keys(env).length > 64 || !object(args) || Object.keys(args).length > 64
      || !Array.isArray(plain) || plain.length > 64) fail("Invalid agent launch metadata");
    for (const [name, value] of Object.entries(env)) {
      if (name.length > 128 || !/^[a-zA-Z_][a-zA-Z0-9_]*$/.test(name) || !text(value)) fail("Invalid agent launch environment");
    }
    for (const [name, value] of Object.entries(args)) {
      if (!flag(name) || !text(value) || value.includes("{config}") && !agent.configFiles?.length && agent.profile == null) fail("Invalid agent launch argument");
    }
    const unique = new Set();
    for (const value of plain) {
      if (typeof value !== "string" || !value || Buffer.byteLength(value) > 128
        || /[\s*?\[\](){}$;|&<>`'"\\]/u.test(value) || unique.has(value)) fail("Invalid or duplicate plain agent argument");
      unique.add(value);
    }
  }
  if (agent.library != null) {
    const library = agent.library;
    if (!object(library) || Object.keys(library).some(key => !["instructions", "mcp", "mcpFormat", "skills"].includes(key))
      || !library.instructions && !library.mcp && !library.skills) fail("Invalid agent library target");
    for (const key of ["instructions", "mcp", "skills"]) {
      if (library[key] != null && library[key] !== "" && !homePath(library[key])) fail("Agent library locations must be home-relative paths");
    }
    if (Boolean(library.mcp) !== Boolean(library.mcpFormat) || library.mcpFormat && !mcpFormats.has(library.mcpFormat)) fail("Agent MCP target requires a location and supported format");
  }
}

function bindingValues(descriptor, values, fail) {
  if (!object(values)) fail("Agent binding values must be an object");
  const fields = new Set((descriptor.fields ?? []).map(field => field.key));
  if (Object.entries(values).some(([key, value]) => !fields.has(key) || !text(value))
    || Buffer.byteLength(JSON.stringify(values)) > 64 * 1024 - 1024) fail("Agent binding contains invalid or undeclared values");
}

function revision(value) {
  return typeof value === "string" && /^(0|[1-9][0-9]*)$/.test(value) && value.length <= 20 && BigInt(value) <= 18446744073709551615n;
}

export function validateAgentBindingSnapshot(manifest, descriptor, value, fail = invalid) {
  if (value == null) return;
  if (manifest.contract.version.minor < 5 || !manifest.contract.capabilities.includes("agent-wiring")
    || !object(value) || Object.keys(value).some(key => !["target", "revision", "connected", "values"].includes(key))
    || value.target !== (descriptor.profile?.target ?? descriptor.id) || !revision(value.revision)
    || typeof value.connected !== "boolean" || value.connected && value.revision === "0"
    || value.connected && descriptor.launch?.model && !value.values?.model) fail("Agent binding requires a current SDK 1.5 host snapshot");
  bindingValues(descriptor, value.values, fail);
}

export function validateAgentBindingChange(manifest, descriptor, before, action, value, fail = invalid) {
  if (value == null) return;
  validateAgentBindingSnapshot(manifest, descriptor, before, fail);
  if (!before || !object(value) || Object.keys(value).some(key => !["revision", "connected", "values"].includes(key))
    || value.revision !== before.revision || typeof value.connected !== "boolean"
    || !["connect", "disconnect", "sync", "set"].includes(action)
    || action === "connect" && !value.connected || action === "disconnect" && value.connected
    || action === "sync" && value.connected !== before.connected
    || value.connected && descriptor.launch?.model && !value.values?.model) fail("Agent binding change does not match its host snapshot and action");
  bindingValues(descriptor, value.values, fail);
}
