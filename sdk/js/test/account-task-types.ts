import { scheduleAccountTask, cancelAccountTask, reportAccountTaskUsage, validateAccountTaskRequest,
  type Manifest, type Stream, type AccountTaskResult, type ComponentIdentity } from "../index.js";

declare const stream: Stream;
const manifest: Manifest = {
  schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 9 }, capabilities: ["outbound-executor", "account-auth", "module-rpc", "usage-reporter"] },
  providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"], accountTasks: [{ id: "ping", action: "paid.ping" }] }],
};
const scheduled: Promise<{ operationId: string }> = scheduleAccountTask(stream, { task: "ping", key: "conversation", job: "opaque-template", dueAt: 1794656400000 });
const canceled: Promise<{ canceled: boolean }> = cancelAccountTask(stream, { task: "ping", key: "conversation" });
const request = validateAccountTaskRequest({ task: "ping", key: "conversation", job: "opaque-template", dueAt: 1794656400000, model: "m1" });
const result: AccountTaskResult = { outcome: "completed", call: {
  kind: "cache_keepalive", model: request.model, usage: { inputTokens: 1 },
} };
const snapshot: Promise<void> = reportAccountTaskUsage(stream, {
  kind: "cache_keepalive", model: request.model, usage: { facts: { inputTokens: 7, cachedTokens: 5 } },
});
const taskService: ComponentIdentity = { id: "paid", contract: { id: "paid", version: 1 },
  accountTaskActions: ["paid.ping"] };
void [manifest, scheduled, canceled, result, snapshot, taskService];
