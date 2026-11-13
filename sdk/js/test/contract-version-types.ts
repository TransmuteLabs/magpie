import { type Contract, type Manifest } from "../index.js";

for (const minor of [0, 1, 2, 3, 4, 5, 6] as const) {
  const contract: Contract = { version: { major: 1, minor }, capabilities: ["module-rpc"] };
  const manifest: Manifest = {
    schema: 1, id: "typed-contract", version: "1.0.0", runtime: "bun", entry: "worker.js", contract,
  };
  void manifest;
}

type AssertFalse<Value extends false> = Value;
type UnsupportedMinorRejected = AssertFalse<7 extends Contract["version"]["minor"] ? true : false>;
type UnsupportedMajorRejected = AssertFalse<2 extends Contract["version"]["major"] ? true : false>;
const sourcePreview: Contract = { version: { major: 1, minor: 6 }, capabilities: ["session-source"] };
const legacyContracts: Contract[] = [
  { version: { major: 1, minor: 0 }, capabilities: ["session-migration"] },
  { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 2 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 3 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 4 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 5 }, capabilities: ["module-rpc"] },
];
const future: Contract = {
  // @ts-expect-error SDK API 1.7 is not negotiated by Source 1.6-preview.
  version: { major: 1, minor: 7 },
  capabilities: ["session-source"],
};
void sourcePreview;
void legacyContracts;
void future;
