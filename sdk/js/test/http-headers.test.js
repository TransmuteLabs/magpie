import assert from "node:assert/strict";
import { test } from "node:test";
import * as SDK from "../index.js";

test("module HTTP proof headers are fresh plain maps without opaque state, config values or auth", () => {
  assert.equal(typeof SDK.moduleHTTPHeaders, "function");
  const context = {
    instance: "instance-7",
    component: "backend",
    config: {
      version: "18446744073709551615",
      get values() { throw new Error("config values must not be inspected"); },
    },
    get state() { throw new Error("opaque state must not be inspected"); },
    get auth() { throw new Error("auth must not be inspected"); },
  };
  const expected = {
    "X-Magpie-Component-Instance": "instance-7",
    "X-Magpie-Component": "backend",
    "X-Magpie-Component-Config": "18446744073709551615",
  };
  const first = SDK.moduleHTTPHeaders(context);
  assert.equal(Object.getPrototypeOf(first), Object.prototype);
  assert.deepEqual(first, expected);
  first["X-Magpie-Component"] = "changed";
  first.Authorization = "must not persist";
  delete first["X-Magpie-Component-Instance"];
  const second = SDK.moduleHTTPHeaders(context);
  assert.notEqual(second, first);
  assert.deepEqual(second, expected);
  assert.equal(context.component, "backend");
});
