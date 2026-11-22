// crewlet-kimi — the driver of a Kimi Code run: it starts `kimi web`, runs the
// brief in one session through Kimi's own API (KAP), hands the run a person's
// notes while it works, and writes what the run did to its stream.
//
// The engine embeds this file (kimisteer.go) and writes it into the run's work
// directory; the run's script starts it as `node <this file> <settings>`, with
// the brief on stdin (kimi.go, Kimi.Command). Plain ECMAScript on Node's own
// fetch and WebSocket, no imports beyond Node's: Kimi Code is an npm package,
// so the node that runs it runs this, and nothing is installed.
//
// THE RUN, IN ORDER. `kimi web --port 0 --json` started, its one line read
// (origin, token; the server binds 127.0.0.1) within startBoundMs; the session
// created under the run's agent profile in permission mode `auto`; its id
// reported ONCE, written beside and moved over the report, before any prompt
// (the seam's sessionMinter); the session's events subscribed; the brief
// posted in a request body, so no argv or process table holds it; then waited
// on with `:wait` until the session is idle. Its events reach stdout, a line
// each, for the live view and the decoder (kimievents.go); the driver's own
// lines are `crewlet.*`.
//
// HOW A NOTE ARRIVES. While the session works, the notes route is polled and
// each note posted to `/steer` under a prompt_id made of its inbox id
// (notePrefix), which KAP keeps as the message's id and answers, reused, with
// the prompt it holds (40927 where they conflict), so a note admitted twice
// lands once. Kimi hands it over as a user message opening a turn after the
// running tool's result; one sent while the model answers cuts that request
// short, and at idle a steer starts a turn (measured on 2.7.0 and 2.7.2). THIS DRIVER
// CONFIRMS NOTHING: a note counts as read only once the runner finds it in the
// session's own record (kimitranscript.go), so it never posts the route's /read.
// As the session goes idle the route is asked once more, briefly (stopAskMs),
// so a waiting note keeps the run going rather than waiting for its review;
// once the run is over, nothing is admitted.
//
// Bounded, paced and refused as opencode_steer.js: every request is bounded,
// a failed poll waits a second doubling to thirty, an empty answer is never
// followed sooner than a second after its poll began, and a 401 or 404 ends
// the polling for the rest of the run. SIGTERM is the end request (end.go):
// the turn is cancelled, the server stopped, and the driver exits 143.

import { spawn } from "node:child_process"
import fs from "node:fs"

// startBoundMs bounds `kimi web`'s first line: about four seconds on one core
// (measured on 2.7.2), so a minute is room for a loaded box.
const startBoundMs = 60_000

// callBoundMs bounds every request to the server but a wait.
const callBoundMs = 30_000

// waitMs is one `:wait`'s own timeout, KAP's most (60,000); waitBoundMs bounds
// the request around it.
const waitMs = 60_000
const waitBoundMs = waitMs + 15_000

// pollAbortMs bounds one poll of the notes route, which holds an empty one ten
// seconds (internal/api/mcpbridge, steerHold): that and the same again.
const pollAbortMs = 20_000

// settleMs is the pause before a session that is not yet settled is looked at
// again: the gap it closes is milliseconds long (measured on 2.7.2).
const settleMs = 250

// stopAskMs bounds the ask made as the session goes idle: one second, a round
// trip to a relayed node many times over.
const stopAskMs = 1_000

// The pause after a failed poll, and the least time from a poll that admitted
// nothing to the next: opencode_steer.js's, for its reasons.
const backoffFirstMs = 1_000
const backoffMostMs = 30_000
const pollFloorMs = backoffFirstMs

// endBoundMs bounds each step of an asked end — the cancel, then the server's
// stop — within the five seconds the script gives the driver (JobStopGrace).
const endBoundMs = 1_500

// stopBoundMs bounds the server's stop at the run's own end.
const stopBoundMs = 10_000

// notePrefix opens every prompt_id a note is steered under, and briefID is the
// brief's (kimisteer.go, kimiNotePrefix and kimiBriefID).
const notePrefix = "crewlet-note-"
const briefID = "crewlet-brief"

// kept is every event type the stream keeps: a turn's start, step and end, an
// item's start and end, the context's size and a notice. Deltas and the
// session's bookkeeping frames are left out, as nothing reads them.
const kept = new Set(["turn.started", "turn.updated", "turn.ended", "item.started", "item.ended", "context", "notice", "turns"])

