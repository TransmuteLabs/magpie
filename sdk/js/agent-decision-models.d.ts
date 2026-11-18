import type { Manifest } from "./index.js";

export type AgentDecisionEndpoint = "/v1/systemone" | "/v1/decisions";

export type AgentDecisionModel = {
  id: string;
  name: string;
  endpoints: AgentDecisionEndpoint[];
} & (
  { provider: string; group?: false }
  | { group: true; provider?: never }
);

export function validateAgentDecisionModels(manifest: Manifest, models: AgentDecisionModel[]): void;
