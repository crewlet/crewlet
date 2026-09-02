/**
 * The store's contract with the server.
 *
 * These are the client half of the wire protocol, and each case here is a
 * shape the server actually sent once and got wrong — or a guard whose removal
 * would silently blank a screen. The e2e replay
 * (internal/e2e/golden_test.go) checks the same module against frames a REAL
 * engine produced; this file checks the rules that replay cannot express.
 */

import { describe, expect, test, vi } from "vitest";
import { MAX_EVENTS, Store } from "./store.ts";
import type { EventEnvelope, FeedRow } from "./types.ts";

function feedRow(id: string, over: Partial<FeedRow> = {}): FeedRow {
  return {
    id,
    type: "agent_turn_completed",
    timestamp: "2026-01-01T00:00:00Z",
    source: "engine",
    actor: "PM",
    summary: "did a thing",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    ...over,
  };
}

describe("agent overlays", () => {
  test("an overlay MERGES onto the row a client already holds", () => {
    // Merge, not replace: the overlay carries what MOVED, and a screen that
    // lost the seat's static identity on every progress round would redraw
    // the roster several times a second with half its fields blank.
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", handle: "pm", state: "idle" }] });
    store.applyAgents([{ role: "PM", state: "working", current_phase: "execute" }]);

    const [row] = store.state.agents;
    expect(row?.handle).toBe("pm");
    expect(row?.state).toBe("working");
    expect(row?.current_phase).toBe("execute");
  });

  test("a keyed object is DISCARDED rather than half-applied", () => {
    // The server sent a full turn's worth of overlays as an object keyed by
    // role once. Both sides' own suites passed, the socket carried every
    // frame, and the seat rendered idle from the first phase to the last.
    // The guard is what makes that loud rather than silent — and the e2e
    // replay is what makes it impossible to ship again.
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", state: "idle" }] });
    store.applyAgents({ PM: { state: "working" } } as never);
    expect(store.state.agents[0]?.state).toBe("idle");
  });

  test("an overlay for a role the roster does not carry is appended", () => {
    // A live revision can add a seat before the roster push lands.
    const store = new Store();
    store.applyAgents([{ role: "New", state: "working" }]);
    expect(store.state.agents).toHaveLength(1);
    expect(store.state.agents[0]?.id).toBe("New");
  });

  test("a seats push can express a DELETION, which a merge cannot", () => {
    const store = new Store();
    store.applySnapshot({
      agents: [
        { id: "pm", role: "PM" },
        { id: "eng", role: "Engineer" },
      ],
    });
    store.applySeats([{ id: "pm", role: "PM" }]);
    expect(store.state.agents.map((a) => a.role)).toEqual(["PM"]);
  });

  test("a seats push keeps the live overlay the roster knows nothing about", () => {
    const store = new Store();
    store.applyAgents([{ role: "PM", state: "working" }]);
    store.applySeats([{ id: "pm", role: "PM", handle: "pm" }]);
    expect(store.state.agents[0]?.state).toBe("working");
    expect(store.state.agents[0]?.handle).toBe("pm");
  });
});

describe("the event feed", () => {
  test("only a PERSISTED event joins the feed", () => {
    // An event with no category is one the server does not store. Streaming
    // it into the feed produced rows that vanished on the next snapshot and
    // 404'd when clicked.
    const store = new Store();
    store.applyEvent({ ...feedRow("e1"), category: "" } as EventEnvelope);
    expect(store.state.events).toHaveLength(0);
    store.applyEvent(feedRow("e2") as EventEnvelope);
    expect(store.state.events).toHaveLength(1);
  });

  test("a duplicate id does not double the row", () => {
    // The hub registers a client BEFORE it sends the snapshot, so the overlap
    // between the two is real and is deduped here.
    const store = new Store();
    store.applyEvent(feedRow("e1") as EventEnvelope);
    store.applyEvent(feedRow("e1") as EventEnvelope);
    expect(store.state.events).toHaveLength(1);
  });

  test("the feed is capped at the server's own retention", () => {
    const store = new Store();
    for (let i = 0; i < MAX_EVENTS + 50; i++) {
      store.applyEvent(feedRow(`e${i}`) as EventEnvelope);
    }
    expect(store.state.events).toHaveLength(MAX_EVENTS);
    // Newest first: the last one published is the first one held.
    expect(store.state.events[0]?.id).toBe(`e${MAX_EVENTS + 49}`);
  });

  test("every envelope reaches an event listener, feed or not", () => {
    // A screen watching for one event type must see the ones the feed
    // declines to keep.
    const store = new Store();
    const seen: string[] = [];
    store.onEvent((ev) => seen.push(ev.id));
    store.applyEvent({ ...feedRow("e1"), category: "" } as EventEnvelope);
    expect(seen).toEqual(["e1"]);
  });
});

