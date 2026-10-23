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
import { MAX_CHAT_LIVE, MAX_CHAT_ROOMS, MAX_PHASES, Store } from "./store.ts";
import { LiveSocket, QueryError } from "./socket.ts";
import { nodeCountLabel } from "../lib/format.ts";
import type {
  ChatChange,
  ChatPresencePush,
  EventEnvelope,
  FeedRow,
  OverlayRow,
  RoomMember,
} from "./types.ts";
import { healthFrame } from "~/test/health.ts";

/** The engine's sentence for a read refused on a log at its ceiling
 *  (`statelog.ConditionSentence`), which is what a frame's `detail` carries:
 *  never the refusal's own detail, which stays in the node's log. */
const FULL_LOG =
  "There's no room left to save this kind of change, so the node answering this can't " +
  "confirm it has the latest ones. An administrator needs to make room for it.";

/** One member of a room, as the presence hub presents it. */
function member(seat: string, over: Partial<RoomMember> = {}): RoomMember {
  return {
    session: `${seat}-tab`,
    seat,
    kind: "human",
    activity: "viewing",
    focus: {},
    at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

/** A `chat_presence` roster: the room whole, as a socket joining it is handed it. */
function roster(room: string, ...members: RoomMember[]): ChatPresencePush {
  return { channel_id: room, roster: true, upsert: members, leave: [], truncated: false };
}

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
      agents: [{ id: "pm", agent_id: "a-pm", role: "PM", handle: "pm", activity: "idle" }],
    });
    store.applyAgents([{ agent_id: "a-pm", activity: "working", current_phase: "execute" }]);

    const [row] = store.state.agents;
    expect(row?.handle).toBe("pm");
    expect(row?.activity).toBe("working");
    expect(row?.current_phase).toBe("execute");
  });

  test("a push that says nothing new keeps every row and tells nobody", () => {
    // Every push decodes into new objects. A row re-sent unchanged (a seat's
    // state beside another seat's change, the same refusal once a second)
    // used to replace the row held, and every screen keyed on it drew it
    // again: the Inbox redrew thirteen times a second for nothing visible.
    const store = new Store();
    store.applySnapshot({
      agents: [
        { id: "pm", agent_id: "a-pm", role: "PM", handle: "pm", activity: "idle" },
        { id: "qa", agent_id: "a-qa", role: "QA", handle: "qa", activity: "idle" },
      ],
    });
    store.applyAgents([{ agent_id: "a-pm", activity: "stopped", stopped_reason: "provider" }]);
    const held = store.state.agents;
    const told = vi.fn();
    store.subscribe(["agents"], told);
    for (let i = 0; i < 20; i++) {
      store.applyAgents([
        { agent_id: "a-pm", activity: "stopped", stopped_reason: "provider" },
        { agent_id: "a-qa", activity: "idle" },
      ]);
    }
    expect(told).not.toHaveBeenCalled();
    expect(store.state.agents).toBe(held);

    // A push that moves one row replaces that row and no other.
    store.applyAgents([{ agent_id: "a-qa", activity: "working" }]);
    expect(told).toHaveBeenCalledTimes(1);
    expect(store.state.agents[0]).toBe(held[0]);
    expect(store.state.agents[1]).not.toBe(held[1]);
  });

  test("a keyed object is DISCARDED rather than half-applied", () => {
    // The server sent a full turn's worth of overlays as an object keyed by
    // role once. Both sides' own suites passed, the socket carried every
    // frame, and the seat rendered idle from the first phase to the last.
    // The guard is what makes that loud rather than silent — and the e2e
    // replay is what makes it impossible to ship again.
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", agent_id: "pm", role: "PM", activity: "idle" }] });
    store.applyAgents({ PM: { activity: "working" } } as never);
    expect(store.state.agents[0]?.activity).toBe("idle");
  });

  // A SEAT REACHES THE LIST THROUGH THE ROSTER, which the server merges its
  // live overlay into before sending: an overlay appended as a row of its own
  // was a card with no name, no handle and no page.
  test("an overlay for a seat the roster does not carry is dropped", () => {
    const store = new Store();
    store.applyAgents([{ agent_id: "a-new", activity: "working" }]);
    expect(store.state.agents).toHaveLength(0);
  });

  // BY AGENT ID AND NOTHING ELSE: merged by role name, two seats sharing a
  // name both took every overlay either of them moved.
  test("an overlay moves only its own seat, whoever shares its name", () => {
    const store = new Store();
    store.applySnapshot({
      agents: [
        { id: "support", agent_id: "a-1", role: "Support", activity: "idle" },
        { id: "support-2", agent_id: "a-2", role: "Support", activity: "idle" },
      ],
    });
    store.applyAgents([{ agent_id: "a-2", role: "Support", activity: "working" }]);
    expect(store.state.agents.map((a) => a.activity)).toEqual(["idle", "working"]);
  });

  test("a seats push can express a DELETION, which a merge cannot", () => {
    const store = new Store();
    store.applySnapshot({
      agents: [
        { id: "pm", agent_id: "pm", role: "PM" },
        { id: "eng", agent_id: "eng", role: "Engineer" },
      ],
    });
    store.applySeats([{ id: "pm", agent_id: "pm", role: "PM" }]);
    expect(store.state.agents.map((a) => a.role)).toEqual(["PM"]);
  });

  test("a seats push keeps the live overlay the roster knows nothing about", () => {
    const store = new Store();
    store.applySnapshot({ agents: [{ id: "pm", agent_id: "a-pm", role: "PM" }] });
    store.applyAgents([{ agent_id: "a-pm", activity: "working" }]);
    store.applySeats([{ id: "pm", agent_id: "a-pm", role: "Product", handle: "pm" }]);
    expect(store.state.agents[0]?.activity).toBe("working");
    expect(store.state.agents[0]?.role).toBe("Product");
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
    const frame = healthFrame({
      node: "node-a",
      applied_epoch: 41,
      nodes: 3,
      alarms: { count: 1, worst: "backup_age" },
    });
    store.applyHealth(frame);
    expect(store.state.health).toEqual(frame);
    expect(nodeCountLabel(store.state.health?.nodes)).toBe("3 nodes");
  });

  // A FRAME WHOSE PRESENCE READ FAILED carries no `nodes` at all. The count is
  // then unknown and said so — never 0, which the node answering could not
  // be, and never a guessed 1.
  test("a frame without a node count renders the count as unavailable", () => {
    const store = new Store();
    store.applyHealth(healthFrame({ applied_epoch: 7, posture: "serve" }));
    expect(store.state.health?.nodes).toBeUndefined();
    expect(nodeCountLabel(store.state.health?.nodes)).toBe("node count unavailable");
  });
});