const settings = JSON.parse(fs.readFileSync(process.argv[2], "utf8"))
const life = new AbortController()
let server = null
let api = null
let sid = ""
let asked = false
let socket = null
let ending = false

process.on("SIGTERM", () => {
  if (asked) return
  asked = true
  life.abort()
})

const say = (line) => process.stdout.write(JSON.stringify(line) + "\n")
const fail = (why) => {
  process.stderr.write("crewlet: " + why + "\n")
  return 1
}

process.exitCode = await run().catch((error) => fail("the run's driver failed: " + (error?.message ?? error)))
await stop()
say({ type: "crewlet.end", asked, code: process.exitCode })
process.exit(process.exitCode)

async function run() {
  const brief = fs.readFileSync(0, "utf8")
  const address = await start()
  if (typeof address === "string") return fail(address)
  api = address
  if (asked) return 143

  const config = { permission_mode: "auto", profile: settings.profile }
  if (settings.model) config.model = settings.model
  if (settings.thinking) config.thinking = settings.thinking
  const created = await call("POST", "/api/v1/sessions", { cwd: process.cwd(), agent_config: config })
  if (!created.ok || typeof created.data?.id !== "string") return fail("Kimi refused the session: " + created.why)
  sid = created.data.id
  // THE ID REPORTED ONCE, BEFORE THE BRIEF: a reader finds the whole id or none.
  fs.writeFileSync(settings.report + ".next", sid + "\n")
  fs.renameSync(settings.report + ".next", settings.report)
  if (asked) return 143

  await subscribe()
  const posted = await call("POST", `/api/v1/sessions/${sid}/prompts`, { prompt_id: briefID, content: [{ type: "text", text: brief }] })
  if (!posted.ok) return fail("Kimi refused the brief: " + posted.why)

  const notes = typeof settings.notes === "string" && settings.notes !== "" ? poller(settings.notes) : null
  notes?.open()
  for (;;) {
    if (asked) return end()
    const waited = await call("POST", `/api/v1/sessions/${sid}:wait`, { timeout_ms: waitMs }, waitBoundMs)
    if (asked) return end()
    if (!waited.ok) return fail("waiting on the session failed: " + waited.why)
    const session = waited.data?.session ?? {}
    for (const pending of waited.data?.pending_interactions ?? []) await answer(pending)
    if (session.activity === "turn") continue
    // NOT IDLE WHILE A PROMPT IS ACTIVE OR QUEUED: between the turn a note was
    // steered into and the turn it opens, the session reads idle for an
    // instant, and a run ended there cancels the note's turn (measured on
    // 2.7.2); the steered note is the active prompt throughout. Background
    // tasks run on and their end opens a turn. Either way `:wait` returns at
    // once outside a turn, so the session is looked at again after a pause.
    if (session.activity === "background" || !(await settled())) {
      await sleep(settleMs, life.signal)
      continue
    }
    notes?.close()
    if (notes && (await notes.admits(stopAskMs))) {
      notes.open()
      continue
    }
    notes?.end()
    return session.last_turn_outcome === "completed" ? 0 : 1
  }
}

// start starts the server and reads its line, or says what it never printed.
async function start() {
  server = spawn(settings.kimi || "kimi", ["web", "--port", "0", "--json"], { stdio: ["ignore", "pipe", "inherit"] })
  if (server.pid) fs.writeFileSync(settings.serverPid, String(server.pid) + "\n")
  const exited = new Promise((resolve) => server.once("exit", (code, signal) => resolve(`kimi web exited (${signal ?? code}) before it printed its address`)))
  const failed = new Promise((resolve) => server.once("error", (error) => resolve("kimi web could not be started: " + error.message)))
  const line = new Promise((resolve) => {
    let held = ""
    server.stdout.on("data", (chunk) => {
      if (held === null) return
      held += chunk
      const at = held.indexOf("\n")
      if (at < 0) return
      const first = held.slice(0, at)
      held = null
      try {
        const v = JSON.parse(first)
        if (typeof v.origin === "string" && typeof v.token === "string" && typeof v.ws_url === "string") return resolve(v)
      } catch {}
      resolve("kimi web's first line is not its address")
    })
  })
  const timer = new Promise((resolve) => setTimeout(resolve, startBoundMs, `kimi web printed no address within ${startBoundMs / 1000} seconds`).unref())
  return Promise.race([line, exited, failed, timer, aborted(life.signal, "")])
}

