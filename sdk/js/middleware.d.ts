import type { GatewayContext, JSONValue } from "./index.js";

export interface GatewayMiddlewareDescriptor {
  request?: boolean; account?: boolean; provider?: boolean; event?: boolean; response?: boolean;
  events?: string[]; origins?: ("local" | "magpie-key" | "gateway-key" | "remote" | "browser")[];
}
export interface MiddlewareProvider { id: string; name: string; account?: string; model: string }
export interface MiddlewareAccount { ref: string; provider: string; name: string; kind: string; model: string }
export interface MiddlewareContext extends GatewayContext {
  provider?: MiddlewareProvider; account?: MiddlewareAccount; event?: string; status?: number;
}
export interface MiddlewareCall { body?: Record<string, JSONValue>; state?: JSONValue; context: MiddlewareContext }
export interface MiddlewareRejection { status: number; message: string }
export interface MiddlewareResult {
  body?: Record<string, JSONValue>; state?: JSONValue; reject?: MiddlewareRejection; skip?: boolean; drop?: boolean;
}
export const MAX_MIDDLEWARE_BODY_BYTES: number;
export const MIDDLEWARE_METHODS: readonly string[];
export function validateMiddlewareResult(method: string, result: unknown, serialize?: (value: unknown) => string): MiddlewareResult;
