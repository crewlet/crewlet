// Where a seat's raw event list lives.
//
// It used to be a second list at the bottom of the agent page, below the
// turns — the thing the page is actually for. Activity already renders
// that list, with filters the agent page never had, so the agent page
// links across instead of carrying a copy. That only holds if the link
// actually lands on a filtered view, and if the filter it sets is
// visible to the reader who arrives on it.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";

installDom();
const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { createEventsView } = await import(new URL("views/events.js", base));
const { createAgentView } = await import(new URL("views/agent.js", base));
const { parseRoute } = await import(new URL("router.js", base));

// ---- the agent page ----

function makeAgentView(agent) {
  const state = { agents: [agent], sandboxes: [], connected: true };
  const store = {
    state,
    agentById: () => agent,
    subscribe: () => () => {},
  };
  const navigated = [];
  let markup = "";
  const view = createAgentView({
    store,
    params: { id: agent.id },
    navigate: (hash) => navigated.push(hash),
    refresh: () => {
      markup = view.render(state);
    },
    query: async (what) => {
      if (what === "agent") return agent;
      if (what === "agent_memory") return {};
      if (what === "tokens") return { by_phase: [], totals: {} };
      return [];
    },
  });
  view.mount();
  return { view, navigated, markup: () => view.render(state) };
}

const SEAT = {
  id: "a1",
  role: "Agent CEO",
  handle: "ceo",
  state: "idle",
  llm_history: [],
  input_tokens: 0,
  output_tokens: 0,
  total_tokens: 0,
};

test("the agent page no longer renders a second event list", async () => {
  const { markup } = makeAgentView(SEAT);
  await Promise.resolve();
  await Promise.resolve();
  const html = markup();
  assert.ok(!html.includes("Event history"), "the event list is still on the page");
  assert.ok(html.includes("LLM Invocations"), "the page lost what it is for");
});

test("the agent page links to Activity filtered to the seat", async () => {
  const { view, navigated } = makeAgentView(SEAT);
  await Promise.resolve();
  await Promise.resolve();
  view.onAction("seat-events", { dataset: {} });
  assert.deepStrictEqual(navigated, ["/events?actor=Agent%20CEO"]);
});

// ---- the link's other end ----

test("the route carries the actor through to the view", () => {
  globalThis.location.hash = "#/events?actor=Agent%20CEO";
  const route = parseRoute();
  assert.strictEqual(route.name, "events");
  assert.strictEqual(route.params.actor, "Agent CEO");
});

function makeEventsView(params, liveEvents = []) {
  const state = { events: liveEvents, connected: true, health: { status: "ok" } };
  const store = { state, subscribe: () => () => {} };
  let markup = "";
  const view = createEventsView({
    store,
    params,
    navigate: () => {},
    refresh: () => {
      markup = view.render(state);
    },
    query: async () => [],
  });
  view.mount();
  markup = view.render(state);
  return { view, markup: () => markup };
}

const ev = (id, actor) => ({
  id,
  type: "task_created",
  category: "task",
  actor,
  summary: id,
  timestamp: "2026-04-01T12:00:00Z",
  trace_id: "",
});

test("Activity opens filtered when the URL names an actor", () => {
  const { markup } = makeEventsView({ actor: "Agent CEO" }, [
    ev("e1", "Agent CEO"),
    ev("e2", "Agent PM"),
  ]);
  const html = markup();
  assert.ok(html.includes("e1"), "the seat's own event was filtered out");
  assert.ok(!html.includes("e2"), "another seat's event survived the filter");
});

test("the preset actor gets a visible, clearable pill with no matches yet", () => {
  // Reachable from a URL, so the filter can be set before one matching
  // event has loaded. An active filter the reader cannot see is an empty
  // list with no way back out of it.
  const { markup } = makeEventsView({ actor: "Agent CEO" }, [ev("e2", "Agent PM")]);
  const html = markup();
  assert.ok(
    /data-action="actor" data-actor="Agent CEO"/.test(html),
    "no pill for the active filter",
  );
});

test("Activity with no actor param filters nothing", () => {
  const { markup } = makeEventsView({}, [ev("e1", "Agent CEO"), ev("e2", "Agent PM")]);
  const html = markup();
  assert.ok(html.includes("e1") && html.includes("e2"));
});

run();
