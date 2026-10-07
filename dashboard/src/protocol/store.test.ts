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
import { LIVE_CALL_DETAIL, MAX_EVENTS } from "../contract/wire.ts";
import { MAX_PHASES, Store } from "./store.ts";
import { LiveSocket, QueryError } from "./socket.ts";
import { nodeCountLabel } from "../lib/format.ts";
import type { EventEnvelope, FeedRow, Overlay } from "./types.ts";

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
    fresh.applySnapshot({ agents: [] } as never);
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

describe("a live call's heavy fields", () => {
  // What the engine pushes for one call: its identity, its light fields, and
  // every heavy field's version — with only the fields it names carried.
  function pushed(
    versions: Partial<Record<keyof typeof LIVE_CALL_DETAIL, number>>,
    carried: Record<string, unknown>,
    over: Record<string, unknown> = {},
  ): Overlay & { role: string } {
    return {
      role: "PM",
      live_call: {
        turn_id: "t1",
        phase: "execute",
        iteration: 0,
        model: "m",
        round_num: 2,
        rounds_used: 3,
        input_tokens: 0,
        output_tokens: 0,
        total_tokens: 0,
        trigger: null,
        in_progress: true,
        updated_at: "2026-10-06T09:00:00Z",
        versions: { prompt: 0, response: 0, narration: 0, executions: 0, rounds: 0, ...versions },
        ...carried,
        ...over,
      } as never,
    };
  }
  const prompt = { prompt: "fix it", prompt_messages: [{ role: "system", content: "lead" }] };
  const narration = (n: number) => ({
    round_narration: Array.from({ length: n }, (_, i) => ({ round: i + 1, content: `r${i + 1}` })),
  });

  // A PUSH LEAVES OUT WHAT DID NOT MOVE, and the tab keeps the copy it holds:
  // the prompt the opening frame carried stays on screen through every later
  // frame that does not, and the narration moves when a frame carries it.
  //
  // Mutation: replace the call with the push's, and the prompt is blank from
  // the second frame on.
  test("keeps the copy it holds of a field a push leaves out", () => {
    const store = new Store();
    expect(
      store.applyAgents([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]),
    ).toEqual([]);
    expect(store.applyAgents([pushed({ prompt: 1, narration: 2 }, narration(2))])).toEqual([]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.prompt).toBe("fix it");
    expect(call?.prompt_messages).toEqual(prompt.prompt_messages);
    expect(call?.round_narration).toHaveLength(2);
    expect(call?.versions).toMatchObject({ prompt: 1, narration: 2 });
  });

  // A PUSH NAMING A NEWER VERSION THAN THE ONE HELD, WITHOUT THE FIELD, is a tab
  // that missed the push which carried it — the server's queue drops a slow
  // tab's oldest frame. The tab says so, keeps what it holds, and takes the
  // fetched call whole.
  test("says when it missed a change, and takes the call fetched whole", () => {
    const store = new Store();
    store.applyAgents([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]);
    // The push carrying narration 2 was dropped; this one names 3 and carries nothing.
    expect(store.applyAgents([pushed({ prompt: 1, narration: 3 }, {})])).toEqual(["PM"]);
    expect(store.state.agents[0]?.live_call?.round_narration).toHaveLength(1);

    store.applyLiveCall({
      role: "PM",
      live_call:
        pushed({ prompt: 1, narration: 3 }, { ...prompt, ...narration(3) }).live_call ?? null,
    });
    const call = store.state.agents[0]?.live_call;
    expect(call?.round_narration).toHaveLength(3);
    expect(call?.versions?.narration).toBe(3);
    expect(store.applyAgents([pushed({ prompt: 1, narration: 3 }, {})])).toEqual([]);
  });

  // AN OLDER COPY NEVER REPLACES A NEWER ONE: a push or an answer overtaken by
  // what the tab already holds is read for its light fields only.
  test("never takes a field back to an older copy", () => {
    const store = new Store();
    store.applyAgents([pushed({ prompt: 1, narration: 3 }, { ...prompt, ...narration(3) })]);
    store.applyAgents([pushed({ prompt: 1, narration: 2 }, narration(2), { model: "later" })]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.round_narration).toHaveLength(3);
    expect(call?.model).toBe("later");
  });

  // A CALL OF ITS OWN HOLDS NOTHING OF THE LAST ONE: a new phase whose push
  // left a field out is not drawn with the previous phase's prompt.
  test("carries nothing from one call to the next", () => {
    const store = new Store();
    store.applyAgents([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]);
    const stale = store.applyAgents([pushed({ prompt: 1, narration: 0 }, {}, { phase: "review" })]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.phase).toBe("review");
    expect(call?.prompt).toBe("");
    expect(call?.round_narration).toBeNull();
    expect(stale).toEqual(["PM"]);
  });

  // A CALL BUILT AGAIN UNDER ITS OWN KEY IS NEWER IN EVERY FIELD. A suspended
  // Execute phase's checkpoint clears its call, and its resumed rounds stream
  // under the same turn, phase and iteration; a tab that missed the push
  // clearing it and the first push after holds the call from before the
  // suspension. The engine never hands a version out twice, so the next push
  // names versions past every one the tab holds: a field it carries is taken,
  // and one it leaves out says the tab is behind, and is fetched whole. Counted
  // per call, the resumed versions were below the held ones, and the tab kept
  // the old call and asked for nothing — the engine's half of that is
  // livestate's TestACallBuiltAgainUnderItsKeyIsNewerThanTheOneBefore.
  //
  // Mutation: stop reporting a field left out at a newer version, and the
  // prompt from before the suspension stays on screen.
  test("takes a call built again under its key, after missing its clearing", () => {
    const store = new Store();
    store.applyAgents([
      pushed(
        { prompt: 5, response: 5, narration: 5 },
        { ...prompt, ...narration(3), response: "old" },
      ),
    ]);
    // The clearing push and the resumed call's first push were dropped.
    const stale = store.applyAgents([
      pushed({ prompt: 9, response: 11, narration: 11 }, { ...narration(1), response: "new" }),
    ]);
    let call = store.state.agents[0]?.live_call;
    expect(call?.response).toBe("new");
    expect(call?.round_narration).toHaveLength(1);
    expect(stale).toEqual(["PM"]);

    const resumed = { prompt: "resume it", prompt_messages: [{ role: "system", content: "lead" }] };
    store.applyLiveCall({
      role: "PM",
      live_call:
        pushed(
          { prompt: 9, response: 11, narration: 11 },
          { ...resumed, ...narration(1), response: "new" },
        ).live_call ?? null,
    });
    call = store.state.agents[0]?.live_call;
    expect(call?.prompt).toBe("resume it");
    expect(call?.versions).toMatchObject({ prompt: 9, response: 11, narration: 11 });
  });

  // THE SOCKET ASKS FOR THE CALL WHOLE, once a seat while an ask is out, and the
  // answer lands on the row.
  test("the socket fetches a call it holds behind, once", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const ask = vi.spyOn(socket, "query");
    const frame = (data: unknown) => socket.onMessage(JSON.stringify({ kind: "agents", data }));
    frame([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]);
    frame([pushed({ prompt: 1, narration: 3 }, {})]);
    frame([pushed({ prompt: 1, narration: 4 }, {})]);
    expect(ask).toHaveBeenCalledTimes(1);
    expect(ask).toHaveBeenCalledWith("live_call", { role: "PM" });

    socket.onMessage(
      JSON.stringify({
        kind: "result",
        id: 1,
        data: {
          role: "PM",
          live_call: pushed({ prompt: 1, narration: 4 }, { ...prompt, ...narration(4) }).live_call,
        },
      }),
    );
    await vi.waitFor(() =>
      expect(store.state.agents[0]?.live_call?.round_narration).toHaveLength(4),
    );
  });
});

