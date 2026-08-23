// Degraded mode: what the page shows when it cannot reach the engine.
//
// This is the one path where the client has no fresh truth, so the only
// thing it owns is not making the situation worse. The bug pinned first
// did exactly that: the last state the page received was the only thing
// the operator still had, and a failed poll replaced it with nothing.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();
const { Store } = await import(
  new URL("store.js", JS_URL)
);
const { api } = await import(
  new URL("api.js", JS_URL)
);

function snapshot() {
  return {
    health: { status: "ok", configured: true },
    agents: [{ id: "1", role: "CEO", state: "working", live_call: null }],
    events: [{ id: "e1", type: "task_started", category: "task" }],
    sandboxes: [],
    org: { name: "Acme" },
    tools: [{ name: "post_message" }],
    tokens: { totals: { total_tokens: 10 } },
    schedules: [],
  };
}

// The tiny slice of LiveSocket this exercises: the degraded poll. The
// real class needs a WebSocket to construct, and the behaviour under
// test is entirely in what it does with the fetch's answer.
async function poll(store) {
  const snap = await api.snapshot();
  if (snap) store.applySnapshot(snap);
}

test("a refused fetch leaves the last known state alone", async () => {
  // `api.snapshot()` used to answer `{_error: 0}` when the fetch threw,
  // and the caller guarded with `!snap._error` — true for zero. So the
  // total-network-loss case, the one this fallback exists for, applied
  // the error object AS a snapshot: agents, events, org and tools all
  // replaced with empties, at the exact moment the last state received
  // was the only thing the page had.
  const store = new Store();
  store.applySnapshot(snapshot());
  globalThis.fetch = async () => {
    throw new TypeError("Failed to fetch");
  };
  await poll(store);
  assert.strictEqual(store.state.agents.length, 1, "the seat list was wiped");
  assert.strictEqual(store.state.events.length, 1, "the feed was wiped");
  assert.strictEqual(store.state.org.name, "Acme", "the org was wiped");
});

test("an error status leaves the last known state alone", async () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  globalThis.fetch = async () => ({ ok: false, status: 503, json: async () => ({}) });
  await poll(store);
  assert.strictEqual(store.state.agents.length, 1);
  assert.strictEqual(store.state.org.name, "Acme");
});

test("a proxy answering 200 with an HTML error page is not a snapshot", async () => {
  const store = new Store();
  store.applySnapshot(snapshot());
  globalThis.fetch = async () => ({
    ok: true,
    json: async () => {
      throw new SyntaxError("Unexpected token <");
    },
  });
  await poll(store);
  assert.strictEqual(store.state.agents.length, 1);
});

test("a real snapshot still lands", async () => {
  const store = new Store();
  globalThis.fetch = async () => ({ ok: true, json: async () => snapshot() });
  await poll(store);
  assert.strictEqual(store.state.agents.length, 1);
  assert.strictEqual(store.state.org.name, "Acme");
  // Still not connected: a REST reading is what the page falls back to
  // WHILE the socket is down, so it must never announce a live one.
  assert.strictEqual(store.state.connected, false);
});

run();