describe("connection state", () => {
  test("a snapshot does NOT claim a connection", () => {
    // The degraded-mode REST poll applies a snapshot while the socket is
    // down. Deriving `connected` from a payload's contents announced a live
    // connection that did not exist.
    const store = new Store();
    store.applySnapshot({ health: { status: "ok" }, agents: [] });
    expect(store.state.connected).toBe(false);
  });

  test("a dropped socket CLEARS health rather than freezing it", () => {
    // A stale "healthy" is a lie with a timestamp nobody can see.
    const store = new Store();
    store.applyHealth({ status: "ok" });
    expect(store.state.connected).toBe(true);
    store.setConnected(false);
    expect(store.state.health.status).toBe("unknown");
  });

  test("refused and unreachable are different facts", () => {
    // The repair differs and the reader cannot guess which they are looking
    // at: a stopped engine comes back on its own, a rejected token never does.
    const store = new Store();
    store.setConnected(false);
    expect(store.state.authRejected).toBe(false);
    store.setAuthRejected(true);
    expect(store.state.authRejected).toBe(true);
  });
});

describe("subscriptions", () => {
  test("a listener wakes only for the slices it asked for", () => {
    // An `agents` overlay is pushed TWICE PER TOOL-LOOP ROUND. A store that
    // woke every listener on every envelope would re-render the whole
    // application several times a second for the length of a turn.
    const store = new Store();
    const tokens = vi.fn();
    const agents = vi.fn();
    store.subscribe(["tokens"], tokens);
    store.subscribe(["agents"], agents);

    store.applyAgents([{ role: "PM", state: "working" }]);
    expect(agents).toHaveBeenCalledTimes(1);
    expect(tokens).not.toHaveBeenCalled();
  });

  test("a listener on several changed slices is called once", () => {
    const store = new Store();
    const fn = vi.fn();
    store.subscribe(["sandboxes", "agents"], fn);
    // A sandbox move IS a seat move — a seat's effective state folds in
    // whether it is parked on a question — so both slices change together.
    store.applySandboxes([]);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  test("unsubscribing actually detaches", () => {
    const store = new Store();
    const fn = vi.fn();
    const off = store.subscribe(["agents"], fn);
    off();
    store.applyAgents([{ role: "PM" }]);
    expect(fn).not.toHaveBeenCalled();
  });

  test("each slice carries a version React can compare", () => {
    // The slices are mutated in place — they are large and pushed at high
    // frequency — so the version is what gives each a cheap stable identity
    // for useSyncExternalStore.
    const store = new Store();
    const before = store.version("agents");
    store.applyAgents([{ role: "PM" }]);
    expect(store.version("agents")).toBeGreaterThan(before);
    expect(store.version("tokens")).toBe(0);
  });
});

describe("partial pushes", () => {
  test("a schedules push without recent_runs leaves what was fetched", () => {
    const store = new Store();
    store.applySchedules({
      schedules: [],
      recent_runs: [
        { name: "a", scope: "role", scope_name: "PM", fired_at: "", outcome: "fired", detail: "" },
      ],
    });
    store.applySchedules({ schedules: [] });
    expect(store.state.recentRuns).toHaveLength(1);
  });

  test("a rollup with no totals is refused", () => {
    // applySnapshot requires `totals`; a bare list of records passes neither
    // path and would leave the Spend screen blank with the numbers in memory.
    const store = new Store();
    store.applySnapshot({ tokens: { by_phase: [] } as never });
    expect(store.state.tokens).toBeNull();
  });
});

describe("seat lookup", () => {
  test("a seat resolves by handle, id, role, and case-insensitively", () => {
    // Links minted before seats were addressed by handle used ids and role
    // names, and they are in people's history.
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "uuid-1", role: "Product Manager", handle: "pm" }] });
    for (const key of ["pm", "PM", "uuid-1", "Product Manager", "product manager"]) {
      expect(store.agentByKey(key)?.handle, key).toBe("pm");
    }
    expect(store.agentByKey("nobody")).toBeNull();
  });
});
