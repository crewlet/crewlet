// The router, and specifically the Back button.
//
// The router had no test at all, and what that cost was not a parsing
// bug — parsing was fine. It was the session stack, which no amount of
// reading a URL will show you. Three defects shipped together and all
// three had the same shape: a navigation decided to push or to replace,
// nobody wrote down which, and the consequence only appears when
// somebody presses Back.
//
// Measured in a browser before the fix:
//
//   - from a moved route (`#/events`), Back could not escape AT ALL. The
//     redirect pushed, so Back landed on the dead URL, which redirected
//     forward, for ever.
//   - after three lens clicks in one room, Back left the room entirely
//     and landed wherever the reader had come in from.
//   - Back to a list the reader had scrolled halfway down landed at the
//     top, because the shell reset the scroll on every mount.
//
// So this suite asserts stacks and positions, not URLs.

import assert from "node:assert";
import { installDom, installHistory } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();
const session = installHistory("#/mission");
const base = JS_URL;
const {
  parseRoute,
  navigate,
  pushParams,
  replaceParams,
  takeRoute,
  sameScreen,
} = await import(new URL("router.js", base));

/** Start each test from a known one-entry stack. */
function reset(hash = "#/mission") {
  const fresh = installHistory(hash);
  Object.assign(session, fresh);
  return fresh;
}

// ---------------------------------------------------------------------
// Reading the URL
// ---------------------------------------------------------------------

test("the empty hash is Mission Control, not a 404", () => {
  reset("#/");
  assert.strictEqual(parseRoute().name, "mission");
});

test("an unknown room falls back rather than rendering nothing", () => {
  reset("#/does-not-exist");
  assert.strictEqual(parseRoute().name, "mission");
});

test("a seat is addressed by handle, and its tab rides in the query", () => {
  reset("#/seats/eng?tab=cost");
  const route = parseRoute();
  assert.strictEqual(route.name, "seat");
  assert.strictEqual(route.params.key, "eng");
  assert.strictEqual(route.params.tab, "cost");
});

test("a moved route reports a redirect and keeps its query string", () => {
  // The filtered form is the one that gets shared, so losing the query
  // on the way through the redirect would land the reader on an
  // unfiltered screen and look like the filter never worked.
  reset("#/events?actor=CEO");
  assert.strictEqual(parseRoute().redirect, "/activity?actor=CEO");
});

// ---------------------------------------------------------------------
// What ends up in the stack
// ---------------------------------------------------------------------

test("a redirect REPLACES, so Back is not a trap", () => {
  const s = reset("#/mission");
  s.location.hash = "#/events"; // the reader follows an old link
  assert.deepStrictEqual(s.urls(), ["#/mission", "#/events"]);

  const route = parseRoute();
  assert.ok(route.redirect, "expected #/events to redirect");
  navigate(route.redirect, { replace: true });

  // The dead URL is gone from the stack rather than sitting behind the
  // live one. Pushing here is what made Back land on #/events, redirect
  // forward, and arrive back where it started — for ever.
  assert.deepStrictEqual(s.urls(), ["#/mission", "#/activity"]);

  s.history.back();
  assert.strictEqual(s.location.hash, "#/mission");
});

test("a section PUSHES, so Back walks out one at a time", () => {
  const s = reset("#/activity");
  s.location.hash = "#/org";
  pushParams("org", { lens: "directory" });
  pushParams("org", { lens: "charter" });

  s.history.back();
  assert.strictEqual(s.location.hash, "#/org?lens=directory");
  s.history.back();
  assert.strictEqual(s.location.hash, "#/org");
  s.history.back();
  assert.strictEqual(s.location.hash, "#/activity", "Back never left the room");
});

test("a filter REPLACES, so Back leaves the screen", () => {
  // Four ticked pills are one screen, not four. A reader who narrows a
  // list and presses Back means "take me off this list".
  const s = reset("#/mission");
  s.location.hash = "#/activity";
  replaceParams("activity", { cat: "task" });
  replaceParams("activity", { cat: "task,system" });
  replaceParams("activity", { cat: "task,system", failed: "1" });

  assert.strictEqual(s.urls().length, 2, `stack grew: ${s.urls()}`);
  s.history.back();
  assert.strictEqual(s.location.hash, "#/mission");
});

test("navigating to where you already are still fires", () => {
  // Clicking the nav entry for the room you are in should re-mount it,
  // and nothing fires on its own when the URL does not change.
  const s = reset("#/org");
  let fired = 0;
  globalThis.addEventListener("hashchange", () => (fired += 1));
  navigate("/org");
  assert.strictEqual(fired, 1);
  assert.strictEqual(s.urls().length, 1, "it should not have pushed");
});

