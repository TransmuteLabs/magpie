import { type AgentModelMetadata, type Manifest, type Model, validateAgentModelMetadata } from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "model-metadata", version: "1.0.0", runtime: "exec", entry: "worker",
  contract: { version: { major: 1, minor: 9 }, capabilities: ["agent-wiring"] },
  compatibility: { hostAPI: "^1.9" },
  agents: [{ id: "editor", name: "Editor" }],
};
const unknown: AgentModelMetadata = { efforts: null };
const empty: AgentModelMetadata = { efforts: [] };
const model: AgentModelMetadata = { maxOutput: 393216, efforts: ["low", "high"], defaultEffort: "high" };
for (const value of [unknown, empty, model]) validateAgentModelMetadata(manifest, value);
const unknownModel: Model = { id: "unknown", efforts: null };
const emptyModel: Model = { id: "empty", efforts: [] };
void [unknownModel, emptyModel];
