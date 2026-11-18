import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { validReadyURL } from "../composite.js";

const cases = JSON.parse(await readFile(new URL("../../../pkg/extension/testdata/ready-urls.json", import.meta.url), "utf8"));
for (const value of cases) {
  test(`component readiness URL: ${value.name}`, () => {
    assert.equal(validReadyURL(value.url), value.valid);
  });
}
