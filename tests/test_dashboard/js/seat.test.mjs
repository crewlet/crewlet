// The seat page: four questions, four tabs, and the numbers it must not
// invent.
//
// This is the deepest view in the product and the one with the most ways
// to say something false: a budget bar drawn from two different spans, a
// cap reported as spent because no meter is running, a streaming record
// that re-opens itself under the reader's cursor on every round.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { createSeatView } = await import(new URL("views/seat.js", base));

const SEAT = {
  id: "rt-1",
  name: "Engineer",
  role: "Engineer",
  handle: "eng",
  goal: "Write code",
  state: "idle",
  input_tokens: 0,
  output_tokens: 0,
  total_tokens: 0,
};

function mount(agent = SEAT, params = { key: "eng" }, answers = {}) {
  const state = {
    agents: [agent],
    sandboxes: [],
    budget: {},
    events: [],
    org: {},
    connected: true,
    health: { status: "ok", configured: true },
  };
  const asked = [];
  const view = createSeatView({
    store: {
      state,
      agentByKey: () => agent,
      subscribe: () => () => {},
    },
    query: async (what, p) => {
      asked.push(what);
      if (what in answers) return answers[what];
      if (what === "agent") return { ...agent, llm_history: [] };
      if (what === "agent_memory") {
        return {
          personal_memories: { long: [], short: [] },
          episodes: [],
          counterparty_profiles: [],
          synthesized_skills: [],
        };
      }
      if (what === "tokens") return { totals: {}, by_phase: [] };
      return {};
    },
    navigate: () => {},
    refresh: () => {},
    params,
  });
  if (view.mount) view.mount(document.createElement("div"));
  return { view, state, asked, markup: () => view.render(state) };
}

const settle = async () => {
  await Promise.resolve();
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
};

test("the four tabs are the four questions", async () => {
  const { markup } = mount();
  await settle();
  const html = markup();
  for (const label of ["Now", "Memory", "Cost", "Access"]) {
    assert.ok(html.includes(label), `no ${label} tab`);
  }
});

test("only the mounted tab's data is asked for", async () => {
  // The memory read is a per-agent scan; a reader who opened the page to
  // see what a turn did should not pay for it.
  const { asked } = mount();
  await settle();
  assert.ok(!asked.includes("agent_memory"), `memory was fetched eagerly: ${asked}`);
});

test("opening Memory fetches it, once", async () => {
  const { view, asked } = mount(SEAT, { key: "eng", tab: "memory" });
  await settle();
  assert.ok(asked.includes("agent_memory"), `memory was never fetched: ${asked}`);
  const before = asked.filter((a) => a === "agent_memory").length;
  view.render({
    agents: [SEAT],
    sandboxes: [],
    budget: {},
    events: [],
    connected: true,
    health: {},
  });
  await settle();
  assert.strictEqual(
    asked.filter((a) => a === "agent_memory").length,
    before,
    "re-rendering re-fetched memory",
  );
});

// ---- the budget meter's three honest states ----

test("a seat with no cap says so rather than drawing an empty bar", async () => {
  const { markup } = mount({ ...SEAT }, { key: "eng", tab: "cost" });
  await settle();
  const html = markup();
  assert.match(html, /No <code>token_budget<\/code> is set/);
  assert.ok(!html.includes("seat-budget-track"), "a bar was drawn with no cap");
});

test("a cap with no live meter states the cap and divides nothing", async () => {
  // The cap is configuration and is real; how much of it has been spent
  // is a number this page does not have.
  const { markup } = mount(
    { ...SEAT, token_budget: 50000 },
    { key: "eng", tab: "cost" },
  );
  await settle();
  const html = markup();
  assert.match(html, /50,000/);
  assert.match(html, /No engine is reporting a live meter/);
  assert.ok(!html.includes("seat-budget-track"), "a ratio was drawn without a meter");
});