describe("connection state", () => {
  test("a snapshot does NOT claim a connection", () => {
    // The degraded-mode REST poll applies a snapshot while the socket is
    // down. Deriving `connected` from a payload's contents announced a live
    // connection that did not exist.
    const store = new Store();
    store.applySnapshot({ health: healthFrame(), agents: [] });
    expect(store.state.connected).toBe(false);
  });

  test("a dropped socket CLEARS health rather than freezing it", () => {
    // A stale "healthy" is a lie with a timestamp nobody can see.
    const store = new Store();
    store.applyHealth(healthFrame());
    expect(store.state.connected).toBe(true);
    store.setConnected(false);
    expect(store.state.health).toBeNull();
  });

  // WHAT THE NODE LAST SAID OUTLIVES THE SOCKET, and the tab keeps it: a
  // follower of a configuration write asks that rather than whether the node
  // is up, and one mounted during a drop has no earlier render to keep it
  // from (`lib/useLastHealth.ts`).
  test("a dropped socket keeps the last frame the tab received", () => {
    const store = new Store();
    expect(store.state.lastHealth).toBeNull();
    const pushed = healthFrame({ applied_epoch: 4 });
    store.applyHealth(pushed);
    store.setConnected(false);
    expect(store.state.health).toBeNull();
    expect(store.state.lastHealth).toBe(pushed);
    // An empty frame is no frame the node sent.
    store.applyHealth(null);
    expect(store.state.lastHealth).toBe(pushed);
    // The snapshot's health is a frame like the push.
    const snapped = healthFrame({ applied_epoch: 5 });
    store.applySnapshot({ health: snapped, agents: [] });
    expect(store.state.lastHealth).toBe(snapped);
    // A refusal drops the company, never the node's own health.
    store.setAccessRefused("your seat is no longer in the chart");
    expect(store.state.lastHealth).toBe(snapped);
  });

  // A TAB STILL CONNECTING IS NOT A TAB THAT LOST THE ENGINE. Both are
  // disconnected, and only the second has been answered, by a close or by a
  // failed dial; the engine panel draws an amber dot for that one alone, and
  // says how long it has been gone rather than when it was last retried.
  test("the engine is lost from the first close, and a connection clears it", () => {
    vi.useFakeTimers();
    try {
      vi.setSystemTime(1_000);
      const store = new Store();
      expect(store.state.lostAt).toBeNull();
      expect(store.state.everConnected).toBe(false);
      store.setConnected(false); // the first dial failed
      expect(store.state.lostAt).toBe(1_000);
      vi.setSystemTime(5_000);
      store.setConnected(false); // and a retry failed again
      expect(store.state.lostAt).toBe(1_000);
      expect(store.state.everConnected).toBe(false);
      store.setConnected(true);
      expect(store.state.lostAt).toBeNull();
      expect(store.state.everConnected).toBe(true);
      vi.setSystemTime(9_000);
      store.setConnected(false);
      expect(store.state.lostAt).toBe(9_000);
      expect(store.state.everConnected).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  test("a health frame answers the loss as the socket's own open does", () => {
    const store = new Store();
    store.setConnected(false);
    store.applyHealth(healthFrame());
    expect(store.state.lostAt).toBeNull();
    expect(store.state.everConnected).toBe(true);
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

    store.applySnapshot({ agents: [{ id: "pm", agent_id: "a-pm", role: "PM" }] });
    agents.mockClear();
    tokens.mockClear();
    store.applyAgents([{ agent_id: "a-pm", activity: "working" }]);
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
    store.applySeats([{ id: "pm", agent_id: "a-pm", role: "PM" }]);
    expect(fn).not.toHaveBeenCalled();
  });

  test("each slice carries a version React can compare", () => {
    // The slices are mutated in place — they are large and pushed at high
    // frequency — so the version is what gives each a cheap stable identity
    // for useSyncExternalStore.
    const store = new Store();
    const before = store.version("agents");
    store.applySeats([{ id: "pm", agent_id: "a-pm", role: "PM" }]);
    expect(store.version("agents")).toBeGreaterThan(before);
    expect(store.version("tokens")).toBe(0);
  });
});

// THE ROSTER FIRST: an overlay merges onto a seat the roster carries and is
// dropped for one it does not, so every case here seats PM before it pushes.
const PM_ROW = { id: "pm", agent_id: "a-pm", role: "PM", handle: "pm", activity: "working" };
function seated(): Store {
  const store = new Store();
  store.applySeats([PM_ROW]);
  return store;
}

describe("a live call's heavy fields", () => {
  // What the engine pushes for one call: its identity, its light fields, and
  // every heavy field's version — with only the fields it names carried.
  function pushed(
    versions: Partial<Record<keyof typeof LIVE_CALL_DETAIL, number>>,
    carried: Record<string, unknown>,
    over: Record<string, unknown> = {},
  ): OverlayRow {
    return {
      agent_id: "a-pm",
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
        versions: {
          prompt: 0,
          response: 0,
          narration: 0,
          executions: 0,
          rounds: 0,
          invocations: 0,
          ...versions,
        },
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
    const store = seated();
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
    const store = seated();
    store.applyAgents([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]);
    // The push carrying narration 2 was dropped; this one names 3 and carries nothing.
    expect(store.applyAgents([pushed({ prompt: 1, narration: 3 }, {})])).toEqual(["a-pm"]);
    expect(store.state.agents[0]?.live_call?.round_narration).toHaveLength(1);

    store.applyLiveCall({
      agent_id: "a-pm",
      handle: "pm",
      live_call:
        pushed({ prompt: 1, narration: 3 }, { ...prompt, ...narration(3) }).live_call ?? null,
      live_call_seq: 0,
    });
    const call = store.state.agents[0]?.live_call;
    expect(call?.round_narration).toHaveLength(3);
    expect(call?.versions?.narration).toBe(3);
    expect(store.applyAgents([pushed({ prompt: 1, narration: 3 }, {})])).toEqual([]);
  });

  // AN OLDER COPY NEVER REPLACES A NEWER ONE: a push or an answer overtaken by
  // what the tab already holds is read for its light fields only.
  test("never takes a field back to an older copy", () => {
    const store = seated();
    store.applyAgents([pushed({ prompt: 1, narration: 3 }, { ...prompt, ...narration(3) })]);
    store.applyAgents([pushed({ prompt: 1, narration: 2 }, narration(2), { model: "later" })]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.round_narration).toHaveLength(3);
    expect(call?.model).toBe("later");
  });

  // A CALL OF ITS OWN HOLDS NOTHING OF THE LAST ONE: a new phase whose push
  // left a field out is not drawn with the previous phase's prompt.
  test("carries nothing from one call to the next", () => {
    const store = seated();
    store.applyAgents([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]);
    const stale = store.applyAgents([pushed({ prompt: 1, narration: 0 }, {}, { phase: "review" })]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.phase).toBe("review");
    expect(call?.prompt).toBe("");
    expect(call?.round_narration).toBeNull();
    expect(stale).toEqual(["a-pm"]);
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
    const store = seated();
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
    expect(stale).toEqual(["a-pm"]);

    const resumed = { prompt: "resume it", prompt_messages: [{ role: "system", content: "lead" }] };
    store.applyLiveCall({
      agent_id: "a-pm",
      handle: "pm",
      live_call:
        pushed(
          { prompt: 9, response: 11, narration: 11 },
          { ...resumed, ...narration(1), response: "new" },
        ).live_call ?? null,
      live_call_seq: 0,
    });
    call = store.state.agents[0]?.live_call;
    expect(call?.prompt).toBe("resume it");
    expect(call?.versions).toMatchObject({ prompt: 9, response: 11, narration: 11 });
  });

  // A STATUS READER IS NEVER SENT A BODY, so a withheld call — every body
  // version zero, no body key, `body_withheld` — is never behind for one:
  // asked for whole, the engine would answer the same status again. Its
  // rounds and its CLI runs are status and still merge as any call's do. The
  // CONTROL is the case below, where a version moved past the copy held is
  // fetched.
  // Mutation: count a withheld body field as one the tab is behind on, and a
  // status reader asks for its call on every push; treat the runs as body,
  // and the status reader loses them.
  test("a status reader never refetches a live call", () => {
    const store = seated();
    const socket = new LiveSocket(store);
    const ask = vi.spyOn(socket, "query");
    const frame = (data: unknown) => socket.onMessage(JSON.stringify({ kind: "agents", data }));
    const withheld = (rounds: number) =>
      pushed(
        { rounds, invocations: rounds },
        {
          rounds: Array.from({ length: rounds }, () => ({})),
          invocations: Array.from({ length: rounds }, () => ({})),
        },
        {
          body_withheld: true,
        },
      );
    frame([withheld(1)]);
    frame([withheld(2)]);
    frame([withheld(3)]);
    expect(ask.mock.calls.filter(([what]) => what === "live_call")).toEqual([]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.body_withheld).toBe(true);
    expect(call?.prompt).toBeUndefined();
    expect(call?.versions).toMatchObject({ prompt: 0, narration: 0, rounds: 3, invocations: 3 });
    expect(call?.rounds).toHaveLength(3);
    expect(call?.invocations).toHaveLength(3);
  });

  // A BODY HELD A MOMENT BEFORE IS NOT KEPT once the engine withholds it: a
  // reader who could read this seat's trail and now cannot is shown none of
  // it, and is not sent asking for it either.
  test("a call withheld after it was whole keeps none of its body", () => {
    const store = seated();
    store.applyAgents([pushed({ prompt: 4, narration: 4 }, { ...prompt, ...narration(4) })]);
    const stale = store.applyAgents([pushed({}, {}, { body_withheld: true })]);
    expect(stale).toEqual([]);
    const call = store.state.agents[0]?.live_call;
    expect(call?.prompt).toBeUndefined();
    expect(call?.round_narration).toBeUndefined();
    expect(call?.versions).toMatchObject({ prompt: 0, narration: 0 });
  });

  // THE SOCKET ASKS FOR THE CALL WHOLE, once a seat while an ask is out, and the
  // answer lands on the row.
  test("the socket fetches a call it holds behind, once", async () => {
    const store = seated();
    const socket = new LiveSocket(store);
    const ask = vi.spyOn(socket, "query");
    const frame = (data: unknown) => socket.onMessage(JSON.stringify({ kind: "agents", data }));
    frame([pushed({ prompt: 1, narration: 1 }, { ...prompt, ...narration(1) })]);
    frame([pushed({ prompt: 1, narration: 3 }, {})]);
    frame([pushed({ prompt: 1, narration: 4 }, {})]);
    expect(ask).toHaveBeenCalledTimes(1);
    expect(ask).toHaveBeenCalledWith("live_call", { agent_id: "a-pm" });

    socket.onMessage(
      JSON.stringify({
        kind: "result",
        id: 1,
        data: {
          agent_id: "a-pm",
          handle: "pm",
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
  const running = (seq: number, phase = "execute"): OverlayRow => ({
    agent_id: "a-pm",
    live_call_seq: seq,
    live_call: {
      turn_id: "t1",
      phase,
      iteration: 0,
      in_progress: true,
      versions: { prompt: 1, response: 1, narration: 1, executions: 1, rounds: 1, invocations: 1 },
    } as never,
  });
  // The same running call at `seq`, its narration at version `narration`,
  // carrying only the fields in `carried` — a lean push, or an answer whole.
  const call = (
    seq: number,
    narration: number,
    carried: object,
    over: object = {},
  ): OverlayRow => ({
    agent_id: "a-pm",
    live_call_seq: seq,
    live_call: {
      ...running(seq).live_call,
      versions: { prompt: 1, response: 1, narration, executions: 1, rounds: 1, invocations: 1 },
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
    invocations: [],
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
    const store = seated();
    store.applyAgents([running(10)]);
    // The completion push clears the call at a newer sequence.
    store.applyAgents([{ agent_id: "a-pm", activity: "idle", live_call: null, live_call_seq: 12 }]);
    expect(store.state.agents[0]?.live_call).toBeNull();

    // The overtaken answer — the running call read a moment before the clear.
    store.applyLiveCall({
      agent_id: "a-pm",
      handle: "pm",
      live_call: running(10).live_call ?? null,
      live_call_seq: 10,
    });
    expect(store.state.agents[0]?.live_call).toBeNull();
    expect(store.state.agents[0]?.live_call_seq).toBe(12);
  });

  // NOR A NEWER CALL. The push that overtook the answer began a new phase
  // rather than clearing; the older answer must not replace it.
  test("a new call is not replaced by an answer for the one before it", () => {
    const store = seated();
    store.applyAgents([running(10, "execute")]);
    store.applyAgents([running(12, "review")]);
    store.applyLiveCall({
      agent_id: "a-pm",
      handle: "pm",
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
    const store = seated();
    store.applyAgents([call(5, 1, whole(1))]);
    // The push carrying narration 9 was dropped: this one names 9 and carries
    // nothing, so the socket asks for the call whole.
    expect(store.applyAgents([call(10, 9, {})])).toEqual(["a-pm"]);
    // While that ask is out a later round lands, still naming 9.
    expect(store.applyAgents([call(12, 9, {}, { model: "later" })])).toEqual(["a-pm"]);

    // The answer was read at 10: behind the applied 12, the same call.
    const current = store.applyLiveCall({
      agent_id: "a-pm",
      handle: "pm",
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
    const store = seated();
    const socket = new LiveSocket(store);
    const ask = vi.spyOn(socket, "query");
    const frame = (data: unknown) => socket.onMessage(JSON.stringify({ kind: "agents", data }));
    const answer = (id: number, row: OverlayRow) =>
      socket.onMessage(JSON.stringify({ kind: "result", id, data: row }));
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
    const store = seated();
    store.applyAgents([{ agent_id: "a-pm", activity: "idle", live_call: null, live_call_seq: 8 }]);
    store.applyLiveCall({
      agent_id: "a-pm",
      handle: "pm",
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
});

describe("a refused query", () => {
  // THE WAIT THE ENGINE NAMED TRAVELS WITH THE REFUSAL. The socket used to
  // reject with the bare code, so `retry_after` reached no screen and each
  // guessed a wait of its own.
  test("an unavailable frame's retry hint reaches the rejection", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("fleet");
    socket.onMessage(
      JSON.stringify({
        kind: "error",
        id: 1,
        error: "unavailable",
        retry_after: 4,
        refusal: "log_full",
        detail: FULL_LOG,
      }),
    );
    const err = await asked.catch((e: unknown) => e);
    expect(err).toBeInstanceOf(QueryError);
    expect((err as QueryError).message).toBe("unavailable");
    expect((err as QueryError).refusal).toEqual({
      code: "log_full",
      detail: FULL_LOG,
      retryAfter: 4,
    });
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
        detail: "days=91, and a spend window is 1 to 90 company days. Ask for at most 90",
      }),
    );
    const err = (await asked.catch((e: unknown) => e)) as QueryError;
    expect(err.message).toBe("bad_params");
    expect(err.detail).toMatch(/Ask for at most 90/);
  });

  // A REVISION THE NODE CANNOT OPEN SAYS WHAT FIXES IT. The engine writes
  // that refusal for the reader too — which key, or which document — and a
  // rejection carrying only the code left the config screen a fixed line.
  test("a revision refusal's sentence reaches the rejection", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("config");
    socket.onMessage(
      JSON.stringify({
        kind: "error",
        id: 1,
        error: "unreadable_fleet_revision",
        detail: "Add the key the fleet seals under to this node's secrets.keys and restart it.",
      }),
    );
    const err = (await asked.catch((e: unknown) => e)) as QueryError;
    expect(err.message).toBe("unreadable_fleet_revision");
    expect(err.detail).toMatch(/secrets\.keys/);
  });

  test("a refusal that names no wait carries none", async () => {
    const store = new Store();
    const socket = new LiveSocket(store);
    const asked = socket.query("fleet");
    socket.onMessage(JSON.stringify({ kind: "error", id: 1, error: "not_found" }));
    const err = await asked.catch((e: unknown) => e);
    expect((err as QueryError).refusal).toBeNull();
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
    store.applySnapshot({ budget: null, health: healthFrame() });
    expect(store.state.budget).toBeNull();
    const uncapped = { meter_id: "n:1", seq: 1, timezone: "UTC", org: { windows: [] } };
    store.applyBudget(uncapped);
    expect(store.state.budget).toEqual(uncapped);
    store.applyBudget(null);
    expect(store.state.budget).toBeNull();
  });
});

describe("chat", () => {
  function change(over: Partial<ChatChange> = {}): ChatChange {
    return {
      channel_id: "room-1",
      op: "post",
      op_id: "op-1",
      message_id: "m-1",
      channel_seq: 1,
      author: "ada",
      author_kind: "human",
      at: "2026-01-01T00:00:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
      ...over,
    };
  }

  test("the first frame for a room sets the live mark without claiming the tab is behind", () => {
    // A tab that has just connected is not behind on a room it has never
    // seen. Measured against zero instead, every room would open with a
    // phantom hole and a refetch nobody needed.
    const store = new Store();
    store.applyChatChange(change({ channel_seq: 4_211 }));
    expect(store.state.chat.rooms["room-1"]?.seq).toBe(4_211);
    expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(1);
  });

  test("a redelivered frame is the same record, not a second message", () => {
    // The op id is the record's own idempotency key. A reconnect that
    // overlaps, or a hub that re-sent, hands the same frame twice — and
    // counting it twice is a message rendered twice everywhere a screen
    // counts these.
    const store = new Store();
    store.applyChatChange(change());
    const version = store.version("chat");
    store.applyChatChange(change());
    expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(1);
    // And it does not even wake a listener: nothing about the room moved.
    expect(store.version("chat")).toBe(version);
  });

  test("a late frame never moves the live mark backwards", () => {
    // Out of order is LATE, not new. A mark that went down would leave a
    // screen re-reading a room it is already ahead of for ever.
    const store = new Store();
    store.applyChatChange(change({ op_id: "op-9", channel_seq: 9 }));
    store.applyChatChange(change({ op_id: "op-7", channel_seq: 7 }));
    expect(store.state.chat.rooms["room-1"]?.seq).toBe(9);
    expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(2);
  });

  test("the live tail is bounded per room, and so is the number of rooms", () => {
    // A tab left open on a busy company holds a bounded amount of chat, the
    // way MAX_EVENTS and MAX_PHASES already bound their slices. Frames arrive
    // for every room the viewer may READ, which is wider than their rail, so
    // the room bound is a real ceiling rather than a restatement of one.
    const store = new Store();
    for (let i = 0; i < MAX_CHAT_LIVE + 40; i++) {
      store.applyChatChange(change({ op_id: `op-${i}`, channel_seq: i + 1 }));
    }
    expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(MAX_CHAT_LIVE);
    // Newest first, so the bound drops the OLDEST frame.
    expect(store.state.chat.rooms["room-1"]?.changes[0]?.op_id).toBe(`op-${MAX_CHAT_LIVE + 39}`);

    for (let i = 0; i < MAX_CHAT_ROOMS + 10; i++) {
      store.applyChatChange(
        change({
          channel_id: `other-${i}`,
          op_id: `other-op-${i}`,
          // The log position is what orders the eviction, so it has to move.
          position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1_000 + i },
        }),
      );
    }
    expect(Object.keys(store.state.chat.rooms).length).toBeLessThanOrEqual(MAX_CHAT_ROOMS);
  });

  test("the count a screen re-reads on moves on every frame, past both bounds", () => {
    // WHAT A SCREEN RE-READS ON has to move on every accepted frame and on
    // nothing else. The frames a room holds are bounded, so the length of
    // what is held stands still past the bound — the room that re-read on
    // it stopped re-reading at its two-hundredth frame.
    const store = new Store();
    for (let i = 0; i <= MAX_CHAT_LIVE; i++) {
      store.applyChatChange(change({ op_id: `op-${i}`, channel_seq: i + 1 }));
    }
    const room = store.state.chat.rooms["room-1"];
    expect(room?.changes).toHaveLength(MAX_CHAT_LIVE);
    expect(room?.frame).toBe(MAX_CHAT_LIVE + 1);
    expect(store.state.chat.frames).toBe(MAX_CHAT_LIVE + 1);

    // A REDELIVERY MOVES NEITHER: it is the same record.
    store.applyChatChange(change({ op_id: `op-${MAX_CHAT_LIVE}`, channel_seq: MAX_CHAT_LIVE + 1 }));
    expect(store.state.chat.frames).toBe(MAX_CHAT_LIVE + 1);

    // A ROOM EVICTED AND HEARD FROM AGAIN comes back ABOVE every number it
    // had: a count that started again at one could land on the very number a
    // screen holds, and that screen would not re-read.
    const before = room!.frame;
    for (let i = 0; i < MAX_CHAT_ROOMS; i++) {
      store.applyChatChange(
        change({
          channel_id: `other-${i}`,
          op_id: `other-op-${i}`,
          position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1_000 + i },
        }),
      );
    }
    expect(store.state.chat.rooms["room-1"]).toBeUndefined();
    store.applyChatChange(
      change({
        op_id: "op-back",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 5_000 },
      }),
    );
    expect(store.state.chat.rooms["room-1"]?.frame).toBeGreaterThan(before);
    // AND THE TAB'S TOTAL counted every one of them, the rooms it let go
    // included — a sum of what the rooms hold would have stood still.
    expect(store.state.chat.frames).toBe(MAX_CHAT_LIVE + 1 + MAX_CHAT_ROOMS + 1);
  });

  test("forgetting a seat keeps the frame count, which is nobody's", () => {
    const store = new Store();
    store.applyChatChange(change());
    store.forgetSeat();
    expect(store.state.chat.rooms).toEqual({});
    expect(store.state.chat.frames).toBe(1);
    // AND EVERY FRAME IT COUNTED IS LET GO OF, so a screen asking what
    // arrived since one it answered is told it cannot say.
    expect(store.state.chat.dropped).toBe(1);
  });

  test("every frame held says which accepted frame it was", () => {
    // WHAT A SCREEN ASKS "WHAT ARRIVED SINCE" WITH, across rooms as well as
    // within one: two rooms' frames arrive in the order the hub sent them,
    // which a position cannot say.
    const store = new Store();
    store.applyChatChange(change({ op_id: "op-a" }));
    store.applyChatChange(change({ channel_id: "room-2", op_id: "op-b" }));
    store.applyChatChange(change({ op_id: "op-c" }));
    expect(store.state.chat.rooms["room-1"]?.changes.map((held) => held.frame)).toEqual([3, 1]);
    expect(store.state.chat.rooms["room-2"]?.changes.map((held) => held.frame)).toEqual([2]);
    expect(store.state.chat.dropped).toBe(0);
  });

  test("a frame let go of by either bound is the latest one let go of", () => {
    // WHAT TELLS "NOTHING ARRIVED" FROM "WHAT ARRIVED IS GONE": a screen
    // holding a frame below this one cannot say what changed since.
    const store = new Store();
    for (let i = 0; i <= MAX_CHAT_LIVE; i++) {
      store.applyChatChange(change({ op_id: `op-${i}`, channel_seq: i + 1 }));
    }
    // The room's first frame was pushed off its list.
    expect(store.state.chat.dropped).toBe(1);
    const newest = store.state.chat.rooms["room-1"]!.frame;
    for (let i = 0; i < MAX_CHAT_ROOMS; i++) {
      store.applyChatChange(
        change({
          channel_id: `other-${i}`,
          op_id: `other-op-${i}`,
          position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1_000 + i },
        }),
      );
    }
    // And then the whole room, whose newest frame goes with it.
    expect(store.state.chat.rooms["room-1"]).toBeUndefined();
    expect(store.state.chat.dropped).toBe(newest);
  });

  // THE LOG'S ORDER, NOT THE INSTANT'S TEXT. The broker's instant is RFC 3339
  // with its trailing zeros trimmed, so as text `…:05Z` sorts after every
  // fraction of its own second: ordered that way, the room that was quietest
  // read as the busiest and was the one kept.
  test("eviction drops the room earliest on the log, whatever its instant reads as text", () => {
    const store = new Store();
    store.applyChatChange(
      change({
        channel_id: "quiet",
        op_id: "quiet-op",
        at: "2026-01-01T00:00:05Z",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
      }),
    );
    for (let i = 0; i < MAX_CHAT_ROOMS; i++) {
      store.applyChatChange(
        change({
          channel_id: `busy-${i}`,
          op_id: `busy-op-${i}`,
          at: "2026-01-01T00:00:05.123456789Z",
          position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 2 + i },
        }),
      );
    }
    const rooms = store.state.chat.rooms;
    expect(rooms["quiet"]).toBeUndefined();
    expect(Object.keys(rooms)).toHaveLength(MAX_CHAT_ROOMS);
  });

  test("a room's newest moment is the latest on the log, and a late frame cannot move it back", () => {
    const store = new Store();
    store.applyChatChange(
      change({
        op_id: "op-a",
        at: "2026-01-01T00:00:05.123456789Z",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 8 },
      }),
    );
    // LATER ON THE LOG, though its instant sorts earlier as text.
    store.applyChatChange(
      change({
        op_id: "op-b",
        at: "2026-01-01T00:00:05.12Z",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 9 },
      }),
    );
    // And a late frame, delivered after it.
    store.applyChatChange(
      change({
        op_id: "op-c",
        at: "2026-01-01T00:00:06Z",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 7 },
      }),
    );
    expect(store.state.chat.rooms["room-1"]?.position?.seq).toBe(9);
  });

  test("a frame with no room or no op id is dropped rather than filed under nothing", () => {
    const store = new Store();
    store.applyChatChange(change({ channel_id: "" }));
    store.applyChatChange(change({ op_id: "" }));
    expect(Object.keys(store.state.chat.rooms)).toHaveLength(0);
  });

  test("a degraded snapshot does not blank the open conversation", () => {
    // The 5 s REST snapshot carries no chat at all. Applying one has to leave
    // the transcript, the presence and the read state exactly where they
    // were, or an engine that is merely unreachable empties the screen every
    // five seconds.
    const store = new Store();
    store.applyChatChange(change());
    store.applyChatPresence(roster("room-1", member("ada")));
    store.applyChatRead({ handle: "ada", cursors: { "room-1": 42 } });
    store.applySnapshot({ agents: [] });
    expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(1);
    expect(store.state.chatPresence.rooms["room-1"]?.members?.map((m) => m.seat)).toEqual(["ada"]);
    expect(store.state.chatRead?.cursors?.["room-1"]).toBe(42);
  });

  test("a roster replaces a room's members and a change merges into them", () => {
    // THE HUB'S FAN-OUT: the room whole as the socket joins it, then only
    // what moved. Merged rather than replaced, a roster would leave whoever
    // left while this socket was elsewhere; replaced rather than merged, a
    // change would empty the room of everybody it did not mention.
    const store = new Store();
    store.applyChatPresence(
      roster("room-1", member("ada", { activity: "editing" }), member("cy"), member("eve")),
    );
    store.applyChatPresence({
      channel_id: "room-1",
      upsert: [member("ada"), member("bo")],
      leave: ["cy-tab"],
      truncated: false,
    });
    const held = store.state.chatPresence.rooms["room-1"]?.members ?? [];
    // EVE IS NOT MENTIONED AND IS STILL HERE.
    expect(held.map((m) => [m.seat, m.activity])).toEqual([
      ["ada", "viewing"],
      ["eve", "viewing"],
      ["bo", "viewing"],
    ]);
    store.applyChatPresence(roster("room-1", member("dee")));
    expect(store.state.chatPresence.rooms["room-1"]?.members?.map((m) => m.seat)).toEqual(["dee"]);
  });

  test("a room left with nobody in it is forgotten, and one naming no room changes nothing", () => {
    // THE ENGINE TAKES A SOCKET OUT OF A ROOM WITH A ROSTER NAMING NOBODY —
    // the tab moved on, or the room narrowed past its reader — and the room
    // it was shown must not stand.
    const store = new Store();
    store.applyChatPresence(roster("room-1", member("ada")));
    store.applyChatPresence(roster("room-2", member("bo")));
    store.applyChatPresence(roster("room-1"));
    expect(store.state.chatPresence.rooms["room-1"]).toBeUndefined();
    store.applyChatPresence({
      channel_id: "room-2",
      upsert: null,
      leave: ["bo-tab"],
      truncated: false,
    });
    expect(store.state.chatPresence.rooms).toEqual({});
    // A ROOM PAST ITS TOP COUNTS WHO IT DOES NOT PRESENT, and is not empty.
    store.applyChatPresence({
      channel_id: "room-3",
      roster: true,
      upsert: [],
      leave: [],
      counts: { human: 3, agent: 0 },
      truncated: true,
    });
    expect(store.state.chatPresence.rooms["room-3"]?.counts).toEqual({ human: 3, agent: 0 });
    const woke = vi.fn();
    store.subscribe(["chatPresence"], woke);
    store.applyChatPresence({
      channel_id: "",
      upsert: [member("ada")],
      leave: [],
      truncated: false,
    });
    store.applyChatPresence(null);
    expect(woke).not.toHaveBeenCalled();
  });

  test("an unread read state is null rather than an empty cursor set", () => {
    // Null is "nobody could look"; an empty cursor map is "you have read
    // nothing anywhere". A screen badging a whole company unread because a
    // coordination read failed is what keeping them apart prevents.
    const store = new Store();
    expect(store.state.chatRead).toBeNull();
    store.applyChatRead(null);
    expect(store.state.chatRead).toBeNull();
    store.applyChatRead({ handle: "ada", cursors: {} });
    expect(store.state.chatRead?.cursors).toEqual({});
  });

  test("a read state below the revision held is late, and is dropped", () => {
    // Two flushes from one person land B then A, A's state carrying B's
    // change — and B's push can still arrive after A's. Kept by arrival, the
    // older frame would put back a cursor the record has already moved past.
    const store = new Store();
    const fn = vi.fn();
    store.subscribe(["chatRead"], fn);
    store.applyChatRead({ handle: "ada", cursors: { "room-1": 9, "room-2": 4 }, revision: 12 });
    store.applyChatRead({ handle: "ada", cursors: { "room-2": 4 }, revision: 11 });
    expect(store.state.chatRead?.cursors).toEqual({ "room-1": 9, "room-2": 4 });
    expect(fn).toHaveBeenCalledTimes(1);
    // AND A NEWER ONE REPLACES IT WHOLE, a mute lifted included.
    store.applyChatRead({
      handle: "ada",
      cursors: { "room-1": 9, "room-2": 7 },
      muted: [],
      revision: 13,
    });
    expect(store.state.chatRead?.cursors?.["room-2"]).toBe(7);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  // THE REVISION ORDERS ONE SEAT'S STATES AND NOTHING ELSE. On the
  // coordination KV it is the bucket's own sequence, so the last seat's
  // latest flush can carry a higher number than the next seat's whole record:
  // compared across the two, the next seat's rail answer and every push were
  // dropped as "late" until it happened to write again — which is what a tab
  // meets when the seat its session is bound to moves.
  test("a read state naming another seat replaces the one held, whatever its revision", () => {
    const store = new Store();
    store.applyChatRead({
      handle: "ana",
      cursors: { "room-a": 9 },
      muted: ["room-a"],
      revision: 10,
    });
    store.applyChatRead({ handle: "bo", cursors: { "room-b": 2 }, revision: 3 });
    expect(store.state.chatRead?.handle).toBe("bo");
    expect(store.state.chatRead?.muted).toBeUndefined();
    // …and within that seat, the order is the revision's again.
    store.applyChatRead({ handle: "bo", cursors: {}, revision: 2 });
    expect(store.state.chatRead?.cursors).toEqual({ "room-b": 2 });
  });

  test("a new seat takes the last seat's rooms and presence with it", () => {
    // A FRAME CARRIES ITS ROOM'S NAME AND AN EXCERPT, and reaches a socket
    // only for a room its seat may read — kept, the last seat's private rooms
    // would sit in the tab of whoever holds the next.
    const store = new Store();
    store.applyChatRead({ handle: "ana", cursors: {}, revision: 10 });
    store.applyChatChange(change({ channel_id: "ana-private", excerpt: "salary review" }));
    store.applyChatPresence(roster("ana-private", member("ana")));
    const woke = vi.fn();
    store.subscribe(["chat", "chatPresence"], woke);
    store.applyChatRead({ handle: "bo", cursors: {}, revision: 3 });
    expect(store.state.chat.rooms).toEqual({});
    expect(store.state.chatPresence.rooms).toEqual({});
    expect(woke).toHaveBeenCalled();
    // THE SAME SEAT'S NEXT STATE TAKES NOTHING.
    store.applyChatChange(change({ channel_id: "bo-room" }));
    store.applyChatRead({ handle: "bo", cursors: {}, revision: 4 });
    expect(Object.keys(store.state.chat.rooms)).toEqual(["bo-room"]);
  });

  test("forgetting the seat empties every chat slice and wakes each", () => {
    const store = new Store();
    store.applyChatChange(change());
    store.applyChatPresence(roster("room-1", member("ana")));
    store.applyChatRead({ handle: "ana", cursors: { "room-1": 4 }, revision: 10 });
    const woke = { chat: vi.fn(), chatPresence: vi.fn(), chatRead: vi.fn() };
    for (const [slice, fn] of Object.entries(woke)) store.subscribe([slice as never], fn);
    store.forgetSeat();
    expect(store.state.chat.rooms).toEqual({});
    expect(store.state.chatPresence.rooms).toEqual({});
    expect(store.state.chatRead).toBeNull();
    for (const fn of Object.values(woke)) expect(fn).toHaveBeenCalledTimes(1);
    // AND A STATE THAT FOLLOWS IS KEPT, however low its revision: there is
    // nothing held to be older than.
    store.applyChatRead({ handle: "bo", cursors: {}, revision: 3 });
    expect(store.state.chatRead?.revision).toBe(3);
  });

  // A REFUSAL DROPS WHAT THE ENGINE SERVED, chat included — a frame names a
  // room and quotes it — and KEEPS THE COUNT: an empty state's zero would hand
  // a screen holding frame 3 a count that comes back to 3, and every frame
  // until the count passed it again would read as already answered.
  test("a refusal drops every chat slice and keeps the frame count", () => {
    const store = new Store();
    store.applyChatChange(change());
    store.applyChatChange(change({ op_id: "op-2" }));
    store.applyChatPresence(roster("room-1", member("ana")));
    store.applyChatRead({ handle: "ana", cursors: { "room-1": 4 }, revision: 10 });
    const woke = { chat: vi.fn(), chatPresence: vi.fn(), chatRead: vi.fn() };
    for (const [slice, fn] of Object.entries(woke)) store.subscribe([slice as never], fn);
    store.setAccessRefused("seat_unavailable");
    expect(store.state.chat.rooms).toEqual({});
    expect(store.state.chatPresence.rooms).toEqual({});
    expect(store.state.chatRead).toBeNull();
    expect(store.state.chat.frames).toBe(2);
    expect(store.state.chat.dropped).toBe(2);
    for (const fn of Object.values(woke)) expect(fn).toHaveBeenCalled();
    // AND THE NEXT FRAME IS ABOVE EVERY NUMBER A SCREEN HELD.
    store.setAccessRefused(null);
    store.applyChatChange(change({ op_id: "op-3" }));
    expect(store.state.chat.rooms["room-1"]?.frame).toBe(3);
  });

  // A FRAME NAMING NO ROOM IS THE ENGINE SAYING THIS TAB MAY HAVE MISSED
  // FRAMES ANYWHERE — its relay switched source, or the hub could not deliver
  // a batch. Dropped as unfileable, as it once was, the last messages before a
  // quiet spell stayed missing until a reload.
  test("a frame naming no room is a gap every screen re-reads on", () => {
    const store = new Store();
    store.applyChatChange(change());
    const woke = vi.fn();
    store.subscribe(["chat"], woke);
    store.applyChatChange({ channel_id: "" });
    expect(woke).toHaveBeenCalledTimes(1);
    // COUNTED, so the rail moves; and everything before it is let go of, so a
    // screen asking what arrived is told it cannot say.
    expect(store.state.chat.frames).toBe(2);
    expect(store.state.chat.dropped).toBe(2);
    expect(store.state.chat.gap).toBe(2);
    // WHAT DID ARRIVE STAYS: the room's own frames are not a gap's to take.
    expect(store.state.chat.rooms["room-1"]?.changes).toHaveLength(1);
    // A SECOND GAP IS A SECOND RE-READ, never collapsed into the first.
    store.applyChatChange({ channel_id: "" });
    expect(store.state.chat.gap).toBe(3);
  });

  test("each chat slice wakes only its own listeners", () => {
    // Presence moves with every keystroke re-asserted in the room and every
    // phase a seat at work moves to, whatever the conversation does, so a
    // rail that woke on it would re-render the channel list on each.
    const store = new Store();
    const fn = vi.fn();
    store.subscribe(["chat"], fn);
    store.applyChatPresence(roster("room-1", member("ada")));
    store.applyChatRead({ handle: "ada", cursors: {} });
    expect(fn).not.toHaveBeenCalled();
    store.applyChatChange(change());
    expect(fn).toHaveBeenCalledTimes(1);
  });
});
