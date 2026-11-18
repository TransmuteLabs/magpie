import {
  validateBudgetPolicyCall, validateBudgetPolicyResult,
  type BudgetPolicyRequest, type BudgetPolicyResult, type Call, type Manifest, type Method,
} from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "budget", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 13 }, capabilities: ["budget-policy"] },
  budgetPolicies: [{ id: "extra" }],
};
const method = "budget.spent" satisfies Method;
function parse(call: Call): BudgetPolicyResult {
  const request: BudgetPolicyRequest = validateBudgetPolicyCall(manifest, call);
  return validateBudgetPolicyResult(method, request, { restrictions: [] });
}
void parse;
