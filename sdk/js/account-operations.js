const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const validID = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);

export function validateAccountOperations(manifest, fail) {
  const operations = manifest.accountOperations ?? [];
  const has = capability => manifest.contract.capabilities.includes(capability);
  if (!Array.isArray(operations) || operations.length > 32
    || has("native-account-operations") && operations.length === 0) {
    fail("Native account operations need finite declarations");
  }
  if (!operations.length) return;
  if (!has("native-account-operations") || manifest.contract.version.minor < 6
    || !["inbound-codec", "outbound-executor", "account-auth", "agent-wiring", "module-rpc"].every(has)) {
    fail("Native account operations require their SDK 1.6 capabilities");
  }
  const seen = new Set();
  for (const operation of operations) {
    const action = manifest.actions?.find(value => value.id === operation?.action);
    const agent = manifest.agents?.find(value => value.id === operation?.agent);
    const provider = manifest.providers?.find(value => value.id === operation?.provider);
    if (!object(operation) || !validID(operation.id) || seen.has(operation.id) || !agent || !provider
      || !action || action.component && action.component !== "primary"
      || !["read", "write"].includes(action.effect) || !agent.launch?.model) {
      fail("Invalid native account operation declaration");
    }
    seen.add(operation.id);
  }
}
