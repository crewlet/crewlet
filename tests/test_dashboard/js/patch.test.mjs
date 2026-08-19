// The patcher's contract: a re-render must touch only what changed.
//
// Every one of these is a property the dashboard depends on. If keyed
// rows stop being reused, a streaming turn rebuilds its row several
// times a second and the page strobes again — which is the bug this
// module exists to fix.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

const { root } = installDom();
const { patch } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/patch.js", import.meta.url)
);

const ROWS = `<div data-k="a" class="row">alpha</div><div data-k="b" class="row">beta</div>`;

test("an unchanged row keeps its node", () => {
  patch(root, ROWS);
  const a = root.querySelector('[data-k="a"]');
  const b = root.querySelector('[data-k="b"]');
  patch(root, ROWS);
  assert.strictEqual(root.querySelector('[data-k="a"]'), a);
  assert.strictEqual(root.querySelector('[data-k="b"]'), b);
});

test("changed text is written into the existing node", () => {
  patch(root, ROWS);
  const a = root.querySelector('[data-k="a"]');
  patch(
    root,
    `<div data-k="a" class="row">alpha!</div><div data-k="b" class="row">beta</div>`,
  );
  assert.strictEqual(root.querySelector('[data-k="a"]'), a, "node was replaced");
  assert.strictEqual(a.textContent, "alpha!");
});

test("a row prepended to a list reuses the rows below it", () => {
  patch(root, ROWS);
  const a = root.querySelector('[data-k="a"]');
  const b = root.querySelector('[data-k="b"]');
  patch(root, `<div data-k="z" class="row">zed</div>${ROWS}`);
  assert.strictEqual(root.children.length, 3);
  assert.strictEqual(root.children[0].getAttribute("data-k"), "z");
  assert.strictEqual(root.querySelector('[data-k="a"]'), a);
  assert.strictEqual(root.querySelector('[data-k="b"]'), b);
});

test("only a genuinely new row is marked as entering", () => {
  patch(root, ROWS);
  // Clear the markers the initial insert set, as the timer would.
  for (const child of root.children) child.classList.remove("is-entering");
  patch(root, `<div data-k="z" class="row">zed</div>${ROWS}`);
  assert.ok(root.querySelector('[data-k="z"]').classList.contains("is-entering"));
  assert.ok(!root.querySelector('[data-k="a"]').classList.contains("is-entering"));
});

test("a dropped row is the only one removed", () => {
  patch(root, ROWS);
  const b = root.querySelector('[data-k="b"]');
  patch(root, `<div data-k="b" class="row">beta</div>`);
  assert.strictEqual(root.children.length, 1);
  assert.strictEqual(root.querySelector('[data-k="b"]'), b);
  assert.strictEqual(root.querySelector('[data-k="a"]'), null);
});

test("attributes are added, changed, and removed in place", () => {
  patch(root, `<div data-k="z" class="row">zed</div>`);
  const z = root.querySelector('[data-k="z"]');
  patch(root, `<div data-k="z" class="row open" title="hello">zed</div>`);
  assert.strictEqual(root.querySelector('[data-k="z"]'), z);
  assert.strictEqual(z.getAttribute("title"), "hello");
  assert.ok(z.classList.contains("open"));
  patch(root, `<div data-k="z" class="row">zed</div>`);
  assert.strictEqual(z.getAttribute("title"), null);
  assert.ok(!z.classList.contains("open"));
});

test("nested children patch without replacing their parent", () => {
  patch(
    root,
    `<div data-k="p" class="panel"><span class="a">one</span><span class="b">two</span></div>`,
  );
  const inner = root.querySelector(".b");
  patch(
    root,
    `<div data-k="p" class="panel"><span class="a">one</span><span class="b">TWO</span></div>`,
  );
  assert.strictEqual(root.querySelector(".b"), inner);
  assert.strictEqual(inner.textContent, "TWO");
});

test("a streaming row is never rebuilt", () => {
  const row = (text) =>
    `<div data-k="live" class="live-row"><div class="live-text">${text}</div></div>`;
  patch(root, row("a"));
  const liveRow = root.querySelector('[data-k="live"]');
  const liveText = root.querySelector(".live-text");
  for (const text of ["ab", "abc", "abcd", "abcde"]) patch(root, row(text));
  assert.strictEqual(root.querySelector('[data-k="live"]'), liveRow);
  assert.strictEqual(root.querySelector(".live-text"), liveText);
  assert.strictEqual(liveText.textContent, "abcde");
});

test("a node of a different element type is replaced", () => {
  patch(root, `<div data-k="x" class="row">x</div>`);
  patch(root, `<span data-k="x" class="row">x</span>`);
  assert.strictEqual(root.querySelector('[data-k="x"]').tagName, "SPAN");
});

test("reordering moves nodes instead of recreating them", () => {
  patch(root, `<i data-k="1">1</i><i data-k="2">2</i><i data-k="3">3</i>`);
  const [n1, n2, n3] = ["1", "2", "3"].map((k) =>
    root.querySelector(`[data-k="${k}"]`),
  );
  patch(root, `<i data-k="3">3</i><i data-k="1">1</i><i data-k="2">2</i>`);
  assert.strictEqual(root.children[0], n3);
  assert.strictEqual(root.children[1], n1);
  assert.strictEqual(root.children[2], n2);
});

test("repeated parses do not reuse a stale template", () => {
  // A single <template> reused across calls caches its parsed content in
  // some DOM implementations, which silently turns every patch after the
  // first into a no-op. This is that regression, pinned.
  patch(root, `<div data-k="one">one</div>`);
  patch(root, `<div data-k="two">two</div>`);
  assert.strictEqual(root.children.length, 1);
  assert.strictEqual(root.children[0].getAttribute("data-k"), "two");
});

test("duplicate keys do not leak a node per render", () => {
  // Views should not emit a duplicate key, but a key derived from data
  // can collide — two policies with the same text, two seats sharing a
  // role name. Removing only the *indexed* leftovers left the second
  // sibling in place AND inserted a replacement for it, growing the DOM
  // by one node on every render, forever.
  const markup = `<i data-k="dup">a</i><i data-k="dup">b</i>`;
  patch(root, markup);
  for (let i = 0; i < 5; i++) patch(root, markup);
  assert.strictEqual(root.children.length, 2);
});

test("the entrance marker survives an update with no class attribute", () => {
  patch(root, `<i data-k="x" class="row">x</i>`);
  const node = root.querySelector('[data-k="x"]');
  node.classList.add("is-entering");
  patch(root, `<i data-k="x">x</i>`);
  assert.ok(
    node.classList.contains("is-entering"),
    "the marker was stripped mid-animation",
  );
});

run();
