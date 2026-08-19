// The pulse is the overview's lead panel, and it is only worth the space
// if every lit cell is a real thing that happened at a real time.
//
// These pin the bucketing: the right seat, the right minute, failures
// marked, the window's edges honoured, and a roster that lists a seat
// that has done nothing (which is itself a finding).

import assert from "node:assert";
import { test, run } from "./harness.mjs";

const { buildPulse, PULSE_BUCKETS } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/pulse.js", import.meta.url)
);

// A fixed "now" so the assertions are about bucketing, not about when
// the suite happens to run.
const NOW = Date.parse("2026-04-01T12:00:00Z");
const minutesAgo = (n) => new Date(NOW - n * 60_000).toISOString();

const SEATS = [
  { name: "CEO", kind: "agent" },
  { name: "Engineer", kind: "agent" },
  { name: "Dana", kind: "human" },
];

function pulse(events, extra = {}) {
  return buildPulse(
    { seats: SEATS, agents: [], events, tokens: null, ...extra },
    { now: NOW },
  );
}

test("an event lands in the minute it happened", () => {
  const p = pulse([{ actor: "CEO", timestamp: minutesAgo(10) }]);
  const row = p.rows.find((r) => r.name === "CEO");
  // The last cell is the current minute, so ten minutes ago is ten back.
  assert.strictEqual(row.cells[PULSE_BUCKETS - 1 - 10].n, 1);
  assert.strictEqual(row.events, 1);
});

test("the newest event lands in the last cell", () => {
  const p = pulse([{ actor: "CEO", timestamp: new Date(NOW - 1000).toISOString() }]);
  const row = p.rows.find((r) => r.name === "CEO");
  assert.strictEqual(row.cells[PULSE_BUCKETS - 1].n, 1);
});

test("an event older than the window is dropped", () => {
  const p = pulse([
    { actor: "CEO", timestamp: minutesAgo(61) },
    { actor: "CEO", timestamp: minutesAgo(240) },
  ]);
  assert.strictEqual(p.total, 0);
  assert.strictEqual(p.rows.find((r) => r.name === "CEO").events, 0);
});

test("a naive-UTC timestamp is read as UTC, not as local time", () => {
  // The API emits both forms interchangeably. Read as local time, an
  // event would land in a bucket hours away and usually outside the
  // window entirely — the panel would simply be empty for half the world.
  const naive = new Date(NOW - 5 * 60_000).toISOString().replace("Z", "");
  const p = pulse([{ actor: "CEO", timestamp: naive }]);
  assert.strictEqual(p.rows.find((r) => r.name === "CEO").events, 1);
});

test("a failed event marks its cell and its row", () => {
  const p = pulse([
    { actor: "CEO", timestamp: minutesAgo(3) },
    { actor: "CEO", timestamp: minutesAgo(3), failed: true },
  ]);
  const row = p.rows.find((r) => r.name === "CEO");
  const cell = row.cells[PULSE_BUCKETS - 1 - 3];
  assert.strictEqual(cell.n, 2);
  assert.ok(cell.failed);
  assert.strictEqual(row.failures, 1);
  assert.strictEqual(p.failures, 1);
});

test("an event from an unknown actor is ignored, not invented", () => {
  const p = pulse([{ actor: "Nobody", timestamp: minutesAgo(2) }]);
  assert.strictEqual(p.total, 0);
  assert.strictEqual(p.rows.length, 2);
});

test("human seats are not on the grid", () => {
  const p = pulse([]);
  assert.deepStrictEqual(
    p.rows.map((r) => r.name),
    ["CEO", "Engineer"],
  );
});

test("a seat that did nothing still gets a row", () => {
  const p = pulse([{ actor: "CEO", timestamp: minutesAgo(1) }]);
  const row = p.rows.find((r) => r.name === "Engineer");
  assert.ok(row, "a silent seat vanished from the roster");
  assert.strictEqual(row.events, 0);
  assert.ok(row.cells.every((c) => c.n === 0));
});

test("max is the busiest single cell, so quiet seats stay readable", () => {
  const p = pulse([
    { actor: "CEO", timestamp: minutesAgo(4) },
    { actor: "CEO", timestamp: minutesAgo(4) },
    { actor: "CEO", timestamp: minutesAgo(4) },
    { actor: "Engineer", timestamp: minutesAgo(9) },
  ]);
  assert.strictEqual(p.max, 3);
  assert.strictEqual(p.total, 4);
});

test("max never falls below 1, so an empty grid cannot divide by zero", () => {
  assert.strictEqual(pulse([]).max, 1);
});

test("spend and live state come off the projection, not the feed", () => {
  const p = buildPulse(
    {
      seats: SEATS,
      agents: [{ id: "a1", role: "CEO", state: "working", current_phase: "plan" }],
      events: [],
      tokens: { by_agent: [{ role: "CEO", total_tokens: 4200, calls: 7 }] },
    },
    { now: NOW },
  );
  const row = p.rows.find((r) => r.name === "CEO");
  assert.strictEqual(row.tokens, 4200);
  assert.strictEqual(row.calls, 7);
  assert.strictEqual(row.agentId, "a1");
  assert.strictEqual(row.phase, "plan");
  assert.strictEqual(p.working, 1);
});

test("a full feed marks what it cannot speak for as unknown, not idle", () => {
  // The projection retains a bounded feed and a busy org fills it in
  // minutes. Drawing the gap as idle makes a company that has been flat
  // out all hour look like one that woke up five minutes ago.
  const events = Array.from({ length: 400 }, (_, i) => ({
    actor: "CEO",
    timestamp: minutesAgo(4 + (i % 5)),
  }));
  const p = pulse(events);
  assert.ok(p.blindTo > 0, "a truncated feed claimed the whole window");
  assert.strictEqual(p.covered, PULSE_BUCKETS - p.blindTo);
  const row = p.rows.find((r) => r.name === "CEO");
  assert.ok(row.cells[0].unknown, "the oldest cell claimed to be idle");
  assert.ok(!row.cells[PULSE_BUCKETS - 1].unknown, "the newest cell was blanked");
});

test("a short feed covers the whole window", () => {
  // A quiet org's feed is short because nothing happened, not because
  // anything was dropped — those cells really are idle.
  const p = pulse([{ actor: "CEO", timestamp: minutesAgo(30) }]);
  assert.strictEqual(p.blindTo, 0);
  assert.strictEqual(p.covered, PULSE_BUCKETS);
  assert.ok(p.rows.every((r) => r.cells.every((c) => !c.unknown)));
});

test("the retention edge is read from the whole feed, not just seat events", () => {
  // An engine-authored event (no seat actor) still marks how far back
  // the record goes; ignoring it would over-claim coverage on an org
  // whose oldest retained rows are engine lifecycle events.
  const events = Array.from({ length: 400 }, (_, i) => ({
    actor: i === 0 ? "engine" : "CEO",
    timestamp: minutesAgo(i === 0 ? 20 : 2),
  }));
  const p = pulse(events);
  assert.strictEqual(p.covered, 21, "coverage ignored the engine-authored row");
});

test("with no org chart the roster falls back to the live agents", () => {
  const p = buildPulse(
    {
      seats: [],
      agents: [{ id: "a1", role: "Solo", state: "idle" }],
      events: [{ actor: "Solo", timestamp: minutesAgo(2) }],
      tokens: null,
    },
    { now: NOW },
  );
  assert.deepStrictEqual(
    p.rows.map((r) => r.name),
    ["Solo"],
  );
  assert.strictEqual(p.rows[0].events, 1);
});

run();
