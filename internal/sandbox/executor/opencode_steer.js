// crewlet.steer — the plugin an OpenCode run's server loads to admit a person's
// notes to the run's turn into the run's own session, at its next step.
//
// The engine embeds this file (steer.go) and writes it into the run's work
// directory as index.js, a directory of its own that the run's configuration
// names with the run's notes route as its one option — a {file:} reference,
// so the configuration holds a path and never the route's token
// (opencode.go, OpenCode.WriteConfig). Plain ECMAScript with no imports:
// nothing is installed and nothing is fetched to load it, and it runs alike
// under OpenCode's Bun and under the node its suite drives it with
// (opencode_steer_test.go).
//
// HOW A NOTE ARRIVES. A note is admitted to the session's inbox as a STEER
// (`delivery: "steer"`): OpenCode promotes every steer waiting at its next step
// boundary — after the tool results of the step in flight, or at the boundary
// after a final step that made no tool call — as a user message the next model
// request carries, and reports it delivered (`session.inbox.delivered`). Only
// then is it confirmed read to the engine, which records it read by the run's
// job; a note admitted and never delivered is never confirmed, and waits for
// the run's review. Measured on OpenCode 2.0.32 and 2.1.4.
//
// ONLY WHILE THE SESSION'S EXECUTION RUNS. Notes are polled and admitted
// between the run's session's `session.execution.started` (or any step of it
// starting) and its `succeeded`, `failed` or `interrupted`, and each with
// `resume: false`: a steer admitted once an execution has settled starts
// nothing — measured on 2.1.4 — where one that resumed the session would start
// an execution `opencode run` no longer watches, whose answer nobody reads and
// whose server the run's script then stops. So a note sent after the run's
// last step waits on the run's record for its review. One whose admission
// landed as the execution settled, after OpenCode last read the session's
// inbox, stays pending there too — and the inbox outlives the server, so a run
// carried on in the same session would be handed it ahead of its next prompt
// while the continuation's brief carries it as well: the run's script cancels
// every such admission once the run has ended (opencode.go,
// openCodeLeftoverSteers), so the record alone holds the note and whatever
// reads the turn next reads it once. An execution OpenCode starts itself
// afterwards opens the gate again. The session is the one the
// run's script names, handed over as CREWLET_STEER_SESSION; another session
// of the server's — a subagent's — is never read from or written to.
//
// THE ENGINE IS ASKED, NEVER WAITED FOR. setup only registers and returns:
// OpenCode holds every prompt until every plugin's setup has resolved, so a
// setup that waited on the engine would hold the brief. Every request is
// bounded, a failed one is asked again after a pause that doubles from a
// second to thirty and resets on a success — a stopping node's 503 among them —
// and a poll that admitted nothing is never followed by the next sooner than a
// second after it began, so no answer can drive the plugin into a loop of
// requests. A refusal of the route — its token no longer resolves (401), or the
// run has no notes route (404) — ends the plugin for the rest of the run: its
// notes wait for its review. The cleanup setup returns ends everything at once.
//
// IT WRITES NOTHING, anywhere: OpenCode reloads a configured plugin whose
// directory changes, and the notes themselves never touch the box's disk.

// pollAbortMs bounds one poll: the engine holds a poll for up to ten seconds
// while no note waits (internal/api/mcpbridge, steerHold), so twenty is that
// hold and the same again for a relay and a slow network — and short of Bun's
// own idle deadline on a request, which every request here turns off in its
// favour (`timeout: false`).
const pollAbortMs = 20_000

// readAbortMs bounds one confirmation: a single write on the run's record.
const readAbortMs = 10_000

// The pause after a failed request: a second, doubling to thirty, reset by the
// next request that succeeds — so a bridge that is down delays a note by at
// most thirty seconds once it is back, and costs a request every thirty while
// it is not.
const backoffFirstMs = 1_000
const backoffMostMs = 30_000

