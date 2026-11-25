// crewlet.steer — the plugin an OpenCode run's server loads to admit a person's
// notes to the run's turn into the run's own session, at its next step.
//
// The engine embeds this file (opencodesteer.go) and writes it into the run's work
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
// request carries, and reports it delivered (`session.inbox.delivered`).
//
// CONFIRMED ONLY ONCE THE MODEL IS SENT IT PLAIN. Delivery is not reading: a
// step boundary that tips the session past its compaction point promotes the
// note and then compacts, and the model gets it only inside the checkpoint,
// framed as history (ADR-0177's confirm). So the plugin confirms a note to the
// engine — which records it read by the run's job — only from the session's
// "context" hook, which OpenCode runs on every model request before sending
// it, and only where a user message of that request IS the note: the message
// OpenCode made of the plugin's own admission, named by the id the note was
// admitted under (2.3.2 hands the hook each message's id), holding one text
// part equal to the note's text. So one message confirms one note, a note of
// the same words, an earlier run's or a checkpoint quoting it never does, and a
// message of any other shape confirms nothing. Between a compaction's start
// and end no request confirms anything. The hook does no I/O: OpenCode holds
// the request until it returns. A note delivered and never seen plain
// when the session's compaction ends was folded: the plugin asks the route's
// `again` for a second id to admit it under, ONCE, which the engine grants
// only within the note's serves and only for a person's note or a late call
// (a wake is served once and never re-admitted), counting it as a serve then,
// so a note that never folds spends nothing (an admission under it that
// fails is made again, under the same id, while the execution runs). A note
// the engine answers spent, one folded again, or one the execution settles on
// before it is admitted again, is given up: never confirmed, its id posted to
// the route's `folded` so the engine says why it waits for the review. A failed
// compaction confirms and posts nothing. Measured on 2.3.2: the hook precedes
// the delivered event, which precedes the compaction's start; a re-admitted
// note lands plain one step later.

// ONLY WHILE THE SESSION'S EXECUTION RUNS. Notes are polled and admitted
// between the run's session's `session.execution.started` (or any step of it
// starting) and its `succeeded`, `failed` or `interrupted`, and each with
// `resume: false`: a steer admitted once an execution has settled starts
// nothing — measured on 2.3.2 — where one that resumed the session would start
// an execution `opencode run` no longer watches, whose answer nobody reads and
// whose server the run's script then stops. So a note sent after the run's
// last step waits on the run's record for its review. One whose admission
// landed as the execution settled, after OpenCode last read the session's
// inbox, stays pending there too — and the inbox outlives the server, so a run
// carried on in the same session would be handed it ahead of its next prompt
// while the continuation's brief carries it as well: the run's script cancels
// every such admission once the run has ended (opencode.go,
// openCodeLeftoverSteers), and a run continuing the copy a killed run left
// cancels its leftovers before it starts (openCodeStaleSteers), so the record
// alone holds the note and whatever reads the turn next reads it once. An execution OpenCode starts itself
// afterwards opens the gate again. The session is the one the
// run's script names, handed over as CREWLET_RUN_SESSION; another session
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
// notes wait for its review. The cleanup setup returns stops polling at once,
// and lets the confirmations in flight finish first (endWaitMs).
//
// IT WRITES NOTHING, anywhere: OpenCode reloads a configured plugin whose
// directory changes, and the notes themselves never touch the box's disk.

// pollAbortMs bounds one poll: the engine holds a poll for up to ten seconds
// while no note waits (internal/api/mcpbridge, steerHold), so twenty is that
// hold and the same again for a relay and a slow network — and short of Bun's
// own idle deadline on a request, which every request here turns off in its
// favour (`timeout: false`).
const pollAbortMs = 20_000

// readAbortMs bounds one confirmation, ask for second ids, or post of folded
// notes: a single write on the run's record.
const readAbortMs = 10_000

// endWaitMs bounds how long the cleanup waits for the confirmations, asks and
// folds in flight before it ends them. OpenCode awaits a plugin's cleanup when its
// server is stopped, and exited within 0.2 s of one ending (measured on
// 2.3.2); the run's script kills the server five seconds after asking
// it to stop (internal/sandbox's JobStopGrace), so four leaves it that second.
// A post the end cuts costs a duplicate, never a lost note: the note waits on
// the record, served, for the review.
const endWaitMs = 4_000

// The pause after a failed request: a second, doubling to thirty, reset by the
// next request that succeeds — so a bridge that is down delays a note by at
// most thirty seconds once it is back, and costs a request every thirty while
// it is not.
const backoffFirstMs = 1_000
const backoffMostMs = 30_000

