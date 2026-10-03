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
import { LiveSocket, queryFailure, QueryRefusedError } from "./socket.ts";
import { MAX_PHASES, Store } from "./store.ts";
import type { AgentRow, EventEnvelope, FeedRow } from "./types.ts";

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
    store.applySnapshot({
      agents: [{ id: "pm", agent_id: "id-pm", role: "PM", handle: "pm", activity: "idle" }],
    });
    store.applyAgents([{ agent_id: "id-pm", activity: "working", current_phase: "execute" }]);

    const [row] = store.state.agents;
    expect(row?.handle).toBe("pm");
    expect(row?.activity).toBe("working");
    expect(row?.current_phase).toBe("execute");
  });

  test("two seats sharing a display name keep separate live state", () => {
    // A seat's name is prose and the chart lets two seats carry the same one.
    // Merged by name, each overlay landed on BOTH cards, so each rendered
    // whatever the other was last doing.
    const store = new Store();
    store.applySnapshot({
      agents: [
        { id: "eng-a", agent_id: "id-a", role: "Engineer", handle: "eng-a", activity: "idle" },
        { id: "eng-b", agent_id: "id-b", role: "Engineer", handle: "eng-b", activity: "idle" },
      ],
    });
    store.applyAgents([{ agent_id: "id-b", activity: "working", current_phase: "execute" }]);

    const [a, b] = store.state.agents;
    expect(a?.activity).toBe("idle");
    expect(a?.current_phase).toBeUndefined();
    expect(b?.activity).toBe("working");
    expect(b?.current_phase).toBe("execute");
  });

  test("a keyed object is DISCARDED rather than half-applied", () => {
    // The server sent a full turn's worth of overlays as an object keyed by
    // role once. Both sides' own suites passed, the socket carried every
    // frame, and the seat rendered idle from the first phase to the last.
    // The guard is what makes that loud rather than silent — and the e2e
    // replay is what makes it impossible to ship again.
    const store = new Store();
    store.applySnapshot({
      agents: [{ id: "pm", agent_id: "id-pm", role: "PM", activity: "idle" }],
    });
    store.applyAgents({ "id-pm": { activity: "working" } } as never);
    expect(store.state.agents[0]?.activity).toBe("idle");
  });

  test("an overlay for a seat the roster does not carry is dropped, and nothing redraws", () => {
    // A seat reaches the list through the roster, which carries its merged
    // overlay; an appended row would be a card with no name, no handle and
    // no page, for a seat the server may already have removed.
    const store = new Store();
    const seen = vi.fn();
    store.subscribe(["agents"], seen);
    store.applyAgents([{ agent_id: "id-new", activity: "working" }]);
    expect(store.state.agents).toHaveLength(0);
    expect(seen).not.toHaveBeenCalled();
  });

  test("a seats push can express a DELETION, which a merge cannot", () => {
    const store = new Store();
    store.applySnapshot({
      agents: [
        { id: "pm", agent_id: "id-pm", role: "PM" },
        { id: "eng", agent_id: "id-eng", role: "Engineer" },
      ],
    });
    store.applySeats([{ id: "pm", agent_id: "id-pm", role: "PM" }]);
    expect(store.state.agents.map((a) => a.role)).toEqual(["PM"]);
  });

  test("a seats push keeps the live overlay of a seat it renamed", () => {
    // The roster push after a rename carries a new handle and a new name for
    // the same agent id, and the seat is still mid-turn.
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", agent_id: "id-pm", role: "PM", handle: "pm" }] });
    store.applyAgents([{ agent_id: "id-pm", activity: "working" }]);
    store.applySeats([{ id: "lead", agent_id: "id-pm", role: "Product Lead", handle: "lead" }]);
    expect(store.state.agents[0]?.activity).toBe("working");
    expect(store.state.agents[0]?.handle).toBe("lead");
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

  test("an unverifiable identity is this socket's, and a new socket starts clear", () => {
    // The engine degrades ONE socket whose credential it cannot check while
    // the node itself stays healthy — so the hold arrives on its own frame and
    // never rides the node's health, and it belongs to the socket that
    // reported it: a reconnect's handshake resolved the credential afresh.
    const store = new Store();
    store.applyHealth({ status: "ok" });
    store.applyIdentity({ state: "unverifiable" });
    expect(store.state.identityUnverifiable).toBe(true);
    expect(store.state.health.status).toBe("ok");
    store.applyIdentity({ state: "verified" });
    expect(store.state.identityUnverifiable).toBe(false);

    store.applyIdentity({ state: "unverifiable" });
    store.setConnected(false);
    expect(store.state.identityUnverifiable).toBe(false);
  });

  test("refused access is a reason, and survives the disconnect it caused", () => {
    // Unlike an identity hold, a refusal is not a fact about the socket that
    // reported it: the socket stops because of it, so a disconnect clearing it
    // would leave a page saying "reconnecting" that never will.
    const store = new Store();
    store.setAccessRefused("grant withdrawn: state:read");
    store.setConnected(false);
    expect(store.state.accessRefused).toBe("grant withdrawn: state:read");
    store.setAccessRefused("");
    expect(store.state.accessRefused).toBe("refused");
    store.setAccessRefused(null);
    expect(store.state.accessRefused).toBeNull();
  });
});

