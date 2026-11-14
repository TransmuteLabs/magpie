import { SDKError } from "./bytes.js";

const levels = new Set(["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]);

export function validateAgentModelMetadata(manifest, value) {
  const invalid = message => { throw new SDKError("invalid_params", message); };
  if (manifest?.contract?.version?.major !== 1 || !Number.isSafeInteger(manifest.contract.version.minor) || manifest.contract.version.minor < 9
    || !manifest.contract.capabilities?.includes("agent-wiring")) {
    invalid("Effective agent model metadata requires SDK API 1.9 agent-wiring");
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)
    || Object.keys(value).some(key => !["maxOutput", "efforts", "defaultEffort"].includes(key))
    || value.maxOutput !== undefined && (!Number.isSafeInteger(value.maxOutput) || value.maxOutput < 0)
    || !Object.hasOwn(value, "efforts")
    || value.efforts !== null && (!Array.isArray(value.efforts) || value.efforts.length > levels.size
      || value.efforts.some(level => !levels.has(level)) || new Set(value.efforts).size !== value.efforts.length)
    || value.defaultEffort !== undefined && (typeof value.defaultEffort !== "string"
      || value.defaultEffort !== "" && !value.efforts?.includes(value.defaultEffort))) {
    invalid("Invalid effective agent model limits, effort levels or default");
  }
}