describe("a live_call answer the socket delivers late", () => {
  // A running call on one seat, and the clear and the new call that follow it,
  // each stamped with the overlay sequence the engine reads when the seat's
  // call changes. `seq` is absent on a running row only where a case does not
  // need it; present where ordering is the point.
  const running = (seq: number, phase = "execute"): Overlay & { role: string } => ({
    role: "PM",
    live_call_seq: seq,
    live_call: {
      turn_id: "t1",
      phase,
      iteration: 0,
      in_progress: true,
      versions: { prompt: 1, response: 1, narration: 1, executions: 1, rounds: 1 },
    } as never,
  });
  // The same running call at `seq`, its narration at version `narration`,
  // carrying only the fields in `carried` — a lean push, or an answer whole.
  const call = (
    seq: number,
    narration: number,
    carried: object,
    over: object = {},
  ): Overlay & { role: string } => ({
    role: "PM",
    live_call_seq: seq,
    live_call: {
      ...running(seq).live_call,
      versions: { prompt: 1, response: 1, narration, executions: 1, rounds: 1 },
      ...carried,
      ...over,
    } as never,
  });
  // Every heavy field of the call, `n` rounds narrated: what an opening push or
  // an answer carries.
  const whole = (n: number) => ({
    prompt: "fix it",
    prompt_messages: [],
    response: "",
    tool_executions: [],
    rounds: [],
    round_narration: Array.from({ length: n }, (_, i) => ({ round: i + 1, content: `r${i + 1}` })),
  });

  // THE ANSWER CANNOT PUT A CLEARED CALL BACK ON SCREEN. A slow tab drops a
  // push, the next push marks the seat stale and the socket asks for the call
  // whole; while that query is out the phase completes and the engine pushes
  // the seat with live_call null, delivered before the answer. The answer
  // carries the running call at an OLDER sequence and is dropped, so the seat
  // stays cleared — not a phase rendering as running on a seat that stopped.
  //
  // Mutation: drop the live_call_seq gate in applyLiveCall, and the cleared
  // seat shows the old running call again.
  test("a clear is not undone by an answer read before it", () => {
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", activity: "working" }] });
    store.applyAgents([running(10)]);
    // The completion push clears the call at a newer sequence.
    store.applyAgents([{ role: "PM", activity: "idle", live_call: null, live_call_seq: 12 }]);
    expect(store.state.agents[0]?.live_call).toBeNull();

    // The overtaken answer — the running call read a moment before the clear.
    store.applyLiveCall({
      role: "PM",
      live_call: running(10).live_call ?? null,
      live_call_seq: 10,
    });
    expect(store.state.agents[0]?.live_call).toBeNull();
    expect(store.state.agents[0]?.live_call_seq).toBe(12);
  });

  // NOR A NEWER CALL. The push that overtook the answer began a new phase
  // rather than clearing; the older answer must not replace it.
  test("a new call is not replaced by an answer for the one before it", () => {
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", activity: "working" }] });
    store.applyAgents([running(10, "execute")]);
    store.applyAgents([running(12, "review")]);
    store.applyLiveCall({
      role: "PM",
      live_call: running(10, "execute").live_call ?? null,
      live_call_seq: 10,
    });
    expect(store.state.agents[0]?.live_call?.phase).toBe("review");
  });

  // BUT AN ANSWER FOR THE SAME CALL STILL BRINGS WHAT THE TAB MISSED. The fetch
  // goes out while rounds stream, so the commonest push to overtake its answer
  // is a lean one for the very call it asked about, still naming a version the
  // tab lacks. The answer is behind that push's sequence and older in its light
  // fields, but its heavy fields are the newest copies the tab can get, and
  // they are what it asked for. Dropped whole, the tab kept the old narration
  // until the seat's next push, which a frozen call never sends.
  //
  // Mutation: drop an answer behind the applied sequence whole, as the gate
  // first did, and the narration stays at one round.
  test("an answer the same call's pushes overtook still brings its newer fields", () => {
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", activity: "working" }] });
    store.applyAgents([call(5, 1, whole(1))]);
    // The push carrying narration 9 was dropped: this one names 9 and carries
    // nothing, so the socket asks for the call whole.
    expect(store.applyAgents([call(10, 9, {})])).toEqual(["PM"]);
    // While that ask is out a later round lands, still naming 9.
    expect(store.applyAgents([call(12, 9, {}, { model: "later" })])).toEqual(["PM"]);

    // The answer was read at 10: behind the applied 12, the same call.
    const current = store.applyLiveCall({
      role: "PM",
      live_call_seq: 10,
      live_call: call(10, 9, whole(9), { model: "earlier" }).live_call ?? null,
    });
    const held = store.state.agents[0]?.live_call;
    expect(current).toBe(false);
    expect(held?.round_narration).toHaveLength(9);
    expect(held?.versions?.narration).toBe(9);
    // The light fields stay the newer push's, and so does the sequence.
    expect(held?.model).toBe("later");
    expect(store.state.agents[0]?.live_call_seq).toBe(12);
    // And the tab no longer reads itself behind.
    expect(store.applyAgents([call(13, 9, {})])).toEqual([]);
  });

  // THE SOCKET ASKS AGAIN WHEN A PUSH FOUND THE CALL BEHIND WHILE ITS ASK WAS
  // OUT and the answer lands behind that push: the answer was read before the
  // change the push named, so even with every field it has newer taken, the
  // tab is still missing one. One ask per seat stays in flight; the push that
  // arrived meanwhile is remembered rather than read as the same thing said
  // twice, and nothing waits on a next push that a frozen call never sends. An
  // answer at or past those pushes holds everything they named, and ends it.
  //
  // Mutation: forget a push that found the call behind while an ask was out,
  // and the second ask is never made.
  test("the socket asks again for a call a push found behind while its ask was out", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const ask = vi.spyOn(socket, "query");
    const frame = (data: unknown) => socket.onMessage(JSON.stringify({ kind: "agents", data }));
    const answer = (id: number, row: Overlay & { role: string }) =>
      socket.onMessage(JSON.stringify({ kind: "result", id, data: row }));
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", activity: "working" }] });
    frame([call(5, 1, whole(1))]);
    frame([call(10, 3, {})]);
    frame([call(12, 4, {})]);
    expect(ask).toHaveBeenCalledTimes(1);

    // Read at 10: narration 3 is taken, and 4 is still missing.
    answer(1, call(10, 3, whole(3)));
    await vi.waitFor(() => expect(ask).toHaveBeenCalledTimes(2));
    expect(store.state.agents[0]?.live_call?.round_narration).toHaveLength(3);

    answer(2, call(12, 4, whole(4)));
    await vi.waitFor(() =>
      expect(store.state.agents[0]?.live_call?.round_narration).toHaveLength(4),
    );
    expect(store.state.agents[0]?.live_call_seq).toBe(12);

    // An answer at the applied sequence ends it, a push meanwhile or not.
    frame([call(14, 6, {})]);
    frame([call(15, 6, {})]);
    expect(ask).toHaveBeenCalledTimes(3);
    answer(3, call(15, 6, whole(6)));
    await vi.waitFor(() =>
      expect(store.state.agents[0]?.live_call?.round_narration).toHaveLength(6),
    );
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(ask).toHaveBeenCalledTimes(3);
  });

  // AND A GENUINELY NEWER ANSWER STILL LANDS: an answer at or past the applied
  // sequence is the repair the fetch exists for.
  test("an answer at a newer sequence is applied", () => {
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", role: "PM", activity: "working" }] });
    store.applyAgents([{ role: "PM", activity: "idle", live_call: null, live_call_seq: 8 }]);
    store.applyLiveCall({
      role: "PM",
      live_call: running(14).live_call ?? null,
      live_call_seq: 14,
    });
    expect(store.state.agents[0]?.live_call?.phase).toBe("execute");
    expect(store.state.agents[0]?.live_call_seq).toBe(14);
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
  // seat with no `activity`, a budget nobody reported and a health frame with
  // no alarm count stay absent, which is what each screen's "unknown" branch
  // reads.
  test("an older node's snapshot applies with this build's fields absent", () => {
    const store = new Store();
    store.applySnapshot({
      agents: [{ id: "pm", role: "PM", handle: "pm" }],
      health: { status: "ok" },
    });
    const [pm] = store.state.agents;
    expect(pm?.activity).toBeUndefined();
    expect(pm?.turn).toBeUndefined();
    expect(pm?.paused).toBeUndefined();
    expect(store.state.budget).toBeNull();
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

  // THE REFUSAL'S SENTENCE TRAVELS WITH IT. A `bad_params` refusal is the
  // one the engine writes for the caller — which parameter, and what it
  // accepts — and a rejection carrying only the code left a screen to say
  // "something was missing" about a window the reader chose.
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
    const err = (await asked.catch((e: unknown) => e)) as QueryError;
    expect(err.message).toBe("bad_params");
    expect(err.detail).toMatch(/ask for at most 90/);
  });

  test("a refusal that names no wait carries none", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("fleet");
    socket.onMessage(JSON.stringify({ kind: "error", id: 1, error: "not_found" }));
    const err = await asked.catch((e: unknown) => e);
    expect((err as QueryError).retryAfterSeconds).toBeNull();
    expect((err as QueryError).detail).toBeNull();
  });

  // NOT REPORTED IS NOT UNCAPPED. The budget slice is `null` until a report
  // has carried it, from the store's birth, through a snapshot taken before any
  // node reported (`budget: null`) and after a push of `null`; a report that
  // caps nothing is held as the report it is. An empty object used to stand
  // for both, and the Spend and Home tiles offered an operator of a capped
  // company "Set one" for the first seconds after every engine start.
  test("the budget is null until a report carries it, and an uncapped report is kept", () => {
    const store = new Store();
    expect(store.state.budget).toBeNull();
    store.applySnapshot({ budget: null, health: { status: "ok" } });
    expect(store.state.budget).toBeNull();
    const uncapped = { meter_id: "n:1", seq: 1, timezone: "UTC", org: { windows: [] } };
    store.applyBudget(uncapped);
    expect(store.state.budget).toEqual(uncapped);
    store.applyBudget(null);
    expect(store.state.budget).toBeNull();
  });
});
