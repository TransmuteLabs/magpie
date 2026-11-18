import type { Manifest } from "./index.js";

export interface AgentModelMetadata {
  maxOutput?: number;
  efforts: string[] | null;
  defaultEffort?: string;
}

export function validateAgentModelMetadata(manifest: Manifest, value: AgentModelMetadata): void;
