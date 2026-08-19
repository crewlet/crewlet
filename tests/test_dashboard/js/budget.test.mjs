// The seat card's budget bar.
//
// This is the ONE place the dashboard divides one token figure by
// another, so the property that matters is which figure it divides. The
// card carries three token numbers with three different spans — a
// 24-hour spend, a 7-day total, and the engine's process-lifetime meter
// — and only the last of them shares a span with the cap. Dividing
// either of the others would render a percentage wrong by however long
// the engine has been up, and it would look completely plausible.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
const { seatCard } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/cards.js", import.meta.url)
);

const SEAT = { name: "Engineer", kind: "agent", integrations: [], unitPath: [] };

function card(seat, agent, extra = {}) {
  return seatCard({ ...SEAT, ...seat }, { agent, ...extra });
}

// The width the bar was drawn at, as a number, or null when there is no bar.
function barPct(markup) {
  const m = markup.match(/seat-budget-track"><i style="width:([\d.]+)%"/);
  return m ? Number(m[1]) : null;
}

test("a metered seat gets a bar drawn from the live meter", () => {
  const markup = card(
    { tokenBudget: 1000 },
    { id: "a1", role: "Engineer", state: "idle", budget: { used: 250, max: 1000, refused_at: "" } },
  );
  assert.strictEqual(barPct(markup), 25);
  assert.ok(markup.includes("this run"), "the bar did not name its span");
});

test("the bar divides the meter, never the 24-hour spend beside it", () => {
  // The pulse row's `tokens` is a 24-hour window. If it ever reached the
  // ratio, this seat would read 90% instead of 25% — a plausible,
  // completely wrong number.
  const markup = card(
    { tokenBudget: 1000 },
    { id: "a1", role: "Engineer", state: "idle", budget: { used: 250, max: 1000, refused_at: "" } },
    { pulseRow: { name: "Engineer", tokens: 900, cells: [], events: 0, lastAt: 0 }, pulseMax: 1 },
  );
  assert.strictEqual(barPct(markup), 25);
});

test("the live cap wins over the configured one", () => {
  // A revision the database activated but the engine has not applied is
  // a real disagreement; the bar shows the cap actually being enforced.
  const markup = card(
    { tokenBudget: 1000 },
    { id: "a1", role: "Engineer", state: "idle", budget: { used: 100, max: 200, refused_at: "" } },
  );
  assert.strictEqual(barPct(markup), 50);
});

test("a capped seat with no meter states the cap and says so", () => {
  const markup = card({ tokenBudget: 1000 }, { id: "a1", role: "Engineer", state: "idle" });
  assert.strictEqual(barPct(markup), null, "a bar was drawn with nothing behind it");
  assert.ok(markup.includes("no live meter"));
  assert.ok(markup.includes("cap 1.0k"));
});

test("a seat with no cap gets nothing at all", () => {
  const markup = card({}, { id: "a1", role: "Engineer", state: "idle" });
  assert.ok(!markup.includes("seat-budget"), "an uncapped seat drew a budget");
});

test("a human seat never gets a budget", () => {
  const markup = card({ kind: "human", tokenBudget: 1000 }, null);
  assert.ok(!markup.includes("seat-budget"));
});

test("exhaustion keys off the engine's refusal, not off the ratio", () => {
  // `consume` refuses an over-cap charge and increments nothing, so the
  // counter stops short of the cap by the size of the round that would
  // not fit. A seat that is completely blocked reads ~96% here, and a
  // ratio test would call that "fine".
  const markup = card(
    { tokenBudget: 1000 },
    {
      id: "a1",
      role: "Engineer",
      state: "afk",
      budget: { used: 960, max: 1000, refused_at: "2026-04-01T12:00:00Z" },
    },
  );
  assert.ok(markup.includes("budget exhausted"));
  assert.ok(/seat-budget [^"]*over/.test(markup), "the exhausted bar is not marked");
});

test("a bar past three quarters warns before it is refused", () => {
  const markup = card(
    { tokenBudget: 1000 },
    { id: "a1", role: "Engineer", state: "idle", budget: { used: 800, max: 1000, refused_at: "" } },
  );
  assert.ok(/seat-budget [^"]*warn/.test(markup));
  assert.ok(!markup.includes("budget exhausted"));
});

test("a meter over its cap cannot draw past the end of the track", () => {
  // `record_spend` counts already-spent tokens whatever the cap says, so
  // `used` can legitimately exceed `max`.
  const markup = card(
    { tokenBudget: 100 },
    {
      id: "a1",
      role: "Engineer",
      state: "idle",
      budget: { used: 400, max: 100, refused_at: "2026-04-01T12:00:00Z" },
    },
  );
  assert.strictEqual(barPct(markup), 100);
});

test("the cost line states its own span and never mentions the cap", () => {
  // The cap belongs to the bar, which shares its span. Repeating it on
  // the 24-hour line would invite exactly the comparison the bar exists
  // to make correctly.
  const markup = card(
    { tokenBudget: 1000 },
    { id: "a1", role: "Engineer", state: "idle", budget: { used: 250, max: 1000, refused_at: "" } },
    { pulseRow: { name: "Engineer", tokens: 900, cells: [], events: 0, lastAt: 0 }, pulseMax: 1 },
  );
  const meta = markup.slice(markup.indexOf("seat-meta"));
  assert.ok(meta.includes("tokens"));
  assert.ok(!meta.includes("cap "), "the cost line repeated the cap");
});

run();
