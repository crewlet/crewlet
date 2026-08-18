// The marks that only ever render on genuinely live state.
//
// Two properties, and both are about not lying: the rail must say where
// a turn has been and where it is going, and a row that has stopped
// moving must stop *claiming* to move.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
const { turnRail } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/ui.js", import.meta.url)
);
const { staleness, STALE_MS, STALLED_MS } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/state.js", import.meta.url)
);

// ---- the turn rail ----

// Which step carries which state, in render order.
function steps(markup) {
  return [...markup.matchAll(/class="rail-step ([^"]*)"/g)].map((m) => m[1].trim());
}

test("the phases before the live one are spent", () => {
  assert.deepStrictEqual(steps(turnRail("review")), [
    "is-done",
    "is-done",
    "is-live",
  ]);
});

test("the phases after the live one are pending", () => {
  assert.deepStrictEqual(steps(turnRail("plan")), ["is-live", "", ""]);
});

test("exactly one step is live", () => {
  for (const phase of ["plan", "execute", "review"]) {
    const live = steps(turnRail(phase)).filter((c) => c === "is-live");
    assert.strictEqual(live.length, 1, `${phase} lit ${live.length} steps`);
  }
});

test("the packet runs the segment entering the live phase", () => {
  const markup = turnRail("execute");
  const links = [...markup.matchAll(/class="(rail-link[^"]*)"/g)].map((m) => m[1]);
  assert.deepStrictEqual(links, ["rail-link is-live", "rail-link"]);
});

test("a phase off the canonical path gets its own single-node rail", () => {
  // Onboarding runs at most once per agent, before the first Plan, and a
  // judge or a sub-agent is not on the plan→execute→review path at all.
  // Drawing any of them as "somewhere in a three-phase turn" would place
  // them on a path they are not on.
  for (const phase of ["onboarding", "judge", "subagent"]) {
    const markup = turnRail(phase);
    assert.deepStrictEqual(steps(markup), ["is-live"]);
    assert.ok(markup.includes(`>${phase}</span>`), `${phase} lost its label`);
    assert.ok(!markup.includes("rail-link"), `${phase} drew a connector to nothing`);
  }
});

test("a turn with no phase yet still draws the canonical path", () => {
  assert.deepStrictEqual(steps(turnRail("")), ["is-live", "", ""]);
});

// ---- staleness ----

test("a call that just moved is not stale", () => {
  const now = Date.parse("2026-04-01T12:00:00Z");
  assert.strictEqual(staleness(new Date(now - 5_000).toISOString(), now), "");
});

test("a call goes stale, then stalled, at the named thresholds", () => {
  const now = Date.parse("2026-04-01T12:00:00Z");
  const at = (ms) => new Date(now - ms).toISOString();
  assert.strictEqual(staleness(at(STALE_MS - 1), now), "");
  assert.strictEqual(staleness(at(STALE_MS), now), "stale");
  assert.strictEqual(staleness(at(STALLED_MS - 1), now), "stale");
  assert.strictEqual(staleness(at(STALLED_MS), now), "stalled");
});

test("a missing timestamp is not reported as stale", () => {
  // A call with no `updated_at` is a call we know nothing about, which is
  // not the same as one we know has hung — claiming the latter would put
  // an amber age on every row the moment a field went missing.
  assert.strictEqual(staleness(""), "");
  assert.strictEqual(staleness(undefined), "");
  assert.strictEqual(staleness("not a date"), "");
});

test("a naive-UTC timestamp is read as UTC", () => {
  const now = Date.parse("2026-04-01T12:00:00Z");
  const naive = new Date(now - 10_000).toISOString().replace("Z", "");
  assert.strictEqual(staleness(naive, now), "");
});

run();
