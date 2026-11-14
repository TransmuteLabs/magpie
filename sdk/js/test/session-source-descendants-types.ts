import {
  sessionSourceDescendantPath, type SessionSourceDescendants, type SessionSourceIdentity,
  type SessionSourceReadRequest, type SessionSourceSummary,
} from "../index.js";

const rule: SessionSourceDescendants = { kind: "journal-stem-jsonl", excludeDirectories: ["btw-history"] };
const admitted: boolean = sessionSourceDescendantPath(rule, "main.jsonl", "main/child.jsonl");
const identity: SessionSourceIdentity = { path: "main/child.jsonl", sessionId: "child" };
const summary: SessionSourceSummary = { title: "main", cwd: "/work", subs: [identity] };
declare const request: SessionSourceReadRequest;
const descendants: string[] | undefined = request.descendants;
void [admitted, summary, descendants];

// @ts-expect-error Descendant acquisition cannot select an arbitrary directory.
const arbitrary: SessionSourceDescendants = { kind: "directory", root: "/private" };
// @ts-expect-error A child identity is bound to its admitted relative document.
const pathless: SessionSourceIdentity = { sessionId: "child" };
void [arbitrary, pathless];
