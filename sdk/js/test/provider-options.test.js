import assert from "node:assert/strict";
import test from "node:test";
import { providerOptionOn, publicManifest, validateManifest, validateProviderOptionValues } from "../index.js";

const fixture = (minor = 9) => ({
  schema: 1, id: "fixture.options", version: "1.0.0", runtime: "exec", entry: "worker",
  contract: { version: { major: 1, minor }, capabilities: ["outbound-executor"] },
  providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"], options: [
    { key: "keepAlive", label: "Paid <mode>", confirm: "Uses account credits", placement: "top",
      labels: { de: "Bezahlt" }, default: true },
  ] }],
});

test("SDK provider switches preserve declarations, explicit false and other configuration", () => {
  const manifest = validateManifest(fixture()), option = manifest.providers[0].options[0];
  assert.equal(publicManifest(manifest).providers[0].options[0].confirm, "Uses account credits");
  assert.equal(providerOptionOn(option, {}), true);
  assert.equal(providerOptionOn(option, { keepAlive: false }), false);
  assert.equal(providerOptionOn(option, { keepAlive: true }), true);
  assert.doesNotThrow(() => validateProviderOptionValues(manifest, { keepAlive: false, unrelated: { keep: 1 } }));
  for (const value of [null, "true", 1, []])
    assert.throws(() => validateProviderOptionValues(manifest, { keepAlive: value }));
});

test("SDK provider switches require their floor and reject ambiguous root keys", () => {
  assert.throws(() => validateManifest(fixture(8)));
  for (const mutate of [
    option => { option.key = "constructor"; },
    option => { option.key = "__proto__"; },
    option => { option.label = ""; },
    option => { option.placement = "outside"; },
    option => { option.default = "true"; },
    option => { option.confirm = "a".repeat(4097); },
    option => { option.labels = { de: null }; },
  ]) {
    const manifest = fixture();
    mutate(manifest.providers[0].options[0]);
    assert.throws(() => validateManifest(manifest));
  }
  const duplicate = fixture();
  duplicate.providers[0].options.push({ ...duplicate.providers[0].options[0] });
  assert.throws(() => validateManifest(duplicate));
  const shared = fixture();
  shared.providers.push({ ...structuredClone(shared.providers[0]), id: "other" });
  assert.doesNotThrow(() => validateManifest(shared));
  shared.providers[1].options[0].default = false;
  assert.throws(() => validateManifest(shared));
});

test("SDK provider switches reject missing and nonstring configuration keys", () => {
  const missing = fixture();
  delete missing.providers[0].options[0].key;
  assert.throws(() => validateManifest(missing));
  for (const key of [undefined, null, true, false, 1, [], {}, { toString: () => "keepAlive" }]) {
    const manifest = fixture();
    manifest.providers[0].options[0].key = key;
    assert.throws(() => validateManifest(manifest));
  }
});

test("SDK provider switches reject unknown declaration fields", () => {
  for (const field of ["unknown", "defualt", "filtersModel"]) {
    const manifest = fixture();
    manifest.providers[0].options[0][field] = true;
    assert.throws(() => validateManifest(manifest));
  }
});

test("SDK provider switch translations retain canonical JSON omission", () => {
  const manifest = fixture();
  manifest.providers[0].options[0].labels = { de: undefined, ja: "有料" };
  assert.deepEqual(validateManifest(manifest).providers[0].options[0].labels, { ja: "有料" });
});
