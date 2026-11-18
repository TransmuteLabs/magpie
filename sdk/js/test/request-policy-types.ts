import { decideRequest, requestWithPolicy, refusedExecution, type Stream, type RequestRecoveryRequest,
  type RequestRecoveryResult, type RequestPolicyHost } from "../index.js";
declare const stream: Stream;
declare const signal: AbortSignal;
const refusal: RequestRecoveryRequest = {
  step:"asset",facts:{status:429,code:"pace",operation:"tool.call",phase:"response",complete:true},
  evidence:"",rules:[{id:"pace",status:[429],code:"pace",class:"transient-rate",scope:"account"}],
};
void decideRequest(signal,stream,refusal);
void requestWithPolicy(signal,stream,"asset",new Request("https://fixture.invalid/asset"),fetch,()=>refusal);

const finalRefusal = refusedExecution(refusal, [{id:"free", models:["free"], retryAfterMs:42000}]);
void finalRefusal.refusal?.windows?.[0].models;

const permit: RequestRecoveryResult = {
  step: "asset", action: "wait", retry: true, reason: "retry", class: "transient-rate",
  scope: "account", rule: "pace", attempt: 0, totalRetries: 1, providerRetries: 1,
};
const host: RequestPolicyHost = { async hostCall(_method, request) {
  return { ...permit, step: request.step };
} };
void requestWithPolicy(signal, host, "asset", new Request("https://fixture.invalid/asset"), fetch,
  async response => { await response.clone().arrayBuffer(); return refusal; });
const invalidAction: RequestRecoveryResult = { ...permit,
  // @ts-expect-error The callback uses wait/next-account/terminal, never a module-owned retry action.
  action: "retry",
};
void invalidAction;
