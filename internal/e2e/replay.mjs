// Replay captured server frames through the dashboard's OWN client.
//
// Not a test of the dashboard — its own Vitest suites in dashboard/ are that.
// This is the last link of the server's end-to-end gate: golden_test.go, beside
// it, runs a real company, captures every frame its WebSocket actually pushed,
// and hands the file here. What runs against those bytes is the dashboard's OWN
// protocol module — the same source its bundle contains, emitted once more as
// plain ESM — through the real message dispatch, so the question it answers is
// the only one that matters at that seam: NOT "did the server send something"
// but "does the client understand what the server sent".
//
// The distinction is not academic. It was written because a full turn's worth
// of `agents` pushes were being sent as an object keyed by role, while the
// store guards `applyAgents` with `Array.isArray` and dropped every one of
// them: the server's own tests passed, the client's own tests passed, the
// socket carried the frames, and the seat rendered idle from the first phase
// to the last.
//
// It lives beside golden_test.go rather than under dashboard/ because it is
// half of that test and not a test of the dashboard: the two change together —
// what the Go half captures is what this half asserts — and nothing else runs
// it.
//
// Usage: node replay.mjs <frames.json> <act.json> <chat.json> [--print]
//
//   frames.json  a JSON array of raw frame strings, in arrival order
//   act.json     a JSON array of `/operator/act` exchanges: {tool, args,
//                op_id, status, body, reads} — the operation key the press
//                sent in its `Idempotency-Key` header, the engine's answer
//                byte for byte, and `reads` the questions that read the log
//                the write landed on
//   chat.json    what the captured tab did in the company's own chat,
//                written beside the frames rather than read from them:
//                {room, person, seat, seq, changes, newest, cursor}, each
//                change {op, message_id, author, author_kind, thread_root}
//   --print      dump the resulting store state as JSON on stdout
//
// Exits non-zero with a diagnosis on stderr when the frames do not drive the
// client to a coherent state.

import { existsSync, readFileSync } from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

// THE DASHBOARD UNDER TEST. CREWLET_DASHBOARD_ROOT names the tree; unset, it is
// `static/dashboard`, the tree the binary embeds — the one that matters, since
// this replay is the compatibility reference for the client's half of the wire
// protocol and has to run against the bytes a browser is actually served.
// golden_test.go always names it, so the gate never rests on the default.
//
// A relative value is read against the REPOSITORY. Read against the working
// directory, `node internal/e2e/replay.mjs` at the root and `node replay.mjs`
// here would test different trees; read against this file's own directory, as
// it once was while its comment said the repository, `static/dashboard` named
// a tree nobody has. The repository is the nearest directory above this file
// holding a go.mod — the rule internal/sourcetree finds the module root by for
// the Go half — never a counted `../..`, which every move would have to get
// right again.
//
// Resolution is CHECKED rather than assumed. A wrong root would otherwise
// surface as ERR_MODULE_NOT_FOUND on the protocol's import, which reads as a
// missing dashboard file rather than as a missing dashboard.
function dashboardDir(named) {
  if (named && isAbsolute(named)) return resolve(named);
  return resolve(repositoryRoot(), named || "static/dashboard");
}

function repositoryRoot() {
  const here = dirname(fileURLToPath(import.meta.url));
  for (let dir = here; ; dir = dirname(dir)) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    if (dirname(dir) === dir) {
      console.error(
        `replay: no go.mod in any directory above ${here}, so there is no ` +
          "repository to read CREWLET_DASHBOARD_ROOT against: name the tree " +
          "to test by an absolute path",
      );
      process.exit(2);
    }
  }
}

const DASHBOARD_DIR = dashboardDir(process.env.CREWLET_DASHBOARD_ROOT);

if (!existsSync(join(DASHBOARD_DIR, "index.html"))) {
  console.error(
    `replay: no dashboard at ${DASHBOARD_DIR} (no index.html there).\n` +
      `Set CREWLET_DASHBOARD_ROOT to the tree to test, or leave it unset to ` +
      `use the one in this checkout. If it is missing entirely, the built ` +
      `dashboard has not been committed — run \`make dashboard\`.`,
  );
  process.exit(2);
}

