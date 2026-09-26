// Replay captured server frames through the dashboard's OWN client.
//
// Not a test of the dashboard — its own Vitest suites in dashboard/ are that.
// This is the last link of the server's end-to-end gate: the Go suite runs a
// real company, captures every frame its WebSocket actually pushed, and hands
// the file here. What runs against those bytes is the dashboard's OWN protocol
// module — the same source its bundle contains, emitted once more as plain ESM
// — through the real message dispatch, so the question it answers is the only
// one that matters at that seam: NOT "did the server send something" but "does
// the client understand what the server sent".
//
// The distinction is not academic. It was written because a full turn's worth
// of `agents` pushes were being sent as an object keyed by role, while the
// store guards `applyAgents` with `Array.isArray` and dropped every one of
// them: the server's own tests passed, the client's own tests passed, the
// socket carried the frames, and the seat rendered idle from the first phase
// to the last.
//
// Usage: node replay.mjs <frames.json> [--print]
//
//   frames.json  a JSON array of raw frame strings, in arrival order
//   --print      dump the resulting store state as JSON on stdout
//
// Exits non-zero with a diagnosis on stderr when the frames do not drive the
// client to a coherent state.

import { readFileSync } from "node:fs";
import { PROTOCOL_URL } from "./dashboardRoot.mjs";

// The browser bits the socket and the token store reach for. This socket is
// never dialled — the frames are fed straight to the message handler — but the
// module builds one at construction, so the global has to exist.
class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  constructor(url) {
    this.url = url;
    this.readyState = InertWebSocket.OPEN;
  }
  send() {}
  close() {
    this.readyState = InertWebSocket.CLOSED;
  }
}
globalThis.WebSocket = InertWebSocket;
globalThis.localStorage = { getItem: () => null, setItem: () => {} };
globalThis.fetch = async () => {
  throw new Error("offline: the replay has no server");
};

const { Store, LiveSocket } = await import(PROTOCOL_URL.href);

const [file, ...flags] = process.argv.slice(2);
if (!file) {
  console.error("usage: node replay.mjs <frames.json> [--print]");
  process.exit(2);
}

const frames = JSON.parse(readFileSync(file, "utf8"));
if (!Array.isArray(frames) || frames.length === 0) {
  console.error(`replay: ${file} holds no frames`);
  process.exit(2);
}

const store = new Store();
const socket = new LiveSocket(store);
// The REAL dispatch table, reached the way an arriving frame reaches it.
// Re-implementing the `switch` here would let the replay agree with a server
// the dashboard does not.
for (const raw of frames) socket.onMessage(raw);

const problems = [];
const state = store.state;

// The frames are one company running one turn. What the client must end up
// holding, and what each absence would have looked like on screen:
if (!state.agents.length) {
  problems.push(
    "no seats: every `agents` push was dropped, so the roster is empty and " +
      "no seat can ever show as working",
  );
}
if (!state.events.length) {
  problems.push("no events: the activity feed is empty");
}

// Somewhere in the run a seat must have been working with a live call — that
// is what "the UI showing a live turn" means. The store holds only the LATEST
// state, so this is re-derived from the frames rather than read off the end:
// by the last frame the turn has finished and the row is idle again, which is
// correct and is exactly why the end state cannot answer this.
let sawWorking = false;
let sawLiveCall = false;
const phases = new Set();
const replay = new Store();
const probe = new LiveSocket(replay);
for (const raw of frames) {
  probe.onMessage(raw);
  for (const agent of replay.state.agents) {
    if (agent.activity === "working") sawWorking = true;
    if (agent.live_call) {
      sawLiveCall = true;
      if (agent.live_call.phase) phases.add(agent.live_call.phase);
    }
  }
}
if (!sawWorking) {
  problems.push("no seat was ever `working`: the dashboard would show an " +
    "idle company for the whole of a turn");
}
if (!sawLiveCall) {
  problems.push("no in-flight call ever reached a seat row: a turn would go " +
    "from idle to done with nothing on screen in between");
}
for (const want of ["execute", "review"]) {
  if (!phases.has(want)) {
    problems.push(`no live call named the ${want} phase (saw ${[...phases].join(", ") || "none"})`);
  }
}

// THE DURABLE HALF OF A LIVE PHASE, which is the whole reason a turn survives
// being watched to its end. The projection clears `live_call` the instant a
// phase completes and the seat page's history query is answered once, at mount
// — so the only thing that can keep a finished turn on screen is the
// `agent_phase_completed` envelope the server pushes, PAYLOAD AND ALL, just
// before the overlay that clears the call. That the payload is on the wire is
// the server's half of the contract and nothing else here checks it: with the
// payload stripped the client keeps a payload-free feed row, every phase card
// disappears the moment its phase lands, and both sides' own suites stay green.
if (!state.phases.length) {
  problems.push(
    "no completed phase reached the store with its payload: a turn watched " +
      "to its end vanishes from the seat page the moment its review lands",
  );
} else {
  const missing = state.phases.filter((p) => !p.payload || !p.payload.turn_id);
  if (missing.length) {
    problems.push(
      `${missing.length} of ${state.phases.length} phase envelopes carry no ` +
        "`payload.turn_id`: a phase record cannot be built from them, so the " +
        "turn they belong to renders with the phase missing",
    );
  }
  const streamed = new Set(state.phases.map((p) => p.payload && p.payload.phase));
  for (const want of ["execute", "review"]) {
    if (!streamed.has(want)) {
      problems.push(
        `no ${want} phase arrived as a durable record (saw ` +
          `${[...streamed].filter(Boolean).join(", ") || "none"})`,
      );
    }
  }
}