test("a metered seat draws the bar and names its span", async () => {
  const { markup } = mount(
    { ...SEAT, token_budget: 50000, budget: { used: 20000, max: 50000, refused_at: "" } },
    { key: "eng", tab: "cost" },
  );
  await settle();
  const html = markup();
  assert.match(html, /seat-budget-track/);
  assert.match(html, /this run/, "the bar did not say which span it covers");
});

test("exhaustion is the refusal, not the ratio", async () => {
  // A refused charge increments nothing, so a fully blocked seat stops
  // short of its own cap and `used >= max` is false for it.
  const near = mount(
    { ...SEAT, token_budget: 100000, budget: { used: 99000, max: 100000, refused_at: "" } },
    { key: "eng", tab: "cost" },
  );
  await settle();
  assert.ok(!near.markup().includes("Refusing charges"), "99% was called exhausted");

  const blocked = mount(
    {
      ...SEAT,
      token_budget: 100000,
      budget: { used: 99000, max: 100000, refused_at: "2026-08-21T09:14:00Z" },
    },
    { key: "eng", tab: "cost" },
  );
  await settle();
  assert.match(blocked.markup(), /Refusing charges/);
});

test("the live cap wins over the configured one", async () => {
  // The engine's meter carries the cap it is actually ENFORCING. A live
  // edit to `token_budget` leaves config and meter disagreeing until the
  // next apply, and dividing the meter's `used` by the config's cap
  // would pair two numbers from different sources.
  const { markup } = mount(
    {
      ...SEAT,
      token_budget: 50000,
      budget: { used: 40000, max: 80000, refused_at: "" },
    },
    { key: "eng", tab: "cost" },
  );
  await settle();
  assert.match(markup(), /of 80,000 tokens/);
});

test("a bar past three quarters warns before it is refused", async () => {
  const { markup } = mount(
    {
      ...SEAT,
      token_budget: 100000,
      budget: { used: 80000, max: 100000, refused_at: "" },
    },
    { key: "eng", tab: "cost" },
  );
  await settle();
  assert.match(markup(), /seat-meter warn/);
});

test("a meter over its cap cannot draw past the end of the track", async () => {
  // The engine refuses an over-cap charge, but a cap LOWERED by a live
  // edit can leave a meter above it — and a bar wider than its track
  // overflows the panel.
  const { markup } = mount(
    {
      ...SEAT,
      token_budget: 10000,
      budget: { used: 90000, max: 10000, refused_at: "" },
    },
    { key: "eng", tab: "cost" },
  );
  await settle();
  const width = /seat-budget-track"><i style="width:([\d.]+)%"/.exec(markup());
  assert.ok(width, "no bar drawn");
  assert.ok(Number(width[1]) <= 100, `bar drawn at ${width[1]}%`);
});

test("the meter names its own span and never claims the durable one", async () => {
  // Three numbers over three spans, and only the meter shares one with
  // the cap. A bar that does not say which span it covers is the two-
  // windows error waiting to be read as the other number.
  const { markup } = mount(
    {
      ...SEAT,
      token_budget: 50000,
      budget: { used: 20000, max: 50000, refused_at: "" },
    },
    { key: "eng", tab: "cost" },
  );
  await settle();
  const html = markup();
  assert.match(html, /this run/);
  assert.match(html, /durable counter that survives a restart/);
});

// ---- identity ----

test("a seat resolves by handle, and the page is addressed by it", async () => {
  const { markup } = mount(SEAT, { key: "eng" });
  await settle();
  const html = markup();
  assert.match(html, /Engineer/);
  assert.ok(!html.includes("/agents/rt-1"), "a link still uses the runtime id route");
});

test("a stopped seat leads with why", async () => {
  const { markup } = mount(
    {
      ...SEAT,
      state: "afk",
      afk_reason: "stall",
      last_error: {
        kind: "stall",
        phase: "execute",
        message: "no forward progress",
        at: "2026-08-21T10:00:00Z",
      },
    },
    { key: "eng" },
  );
  await settle();
  const html = markup();
  assert.match(html, /failure-card/);
  assert.match(html, /no forward progress/);
});

await run();
