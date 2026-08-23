// The attention queue — the one list that answers "is anything waiting
// on me?".
//
// Its failure mode is not a crash. It is telling somebody there is
// nothing to do: an empty list on a page that has lost the socket, a
// stopped seat dropped because the error record expired, a budget that
// stalled just under its cap and so never compared equal to it. Each of
// those reads as a healthy company.

import assert from "node:assert";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

const base = JS_URL;
const { buildAttention, attentionCounts, SEVERITY } = await import(
  new URL("attention.js", base)
);

function state(extra = {}) {
  return {
    connected: true,
    agents: [],
    sandboxes: [],
    budget: {},
    health: { configured: true },
    ...extra,
  };
}

test("a quiet company has nothing waiting", () => {
  const { items, stale } = buildAttention(state());
  assert.deepEqual(items, []);
  assert.equal(stale, false);
});

test("a sandbox question leads with the question itself", () => {
  const { items } = buildAttention(
    state({
      sandboxes: [
        {
          turn_id: "t1",
          role: "Engineer",
          status: "awaiting_input",
          question: "Drop or dead-letter poison messages?",
          audience: "requester",
          coding_agent: "claude-code",
          started_at: "2026-08-20T09:00:00Z",
        },
      ],
    }),
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].title, "Drop or dead-letter poison messages?");
  assert.equal(items[0].severity, SEVERITY.needs);
  assert.equal(items[0].route, "/work");
  assert.match(items[0].detail, /claude-code/);
  assert.match(items[0].detail, /asked requester/);
});

test("a running sandbox is not an obligation", () => {
  const { items } = buildAttention(
    state({
      sandboxes: [{ turn_id: "t1", role: "Engineer", status: "running" }],
    }),
  );
  assert.deepEqual(items, []);
});

test("a stopped seat survives the loss of its error record", () => {
  // `last_error` does not survive an API restart. Keying the list on it
  // alone hid every broken seat at exactly the moment somebody went
  // looking for them.
  const { items } = buildAttention(
    state({
      agents: [
        { role: "Analyst", handle: "analyst", state: "afk", afk_reason: "stall" },
      ],
    }),
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].severity, SEVERITY.broke);
  assert.match(items[0].title, /Analyst stopped/);
  assert.match(items[0].title, /stall/);
});

test("a stopped seat names its cause and phase when it has them", () => {
  const { items } = buildAttention(
    state({
      agents: [
        {
          role: "Analyst",
          handle: "analyst",
          state: "afk",
          last_error: {
            kind: "llm_unavailable",
            phase: "execute",
            message: "provider chain exhausted",
            at: "2026-08-21T10:00:00Z",
          },
        },
      ],
    }),
  );
  assert.match(items[0].title, /llm unavailable in execute/);
  assert.equal(items[0].detail, "provider chain exhausted");
  assert.equal(items[0].route, "/seats/analyst");
});

test("exhaustion is read from the refusal, not from used >= max", () => {
  // A seat charged in 3k rounds against a 100k cap stalls near 99k and
  // never reaches its own maximum, so a ratio test calls a permanently
  // blocked seat healthy.
  const near = buildAttention(
    state({ agents: [{ role: "CEO", budget: { used: 99000, max: 100000 } }] }),
  );
  assert.deepEqual(near.items, [], "a seat under its cap is not blocked");

  const refused = buildAttention(
    state({
      agents: [
        {
          role: "CEO",
          budget: { used: 99000, max: 100000, refused_at: "2026-08-21T09:14:00Z" },
        },
      ],
    }),
  );
  assert.equal(refused.items.length, 1);
  assert.match(refused.items[0].title, /CEO has spent its token budget/);
  assert.match(refused.items[0].detail, /99,000 of 100,000/);
});

test("an org-wide refusal is its own item", () => {
  const { items } = buildAttention(
    state({
      budget: { org: { used: 5, max: 10, refused_at: "2026-08-21T09:00:00Z" } },
    }),
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].route, "/spend");
  assert.match(items[0].title, /org token budget/);
});

test("an unconfigured engine is an obligation, an unknown one is not", () => {
  const unconfigured = buildAttention(state({ health: { configured: false } }));
  assert.equal(unconfigured.items.length, 1);
  assert.match(unconfigured.items[0].title, /No company configuration/);

  // Three-valued: the health slice is CLEARED on disconnect, so
  // `configured` arrives undefined at the moment the reader most needs
  // the truth. `!configured` would announce an unconfigured engine to a
  // page that cannot see one.
  const unknown = buildAttention(state({ health: {} }));
  assert.deepEqual(unknown.items, []);
});

test("what broke outranks what is merely waiting", () => {
  const { items } = buildAttention(
    state({
      sandboxes: [
        {
          turn_id: "t1",
          role: "Engineer",
          status: "awaiting_input",
          question: "q",
          started_at: "2026-08-01T00:00:00Z",
        },
      ],
      agents: [{ role: "Analyst", handle: "a", state: "afk" }],
    }),
  );
  assert.equal(items[0].severity, SEVERITY.broke);
  assert.equal(items[1].severity, SEVERITY.needs);
});

test("the oldest debt comes first within a severity", () => {
  const now = Date.parse("2026-08-21T12:00:00Z");
  const { items } = buildAttention(
    state({
      sandboxes: [
        {
          turn_id: "recent",
          role: "A",
          status: "awaiting_input",
          question: "recent",
          started_at: "2026-08-21T11:55:00Z",
        },
        {
          turn_id: "old",
          role: "B",
          status: "awaiting_input",
          question: "old",
          started_at: "2026-08-17T09:00:00Z",
        },
      ],
    }),
    now,
  );
  assert.equal(items[0].title, "old", "a four-day-old question ranked second");
});

test("a disconnected page reports that it cannot see, not that all is well", () => {
  const result = buildAttention(state({ connected: false }));
  assert.equal(result.stale, true);
  assert.equal(
    attentionCounts(result),
    null,
    "a confident zero during an outage is the failure this guards",
  );
});

test("counts are per zone, with what broke called out", () => {
  const result = buildAttention(
    state({
      agents: [{ role: "A", handle: "a", state: "afk" }],
      budget: { org: { used: 1, max: 2, refused_at: "2026-08-21T09:00:00Z" } },
      health: { configured: false },
    }),
  );
  const counts = attentionCounts(result);
  assert.equal(counts.total, 3);
  assert.equal(counts.broke, 1);
  assert.equal(counts.now, 1);
  assert.equal(counts.operations, 2);
});

await run();
