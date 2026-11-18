import type { Call, Manifest } from "./index.js";

export interface RequestObservationDescriptor {
  timeoutMs?: number;
}

// Time is Unix milliseconds. An attempt carries its own start time and
// duration. The terminal event carries the request's start time and duration.
export interface RequestObservation {
  phase: "start" | "attempt" | "end";
  requestId: string;
  time: number;
  agent?: string;
  kind?: string;
  attempt?: number;
  provider?: string;
  model?: string;
  requested?: string;
  status?: number;
  millis?: number;
}

export function validateRequestObserver(manifest: Manifest): void;
export function validateRequestObservation(event: unknown): RequestObservation;
export function validateRequestObservationCall(call: Call): void;
export function validateRequestObservationResult(result: unknown): Record<string, never>;
