import { SDKError } from "./bytes.js";

export function validateAgentDecisionModels(manifest, models) {
  const invalid = message => { throw new SDKError("invalid_params", message); };
  if (manifest?.contract?.version?.major !== 1 || !Number.isSafeInteger(manifest.contract.version.minor)
    || manifest.contract.version.minor < 9 || !manifest.contract.capabilities?.includes("agent-wiring")) {
    invalid("Agent decision models require SDK API 1.9 agent-wiring");
  }
  if (!Array.isArray(models)) invalid("Agent decision models must be an array");
  for (const model of models) {
    if (model === null || typeof model !== "object" || Array.isArray(model)
      || Object.keys(model).some(key => !["id", "name", "provider", "group", "endpoints"].includes(key))
      || typeof model.id !== "string" || model.id.trim() === ""
      || typeof model.name !== "string" || model.name.trim() === ""
      || model.group === true && Object.hasOwn(model, "provider")
      || model.group !== true && (model.group !== undefined && model.group !== false
        || typeof model.provider !== "string" || model.provider.trim() === "")) {
      invalid("Agent decision model requires an id, name and either provider or group");
    }
    if (!Array.isArray(model.endpoints) || model.endpoints.length === 0 || model.endpoints.length > 2
      || model.endpoints.some(endpoint => !["/v1/systemone", "/v1/decisions"].includes(endpoint))
      || new Set(model.endpoints).size !== model.endpoints.length) {
      invalid("Invalid or duplicate agent decision endpoint");
    }
  }
}
