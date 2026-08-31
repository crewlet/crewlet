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
    if (agent.state === "working") sawWorking = true;
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
for (const want of ["plan", "execute", "review"]) {
  if (!phases.has(want)) {
    problems.push(`no live call named the ${want} phase (saw ${[...phases].join(", ") || "none"})`);
  }
}

// The spend rollup. The store takes it two ways and both have to work: a
// snapshot is accepted only `if (snap.tokens && snap.tokens.totals)`, and a
// push is stored as-is for the Spend screen to read `.since_days` off. A list
// of raw records passes neither, and the screen renders blank with the numbers
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
  if (!state.tokens.since_days) {
    problems.push(
      "the spend rollup has no `since_days`: the screen prints it beside the " +
        "numbers and renders a 0-day window without it",
    );
  }
  if (!Array.isArray(state.tokens.by_phase)) {
    problems.push(
      "`by_phase` is not an array: the Spend screen maps over it and throws, " +
        "taking the whole screen down",
    );
  }
}

if (flags.includes("--print")) {
  console.log(JSON.stringify({
    agents: state.agents,
    events: state.events.map((e) => e.type),
    tokens: state.tokens,
    phases: [...phases],
  }, null, 2));
}

if (problems.length) {
  console.error("the dashboard client could not read the server's frames:");
  for (const p of problems) console.error(`  - ${p}`);
  process.exit(1);
}
console.log(
  `replay ok: ${frames.length} frames, ${state.agents.length} seats, ` +
    `${state.events.length} events, phases ${[...phases].join("/")}`,
);