// The pause before a failed admission again is made again: a tenth of a
// second, doubling to one. An admission answers in under 20 ms in OpenCode's
// own process and costs no model call, and 2.3.2 began the next step 1.5 s
// after the fold (both measured), so four retries land before it.
const readmitFirstMs = 100
const readmitMostMs = 1_000

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
    const session = globalThis.process?.env?.CREWLET_RUN_SESSION ?? ""
    // A run its launch wired to no notes — or one whose reference OpenCode
    // read as empty — has nothing to poll.
    if (url === "" || session === "") return undefined
    return arm(ctx, url, session)
  },
}

// arm starts the plugin's work for one session and returns its cleanup.
async function arm(ctx, url, session) {
  // life is the plugin's whole run: the cleanup, or a refusal, ends it.
  const life = new AbortController()
  // gate is open — a controller of its own — while the session's execution runs.
  let gate = null
  // stopping is set by the cleanup: nothing is polled or admitted any more.
  let stopping = false
  // compacting is set from the session's compaction's start to its end.
  let compacting = false
  // cursor is the place of the last note admitted, which a poll asks past.
  let cursor = 0
  // held is every note admitted and not yet settled, by its id: {id, text,
  // inbox, again, delivered, readmitted, plain}. admitted maps each inbox id a
  // note was handed to the session under — its first and its `again` — to its
  // id. owed holds the notes seen plain and not yet confirmed, asking the notes
  // folded whose second id is not answered yet, folded the notes given up and
  // not yet posted.
  const held = new Map()
  const admitted = new Map()
  const owed = new Set()
  const asking = new Set()
  const folded = new Set()
  // confirming, askingAgain and folding are the loops in flight, null when
  // idle.
  let confirming = null
  let askingAgain = null
  let folding = null

  const open = () => {
    if (gate !== null || life.signal.aborted || stopping) return
    gate = new AbortController()
    quietly(poll(gate.signal))
  }
  const close = () => {
    if (gate === null) return
    gate.abort()
    gate = null
  }

  // see is the session's "context" hook: the request about to go to the model.
  // No I/O and no await, since OpenCode holds the request on it. Fails closed
  // while the session compacts: its summary runs no context hook on 2.3.2
  // (measured), and a request then is none a note may be confirmed off.
  const see = (input) => {
    if (compacting || input?.sessionID !== session || !Array.isArray(input.messages)) return
    for (const message of input.messages) {
      const note = typeof message?.id === "string" ? held.get(admitted.get(message.id)) : undefined
      if (note === undefined || note.plain || plainText(message) !== note.text) continue
      note.plain = true
      note.text = null
      owed.add(note.id)
    }
    if (owed.size > 0) confirm()
  }

  // compacted settles the notes a compaction of the session folded: each
  // delivered and never seen plain is asked a second id for, once, or, folded
  // again, given up.
  const compacted = () => {
    for (const note of [...held.values()]) {
      if (!note.delivered || note.plain) continue
      note.delivered = false
      if (note.readmitted) {
        giveUp(note)
        continue
      }
      if (gate !== null && !stopping) asking.add(note.id)
    }
    // ONE ASK FOR EVERY NOTE THE COMPACTION FOLDED.
    if (asking.size > 0) askAgain()
  }

  // giveUp drops a note folded with no admission left: never confirmed, and
  // posted so the engine says why it waits for the review.
  const giveUp = (note) => {
    held.delete(note.id)
    folded.add(note.id)
    fold()
  }

  // answered takes the route's answer to an ask for second ids: each note
  // granted one is admitted again under it, each spent is given up, and any
  // other (a wake, served once) is dropped, waiting for its own fallback.
  const answered = (text, named) => {
    let answer
    try {
      answer = JSON.parse(text)
    } catch {
      return false
    }
    const granted = answer?.again
    const spent = answer?.spent
    if (granted === null || typeof granted !== "object" || !Array.isArray(spent)) return false
    for (const id of named) {
      asking.delete(id)
      const note = held.get(id)
      if (note === undefined) continue
      const again = granted[id]
      if (typeof again === "string" && again.startsWith("msg_") && again !== note.inbox) {
        note.again = again
        note.readmitted = true
        admitted.set(again, note.id)
        quietly(readmit(note))
      } else if (spent.includes(id)) giveUp(note)
      else held.delete(id)
    }
    return true
  }

  // readmit admits a folded note again under its second id, made again after
  // a pause ([readmitFirstMs]) while the session's execution runs: OpenCode answers a repeat
  // admission under an id it holds with its first, before and after delivery
  // (measured on 2.3.2), so a call that failed once OpenCode stored it costs
  // nothing. The id stays mapped, so a message under it confirms the note
  // however a call ended. A note the gate closes on first, or the cleanup, is
  // given up: posted folded, never confirmed, waiting for the review.
  const readmit = async (note) => {
    let wait = 0
    for (;;) {
      if (held.get(note.id) !== note || note.plain) return
      const gated = gate
      if (gated === null || stopping || life.signal.aborted) {
        giveUp(note)
        return
      }
      try {
        await ctx.session.prompt({
          sessionID: session,
          id: note.again,
          text: note.text,
          delivery: "steer",
          resume: false,
        })
        return
      } catch {
        // Said above: made again after a pause.
      }
      wait = wait === 0 ? readmitFirstMs : Math.min(wait * 2, readmitMostMs)
      await sleep(wait, life.signal, gated.signal)
    }
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
        const note = held.get(admitted.get(data.inboxID))
        if (note !== undefined && !note.plain) note.delivered = true
        return
      }
      case "session.compaction.started":
        compacting = true
        return
      case "session.compaction.ended":
        compacting = false
        compacted()
        return
      case "session.compaction.failed":
        compacting = false
        return
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
      // KNOWN WHILE IT IS ADMITTED, because OpenCode can deliver a note
      // before the call that admitted it has returned; LET GO IF IT FAILS, so
      // a note held is one admitted. Only the message of its own admitted id
      // confirms it, which a failed admission never has.
      const fresh = !held.has(note.id)
      if (fresh) {
        held.set(note.id, { id: note.id, text: note.message, inbox: note.inbox, again: undefined,
          delivered: false, readmitted: false, plain: false })
        admitted.set(note.inbox, note.id)
      }
      try {
        await ctx.session.prompt({
          sessionID: session,
          id: note.inbox,
          text: note.message,
          delivery: "steer",
          resume: false,
        })
      } catch {
        if (fresh) {
          held.delete(note.id)
          admitted.delete(note.inbox)
        }
        return false
      }
      cursor = note.seq
    }
    return true
  }

  // post sends every id in ids to the route's verb, one request at a time,
  // until none is left; a failed request is made again after a pause. Every
  // id posted is one this job's poll returned: at most a turn's ten notes and
  // the job's own items, ten late calls and four wakes, which is what one
  // request may name (internal/api/mcpbridge's steerReadIDs).
  const post = async (verb, ids) => {
    let wait = 0
    while (ids.size > 0 && !life.signal.aborted) {
      const named = [...ids]
      const answer = await call(life.signal, "POST", url + "/" + verb, JSON.stringify({ ids: named }), readAbortMs)
      if (answer.refused) {
        life.abort()
        return
      }
      if (answer.ok) {
        for (const id of named) ids.delete(id)
        wait = 0
        continue
      }
      wait = next(wait)
      await sleep(wait, life.signal)
    }
  }

  // confirm records every note seen plain and not yet confirmed as read; fold
  // posts every note given up. Each runs one loop at a time, and the cleanup
  // waits for both.
  const confirm = () => {
    confirming ??= post("read", owed).finally(() => {
      confirming = null
      if (owed.size > 0 && !life.signal.aborted) confirm()
    })
  }
  const fold = () => {
    folding ??= post("folded", folded).finally(() => {
      folding = null
      if (folded.size > 0 && !life.signal.aborted) fold()
    })
  }
  // askAgain asks the route for a second id for every note asking holds, one
  // request at a time, a failed one made again after a pause.
  const askAgain = () => {
    askingAgain ??= (async () => {
      let wait = 0
      while (asking.size > 0 && !life.signal.aborted) {
        const named = [...asking]
        const answer = await call(life.signal, "POST", url + "/again", JSON.stringify({ ids: named }), readAbortMs)
        if (answer.refused) {
          life.abort()
          return
        }
        if (answer.ok && answered(answer.text, named)) {
          wait = 0
          continue
        }
        wait = next(wait)
        await sleep(wait, life.signal)
      }
    })().finally(() => {
      askingAgain = null
      if (asking.size > 0 && !life.signal.aborted) askAgain()
    })
  }
  const settled = async () => {
    while (confirming !== null || askingAgain !== null || folding !== null) {
      await Promise.allSettled([confirming, askingAgain, folding])
    }
  }

  // THE HOOK BEFORE ANY REQUEST: OpenCode holds every prompt until setup has
  // resolved. A release without the hook confirms nothing: its notes wait for
  // the review, the safe side.
  try {
    await ctx.session.hook("context", see)
  } catch {
    // Said above: nothing is confirmed.
  }
  quietly(listen())
  return async () => {
    stopping = true
    close()
    await Promise.race([settled(), sleep(endWaitMs, life.signal)])
    life.abort()
  }
}

// plainText is a model request's user message's text where the message IS one
// text part, else null: the form OpenCode hands the "context" hook on every
// release measured. Any other form — a string, extra parts — matches no note.
function plainText(message) {
  if (message?.role !== "user" || !Array.isArray(message.content) || message.content.length !== 1) return null
  const part = message.content[0]
  return part?.type === "text" && typeof part.text === "string" ? part.text : null
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
    const end = () => {
      done()
      resolve()
    }
    // deadline ends its signal at once for a signal already ended, and an
    // ended signal fires no listener added later.
    if (signal.aborted) end()
    else signal.addEventListener("abort", end, { once: true })
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
