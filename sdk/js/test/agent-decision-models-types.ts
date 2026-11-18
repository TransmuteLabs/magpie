import { type AgentDecisionModel, type Manifest, validateAgentDecisionModels } from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "decision-options", version: "1.0.0", runtime: "exec", entry: "worker",
  contract: { version: { major: 1, minor: 9 }, capabilities: ["agent-wiring"] },
  compatibility: { hostAPI: "^1.9" },
  agents: [{ id: "editor", name: "Editor" }],
};
const models: AgentDecisionModel[] = [
  { id: "mixed/judge", name: "Judge", provider: "mixed", endpoints: ["/v1/systemone", "/v1/decisions"] },
  { id: "group/judges", name: "Judges", group: true, endpoints: ["/v1/decisions"] },
];
validateAgentDecisionModels(manifest, models);
validateAgentDecisionModels(manifest, []);
type RefusesMixedVariant = {
  id: string; name: string; provider: string; group: true; endpoints: ["/v1/systemone"];
} extends AgentDecisionModel ? false : true;
type HasCredentialField = "credentials" extends keyof AgentDecisionModel ? true : false;
const refusesMixedVariant: RefusesMixedVariant = true;
const hasCredentialField: HasCredentialField = false;
void [refusesMixedVariant, hasCredentialField];