// subscribe streams the session's events to stdout, again after a drop while
// the run goes on: a resubscription's `turns` frame repeats the latest page.
async function subscribe() {
  let tries = 0
  const open = () =>
    new Promise((resolve) => {
      let ws
      try {
        ws = new WebSocket(api.ws_url, { headers: { authorization: "Bearer " + api.token } })
      } catch {
        return resolve(false)
      }
      socket = ws
      ws.onopen = () => {
        tries = 0
        ws.send(JSON.stringify({ type: "subscribe", request_id: "crewlet", session_id: sid }))
        resolve(true)
      }
      ws.onmessage = (m) => {
        const text = typeof m.data === "string" ? m.data : ""
        let type
        try {
          type = JSON.parse(text)?.type
        } catch {
          return
        }
        if (kept.has(type)) process.stdout.write(text.replace(/\n/g, " ") + "\n")
      }
      ws.onclose = () => {
        if (socket !== ws) return
        socket = null
        resolve(false)
        if (!life.signal.aborted && !ending && tries++ < 5) setTimeout(() => quietly(open()), backoffFirstMs * tries)
      }
      ws.onerror = () => {}
    })
  if (!(await open())) process.stderr.write("crewlet: the session's events could not be subscribed, so the live view stays empty\n")
}

// settled is whether the session holds no active or queued prompt.
async function settled() {
  const prompts = await call("GET", `/api/v1/sessions/${sid}/prompts`)
  return prompts.ok && prompts.data?.active == null && (prompts.data?.queued ?? []).length === 0
}

// answer settles an interaction the session waits on, which a headless run has
// nobody to answer: an approval approved (permission mode auto), a question
// dismissed (ask_and_wait is on the bridge).
async function answer(pending) {
  const id = pending?.interaction_id
  if (typeof id !== "string") return
  const body = pending.kind === "approval" ? { kind: "approval", decision: "approved" } : { kind: "question", dismissed: true }
  await call("POST", `/api/v1/sessions/${sid}/interactions/${encodeURIComponent(id)}`, body)
}

// end is an asked end: the turn cancelled, within its bound.
async function end() {
  if (sid !== "") await call("POST", `/api/v1/sessions/${sid}:cancel`, { include_background: true }, endBoundMs, true)
  return 143
}

// stop stops the server, gracefully and then by signal, each bounded.
async function stop() {
  ending = true
  socket?.close()
  if (server === null || server.exitCode !== null || server.signalCode !== null) return
  const gone = new Promise((resolve) => server.once("exit", resolve))
  const bound = asked ? endBoundMs : stopBoundMs
  if (api !== null) await call("POST", "/api/v1/shutdown", {}, bound, true)
  if (await within(gone, bound)) return
  server.kill("SIGTERM")
  if (await within(gone, bound)) return
  server.kill("SIGKILL")
}

