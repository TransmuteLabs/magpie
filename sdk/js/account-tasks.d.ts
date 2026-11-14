import type { Manifest, OwnCall, Stream } from "./index.js";

export interface AccountTaskDescriptor { id: string; action: string }
export interface AccountTaskScheduleRequest { task: string; key: string; job: string; dueAt: number }
export interface AccountTaskScheduleResult { operationId: string }
export interface AccountTaskCancelRequest { task: string; key: string }
export interface AccountTaskCancelResult { canceled: boolean }
export interface AccountTaskRequest { task: string; key: string; job: string; dueAt: number; model: string; proxy?: string }
export interface AccountTaskResult { outcome: "completed" | "not-started" | "uncertain"; call?: OwnCall }
export function validateAccountTasks(manifest: Manifest): void;
export function validateAccountTaskSchedule(value: unknown): AccountTaskScheduleRequest;
export function validateAccountTaskCancel(value: unknown): AccountTaskCancelRequest;
export function validateAccountTaskRequest(value: unknown): AccountTaskRequest;
export function validateAccountTaskResult(manifest: Manifest, value: unknown): AccountTaskResult;
export function scheduleAccountTask(stream: Stream, request: AccountTaskScheduleRequest): Promise<AccountTaskScheduleResult>;
export function cancelAccountTask(stream: Stream, request: AccountTaskCancelRequest): Promise<AccountTaskCancelResult>;
export function reportAccountTaskUsage(stream: Stream, call: OwnCall): Promise<void>;
