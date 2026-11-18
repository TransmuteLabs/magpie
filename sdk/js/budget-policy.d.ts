import type { Call, Manifest } from "./index.js";

export interface BudgetPolicyDescriptor { id: string; timeoutMs?: number }
export interface BudgetScope { owner: "key" | "issuer"; scoped: boolean; tokens: string; costUSD: number }
export interface BudgetPolicyRequest { policy: string; time: number; bodyBytes?: number; scopes: BudgetScope[] }
export interface BudgetRestriction { scope: number; retrySeconds: number }
export interface BudgetPolicyResult { restrictions: BudgetRestriction[] }

export function isBudgetMethod(method: string): method is "budget.reserve" | "budget.spent";
export function validateBudgetPolicyCall(manifest: Manifest, call: Call): BudgetPolicyRequest;
export function validateBudgetPolicyResult(method: "budget.reserve" | "budget.spent", request: BudgetPolicyRequest, result: unknown): BudgetPolicyResult;