// ---------------------------------------------------------------------
// Where the page is left
// ---------------------------------------------------------------------

test("somewhere new starts at the top; somewhere you have been resumes", () => {
  const s = reset("#/mission");

  assert.strictEqual(takeRoute(), 0, "a first visit is not a return");
  s.setScroll(420); // the reader scrolls down Mission Control

  s.location.hash = "#/org";
  assert.strictEqual(takeRoute(), 0, "a new room starts at the top");
  s.setScroll(90);

  s.history.back();
  assert.strictEqual(takeRoute(), 420, "Back did not resume the position");

  s.history.forward();
  assert.strictEqual(takeRoute(), 90, "Forward did not resume the position");
});

test("a filter change keeps the position it was filed under", () => {
  // `replaceParams` writes the entry in place. Nulling its state — which
  // is what `replaceState(null, ...)` does — throws away the key the
  // position is filed under, so ticking a pill would silently forfeit
  // the scroll the reader gets back later.
  const s = reset("#/activity");
  takeRoute();
  s.setScroll(510);

  replaceParams("activity", { cat: "task" });
  assert.ok(s.history.state && s.history.state.k, "the entry lost its key");

  s.location.hash = "#/org";
  takeRoute();
  s.history.back();
  assert.strictEqual(takeRoute(), 510);
});

test("two entries for one URL keep their own positions", () => {
  // The same room reached twice is two places the reader has been, and
  // keying by URL rather than by entry would collapse them onto one.
  const s = reset("#/org");
  takeRoute();
  s.setScroll(100);

  s.location.hash = "#/activity";
  takeRoute();
  s.setScroll(200);

  s.location.hash = "#/org";
  takeRoute();
  s.setScroll(300);

  s.history.back();
  assert.strictEqual(takeRoute(), 200);
  s.history.back();
  assert.strictEqual(takeRoute(), 100, "the first visit took the second's position");
});

// ---------------------------------------------------------------------
// Which changes the shell may absorb
// ---------------------------------------------------------------------

test("a different tab of one seat is the same screen", () => {
  const a = { name: "seat", params: { key: "eng", tab: "now" } };
  const b = { name: "seat", params: { key: "eng", tab: "cost" } };
  assert.ok(sameScreen(a, b));
});

test("a different seat is a different screen", () => {
  // If this said otherwise, moving between seats would hand the new
  // seat's params to the old seat's view and show one seat's data under
  // another's name.
  const a = { name: "seat", params: { key: "eng" } };
  const b = { name: "seat", params: { key: "analyst" } };
  assert.ok(!sameScreen(a, b));
});

test("a different room is a different screen", () => {
  assert.ok(!sameScreen({ name: "org", params: {} }, { name: "spend", params: {} }));
});

test("an LLM record's coordinates are identity, not a view of one", () => {
  // They are path segments: two records are two pages, and absorbing the
  // move would leave the first record rendered under the second's URL.
  const a = { name: "llm", params: { key: "eng", turn: "t1", phase: "plan", iter: "0" } };
  const b = { name: "llm", params: { key: "eng", turn: "t2", phase: "plan", iter: "0" } };
  assert.ok(!sameScreen(a, b));
});

test("a lens that moved rooms redirects, and replaces", () => {
  // `#/org?lens=seats` is a live PATH with a dead lens, so none of the
  // moved-ROUTE machinery sees it: the room simply falls back to its
  // default lens and lands the reader on the org chart, which is not what
  // they clicked. The seat page's own back link emitted this exact form
  // for the life of the seats lens, so it is in real history.
  const s = reset("#/mission");
  s.location.hash = "#/org?lens=seats";
  const route = parseRoute();
  assert.strictEqual(route.redirect, "/agents");

  // And a redirect REPLACES, for the same reason every other one does:
  // leaving the dead lens in the stack means Back returns to it, it
  // redirects forward again, and Back can never escape.
  navigate(route.redirect, { replace: true });
  assert.deepStrictEqual(s.urls(), ["#/mission", "#/agents"]);
  s.history.back();
  assert.strictEqual(s.location.hash, "#/mission");
});

test("a lens that still exists is not redirected", () => {
  reset("#/org?lens=directory");
  assert.deepStrictEqual(parseRoute(), {
    name: "org",
    params: { lens: "directory" },
  });
});

await run();
