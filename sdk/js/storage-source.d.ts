import type { Manifest, Scope, SessionSourceStore, SessionSourceSummary, SessionSourcePart } from "./index.js";

export interface StorageFamily { id: string; primary?: string; sidecars?: string[]; trees?: string[] }
export interface StorageSourceDescriptor {
  id: string; agent: string; codec: string; roots: SessionSourceStore[]; families: StorageFamily[];
  transcript: boolean; resume: "none" | "native";
}
export interface StorageLimits {
  maxEntries: number; maxReadBytes: number; maxParts: number; maxTextBytes: number; maxMediaBytes: number;
}
export interface StorageReadLease {
  sourceId: string; rootRef: string; snapshotRef: string; familyId: string; limits: StorageLimits;
}
export interface StorageSessionLocator { rootRef: string; sessionId: string; key: string }
export interface StorageRequest {
  lease: StorageReadLease; cursor?: string; limit?: number; locator?: StorageSessionLocator; mode?: "summary" | "transcript";
}
export interface StorageEntry { locator: StorageSessionLocator; summary: SessionSourceSummary }
export interface StorageMedia { id: string; name?: string; mime: string; bytes: string }
export interface StorageResult {
  snapshotRef: string; entries?: StorageEntry[]; nextCursor?: string; sessionId?: string;
  mode?: "summary" | "transcript"; summary?: SessionSourceSummary; parts?: SessionSourcePart[];
  media?: StorageMedia[]; cut: boolean;
}
export interface StorageFilesRequest { lease: StorageReadLease; directory: string; cursor?: string; limit: number }
export interface StorageFileEntry { fileRef: string; relativeName: string; kind: "file" | "directory"; size: number }
export interface StorageDirectoryPage { snapshotRef: string; entries: StorageFileEntry[]; nextCursor?: string; missing?: boolean }
export interface StorageBytesRequest { lease: StorageReadLease; fileRef: string; offset: number; length: number }
export interface StorageReadChunk { snapshotRef: string; fileRef: string; offset: number; bytes: string; eof: boolean }
export const STORAGE_METHODS: readonly ["session.storage.list", "session.storage.read"];
export const STORAGE_HOST_METHODS: readonly ["storage.native", "storage.list", "storage.read", "storage.check"];
export function storageFamilyAllows(family: StorageFamily, name: string): boolean;
export function validateStorageSources(manifest: Manifest): void;
export function validateStorageRequest(manifest: Manifest, method: string, scope: Scope, auth: unknown, request: StorageRequest): StorageRequest;
export function validateStorageResult(request: StorageRequest, result: StorageResult, stringify: (value: unknown) => string): StorageResult;
