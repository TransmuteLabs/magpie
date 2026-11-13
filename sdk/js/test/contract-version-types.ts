import { type Contract, type Manifest } from "../index.js";

for (const minor of [0, 1, 2, 3, 4, 5] as const) {
  const contract: Contract = { version: { major: 1, minor }, capabilities: ["module-rpc"] };
  const manifest: Manifest = {
    schema: 1, id: "typed-contract", version: "1.0.0", runtime: "bun", entry: "worker.js", contract,
  };
  void manifest;
}

type AssertFalse<Value extends false> = Value;
type UnsupportedMinorRejected = AssertFalse<6 extends Contract["version"]["minor"] ? true : false>;
type UnsupportedMajorRejected = AssertFalse<2 extends Contract["version"]["major"] ? true : false>;
