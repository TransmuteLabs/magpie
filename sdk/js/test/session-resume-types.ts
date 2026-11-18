import type { Manifest, SessionResumeRecipe, SessionSourceDescriptor } from "../index.js";
import { validateSessionResume } from "../index.js";
import type { StorageSourceDescriptor } from "../storage-source.js";

const recipe: SessionResumeRecipe = { agent: "vendor-sdk", args: ["--resume", "{sessionId}"] };
declare const manifest: Manifest;
validateSessionResume(manifest, "vendor", "agent", recipe);
const jsonl: SessionSourceDescriptor = {
  id: "history", agent: "vendor", codec: "vendor-jsonl", container: "jsonl",
  stores: [{ slot: "home", homeRelativePath: ".vendor" }],
  layout: { kind: "flat", idFrom: "filename-stem" }, transcript: true, resume: "agent", agentResume: recipe,
};
const storage: StorageSourceDescriptor = {
  id: "history", agent: "vendor", codec: "vendor-sqlite",
  roots: [{ slot: "home", homeRelativePath: ".vendor" }],
  families: [{ id: "history", primary: "history.sqlite" }], transcript: true, resume: "agent", agentResume: recipe,
};
void jsonl;
void storage;