// The wire protocol, as plain ESM.
//
// A SEPARATE build target from the application bundle (dashboard/
// vite.protocol.config.ts): the app bundle is minified, code-split and boots
// React against a DOM, none of which a Node process has. This one is the same
// source, emitted once more unminified, so the replay runs the client's REAL
// dispatch table rather than a re-implementation of it that could agree with a
// server the dashboard does not.
const PROTOCOL = join(DASHBOARD_DIR, "protocol.js");

if (!existsSync(PROTOCOL)) {
  console.error(
    `replay: no protocol.js in ${DASHBOARD_DIR}. It is a second Vite build ` +
      "target and easy to lose: `make dashboard` emits it beside the app bundle.",
  );
  process.exit(2);
}

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

const { Store, LiveSocket, act, SessionFloors, domainOf } = await import(pathToFileURL(PROTOCOL).href);

const [file, actFile, chatFile, ...flags] = process.argv.slice(2);
if (!file || !actFile || !chatFile || [actFile, chatFile].some((f) => f.startsWith("--"))) {
  console.error("usage: node replay.mjs <frames.json> <act.json> <chat.json> [--print]");
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
// And who was in the founder's room, read the same way: the room's members
// move with every push, so by the last one the seat has long stopped working
// and only the walk can say the room ever showed it — the founder as a person
// in the room, and the seat as its agent member at work there.
const talked = JSON.parse(readFileSync(chatFile, "utf8"));
let sawLooking = false;
let sawWorkingInRoom = false;
const replay = new Store();
const probe = new LiveSocket(replay);
for (const raw of frames) {
  probe.onMessage(raw);
  const present = replay.state.chatPresence.rooms[talked.room];
  for (const m of (present && present.members) || []) {
    if (m.seat === talked.person && m.kind === "human") sawLooking = true;
    if (m.seat === talked.seat && m.kind === "agent" && m.activity === "editing") {
      sawWorkingInRoom = true;
    }
  }
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
// THE FLEET AND ITS ALARMS, which the sidebar's engine panel draws from the
// push rather than from a request of its own: how many nodes are live, how many
// alarms stand, and which has stood longest. The capture's company has never
// taken a backup, so at least one alarm stands.
if (!health || typeof health.nodes !== "number" || health.nodes < 1) {
  problems.push(
    `the health slice counts no live nodes (${JSON.stringify(health && health.nodes)}): ` +
      "the engine panel cannot say how big the fleet is",
  );
}
const alarms = health && health.alarms;
if (!alarms || typeof alarms.count !== "number" || alarms.count < 1 || !alarms.worst) {
  problems.push(
    `the health slice carries no standing alarm (${JSON.stringify(alarms)}) on a ` +
      "company that has never taken a backup: the engine panel reads as clear",
  );
}

// THE COMPANY'S OWN CHAT, AS THE FOUNDER'S TAB WAS TOLD IT. The capture's tab
// is a person's, so the engine pushed it the room the founder made and talked
// in: a frame per committed record, the room's presence, and the founder's own
// read state. The store files a record frame under its room — the high-water
// mark a screen compares against what it has drawn, and the frames themselves
// — and each of those has to come out as what the tab did in the room and
// what the engine minted for it, which chat.json carries and no frame wrote
// (internal/e2e's chatCapture says where each field comes from): a mark that
// stopped short, a record that went unfiled or was filed under the wrong
// message, author or thread, or a position that moved backwards is a screen
// that re-reads the wrong thing, or nothing, for the life of the tab.
//
// EACH RECORD ON ITS OWN, by its op, the message it names, its author and
// their kind, and a post's thread. Taken together the room's messages are
// named by its posts alone, so an edit, a delete or a reaction filed under the
// wrong message would pass a check over the room's set of ids — and that is
// the frame that sends a screen to re-read the wrong range of a room.
const room = state.chat.rooms[talked.room];
if (!room) {
  problems.push(
    `no frame for the founder's room ${talked.room} reached the store: the chat ` +
      "screen would draw a room nothing ever said anything in",
  );
} else {
  if (room.seq !== talked.seq) {
    problems.push(
      `the store's high-water mark for the room is ${room.seq} and the engine ` +
        `numbered it to ${talked.seq}: a screen holding the room would re-read it ` +
        "for ever, or never",
    );
  }
  // A FIELD THE FRAME LEFT OUT IS EMPTY, as chat.json writes it:
  // a record about the room names no message, and only a reply has a thread.
  const recordOf = (c) => ({
    op: c.op,
    message_id: c.message_id || "",
    author: c.author || "",
    author_kind: c.author_kind || "",
    thread_root: c.thread_root || "",
  });
  const same = (a, b) =>
    a.op === b.op &&
    a.message_id === b.message_id &&
    a.author === b.author &&
    a.author_kind === b.author_kind &&
    a.thread_root === b.thread_root;
  const describe = (r) =>
    `${r.op}${r.message_id ? ` of ${r.message_id}` : ""} by ${r.author} (${r.author_kind})` +
    (r.thread_root ? ` in the thread of ${r.thread_root}` : "");
  const took = talked.changes.map(recordOf);
  const filed = room.changes.map(recordOf);
  const unfiled = took.filter((want) => !filed.some((got) => same(got, want)));
  if (unfiled.length) {
    problems.push(
      `the room took ${unfiled.map(describe).join("; ")} and the store filed no frame ` +
        `saying so (filed ${filed.map(describe).join("; ") || "none"})`,
    );
  }
  const stray = filed.filter((got) => !took.some((want) => same(got, want)));
  if (stray.length) {
    problems.push(
      `the store filed ${stray.map(describe).join("; ")}, which is no record the room ` +
        "took: a screen would re-read a message nothing touched, or credit a change " +
        "to somebody who never made it",
    );
  }
  if (!room.position || room.position.stream !== talked.newest.stream ||
    room.position.generation !== talked.newest.generation ||
    room.position.seq !== talked.newest.seq) {
    problems.push(
      `the room's newest moment is ${JSON.stringify(room.position)} and its last ` +
        `record sits at ${JSON.stringify(talked.newest)}: the rail would order it ` +
        "among the rooms by a moment that is not its own",
    );
  }
}
if (!sawLooking) {
  problems.push(
    `no presence frame ever showed ${talked.person} looking at the room their tab ` +
      "said it was looking at",
  );
}
if (!sawWorkingInRoom) {
  problems.push(
    `no presence frame ever showed ${talked.seat} at work in the room it answered ` +
      "in: a seat's turn there would look like silence until its reply landed",
  );
}
const read = state.chatRead;
if (!read || read.handle !== talked.person) {
  problems.push(
    `the store holds ${JSON.stringify(read && read.handle)}'s read state, not ` +
      `${talked.person}'s: the rail's badges would be counted for somebody else`,
  );
} else {
  const cursor = read.cursors && read.cursors[talked.room];
  if (cursor !== talked.cursor) {
    problems.push(
      `the founder read the room through ${talked.cursor} and the store holds ` +
        `${JSON.stringify(cursor)}: the badge another tab cleared stays lit here`,
    );
  }
  if (typeof read.revision !== "number") {
    problems.push(
      `the read state carries no revision (${JSON.stringify(read)}): two tabs' ` +
        "flushes could not be ordered, and the later would lose",
    );
  }
}

// THE SESSION FLOOR, FROM REAL ANSWERS. The capture carries writes the
// founder made through `/operator/act` — a pin on the tracker's log and a
// message on chat's — and each answer is handed to the client's OWN `act`,
// the one function every button writes through, and its OWN `SessionFloors`.
// The floor the tab then holds for that log must be the position the engine
// answered with, byte for byte, and every question that reads it must name it
// as `min_position` at `session`: that is what the Go half then asks the
// engine with, and got the write back. A position the client could not parse
// raises nothing, so the screen that pressed the button would redraw from
// before the press with nothing to say so.
const exchanges = JSON.parse(readFileSync(actFile, "utf8"));
if (!Array.isArray(exchanges) || exchanges.length === 0) {
  console.error(`replay: ${actFile} holds no act exchanges`);
  process.exit(2);
}
// ONE TAB'S FLOORS ACROSS EVERY WRITE, as a tab holds them: a write on one
// log raises that log's floor and leaves every other where it was.
const floors = new SessionFloors();
const wrote = [];
for (const exchange of exchanges) {
  const sent = [];
  globalThis.fetch = async (href, init) => {
    sent.push({ href: String(href), init });
    return new Response(exchange.body, {
      status: exchange.status,
      headers: { "Content-Type": "application/json" },
    });
  };
  const result = await act(exchange.tool, exchange.args, {
    opId: exchange.op_id,
    floors,
  });
  wrote.push(result);
  const engineAnswer = JSON.parse(exchange.body);
  if (sent.length !== 1) {
    problems.push(`one ${exchange.tool} press sent ${sent.length} requests, want exactly one`);
  } else {
    const { href, init } = sent[0];
    if (new URL(href).pathname !== `/operator/act/${exchange.tool}` || init.method !== "POST") {
      problems.push(`the write went to ${init.method} ${href}, not POST /operator/act/${exchange.tool}`);
    }
    const body = JSON.parse(init.body);
    // THE OPERATION RIDES THE HEADER, and the body is the arguments alone:
    // the engine refuses a body key it does not read, and a key minted
    // anywhere but the press is a retry that writes twice.
    const key = (init.headers || {})["Idempotency-Key"];
    if (key !== exchange.op_id) {
      problems.push(
        `the ${exchange.tool} write carried operation key ${JSON.stringify(key)}, ` +
          "not the press's own",
      );
    }
    const extra = Object.keys(body).filter((k) => k !== "args");
    if (extra.length) {
      problems.push(`the ${exchange.tool} write's body carries ${extra.join(", ")} beside its arguments`);
    }
    if (JSON.stringify(body.args) !== JSON.stringify(exchange.args)) {
      problems.push(
        `the client sent ${JSON.stringify(body.args)}, not the arguments the engine answered: ` +
          JSON.stringify(exchange.args),
      );
    }
  }
  if (result.kind !== engineAnswer.outcome) {
    problems.push(
      `the engine answered ${exchange.tool} ${JSON.stringify(engineAnswer.outcome)} and the ` +
        `client read ${JSON.stringify(result.kind)}${result.reason ? ` (${result.reason})` : ""}`,
    );
  }
  const domain = result.domain;
  if (!domain) {
    problems.push(`${exchange.tool} names no domain a read can wait on, so its write raises no floor`);
    continue;
  }
  const floor = floors.floor(domain);
  if (floor !== engineAnswer.position) {
    problems.push(
      `the ${domain} floor is ${JSON.stringify(floor)} after a write the engine placed at ` +
        `${JSON.stringify(engineAnswer.position)}: the next read waits for the wrong position`,
    );
  }
  if (!Array.isArray(exchange.reads) || exchange.reads.length === 0) {
    problems.push(`the ${exchange.tool} exchange names no question that reads its log`);
  }
  for (const kind of exchange.reads || []) {
    const fresh = floors.freshness(kind);
    if (domainOf(kind) !== domain || !fresh || fresh.read_level !== "session" ||
      fresh.min_position !== engineAnswer.position) {
      problems.push(
        `a read of ${kind} after the ${exchange.tool} names ${JSON.stringify(fresh)}, not a ` +
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
    chat: state.chat,
    chatRead: state.chatRead,
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
    `a room to #${room.seq} through ${[...new Set(room.changes.map((c) => c.op))].join("/")}, ` +
    wrote.map((w) => `${w.tool} ${w.kind} at floor ${w.position}`).join(", "),
);
