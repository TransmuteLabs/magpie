import type { Call, Manifest } from "./index.js";

export interface LedgerObservationDescriptor {
  timeoutMs?: number;
}

export interface LedgerObservation {
  time: number;
  operation?: string;
  agent: string;
  provider: string;
  model: string;
  requested?: string;
  served?: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cacheWrite1h: number;
  cacheUnknown: boolean;
  reasoning: number;
  millis: number;
  ttft: number;
  status: number;
  errorType?: string;
  kind?: string;
  rejected: boolean;
  handedOn: boolean;
  tries: number;
  credits?: number;
  costUSD?: number;
  /** An empty currency preserves an unspecified unit; it does not imply USD. */
  reportedCost?: { amount: number; currency: string };
}

export function validateLedgerObserver(manifest: Manifest): void;
export function validateLedgerObservation(value: unknown): LedgerObservation;
export function validateLedgerCall(call: Call): void;
export function validateLedgerResult(value: unknown): Record<string, never>;
