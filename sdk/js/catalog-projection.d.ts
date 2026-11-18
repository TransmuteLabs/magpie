import type { Manifest, Scope } from "./index.js";

export interface CatalogAccessDescriptor { scope: "selected" | "inventory" }
export interface CatalogReadRequest { [key: string]: never }
export interface CatalogModelProjection { id: string; name?: string; contextWindow?: number; maxOutput?: number }
export interface CatalogWindowProjection {
  name: string; models: string[]; used?: number | null; resetsAt?: number; aside?: boolean; unlimited?: boolean; credits?: boolean;
}
export interface CatalogQuotaProjection { known: boolean; asOf?: number; windows?: CatalogWindowProjection[] }
export interface CatalogAccountProjection {
  ref: string; provider: string; kind: "account" | "key" | "public"; models: CatalogModelProjection[]; quota: CatalogQuotaProjection;
}
export interface CatalogReadResult { readAt: number; accounts: CatalogAccountProjection[] }
export function validateCatalogAccess(manifest: Manifest): void;
export function validateCatalogRead(manifest: Manifest, scope: Scope, request: CatalogReadRequest): void;
export function validateCatalogProjection(value: CatalogReadResult): void;
