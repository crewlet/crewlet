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
import { MAX_EVENTS } from "../contract/wire.ts";
import { MAX_PHASES, Store } from "./store.ts";
import { LiveSocket, QueryError } from "./socket.ts";
import { nodeCountLabel } from "../lib/format.ts";
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
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", handle: "pm", activity: "idle" }] });
    store.applyAgents([{ role: "PM", activity: "working", current_phase: "execute" }]);

    const [row] = store.state.agents;
    expect(row?.handle).toBe("pm");
    expect(row?.activity).toBe("working");
    expect(row?.current_phase).toBe("execute");
  });

  test("a keyed object is DISCARDED rather than half-applied", () => {
    // The server sent a full turn's worth of overlays as an object keyed by
    // role once. Both sides' own suites passed, the socket carried every
    // frame, and the seat rendered idle from the first phase to the last.
    // The guard is what makes that loud rather than silent — and the e2e
    // replay is what makes it impossible to ship again.
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", activity: "idle" }] });
    store.applyAgents({ PM: { activity: "working" } } as never);
    expect(store.state.agents[0]?.activity).toBe("idle");
  });

  test("an overlay for a role the roster does not carry is appended", () => {
    // A live revision can add a seat before the roster push lands.
    const store = new Store();
    store.applyAgents([{ role: "New", activity: "working" }]);
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
    store.applyAgents([{ role: "PM", activity: "working" }]);
    store.applySeats([{ id: "pm", role: "PM", handle: "pm" }]);
    expect(store.state.agents[0]?.activity).toBe("working");
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
});

describe("completed phases", () => {
  // THE DURABLE HALF OF A LIVE PHASE. The projection clears `live_call` the
  // instant a phase completes, and the query that answered a seat's history was
  // answered once, at mount — so without this slice the turn a reader is
  // watching vanishes the moment its review lands, and on a seat's first turn
  // the page is left claiming the seat has never run.
  const phaseEvent = (id: string, over: Record<string, unknown> = {}): EventEnvelope =>
    ({
      ...feedRow(id, { type: "agent_phase_completed", category: "llm" }),
      payload: { turn_id: "t1", phase: "review", iteration: 0, role: "PM", ...over },
    }) as EventEnvelope;

  test("a completed phase is kept WITH its payload", () => {
    const store = new Store();
    store.applyEvent(phaseEvent("p1"));
    expect(store.state.phases).toHaveLength(1);
    expect(store.state.phases[0]?.payload?.phase).toBe("review");
  });

  test("a payload-free row is not kept", () => {
    // Snapshots and the `events` query both answer with payload-free rows. One
    // of those would evict a real record for a phase nothing can render.
    const store = new Store();
    store.applyEvent(feedRow("p1", { type: "agent_phase_completed" }) as EventEnvelope);
    expect(store.state.phases).toHaveLength(0);
  });

  test("only phase completions are kept", () => {
    const store = new Store();
    store.applyEvent({ ...phaseEvent("t1"), type: "agent_turn_completed" });
    expect(store.state.phases).toHaveLength(0);
  });

  test("a redelivered phase does not double", () => {
    const store = new Store();
    store.applyEvent(phaseEvent("p1"));
    store.applyEvent(phaseEvent("p1"));
    expect(store.state.phases).toHaveLength(1);
  });

  test("the buffer is bounded, newest first", () => {
    // The payloads carry verbatim prompts, responses and tool results, so an
    // unbounded buffer grows with the length of a session.
    const store = new Store();
    for (let i = 0; i < MAX_PHASES + 10; i++) store.applyEvent(phaseEvent(`p${i}`));
    expect(store.state.phases).toHaveLength(MAX_PHASES);
    expect(store.state.phases[0]?.id).toBe(`p${MAX_PHASES + 9}`);
  });

  test("a snapshot leaves the slice alone", () => {
    // The snapshot carries payload-free feed rows and no phase payloads, so a
    // reconnect must add to this rather than blank it — the records are still
    // true, and each screen re-asks its own query anyway.
    const store = new Store();
    store.applyEvent(phaseEvent("p1"));
    store.applySnapshot({ agents: [], events: [] });
    expect(store.state.phases).toHaveLength(1);
  });

  test("a phase completion wakes only phase readers", () => {
    // `agents` is pushed twice per tool-loop round; a slice that woke every
    // listener would re-render the whole application several times a second.
    const store = new Store();
    const agentsWoke = vi.fn();
    const phasesWoke = vi.fn();
    store.subscribe(["agents"], agentsWoke);
    store.subscribe(["phases"], phasesWoke);
    store.applyEvent(phaseEvent("p1"));
    expect(phasesWoke).toHaveBeenCalled();
    expect(agentsWoke).not.toHaveBeenCalled();
  });
});

