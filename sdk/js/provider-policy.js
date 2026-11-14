const operations = new Set([
  "embeddings", "rerank", "images.generate", "images.edit",
  "audio.transcribe", "audio.translate", "audio.speech",
  "videos.create", "videos.get", "videos.content", "videos.delete", "videos.remix",
  "systemone",
]);
const kinds = new Set(["", "chat", "image", "video", "embedding", "rerank", "speech", "transcription", "decision"]);
const modalities = new Set(["text", "image", "audio", "video", "pdf"]);

export function validateProviderPolicy(manifest, provider, fail) {
  if (provider.operations === undefined || provider.operations === null) return;
  if (!Array.isArray(provider.operations)) fail("Invalid provider operations");
  if (provider.operations.length === 0) return;
  if (manifest.contract.version.minor < 3 || !manifest.contract.capabilities.includes("outbound-executor"))
    fail("Provider operations require SDK API 1.3 and outbound-executor");
  if (new Set(provider.operations).size !== provider.operations.length
    || provider.operations.some(operation => !operations.has(operation))) fail("Invalid or duplicate provider operation");
  if (provider.operations.includes("systemone") && manifest.contract.version.minor < 7)
    fail("Decision operations require SDK API 1.7");
  if (provider.operations.includes("systemone") && !manifest.contract.capabilities.includes("model-discovery"))
    fail("Decision operations require selected-account model discovery");
  if (provider.operations.some(operation => operation !== "systemone") && !provider.protocols.includes("chat"))
    fail("Provider operations require the OpenAI-compatible chat protocol");
}

export function validateExecuteOperation(manifest, call, fail) {
  const operation = call.params?.operation;
  const provider = manifest.providers?.find(provider => provider.id === call.scope?.provider);
  const model = provider?.models?.find(model => model.id === call.params?.model);
  if (operation == null || operation === "") {
    if (model?.kind === "decision") fail("Decision model requires its declared operation");
    return;
  }
  const decision = operation === "systemone";
  if (model?.kind === "decision" && !decision) fail("Decision model requires its systemone operation");
  if (manifest.contract.version.minor < (decision ? 7 : 3) || !operations.has(operation)
    || call.params?.protocol !== (decision ? "" : "chat"))
    fail("Provider operation requires its negotiated API and protocol envelope");
  if (!provider?.operations?.includes(operation) || !decision && !provider.protocols.includes("chat"))
    fail("Provider operation is not declared by this generation");
  if (decision && (!call.params?.model || !manifest.contract.capabilities.includes("model-discovery")
    && (model?.kind !== "decision" || !model.operations?.includes(operation))))
    fail("Decision model did not declare the requested operation");
}

export function validateModelOperations(manifest, provider, model, fail) {
  if ((model.kind === "decision" || model.operations?.length) && manifest.contract.version.minor < 7)
    fail("Model operations and decision kind require SDK API 1.7");
  if (model.operations != null && (!Array.isArray(model.operations)
    || new Set(model.operations).size !== model.operations.length
    || model.operations.some(operation => !operations.has(operation) || !provider.operations?.includes(operation))))
    fail("Invalid, undeclared or duplicate model operation");
  if (model.kind === "decision") {
    if (model.protocols?.length || model.operations?.length !== 1 || model.operations[0] !== "systemone")
      fail("Decision models require only systemone and no conversational protocols");
  } else if (model.operations?.includes("systemone")) fail("Systemone requires a decision model");
}

export function validateExtendedModel(manifest, model, fail) {
  const extended = model.kind || model.inputModalities != null || model.outputModalities != null
    || model.released || model.temperature != null || model.serviceTiers?.length
    || model.free || model.rate || model.rateWas || model.maxContextWindow
    || model.alwaysThinks || model.dynamicTools || model.defaultEffort;
  if (extended && manifest.contract.version.minor < 3) fail("Extended model metadata requires SDK API 1.3");
  if (!kinds.has(model.kind ?? "")) fail("Unsupported model kind");
  if (model.maxContextWindow != null && (!Number.isSafeInteger(model.maxContextWindow)
    || model.maxContextWindow < 0 || model.maxContextWindow > 0 && model.maxContextWindow < (model.contextWindow ?? 0)))
    fail("Invalid maximum model context");
  for (const field of ["inputModalities", "outputModalities"]) {
    if (model[field] == null) continue;
    if (!Array.isArray(model[field]) || new Set(model[field]).size !== model[field].length
      || model[field].some(modality => !modalities.has(modality))) fail("Invalid or duplicate model modality");
  }
  if (model.released != null && model.released !== "") {
    const value = model.released;
    const date = typeof value === "string" && /^\d{4}-\d{2}-\d{2}$/.test(value) ? new Date(value + "T00:00:00Z") : null;
    if (!date || !Number.isFinite(date.valueOf()) || date.toISOString().slice(0, 10) !== value) fail("Invalid model release date");
  }
  if (model.serviceTiers != null && (!Array.isArray(model.serviceTiers) || model.serviceTiers.length > 2
    || new Set(model.serviceTiers).size !== model.serviceTiers.length
    || model.serviceTiers.some(tier => !["priority", "ultrafast"].includes(tier)))) fail("Invalid model service tiers");
  for (const field of ["rate", "rateWas"]) {
    if (model[field] == null) continue;
    if (typeof model[field] !== "number" || !Number.isFinite(model[field]) || model[field] < 0) fail("Invalid model subscription rate");
  }
  for (const field of ["temperature", "free", "alwaysThinks", "dynamicTools"]) {
    if (model[field] != null && typeof model[field] !== "boolean") fail("Invalid model boolean");
  }
  if (model.defaultEffort != null && model.defaultEffort !== "" && !model.efforts?.includes(model.defaultEffort))
    fail("Default model effort must be declared");
}
