import { type Manifest, type AgentDescriptor, type AgentBindingSnapshot, type AgentBindingChange, validateAgentBindingChange } from "../index.js";

const descriptor: AgentDescriptor = {
  id: "native-profile", name: "Native profile", aliases: ["native-cli"], ua: ["native/"],
  profile: { target: "existing-agent" },
  fields: [{ key: "model", label: "Model" }],
  launch: { env: { AGENT_URL: "{url}" }, args: { "--settings": "{config}" }, model: "--model", plain: ["login"] },
  library: { instructions: ".native/AGENTS.md", mcp: ".native/mcp.json", mcpFormat: "claude", skills: ".native/skills" },
};
const manifest: Manifest = {
  schema: 1, id: "native", version: "1.0.0", runtime: "exec", entry: "worker",
  contract: { version: { major: 1, minor: 5 }, capabilities: ["agent-wiring"] }, agents: [descriptor],
};
const before: AgentBindingSnapshot = { target: "existing-agent", revision: "0", connected: false, values: {} };
const change: AgentBindingChange = { revision: "0", connected: true, values: { model: "magpie/model" } };
validateAgentBindingChange(manifest, descriptor, before, "connect", change);