describe("the engine's health", () => {
  // THE PUSH IS THE WHOLE ENVELOPE, and the slice keeps all of it: every
  // screen reads the applied epoch, the posture and the fleet's size off this
  // slice rather than asking a query for them.
  test("a health frame is kept whole", () => {
    const store = new Store();
    const frame = {
      status: "ok",
      node: "node-a",
      applied_epoch: 41,
      posture: "serve",
      nodes: 3,
      alarms: { count: 1, worst: "backup_age" },
    };
    store.applyHealth(frame);
    expect(store.state.health).toEqual(frame);
    expect(nodeCountLabel(store.state.health.nodes)).toBe("3 nodes");
  });

  // AN OLDER NODE'S FRAME, or one whose presence read failed, carries no
  // `nodes` at all. The count is then unknown and said so — never 0, which the
  // node answering could not be, and never a guessed 1.
  test("a frame without a node count renders the count as unavailable", () => {
    const store = new Store();
    store.applyHealth({ status: "ok", applied_epoch: 7, posture: "serve" });
    expect(store.state.health.nodes).toBeUndefined();
    expect(nodeCountLabel(store.state.health.nodes)).toBe("node count unavailable");
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

    store.applyAgents([{ role: "PM", activity: "working" }]);
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

describe("a peer this build was not built against", () => {
  // A FLEET MID-UPGRADE has a node pushing what this bundle was built before.
  // Its unknown kind must neither throw nor land in a slice by a guess — and it
  // must not vanish either, because the same fall-through is what this build's
  // own engine sending a kind its own client forgot looks like. The e2e replay
  // asserts the count is zero for exactly that reason.
  test("an unknown push kind is ignored and counted", () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const before = JSON.stringify(store.state);
    const woken = vi.fn();
    store.subscribe(["agents", "events", "health", "budget"], woken);

    socket.onMessage(JSON.stringify({ kind: "hologram", data: { agents: [] } }));
    socket.onMessage(JSON.stringify({ kind: "hologram", data: {} }));
    socket.onMessage(JSON.stringify({ data: {} }));

    expect(JSON.stringify(store.state)).toBe(before);
    expect(woken).not.toHaveBeenCalled();
    expect(Object.fromEntries(store.unknownPushes)).toEqual({ hologram: 2, null: 1 });
  });

  test("every kind this build dispatches is not counted", () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    for (const kind of ["snapshot", "event", "agents", "seats", "sandboxes", "tokens"]) {
      socket.onMessage(JSON.stringify({ kind, data: kind === "snapshot" ? {} : [] }));
    }
    for (const kind of ["budget", "schedules", "org", "tools", "health", "pong"]) {
      socket.onMessage(JSON.stringify({ kind, data: {} }));
    }
    socket.onMessage(JSON.stringify({ kind: "result", id: 99, data: {} }));
    socket.onMessage(JSON.stringify({ kind: "error", id: 99, error: "not_found" }));
    expect(store.unknownPushes.size).toBe(0);
  });

  // EVERY FIELD THIS PR ADDED IS OPTIONAL, because an older node's snapshot
  // does not carry it. Applying one must neither throw nor invent a value: a
  // seat with no `activity`, a budget with no clock and a health frame with no
  // alarm count stay absent, which is what each screen's "unknown" branch
  // reads.
  test("an older node's snapshot applies with this build's fields absent", () => {
    const store = new Store();
    store.applySnapshot({
      agents: [{ id: "pm", role: "PM", handle: "pm" }],
      budget: {},
      health: { status: "ok" },
    });
    const [pm] = store.state.agents;
    expect(pm?.activity).toBeUndefined();
    expect(pm?.turn).toBeUndefined();
    expect(pm?.paused).toBeUndefined();
    expect(store.state.budget.timezone).toBeUndefined();
    expect(store.state.budget.org).toBeUndefined();
    expect(store.state.health.alarms).toBeUndefined();
    expect(nodeCountLabel(store.state.health.nodes)).toBe("node count unavailable");
  });
});

describe("a refused query", () => {
  // THE WAIT THE ENGINE NAMED TRAVELS WITH THE REFUSAL. The socket used to
  // reject with the bare code, so `retry_after_seconds` reached no screen and
  // each guessed a wait of its own.
  test("an unavailable frame's retry hint reaches the rejection", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("fleet");
    socket.onMessage(
      JSON.stringify({ kind: "error", id: 1, error: "unavailable", retry_after_seconds: 4 }),
    );
    const err = await asked.catch((e: unknown) => e);
    expect(err).toBeInstanceOf(QueryError);
    expect((err as QueryError).message).toBe("unavailable");
    expect((err as QueryError).retryAfterSeconds).toBe(4);
  });

  test("a refusal that names no wait carries none", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("fleet");
    socket.onMessage(JSON.stringify({ kind: "error", id: 1, error: "not_found" }));
    const err = await asked.catch((e: unknown) => e);
    expect((err as QueryError).retryAfterSeconds).toBeNull();
  });
});
