import type { Manifest, Scope, Stream, Usage } from "./index.js";

export interface OwnCall {
  kind: string;
  model: string;
  status?: number;
  millis?: number;
  error?: string;
  usage?: Usage;
}
export interface OwnCallRequest { call: OwnCall }
export interface OwnCallResult { accepted: boolean }
export interface OutsideCall extends Omit<OwnCall, "kind"> { kind?: string; id: string; time: number; session?: string }
export interface OutsideCallsRequest { since: number; until: number; cursor?: string }
export interface OutsideCallsResult { calls: OutsideCall[]; nextCursor?: string }
export const MAX_OUTSIDE_CALLS: 500;
export const MAX_OUTSIDE_CALL_PAGES: 32;
export function validateOwnCall(manifest: Manifest, request: unknown): OwnCallRequest;
export function validateOutsideCallsRequest(manifest: Manifest, scope: Scope, request: unknown): OutsideCallsRequest;
export function validateOutsideCallsResult(manifest: Manifest, request: OutsideCallsRequest, result: unknown): OutsideCallsResult;
export function appendOwnCall(stream: Stream, call: OwnCall): Promise<OwnCallResult>;