describe("subscriptions", () => {
  const PM: AgentRow = { id: "pm", agent_id: "id-pm", role: "PM", handle: "pm" };

  test("a listener wakes only for the slices it asked for", () => {
    // An `agents` overlay is pushed TWICE PER TOOL-LOOP ROUND. A store that
    // woke every listener on every envelope would re-render the whole
    // application several times a second for the length of a turn.
    const store = new Store();
    store.applySeats([PM]);
    const tokens = vi.fn();
    const agents = vi.fn();
    store.subscribe(["tokens"], tokens);
    store.subscribe(["agents"], agents);

    store.applyAgents([{ agent_id: PM.agent_id, activity: "working" }]);
    expect(agents).toHaveBeenCalledTimes(1);
    expect(tokens).not.toHaveBeenCalled();
  });

  test("a listener on several changed slices is called once", () => {
    const store = new Store();
    const fn = vi.fn();
    store.subscribe(["sandboxes", "agents"], fn);
    // A sandbox move IS a seat move — a seat's effective state folds in
    // whether it is parked on a question — so both slices change together.
    store.applySandboxes([{ turn_id: "t-1" } as never]);
    expect(fn).toHaveBeenCalledTimes(1);
  });

  test("unsubscribing actually detaches", () => {
    // Against a seat the roster carries, so the overlay really does move the
    // slice — an overlay for nobody emits nothing, and would pass this with
    // the listener still attached.
    const store = new Store();
    store.applySeats([PM]);
    const fn = vi.fn();
    const off = store.subscribe(["agents"], fn);
    off();
    store.applyAgents([{ agent_id: PM.agent_id, activity: "working" }]);
    expect(fn).not.toHaveBeenCalled();
  });

  test("each slice carries a version React can compare", () => {
    // The slices are mutated in place — they are large and pushed at high
    // frequency — so the version is what gives each a cheap stable identity
    // for useSyncExternalStore.
    const store = new Store();
    store.applySeats([PM]);
    const before = store.version("agents");
    store.applyAgents([{ agent_id: PM.agent_id, activity: "working" }]);
    expect(store.version("agents")).toBeGreaterThan(before);
    expect(store.version("tokens")).toBe(0);
  });
});

