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
// Usage: node replay.mjs <frames.json> <act.json> [--print]
//
//   frames.json  a JSON array of raw frame strings, in arrival order
//   act.json     one `/operator/act` exchange: {tool, args, op_id, status,
//                body} — the operation key the press sent in its
//                `Idempotency-Key` header, and the engine's answer byte for byte
//   --print      dump the resulting store state as JSON on stdout
//
// Exits non-zero with a diagnosis on stderr when the frames do not drive the
// client to a coherent state.

import { readFileSync } from "node:fs";
import { PROTOCOL_URL } from "./dashboardRoot.mjs";

// The browser bits the socket reaches for. This socket is never dialled — the
// frames are fed straight to the message handler — but the module builds one
// at construction, so the global has to exist. There is no token store to
// stub: the dashboard's credential is the session cookie, which no script
// holds.
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
globalThis.fetch = async () => {
  throw new Error("offline: the replay has no server");
};
// Where `rest.ts` addresses a request. Never dialled: the one request the
// replay lets through is answered by the captured exchange below.
globalThis.location = { origin: "http://replay.invalid" };

const { Store, LiveSocket, act, SessionFloors, domainOf } = await import(PROTOCOL_URL.href);

const [file, actFile, ...flags] = process.argv.slice(2);
if (!file || !actFile || actFile.startsWith("--")) {
  console.error("usage: node replay.mjs <frames.json> <act.json> [--print]");
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

// EVERY KIND THIS ENGINE PUSHED IS ONE ITS OWN CLIENT DISPATCHES. The store
// ignores a kind it does not know, because a newer peer in a fleet mid-upgrade
// pushes kinds this bundle was built before — and counts it, because that
// same silent fall-through is exactly what a kind this build's engine sends
// and this build's client forgot looks like. Here engine and client are one
// build, so the count must be zero.
if (store.unknownPushes.size) {
  const seen = [...store.unknownPushes].map(([kind, n]) => `${kind} ×${n}`).join(", ");
  problems.push(
    `the client does not dispatch every kind the engine pushed (${seen}): each ` +
      "of those frames fell through the socket's switch and reached no screen",
  );
}

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

// A SEAT IS PAIRED BY ITS AGENT ID, never by its name: the store merges an
// overlay onto the roster row carrying the same `agent_id` and drops one for
// a seat the roster does not carry. So both halves of the pairing are the
// server's to send — every roster row and every overlay must carry the id —
// and a push that stopped sending it would leave every seat idle with each
// overlay silently dropped, which the `working` check below reports only as a
// symptom.
const anonymousRows = state.agents.filter((a) => typeof a.agent_id !== "string" || a.agent_id === "");
if (anonymousRows.length) {
  problems.push(
    `${anonymousRows.length} of ${state.agents.length} roster rows carry no \`agent_id\`: ` +
      "no overlay can ever reach them",
  );
}
const rosterIDs = new Set(state.agents.map((a) => a.agent_id));
let overlays = 0;
let unkeyed = 0;
const strangers = new Set();
for (const raw of frames) {
  let msg;
  try {
    msg = JSON.parse(raw);
  } catch {
    continue;
  }
  if (msg.kind !== "agents" || !Array.isArray(msg.data)) continue;
  for (const row of msg.data) {
    overlays++;
    if (typeof row.agent_id !== "string" || row.agent_id === "") unkeyed++;
    else if (!rosterIDs.has(row.agent_id)) strangers.add(row.agent_id);
  }
}
if (overlays === 0) {
  problems.push("no `agents` overlay was pushed: no seat's live state could ever move");
}
if (unkeyed) {
  problems.push(`${unkeyed} of ${overlays} \`agents\` overlays carry no \`agent_id\`: the store drops them`);
}
if (strangers.size) {
  problems.push(
    `\`agents\` overlays name ${[...strangers].join(", ")}, which no roster row carries: ` +
      "the store drops them",
  );
}

// Somewhere in the run a seat must have been working with a live call — that
// is what "the UI showing a live turn" means. The store holds only the LATEST
// state, so this is re-derived from the frames rather than read off the end:
// by the last frame the turn has finished and the row is idle again, which is
// correct and is exactly why the end state cannot answer this.
let sawWorking = false;
let sawLiveCall = false;
const phases = new Set();
// What else a live row is drawn from, read the same way: the item the turn is
// charged to (every live row, turn row and task page links on it), the round
// cap a running phase is measured against, the stage of the turn a seat is on,
// and a seat meter the gate has refused.
const workItems = new Set();
let sawRoundCap = false;
const stages = new Set();
let refusedWindow = null;
const replay = new Store();
const probe = new LiveSocket(replay);
for (const raw of frames) {
  probe.onMessage(raw);
  for (const agent of replay.state.agents) {
    if (agent.activity === "working") sawWorking = true;
    if (agent.live_call) {
      sawLiveCall = true;
      if (agent.live_call.phase) phases.add(agent.live_call.phase);
      const item = agent.live_call.work_item;
      if (item && item.key && item.id && item.project) workItems.add(item.key);
      if (typeof agent.live_call.max_rounds === "number" && agent.live_call.max_rounds > 0) {
        sawRoundCap = true;
      }
    }
    if (agent.turn && agent.turn.stage) stages.add(agent.turn.stage);
    const windows = (agent.budget && agent.budget.windows) || [];
    for (const w of windows) {
      if (w.refused_at && w.state === "refusing") refusedWindow = w;
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

// THE ITEM A TURN IS ON. The capture's company turn is woken by a task, so a
// live call must name it whole — backend-qualified id, key and project —
// or the live row cannot link to the task it is working and the task page
// cannot find the turn running on it.
if (!workItems.size) {
  problems.push(
    "no live call named the work item its turn was woken for: every live row " +
      "reads as a turn on nothing",
  );
}
if (!sawRoundCap) {
  problems.push(
    "no live call stated `max_rounds`: a running phase cannot say how far " +
      "through its round cap it is",
  );
}

// THE STAGE OF THE TURN A SEAT IS ON — `context`, `phase` or `parked` — which
// is what tells a seat assembling its context from one mid-phase and from one
// waiting on a coding run. Every value must be one the client knows, and the
// capture's turn must have been seen in its phases.
const STAGES = ["context", "phase", "parked"];
for (const stage of stages) {
  if (!STAGES.includes(stage)) {
    problems.push(`a seat's turn is in stage ${JSON.stringify(stage)}, which no screen draws`);
  }
}
if (!stages.has("phase")) {
  problems.push(
    `no seat's turn was ever in its \`phase\` stage (saw ${[...stages].join(", ") || "none"})`,
  );
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

// AND A WINDOW THE GATE HAS REFUSED. One seat in the capture caps its day
// below a single model call, so its meter must arrive stamped and judged —
// `refused_at` beside `state: refusing`, with the span it resets at — or the
// "refusing charges" row every budget surface draws is one nothing can reach.
if (!refusedWindow) {
  problems.push(
    "no seat meter ever carried a refused window: the seat the gate turned " +
      "away reads as merely full, and nothing says when it was refused",
  );
} else {
  if (Number.isNaN(Date.parse(refusedWindow.refused_at))) {
    problems.push(`the refused window's refused_at is not a time: ${JSON.stringify(refusedWindow)}`);
  }
  if (!refusedWindow.resets_at || Number.isNaN(Date.parse(refusedWindow.resets_at))) {
    problems.push(
      `the refused window says nothing about when it resets: ${JSON.stringify(refusedWindow)}`,
    );
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
// THE FLEET AND ITS ALARMS, which the sidebar's health card draws from the
// push rather than from a request of its own: how many nodes are live, and how
// many alarms stand and how bad the worst is. The capture's company has never
// taken a backup, so at least one alarm stands.
if (!health || typeof health.nodes !== "number" || health.nodes < 1) {
  problems.push(
    `the health slice counts no live nodes (${JSON.stringify(health && health.nodes)}): ` +
      "the health card cannot say how big the fleet is",
  );
}
const alarms = health && health.alarms;
if (!alarms || typeof alarms.count !== "number" || alarms.count < 1 || !alarms.worst) {
  problems.push(
    `the health slice carries no standing alarm (${JSON.stringify(alarms)}) on a ` +
      "company that has never taken a backup: the health card reads as clear",
  );
}

// THE SESSION FLOOR, FROM A REAL ANSWER. The capture ends with a write the
// founder made through `/operator/act`, and its answer is handed to the
// client's OWN `act` — the one function every button writes through — and its
// OWN `SessionFloors`. The floor the tab then holds must be the position the
// engine answered with, byte for byte, and every question that reads the
// written domain must name it as `min_position` at `session`: that is what
// the Go half then asks the engine with, and got the write back. A position
// the client could not parse raises nothing, so the screen that pressed the
// button would redraw from before the press with nothing to say so.
const exchange = JSON.parse(readFileSync(actFile, "utf8"));
const sent = [];
globalThis.fetch = async (href, init) => {
  sent.push({ href: String(href), init });
  return new Response(exchange.body, {
    status: exchange.status,
    headers: { "Content-Type": "application/json" },
  });
};
const floors = new SessionFloors();
const wrote = await act(exchange.tool, exchange.args, {
  opId: exchange.op_id,
  floors,
});
const engineAnswer = JSON.parse(exchange.body);
if (sent.length !== 1) {
  problems.push(`one press sent ${sent.length} requests, want exactly one`);
} else {
  const { href, init } = sent[0];
  if (new URL(href).pathname !== `/operator/act/${exchange.tool}` || init.method !== "POST") {
    problems.push(`the write went to ${init.method} ${href}, not POST /operator/act/${exchange.tool}`);
  }
  const body = JSON.parse(init.body);
  // THE OPERATION RIDES THE HEADER, and the body is the arguments alone: the
  // engine refuses a body key it does not read, and a key minted anywhere but
  // the press is a retry that writes twice.
  const key = (init.headers || {})["Idempotency-Key"];
  if (key !== exchange.op_id) {
    problems.push(`the write carried operation key ${JSON.stringify(key)}, not the press's own`);
  }
  const extra = Object.keys(body).filter((k) => k !== "args");
  if (extra.length) {
    problems.push(`the write's body carries ${extra.join(", ")} beside its arguments`);
  }
  if (JSON.stringify(body.args) !== JSON.stringify(exchange.args)) {
    problems.push(
      `the client sent ${JSON.stringify(body.args)}, not the arguments the engine answered: ` +
        JSON.stringify(exchange.args),
    );
  }
}
if (wrote.kind !== engineAnswer.outcome) {
  problems.push(
    `the engine answered ${JSON.stringify(engineAnswer.outcome)} and the client read ` +
      `${JSON.stringify(wrote.kind)}${wrote.reason ? ` (${wrote.reason})` : ""}`,
  );
}
const domain = wrote.domain;
if (!domain) {
  problems.push(`${exchange.tool} names no domain a read can wait on, so its write raises no floor`);
} else {
  const floor = floors.floor(domain);
  if (floor !== engineAnswer.position) {
    problems.push(
      `the ${domain} floor is ${JSON.stringify(floor)} after a write the engine placed at ` +
        `${JSON.stringify(engineAnswer.position)}: the next read waits for the wrong position`,
    );
  }
  for (const kind of ["work_person", "work_items"]) {
    const fresh = floors.freshness(kind);
    if (domainOf(kind) !== domain || !fresh || fresh.read_level !== "session" ||
      fresh.min_position !== engineAnswer.position) {
      problems.push(
        `a read of ${kind} after the write names ${JSON.stringify(fresh)}, not a ` +
          `session read at ${JSON.stringify(engineAnswer.position)}`,
      );
    }
  }
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
    `${state.phases.length} durable phase records, on ${[...workItems].join("/")}, ` +
    `stages ${[...stages].join("/")}, a refused ${refusedWindow.period} window, ` +
    `${exchange.tool} ${wrote.kind} at floor ${wrote.position}`,
);
