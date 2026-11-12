import { test } from "node:test";
import { bunRuntime, conformance } from "./conformance.js";

test("real Node child conforms across all nine capabilities", { timeout: 20000 }, async () => {
  await conformance(process.execPath);
});

const bun = bunRuntime();
test("real Bun child conforms across all nine capabilities", { timeout: 20000, skip: !bun && "Bun is not installed" }, async () => {
  await conformance(bun);
});