// pollFloorMs is the least time from one poll's start to the next one's, after
// an answer that admitted nothing. The engine answers an empty poll only once
// its hold is over, so polling again at once is the long poll working — but an
// answer that came back empty sooner was cut short, by the engine or by
// anything between, and asking again at once would make the plugin's rate of
// requests whatever the network's round trip is, for as long as that lasts. A
// second, the first pause after a failure: no answer of any server can drive
// the plugin faster than that.
const pollFloorMs = backoffFirstMs

export default {
  id: "crewlet.steer",
  async setup(ctx) {
    const url = typeof ctx?.options?.url === "string" ? ctx.options.url : ""
    const session = globalThis.process?.env?.CREWLET_STEER_SESSION ?? ""
    // A run its launch wired to no notes — or one whose reference OpenCode
    // read as empty — has nothing to poll.
    if (url === "" || session === "") return undefined
    return arm(ctx, url, session)
  },
}

// arm starts the plugin's work for one session and returns its cleanup.
function arm(ctx, url, session) {
  // life is the plugin's whole run: the cleanup, or a refusal, ends it.
  const life = new AbortController()
  // gate is open — a controller of its own — while the session's execution runs.
  let gate = null
  // cursor is the place of the last note admitted, which a poll asks past.
  let cursor = 0
  // admitted maps the inbox id each note was handed to the session under to
  // the note's own id; owed holds the notes delivered and not yet confirmed.
  const admitted = new Map()
  const owed = new Set()
  let confirming = false

  const open = () => {
    if (gate !== null || life.signal.aborted) return
    gate = new AbortController()
    quietly(poll(gate.signal))
  }
  const close = () => {
    if (gate === null) return
    gate.abort()
    gate = null
  }

  // on reads one event of the server's: only the run's own session's.
  const on = (event) => {
    const data = event?.data
    if (data === null || typeof data !== "object" || data.sessionID !== session) return
    switch (event.type) {
      case "session.execution.started":
      case "session.step.started":
        open()
        return
      case "session.execution.succeeded":
      case "session.execution.failed":
      case "session.execution.interrupted":
        close()
        return
      case "session.inbox.delivered": {
        const note = admitted.get(data.inboxID)
        if (note === undefined) return
        admitted.delete(data.inboxID)
        owed.add(note)
        quietly(confirm())
        return
      }
    }
  }

  // listen follows the server's events for the plugin's whole run, listening
  // again after a pause where the subscription ends without being asked to.
  const listen = async () => {
    let wait = 0
    while (!life.signal.aborted) {
      try {
        for await (const event of ctx.event.subscribe({ signal: life.signal })) {
          wait = 0
          on(event)
        }
      } catch {
        // Said by what follows: the subscription is opened again.
      }
      if (life.signal.aborted) return
      wait = next(wait)
      await sleep(wait, life.signal)
    }
  }

  // poll asks the route for the notes past the cursor and admits them, for as
  // long as the gate it was started under stays open.
  const poll = async (gated) => {
    let wait = 0
    while (!gated.aborted && !life.signal.aborted) {
      const began = Date.now()
      const past = cursor
      const answer = await call(life.signal, "GET", url + "/notes?after=" + cursor, undefined, pollAbortMs, gated)
      if (gated.aborted || life.signal.aborted) return
      if (answer.refused) {
        life.abort()
        return
      }
      if (answer.ok && (await admit(answer.text, gated))) {
        wait = 0
        // AN ANSWER THAT ADMITTED NOTHING waits out the floor (pollFloorMs)
        // before the next poll; one that admitted a note asks at once for
        // the notes after it.
        const early = pollFloorMs - (Date.now() - began)
        if (cursor === past && early > 0) await sleep(early, life.signal, gated)
        continue
      }
      wait = next(wait)
      await sleep(wait, life.signal, gated)
    }
  }

  // admit hands each note of one poll's answer to the session, oldest first,
  // advancing the cursor past each one it admitted. False for an answer that
  // is not one, or an admission that failed — the cursor stays where it was,
  // and the note is asked for, and admitted under the same id, again.
  const admit = async (text, gated) => {
    let notes
    try {
      notes = JSON.parse(text)?.notes
    } catch {
      return false
    }
    if (!Array.isArray(notes)) return false
    for (const note of notes) {
      if (!wellFormed(note)) return false
      if (note.seq <= cursor) continue
      // A gate closed meanwhile admits nothing more: what is left is asked
      // for again by the next execution's poll, or read by the review.
      if (gated.aborted || life.signal.aborted) return true
      // KNOWN BEFORE IT IS ADMITTED, because OpenCode can deliver a note
      // before the call that admitted it has returned.
      admitted.set(note.inbox, note.id)
      try {
        await ctx.session.prompt({
          sessionID: session,
          id: note.inbox,
          text: note.message,
          delivery: "steer",
          resume: false,
        })
      } catch {
        return false
      }
      cursor = note.seq
    }
    return true
  }

  // confirm records every note delivered and not yet confirmed as read, until
  // none is owed. One at a time; a failed confirmation is made again after a
  // pause, and a note never confirmed waits for the run's review. Every note
  // owed is one this job's poll returned, and a turn takes at most ten, which
  // is what one read may name (internal/api/mcpbridge).
  const confirm = async () => {
    if (confirming) return
    confirming = true
    try {
      let wait = 0
      while (owed.size > 0 && !life.signal.aborted) {
        const ids = [...owed]
        const answer = await call(life.signal, "POST", url + "/read", JSON.stringify({ ids }), readAbortMs)
        if (answer.refused) {
          life.abort()
          return
        }
        if (answer.ok) {
          for (const id of ids) owed.delete(id)
          wait = 0
          continue
        }
        wait = next(wait)
        await sleep(wait, life.signal)
      }
    } finally {
      confirming = false
    }
  }

  quietly(listen())
  return async () => {
    life.abort()
    close()
  }
}

