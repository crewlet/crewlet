// The store is a mirror, not a projection.
//
// It used to re-derive agent state, sandbox lifecycle and token
// aggregation from the raw event stream — a second implementation of
// what the server already computed, and one that drifted. These tests
// pin the property that replaced it: the store holds what it is pushed,
// and it wakes only the views that read the slice that moved.

import assert from "node:assert";
import { test, run } from "./harness.mjs";

const { Store } = await import(
  new URL("../../../src/crewlet/static/dashboard/js/store.js", import.meta.url)
);

function snapshot(overrides = {}) {
  return {
    health: { status: "ok" },
    agents: [
      { id: "1", role: "CEO", state: "idle", live_call: null },
      { id: "2", role: "SWE", state: "working", live_call: null },
    ],
    events: [{ id: "e1", type: "task_started", category: "task" }],
    sandboxes: [],
    org: { name: "Acme" },
    tools: [{ name: "post_message" }],
    tokens: { totals: { total_tokens: 10 } },
    schedules: [],
    ...overrides,
  };
}

test("a snapshot populates every slice", () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  assert.strictEqual(store.state.agents.length, 2);
  assert.strictEqual(store.state.org.name, "Acme");
  assert.strictEqual(store.state.tokens.totals.total_tokens, 10);
  assert.strictEqual(store.state.connected, true);
});

test("an agents push merges into the matching seat only", () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  const ceo = store.state.agents[0];
  store.applyAgents([{ role: "SWE", state: "afk", afk_reason: "llm_unavailable" }]);
  assert.strictEqual(store.state.agents[0], ceo, "untouched seat was rebuilt");
  assert.strictEqual(store.state.agents[1].state, "afk");
  assert.strictEqual(store.state.agents[1].id, "2", "identity fields survive a patch");
});

test("a seat the config list does not carry yet still lands", () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  store.applyAgents([{ role: "Designer", state: "idle" }]);
  assert.strictEqual(store.state.agents.length, 3);
  assert.strictEqual(store.state.agents[2].role, "Designer");
});

test("a subscriber wakes only for its own slices", () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  let agentWakes = 0;
  let tokenWakes = 0;
  store.subscribe(["agents"], () => agentWakes++);
  store.subscribe(["tokens"], () => tokenWakes++);

  store.applyAgents([{ role: "CEO", state: "working" }]);
  assert.strictEqual(agentWakes, 1);
  assert.strictEqual(tokenWakes, 0, "a token view redrew for an agent change");

  store.applyTokens({ totals: { total_tokens: 20 } });
  assert.strictEqual(agentWakes, 1, "an agent view redrew for a token change");
  assert.strictEqual(tokenWakes, 1);
});

test("a health tick does not wake the agent views", () => {
  // The server pushes health every five seconds. Treating that as a
  // general state change is what re-rendered the whole page while
  // nothing was happening.
  const store = new Store();
  store.applySnapshot(snapshot());
  let agentWakes = 0;
  let healthWakes = 0;
  store.subscribe(["agents"], () => agentWakes++);
  store.subscribe(["health"], () => healthWakes++);
  store.applyHealth({ status: "ok", in_flight: 2 });
  assert.strictEqual(agentWakes, 0);
  assert.strictEqual(healthWakes, 1);
});

test("a subscriber listing two moved slices is called once", () => {
  const store = new Store();
  let wakes = 0;
  store.subscribe(["agents", "events", "org"], () => wakes++);
  store.applySnapshot(snapshot());
  assert.strictEqual(wakes, 1);
});

test("only persisted events enter the feed", () => {
  // An event with no category is one the server does not store, so it
  // would vanish on the next snapshot and 404 when clicked.
  const store = new Store();
  store.applySnapshot(snapshot());
  store.applyEvent({ id: "e2", type: "agent_turn_progress" });
  assert.strictEqual(store.state.events.length, 1);
  store.applyEvent({ id: "e3", type: "task_completed", category: "task" });
  assert.strictEqual(store.state.events.length, 2);
  assert.strictEqual(store.state.events[0].id, "e3", "newest first");
});

test("a re-delivered event is not duplicated", () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  const dup = { id: "e9", type: "task_completed", category: "task" };
  store.applyEvent(dup);
  store.applyEvent(dup);
  assert.strictEqual(store.state.events.filter((e) => e.id === "e9").length, 1);
});

test("every event reaches the per-event subscribers, feed or not", () => {
  const store = new Store();
  const seen = [];
  store.onEvent((ev) => seen.push(ev.type));
  store.applyEvent({ id: "p1", type: "agent_turn_progress" });
  store.applyEvent({ id: "p2", type: "task_completed", category: "task" });
  assert.deepStrictEqual(seen, ["agent_turn_progress", "task_completed"]);
});

test("losing the socket clears health but keeps the last state", () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  store.setConnected(false);
  assert.strictEqual(store.state.connected, false);
  assert.strictEqual(store.state.health.status, "unknown");
  assert.strictEqual(store.state.agents.length, 2, "state was discarded on drop");
});

test("an empty token rollup does not overwrite a real one", () => {
  // The REST fallback snapshot has no rollup section; applying it must
  // not blank the spend view.
  const store = new Store();
  store.applySnapshot(snapshot());
  store.applySnapshot(snapshot({ tokens: {} }));
  assert.strictEqual(store.state.tokens.totals.total_tokens, 10);
});

run();