// poller is the notes route's reader: open while the session works, asked
// once more as it goes idle, and ended with the run.
function poller(url) {
  const route = new AbortController()
  let gate = null
  let cursor = 0
  let wait = 0
  const admitted = new Set()

  // ask asks the route once for the notes past the cursor and steers those it
  // answers: ok where the route answered, admitted where a note was steered.
  const ask = async (ms, signal) => {
    const answer = await fetchBounded(url + "/notes?after=" + cursor, { method: "GET" }, ms, route.signal, signal)
    if (answer.status === 401 || answer.status === 404) {
      route.abort()
      return { ok: false }
    }
    if (!answer.ok) return { ok: false }
    let notes
    try {
      notes = JSON.parse(answer.text)?.notes
    } catch {
      return { ok: false }
    }
    if (!Array.isArray(notes)) return { ok: false }
    let admittedAny = false
    for (const note of notes) {
      if (!wellFormed(note)) return { ok: false, admitted: admittedAny }
      if (note.seq <= cursor) continue
      if (!admitted.has(note.inbox)) {
        // HELD BEFORE THE STEER IS SENT: the loop's ask and the idle ask can
        // overlap, and both would otherwise steer one note.
        admitted.add(note.inbox)
        const steered = await call("POST", `/api/v1/sessions/${sid}/steer`, { prompt_id: notePrefix + note.inbox, content: [{ type: "text", text: note.message }] })
        // 40927: the prompt_id is taken, so the note was admitted already.
        if (!steered.ok && steered.code !== 40927) {
          admitted.delete(note.inbox)
          return { ok: false, admitted: admittedAny }
        }
      }
      cursor = note.seq
      admittedAny = true
    }
    return { ok: true, admitted: admittedAny }
  }

  const loop = async (gated) => {
    while (!gated.aborted && !route.signal.aborted && !life.signal.aborted) {
      const began = Date.now()
      const got = await ask(pollAbortMs, gated)
      if (gated.aborted || route.signal.aborted || life.signal.aborted) return
      if (got.ok) {
        wait = 0
        const early = pollFloorMs - (Date.now() - began)
        if (!got.admitted && early > 0) await sleep(early, life.signal, gated)
        continue
      }
      wait = wait === 0 ? backoffFirstMs : Math.min(wait * 2, backoffMostMs)
      await sleep(wait, life.signal, gated)
    }
  }

  return {
    open() {
      if (gate !== null || route.signal.aborted) return
      gate = new AbortController()
      quietly(loop(gate.signal))
    },
    close() {
      gate?.abort()
      gate = null
    },
    // admits is whether one brief ask steered a note.
    admits: async (ms) => !route.signal.aborted && !life.signal.aborted && (await ask(ms)).admitted === true,
    end() {
      this.close()
      route.abort()
    },
  }
}

function wellFormed(note) {
  return (
    note !== null &&
    typeof note === "object" &&
    Number.isSafeInteger(note.seq) &&
    note.seq > 0 &&
    typeof note.inbox === "string" &&
    /^[A-Za-z0-9_-]+$/.test(note.inbox) &&
    typeof note.message === "string" &&
    note.message !== ""
  )
}

// call makes one request of the server, bounded by ms: ok with KAP's data, or
// why not, with its code. An asked end's own calls pass after the abort.
async function call(method, path, body, ms = callBoundMs, afterAbort = false) {
  const signals = afterAbort ? [] : [life.signal]
  const answer = await fetchBounded(api.origin + path, { method, headers: { authorization: "Bearer " + api.token, "content-type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) }, ms, ...signals)
  let envelope
  try {
    envelope = JSON.parse(answer.text)
  } catch {
    return { ok: false, why: answer.why ?? `HTTP ${answer.status}` }
  }
  if (envelope?.code === 0) return { ok: true, data: envelope.data }
  return { ok: false, code: envelope?.code, why: `${envelope?.code ?? answer.status} ${envelope?.msg ?? ""}`.trim() }
}

// fetchBounded is one request, bounded by ms and ended early by any of signals.
async function fetchBounded(target, init, ms, ...signals) {
  const { signal, done } = deadline(ms, ...signals)
  try {
    const response = await fetch(target, { ...init, signal })
    return { ok: response.ok, status: response.status, text: await response.text() }
  } catch (error) {
    return { ok: false, status: 0, text: "", why: signal.aborted ? `no answer within ${ms / 1000} seconds` : String(error?.message ?? error) }
  } finally {
    done()
  }
}

function within(promise, ms) {
  return Promise.race([promise.then(() => true), new Promise((resolve) => setTimeout(resolve, ms, false).unref())])
}

function aborted(signal, value) {
  return new Promise((resolve) => (signal.aborted ? resolve(value) : signal.addEventListener("abort", () => resolve(value), { once: true })))
}

function sleep(ms, ...signals) {
  return new Promise((resolve) => {
    const { signal, done } = deadline(ms, ...signals)
    signal.addEventListener("abort", () => (done(), resolve()), { once: true })
  })
}

// deadline is a signal ending after ms or with any of signals, and done, which
// lets go of the timer and every listener (opencode_steer.js's).
function deadline(ms, ...signals) {
  const controller = new AbortController()
  const abort = () => controller.abort()
  const timer = setTimeout(abort, ms)
  const live = signals.filter(Boolean)
  for (const s of live) {
    if (s.aborted) abort()
    else s.addEventListener("abort", abort, { once: true })
  }
  return {
    signal: controller.signal,
    done() {
      clearTimeout(timer)
      for (const s of live) s.removeEventListener("abort", abort)
    },
  }
}

function quietly(task) {
  task.catch(() => {})
}