// A PUSH IS AN ANSWER LIKE ANY OTHER, parsed afresh off the wire. A spend
// rollup is pushed after every phase, and handed over whole it gave the spend
// tables a new object for every seat and every turn — every row of both drawn
// again for the one turn that finished. Shared with what the slice held, a push
// that said nothing new wakes nobody, and one that did keeps the objects of
// everything it left alone.
describe("a push shares what it did not change", () => {
  function rollup(turns: string[]) {
    return JSON.parse(
      JSON.stringify({
        since: "2026-09-30T00:00:00Z",
        until: "2026-09-30T12:00:00Z",
        agent_id: "",
        totals: { total_tokens: 10 },
        by_phase: [],
        by_model: [],
        by_worker: [],
        by_agent: [{ agent_id: "id-pm", total_tokens: 10 }],
        by_turn: turns.map((id) => ({ turn_id: id, total_tokens: 1 })),
      }),
    ) as never;
  }

  test("a push that changed nothing moves no version and wakes nobody", () => {
    const store = new Store();
    store.applyTokens(rollup(["t-1", "t-2"]));
    const held = store.state.tokens;
    const woke = vi.fn();
    store.subscribe(["tokens"], woke);
    const version = store.version("tokens");

    store.applyTokens(rollup(["t-1", "t-2"]));
    expect(store.state.tokens).toBe(held);
    expect(store.version("tokens")).toBe(version);
    expect(woke).not.toHaveBeenCalled();
  });

  test("a push that changed one row keeps every other row's object", () => {
    const store = new Store();
    store.applyTokens(rollup(["t-1", "t-2"]));
    const before = store.state.tokens!;
    store.applyTokens(rollup(["t-0", "t-1", "t-2"]));
    const after = store.state.tokens!;
    expect(after).not.toBe(before);
    expect(after.by_turn!.map((t) => t.turn_id)).toEqual(["t-0", "t-1", "t-2"]);
    expect(after.by_turn![1]).toBe(before.by_turn![0]);
    expect(after.by_turn![2]).toBe(before.by_turn![1]);
    expect(after.by_agent).toBe(before.by_agent);
  });

  test("an overlay restating a seat's state moves nothing", () => {
    const store = new Store();
    store.applySeats([{ id: "pm", agent_id: "id-pm", role: "PM", handle: "pm", activity: "idle" }]);
    const held = store.state.agents;
    const woke = vi.fn();
    store.subscribe(["agents"], woke);
    store.applyAgents([{ agent_id: "id-pm", activity: "idle" }]);
    expect(store.state.agents).toBe(held);
    expect(woke).not.toHaveBeenCalled();
  });

  // AN ORG PUSH IS ALSO AN EVENT: it follows a chart write that landed, and a
  // write that changed only what the projection leaves out — a seat's model
  // chain, its credentials — is pushed as a projection deep-equal to the last.
  // The projection does not move; the push is still counted, and a screen
  // holding a chart read reads it again on the count.
  test("an org push that restates the projection is still counted", () => {
    const store = new Store();
    const org = { name: "Acme", roles: [{ name: "Ada", handle: "ada" }] } as never;
    store.applyOrg(org);
    const held = store.state.org;
    const projection = vi.fn();
    const pushes = vi.fn();
    store.subscribe(["org"], projection);
    store.subscribe(["orgPushes"], pushes);

    store.applyOrg(JSON.parse(JSON.stringify(org)) as never);
    expect(store.state.org).toBe(held);
    expect(projection).not.toHaveBeenCalled();
    expect(pushes).toHaveBeenCalledTimes(1);
    expect(store.state.orgPushes).toBe(2);
  });

  test("a snapshot announces only the slices it moved", () => {
    const store = new Store();
    store.applySnapshot({ agents: [], events: [], tools: [{ name: "x" } as never] });
    const tools = vi.fn();
    const agents = vi.fn();
    store.subscribe(["tools"], tools);
    store.subscribe(["agents"], agents);
    store.applySnapshot({
      agents: [{ id: "pm", agent_id: "id-pm", role: "PM", handle: "pm" }],
      events: [],
      tools: [{ name: "x" } as never],
    });
    expect(agents).toHaveBeenCalledTimes(1);
    expect(tools).not.toHaveBeenCalled();
  });

  test("health that did not move wakes nobody, and a connection that did still does", () => {
    // A SNAPSHOT CARRIES HEALTH AND CLAIMS NO CONNECTION, so the first health
    // frame after it restates the health and moves only `connected` — which
    // is still a move, and the one the connection banner is waiting for.
    const store = new Store();
    store.applySnapshot({ health: { status: "ok" }, agents: [] });
    const woke = vi.fn();
    store.subscribe(["health"], woke);
    store.applyHealth({ status: "ok" });
    expect(store.state.connected).toBe(true);
    expect(woke).toHaveBeenCalledTimes(1);
    store.applyHealth({ status: "ok" });
    expect(woke).toHaveBeenCalledTimes(1);
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

// NOTHING SAID IS NOT AN EMPTY COMPANY. `{}` is the projection of a node
// running none, which is an answer; before the handshake there is no answer,
// and a screen has to be able to tell — the charter printed "No mission is
// set" on every cold tab.
describe("the org chart", () => {
  test("is null until the engine sends one, and an empty one once it has", () => {
    const store = new Store();
    expect(store.state.org).toBeNull();
    store.applyOrg(null);
    expect(store.state.org).toEqual({});
    const fresh = new Store();
    fresh.applySnapshot({ agents: [] });
    expect(fresh.state.org).toEqual({});
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
      alarms: { count: 1, worst: "trim_blocked", worst_domain: "tracker" },
      identity: "ready",
    };
    store.applyHealth(frame);
    expect(store.state.health).toEqual(frame);
  });

  // A FRAME WHOSE PRESENCE READ FAILED carries no `nodes` at all. The count is
  // then unknown — never 0, which the node answering could not be.
  test("a frame without a node count keeps the count absent", () => {
    const store = new Store();
    store.applyHealth({ status: "ok", applied_epoch: 7, posture: "serve" });
    expect(store.state.health.nodes).toBeUndefined();
  });
});

// NOT REPORTED IS NOT UNCAPPED. The budget slice is `null` until a report has
// carried it, from the store's birth, through a snapshot taken before any node
// reported (`budget: null`) and after a push of `null`; a report that caps
// nothing is held as the report it is. An empty object used to stand for both,
// and the Spend and Home tiles offered an operator of a capped company "Set
// one" for the first seconds after every engine start.
describe("the budget", () => {
  test("is null until a report carries it, and an uncapped report is kept", () => {
    const store = new Store();
    expect(store.state.budget).toBeNull();
    store.applySnapshot({ budget: null, health: { status: "ok" } });
    expect(store.state.budget).toBeNull();
    const uncapped = { meter_id: "n:1", seq: 1, timezone: "UTC", org: { windows: [] } };
    store.applyBudget(uncapped as never);
    expect(store.state.budget).toEqual(uncapped);
    store.applyBudget(null);
    expect(store.state.budget).toBeNull();
  });
});

describe("a kind this build does not dispatch", () => {
  // A NODE ON ANOTHER BUILD may push what this bundle was built before. Its
  // unknown kind must neither throw nor land in a slice by a guess — and it
  // must not vanish either, because the same fall-through is what this
  // build's own engine sending a kind its own client forgot looks like. The
  // e2e replay asserts the count is zero for exactly that reason.
  test("is ignored and counted", () => {
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
    socket.onMessage(JSON.stringify({ kind: "inbox_changed", data: { handle: "ada" } }));
    socket.onMessage(JSON.stringify({ kind: "identity", data: { state: "verified" } }));
    socket.onMessage(JSON.stringify({ kind: "result", id: 99, data: {} }));
    socket.onMessage(JSON.stringify({ kind: "error", id: 99, error: "not_found" }));
    expect(store.unknownPushes.size).toBe(0);
  });
});

describe("a refused query", () => {
  // THE REFUSAL'S SENTENCE TRAVELS WITH IT. A `bad_params` refusal is the one
  // the engine writes for the caller — which parameter, and what it accepts —
  // and a rejection carrying only the code left a screen to say "something
  // was missing" about a window the reader chose.
  test("a bad_params frame's sentence reaches the rejection", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("tokens", { days: 91 });
    socket.onMessage(
      JSON.stringify({
        kind: "error",
        id: 1,
        error: "bad_params",
        detail: "days=91, and a spend window is 1 to 90 company days — ask for at most 90",
      }),
    );
    const err = await asked.catch((e: unknown) => e);
    expect(err).toBeInstanceOf(QueryRefusedError);
    expect(queryFailure(err)).toEqual({
      error: "bad_params",
      refusal: null,
      detail: "days=91, and a spend window is 1 to 90 company days — ask for at most 90",
    });
  });

  // AN `unavailable` FRAME'S WORDS ARE ITS REFUSAL'S, beside the wait the
  // engine named — never the bad_params sentence, which no wait changes.
  test("an unavailable frame's words and wait ride its refusal", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("fleet");
    socket.onMessage(
      JSON.stringify({
        kind: "error",
        id: 1,
        error: "unavailable",
        refusal: "behind",
        detail: "catching up",
        retry_after: 4,
      }),
    );
    const failure = queryFailure(await asked.catch((e: unknown) => e));
    expect(failure).toEqual({
      error: "unavailable",
      refusal: { code: "behind", detail: "catching up", retryAfter: 4 },
      detail: null,
    });
  });
});
