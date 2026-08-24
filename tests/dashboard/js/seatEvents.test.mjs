// The seat page does not render a second event list — it links to one.
//
// A seat's page answers "what is this agent doing and what did it cost".
// Its raw event list is a different question — "what happened, across the
// company" — and Activity already answers that one, with category,
// failure and actor filters the seat page never had. Rendering a second
// copy below the turns pushed the page's own content off the fold.
//
// So the rule is: when a screen wants a list another screen already owns,
// it links to it FILTERED. This checks both ends of that link, because a
// filtered link is only useful if the far end actually filters — and the
// two ends live in different modules, which is exactly where a rename
// breaks one and not the other.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();
const base = JS_URL;
const { createSeatView } = await import(new URL("views/seat.js", base));
const { createActivityView } = await import(new URL("views/activity.js", base));
const { parseRoute } = await import(new URL("router.js", base));

const SEAT = {
  id: "rt-1",
  name: "Agent CEO",
  role: "Agent CEO",
  handle: "ceo",
  goal: "Lead",
  state: "idle",
  input_tokens: 0,
  output_tokens: 0,
  total_tokens: 0,
};

function makeSeatView(agent) {
  const state = {
    agents: [agent],
    sandboxes: [],
    budget: {},
    events: [],
    connected: true,
    health: { status: "ok" },
  };
  const navigated = [];
  const view = createSeatView({
    store: { state, agentByKey: () => agent, subscribe: () => () => {} },
    query: async (what) => {
      if (what === "agent") return { ...agent, llm_history: [] };
      if (what === "agent_memory") return {};
      if (what === "tokens") return { totals: {}, by_phase: [] };
      return {};
    },
    navigate: (to) => navigated.push(to),
    refresh: () => {},
    params: { key: "ceo" },
  });
  if (view.mount) view.mount(document.createElement("div"));
  return { view, state, navigated, markup: () => view.render(state) };
}

test("the seat page renders no event list of its own", async () => {
  const { markup } = makeSeatView(SEAT);
  await Promise.resolve();
  await Promise.resolve();
  const html = markup();
  assert.ok(!html.includes("Event history"), "the event list is back on the page");
});

test("the seat page links to Activity filtered to the seat", async () => {
  const { markup } = makeSeatView(SEAT);
  await Promise.resolve();
  await Promise.resolve();
  const html = markup();
  assert.ok(
    html.includes("/activity?actor=Agent%20CEO"),
    `no filtered Activity link on the seat page:\n${html.slice(0, 400)}`,
  );
});

// ---- the link's other end ----

test("the route carries the actor through to Activity", () => {
  globalThis.location.hash = "#/activity?actor=Agent%20CEO";
  const route = parseRoute();
  assert.strictEqual(route.name, "activity");
  assert.strictEqual(route.params.actor, "Agent CEO");
});

test("the old route still lands, filter intact", () => {
  // `#/events?actor=` is in bookmarks and in chat threads, and was the
  // form the seat page itself minted before the rooms were reorganised.
  globalThis.location.hash = "#/events?actor=Agent%20CEO";
  const route = parseRoute();
  assert.ok(route.redirect, "the old Activity route no longer redirects");
  assert.match(route.redirect, /^\/activity\?actor=/);
  // The query survives the move: decoding it must yield the same actor,
  // whichever encoding of a space the redirect used.
  const query = new URLSearchParams(route.redirect.split("?")[1]);
  assert.strictEqual(query.get("actor"), "Agent CEO");
});

test("Activity opens filtered when the URL names an actor", () => {
  const events = [
    { id: "e1", type: "task_created", actor: "Agent CEO", timestamp: "2026-04-01T12:00:00Z", category: "task", summary: "one" },
    { id: "e2", type: "task_created", actor: "Agent PM", timestamp: "2026-04-01T12:00:01Z", category: "task", summary: "two" },
  ];
  const state = { events, connected: true, health: { status: "ok" } };
  const view = createActivityView({
    store: { state, subscribe: () => () => {} },
    query: async () => ({ events: [] }),
    navigate: () => {},
    refresh: () => {},
    params: { actor: "Agent CEO" },
  });
  const html = view.render(state);
  assert.ok(html.includes("one"), "the seat's own event is missing");
  assert.ok(!html.includes("two"), "another seat's event was not filtered out");
});

await run();