// wellFormed says whether a poll's note is one the plugin can admit.
function wellFormed(note) {
  return (
    note !== null &&
    typeof note === "object" &&
    Number.isSafeInteger(note.seq) &&
    note.seq > 0 &&
    typeof note.id === "string" &&
    note.id !== "" &&
    typeof note.inbox === "string" &&
    note.inbox.startsWith("msg_") &&
    typeof note.message === "string" &&
    note.message !== ""
  )
}

// call makes one request of the notes route, bounded by ms and ended early by
// any of signals: refused for a token the route no longer resolves or a run
// with no notes route, ok with the answer's text for any other success, and
// neither for everything else — an error status, a network failure, the bound.
async function call(life, method, target, body, ms, ...signals) {
  const { signal, done } = deadline(ms, life, ...signals)
  try {
    const response = await fetch(target, {
      method,
      body,
      headers: body === undefined ? undefined : { "content-type": "application/json" },
      signal,
      // Bun's own idle deadline on a request, off: this request's bound is ms.
      timeout: false,
    })
    const text = await response.text()
    if (response.status === 401 || response.status === 404) return { refused: true }
    return { ok: response.ok, text }
  } catch {
    return {}
  } finally {
    done()
  }
}

// next is the pause after a failure that followed a pause of wait.
function next(wait) {
  return wait === 0 ? backoffFirstMs : Math.min(wait * 2, backoffMostMs)
}

// sleep resolves after ms, or as soon as any of signals ends.
function sleep(ms, ...signals) {
  return new Promise((resolve) => {
    const { signal, done } = deadline(ms, ...signals)
    signal.addEventListener(
      "abort",
      () => {
        done()
        resolve()
      },
      { once: true },
    )
  })
}

// deadline is a signal that ends after ms or with any of signals, and done,
// which lets go of the timer and of every signal it listens to — the plugin's
// life outlasts thousands of requests, and a listener left on it by each would
// be kept for the whole run.
function deadline(ms, ...signals) {
  const controller = new AbortController()
  const abort = () => controller.abort()
  const timer = setTimeout(abort, ms)
  for (const s of signals) {
    if (s.aborted) abort()
    else s.addEventListener("abort", abort, { once: true })
  }
  return {
    signal: controller.signal,
    done() {
      clearTimeout(timer)
      for (const s of signals) s.removeEventListener("abort", abort)
    },
  }
}

// quietly runs a task whose failures it has nowhere to report: every one is
// handled where it happens, and none may reach OpenCode as an unhandled
// rejection.
function quietly(task) {
  task.catch(() => {})
}
