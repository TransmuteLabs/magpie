import assert from "node:assert/strict";
import { test } from "node:test";
import { createAuthLinkPresentation } from "../ui.js";

function fixture() {
  const calls = [];
  const container = { hidden: false };
  const input = { value: "stale", readOnly: false,
    focus(options) { calls.push(["focus", options]); },
    select() { calls.push(["select", this.value]); },
  };
  const select = new EventTarget();
  const presenter = createAuthLinkPresentation({ container, input, select });
  return { calls, container, input, select, presenter };
}

test("auth address can be selected without a bridge, clipboard or navigation API", () => {
  const { calls, container, input, select, presenter } = fixture();
  assert.equal(input.readOnly, true);
  assert.equal(input.value, "");
  assert.equal(container.hidden, true);
  select.dispatchEvent(new Event("click"));
  assert.deepEqual(calls, []);
  for (const state of ["waiting", "pending"]) {
    presenter.update({ flow: "opaque", state, url: "https://approval.example/authorize?state=fixture" });
    assert.equal(container.hidden, false);
    select.dispatchEvent(new Event("click"));
    assert.deepEqual(calls.splice(0), [["focus", { preventScroll: true }], ["select", input.value]]);
  }
});

test("every terminal, absent and invalid address clears a previous active URL", () => {
  const { container, input, presenter } = fixture();
  const active = { flow: "opaque", state: "waiting", url: "https://approval.example/authorize" };
  const invalid = [
    null,
    ...["complete", "canceled", "expired", "uncertain", "other"].map(state => ({ ...active, state })),
    ...[undefined, "", 3, "javascript:alert(1)", "data:text/html,unsafe", "http://approval.example/",
      "https://user@approval.example/", "https://user:pass@approval.example/",
      "//approval.example/", "/approve", "not a URL", "https://approval.example/\nunsafe"].map(url => ({ ...active, url })),
  ];
  for (const flow of invalid) {
    presenter.update(active);
    assert.equal(container.hidden, false);
    presenter.update(flow);
    assert.equal(input.value, "", JSON.stringify(flow));
    assert.equal(container.hidden, true, JSON.stringify(flow));
  }
});

test("disposed presentation cannot restore a late flow or select its address", () => {
  const { calls, container, input, select, presenter } = fixture();
  const flow = { flow: "opaque", state: "pending", url: "https://approval.example/" };
  presenter.update(flow);
  presenter.dispose();
  presenter.dispose();
  presenter.update(flow);
  select.dispatchEvent(new Event("click"));
  assert.equal(container.hidden, true);
  assert.equal(input.value, "");
  assert.deepEqual(calls, []);
});