// THE LIVE TOKEN METERS. The company in the capture caps its day, so the
// `budget` push must land as windows the screens can draw: a list per scope,
// each window carrying its span, its spend, its ceiling and the engine's
// state. The meters used to be one figure per scope, and the refusal stamp the
// frame carried was dropped on its way to the push, so every "refusing
// charges" row the dashboard renders was unreachable.
const budget = state.budget;
const orgWindows = budget && budget.org && budget.org.windows;
if (!Array.isArray(orgWindows)) {
  problems.push(
    "the budget push has no `org.windows` list: the header meter and the " +
      "Budgets screen have nothing to draw",
  );
} else {
  const day = orgWindows.find((w) => w.period === "day");
  if (!day) {
    problems.push(
      `the company caps its day and the budget push has no day window (saw ` +
        `${orgWindows.map((w) => w.period).join(", ") || "none"})`,
    );
  } else {
    for (const field of ["window", "starts_at", "resets_at", "state"]) {
      if (!day[field]) {
        problems.push(`the day window has no \`${field}\`: ${JSON.stringify(day)}`);
      }
    }
    if (typeof day.used !== "number" || typeof day.limit !== "number") {
      problems.push(`the day window's used and limit are not numbers: ${JSON.stringify(day)}`);
    }
    if (!["ok", "near", "refusing"].includes(day.state)) {
      problems.push(`the day window's state ${JSON.stringify(day.state)} is not the engine's`);
    }
  }
  if (!budget.timezone) {
    problems.push("the budget push names no clock: its windows cannot be read as days");
  }
}

// The spend rollup. The store takes it two ways and both have to work: a
// snapshot is accepted only `if (snap.tokens && snap.tokens.totals)`, and a
// push is stored as-is for the Spend screen to read its window off. A list of
// raw records passes neither, and the screen renders blank with the numbers
// sitting in memory the whole time.
if (state.tokens === null) {
  problems.push(
    "no spend rollup: every `tokens` frame was rejected, so the Spend screen " +
      "has nothing to draw",
  );
} else {
  if (!state.tokens.totals) {
    problems.push(
      "the spend rollup has no `totals`: applySnapshot requires it, so a " +
        "reconnecting tab would drop the rollup it was just sent",
    );
  }
  // THE WINDOW IS TWO INSTANTS, half-open. It was a count of days, which can
  // only name a window anchored at now — and the Cost screen's range control
  // produces two edges that need not be. Both are required: a rollup carrying
  // one edge describes a window with no other end, and the screen prints the
  // pair beside the numbers.
  for (const edge of ["since", "until"]) {
    if (!state.tokens[edge]) {
      problems.push(
        `the spend rollup has no \`${edge}\`: the screen prints the window ` +
          "beside the numbers and can say nothing about what they cover " +
          "without both edges",
      );
    }
  }
  if (state.tokens.since && state.tokens.until && !(state.tokens.until > state.tokens.since)) {
    problems.push(
      "the spend rollup's window ends where it begins: half-open, that names " +
        "no records at all, so the figures beside it are a sum over nothing",
    );
  }
  if (!Array.isArray(state.tokens.by_phase)) {
    problems.push(
      "`by_phase` is not an array: the Spend screen maps over it and throws, " +
        "taking the whole screen down",
    );
  }
}

// THE ENGINE'S HEALTH, WHOLE. The push carries the envelope GET /health
// answers, and the screens read the applied epoch and the posture off it rather
// than polling for them — so a push narrowed back to a few fields, or a store
// that kept only some of what arrived, leaves the rail unable to say whether
// the company's configuration applied.
const health = state.health;
// PRESENT rather than positive: the capture's company is seeded from a file,
// active before the control plane minted an epoch, and 0 says exactly that.
if (!health || typeof health.applied_epoch !== "number") {
  problems.push(
    `the health slice names no applied epoch (${JSON.stringify(health)}): no ` +
      "screen can say whether the configuration it saved has applied",
  );
}
if (!health || !health.posture) {
  problems.push(
    `the health slice names no posture (${JSON.stringify(health)}): a node out ` +
      "of rotation would read exactly like one serving",
  );
}

if (flags.includes("--print")) {
  console.log(JSON.stringify({
    agents: state.agents,
    events: state.events.map((e) => e.type),
    tokens: state.tokens,
    phases: [...phases],
    completedPhases: state.phases.map((p) => p.payload && p.payload.phase),
  }, null, 2));
}

if (problems.length) {
  console.error("the dashboard client could not read the server's frames:");
  for (const p of problems) console.error(`  - ${p}`);
  process.exit(1);
}
console.log(
  `replay ok: ${frames.length} frames, ${state.agents.length} seats, ` +
    `${state.events.length} events, phases ${[...phases].join("/")}, ` +
    `${state.phases.length} durable phase records`,
);
