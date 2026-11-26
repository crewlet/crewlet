// crewlet-notes — the extension a Copilot run loads from its own
// $COPILOT_HOME/extensions/crewlet-notes/extension.mjs: it hands the run a
// person's notes between its steps, holds it to its seat's turn cap, and keeps
// every other MCP server off the bridge's names.
//
// The engine embeds this file (copilotsteer.go) and writes it with the run's
// own settings in place of the `config` placeholder below: the notes route, the
// cap, and its state file and run (copilot.go, Copilot.WriteConfig). It imports
// only the SDK the Copilot binary serves its extensions
// (`@microsoft/copilot-sdk/extension`, 1.0.114 and later) and Node's fs, runs
// under the Node the binary embeds, and writes its state file alone.
//
// HOW A NOTE ARRIVES. Each note the route answers is sent to the session with
// `mode: "immediate"` under a clientTurnId made of its inbox id (turnPrefix):
// sent while a tool runs, Copilot records it as a `user.message` with
// `delivery: "steering"` and the model reads it right after the tool's result;
// sent as the main agent stops, as one with `delivery: "queued"`, which the
// run answers before it ends (measured on 1.0.114). THIS EXTENSION CONFIRMS
// NOTHING: a note counts as read only once the runner finds that line and a
// later usage record in the session's own record (copilottranscript.go), so it
// never posts the route's /read.
//
// ONLY WHILE THE MAIN AGENT WORKS. The route is polled from the main agent's
// `assistant.turn_start` until its `assistant.idle`; a subagent's events (their
// envelope carries an agentId) open nothing. As the main agent stops (onAgentStop) the
// route is asked once more, briefly (stopAskMs), so a note waiting then is
// answered by this run rather than left for its review.
//
// THE TURN CAP. Every main-agent `assistant.turn_start` is one model request.
// Once the cap's last request is made, the run is aborted (`session.abort()`)
// as that turn's tools answer, so no request past the cap is sent; a turn that
// starts past it all the same (a note answered at the stop) is aborted as it
// starts. A log line the run's stream carries (capMessage) comes first. The
// line labels an ending and never makes one: the runner reads a capped run only
// where the stream also shows the abort (copilotevents.go).
//
// THE BRIDGE'S NAMES. A custom agent's own `mcp-servers` entry named as the
// bridge (`crewlet`) connects for that agent and serves the bridge's tool names
// in its calls (a repository's `.github/agents/<n>.agent.md`, measured on
// 1.0.114). Copilot announces it as a `session.mcp_server_status_changed` under
// the agent's id, before the agent's first call; the bridge's own carries none.
// On it the run is stopped (unbridgedMessage, then the abort), and a call the
// agent makes under the bridge's names meanwhile is refused (onPreToolUse).
//
// Bounded, paced and refused as opencode_steer.js: every request is bounded,
// a failure waits a second doubling to thirty, an empty answer is never
// followed sooner than a second after its poll began, and a 401 or 404 ends
// the polling for the rest of the run.
//
// A RESTART RESUMES THE RUN. Copilot offers the agent extensions_reload, which
// ends this process and starts a new one in the same session (measured on
// 1.0.114: a reload let a fourth request past a cap of 3 and sent a note
// again). So the requests counted, the cursor and every inbox id sent are kept
// in the run's state file, written as each changes and read at the start where
// it names this launch: a restarted extension goes on counting where the last
// one stopped, and never sends a note it sent. The two never overlap (measured
// on 1.0.114: the old process got SIGTERM and exited, the new one started 331 ms
// later), so one writer at a time. FAILING CLOSED: a note whose id cannot be
// kept is not sent, and a capped run whose count cannot be kept, or whose file
// cannot be read, is stopped (unkeptMessage); no restart counts from nothing.
//
// A CHECKOUT CLONED DURING THE RUN. After each of the main agent's tool calls
// (a subagent's input carries an agentId) the note helper hands each checkout
// cloned since its skills (`copilot-nested`, internal/notehelper), and their
// listing is appended to that tool's own result, `modifiedResult`'s
// textResultForLlm, which reaches the next request whole, where
// additionalContext is cut at 10,240 characters without a word (measured on
// 1.0.114). Its instructions Copilot loads itself on the first view of a file
// in it. The helper's own skip lines are kept for the run's end, not repeated.
//
// A LAUNCH COPILOT REFUSED. Copilot refuses a launch whose static context
// passes the model's budget before any request (`session.warning`
// compaction_static_context_blocked), and the run's script starts it again
// with fewer of its checkouts' instructions (copilotrepo.go). A refused attempt
// of a NEW session is deleted here, whole, so the next attempt starts clean and
// no model ever reads it from the session store: once a request has gone, or
// for a resumed session, nothing is. Each attempt is its own launch: its state
// is keyed by the attempt the script names, so a refused attempt's count, cursor
// and sent notes count nothing, and a note it sent is sent again.

import { execFileSync } from "node:child_process"
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs"
import { joinSession } from "@microsoft/copilot-sdk/extension"

// config is the run's: {url, cap, state, run}, written over the placeholder by
// the runner. null in the file as embedded, where the extension does nothing.
const config = /*crewlet:config*/ null

// pollAbortMs bounds one poll: the route holds an empty one ten seconds
// (internal/api/mcpbridge, steerHold), and the same again is room for a relay.
const pollAbortMs = 20_000

// stopAskMs bounds the ask made as the main agent stops. A waiting note is
// answered at once; an empty ask is held by the route, so this is what every
// run's end waits when no note waits: one second, a round trip to a relayed
// node and back many times over.
const stopAskMs = 1_000

// The pause after a failed request, and the least time from a poll that
// admitted nothing to the next: opencode_steer.js's, for its reasons.
const backoffFirstMs = 1_000
const backoffMostMs = 30_000
const pollFloorMs = backoffFirstMs

// turnPrefix opens every clientTurnId this extension sends a note under,
// which the runner's confirm reader keys on (copilotsteer.go, copilotTurnPrefix).
const turnPrefix = "crewlet-note:"

// capMessage opens the log line written as the cap stops the run
// (copilotsteer.go, copilotCapMessage).
const capMessage = "crewlet: this run reached its turn cap of "

// unkeptMessage is the log line written as a capped run is stopped because
// its count cannot be kept or read back: the abort, without the cap's line, is
// collected as a failure (copilotevents.go).
const unkeptMessage = "crewlet: this run's turn count could not be kept, so the run was stopped"

// notesUnkeptMessage is the log line written, once, where a note's id cannot be
// kept, so no note is sent (copilotsteer.go, copilotNotesUnkeptMessage): the
// notes stay unconfirmed and are served again after the run.
const notesUnkeptMessage = "crewlet: this run's notes could not be kept, so none is sent until they can be"

// nestedTimeoutMs bounds one firing of the note helper, which Copilot does not
// (measured on 1.0.114: a 75 s hook held the turn and was answered): ten
// seconds, as Codex's nested hook (codexrepo.go, codexNestedTimeout), where the
// slowest firing measured on the build VM took 0.12 s. nestedMaxBuffer holds
// its answer, a listing the helper bounds at 64 KiB (maxListing).
const nestedTimeoutMs = 10_000
const nestedMaxBuffer = 1 << 20

// nestedFailedMessage opens the log line written where a firing of the helper
// failed, so what it claimed is not listed.
const nestedFailedMessage =
  "crewlet: the note helper handing a checkout cloned during the run failed, so its skills are not listed: "

// blockedWarning is the warningType of Copilot's refusal of a launch, or of a
// request, past its static budget (measured on 1.0.114).
const blockedWarning = "compaction_static_context_blocked"

// keptFailedMessage opens the log line written where the launch sets an
// attempt kept could not be stored in its session: a resume then hands none.
const keptFailedMessage = "crewlet: the session could not keep the repositories it was started with: "

// removeFailedMessage opens the log line written where a refused attempt
// could not be deleted; the script finds it left and fails the run.
const removeFailedMessage = "crewlet: the refused launch's session could not be removed: "

// armedMessage is the log line written once the extension runs in the session
// (copilotsteer.go, copilotArmedMessage): a run with a turn cap whose stream
// lacks it is collected as a failure, the cap never enforced.
const armedMessage = "crewlet: the run's extension is running"

// unbridgedMessage opens the error logged as the run is stopped for an agent's
// server under a bridge's name (copilotsteer.go, copilotUnbridgedMessage).
const unbridgedMessage =
  "crewlet: a repository's or home's MCP server may not take a name inside Crewlet's own server's tool names, or its place"

if (config !== null && typeof config === "object") {
  await arm(config)
}

async function arm({ url, cap, bridges, state, run, helper, nested, attempt, kept: keptFile, sessions, armed }) {
  // kept is what an earlier process of this launch left (A RESTART RESUMES THE
  // RUN, above): the requests counted, the cursor, and the inbox ids sent; or
  // broken, a file this process cannot read, which sends no note. The launch
  // is this attempt's (A LAUNCH COPILOT REFUSED, above).
  const launch = attemptOf(run, attempt)
  const kept = launch === null ? { turns: 0, cursor: 0, sent: [], broken: true } : resume(state, launch)
  const polling = typeof url === "string" && url !== "" && !kept.broken
  const capped = Number.isSafeInteger(cap) && cap > 0
  const handing = typeof helper === "string" && helper !== "" && typeof nested === "string" && nested !== ""
  const guarded = Array.isArray(bridges) ? bridges.filter((b) => typeof b === "string" && b !== "") : []
  if (!polling && !capped && !handing && guarded.length === 0) return

  // life is the extension's whole run; a refusal of the route ends its polling.
  const life = new AbortController()
  let gate = null
  let cursor = kept.cursor
  let turns = kept.turns
  const sent = new Set(kept.sent)
  const save = () => launch !== null && keep(state, launch, { turns, cursor, sent: [...sent] })
  // unkept is a capped run whose count was not kept, or not read back.
  let unkept = capped && kept.broken
  // notesSaid is whether the run's stream was told a note's id could not be kept.
  let notesSaid = false
  // admitted holds every inbox id sent or being sent by this process, by its
  // send's outcome: two asks that overlap (the loop's and the stop's) send one
  // note once, and the second moves past it only once the first's send has gone.
  const admitted = new Map()
  let aborting = false
  // agents names each subagent by its id, and taken holds the ids of those
  // that connected a server of their own under a bridge's name.
  const agents = new Map()
  const taken = new Set()

  const open = () => {
    if (!polling || gate !== null || life.signal.aborted) return
    gate = new AbortController()
    quietly(poll(gate.signal))
  }
  const close = () => {
    if (gate === null) return
    gate.abort()
    gate = null
  }

  // ask asks the route once for the notes past the cursor, bounded by ms, and
  // sends those it answers: true where it answered, false otherwise.
  const ask = async (signal, ms) => {
    const answer = await call(life, "GET", url + "/notes?after=" + cursor, ms, signal)
    if (answer.refused) {
      life.abort()
      close()
      return false
    }
    return answer.ok && (await admit(answer.text, signal))
  }

  const poll = async (gated) => {
    let wait = 0
    while (!gated.aborted && !life.signal.aborted) {
      const began = Date.now()
      const past = cursor
      if (await ask(gated, pollAbortMs)) {
        wait = 0
        const early = pollFloorMs - (Date.now() - began)
        if (cursor === past && early > 0) await sleep(early, life.signal, gated)
        continue
      }
      if (gated.aborted || life.signal.aborted) return
      wait = next(wait)
      await sleep(wait, life.signal, gated)
    }
  }

  // admit sends each note of one answer, oldest first, moving the cursor past
  // each once its send has gone: false for an answer that is no list of notes,
  // or a send that failed, which leaves the cursor before that note, so the
  // next ask answers it again. A gate closed meanwhile sends nothing more: what
  // is left is asked for by the next turn's poll, or read by the review.
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
      if (gated.aborted || life.signal.aborted) return true
      let sending = admitted.get(note.inbox)
      if (sending === undefined && sent.has(note.inbox)) {
        // SENT BY AN EARLIER PROCESS OF THIS RUN, whose send may have gone
        // before it ended: never sent twice. Had it not gone, the runner
        // finds no line under its id, and the note is served again later.
        sending = Promise.resolve(true)
      }
      if (sending === undefined) {
        // KEPT BEFORE THE SEND, so a process ended mid-send leaves it sent;
        // an id that cannot be kept is not sent, and is asked for again.
        sent.add(note.inbox)
        if (!save()) {
          sent.delete(note.inbox)
          if (!notesSaid) {
            notesSaid = true
            quietly(session.log(notesUnkeptMessage, { level: "warning" }))
          }
          return false
        }
        sending = session
          .send({ prompt: note.message, mode: "immediate", clientTurnId: turnPrefix + note.inbox })
          .then(
            () => true,
            () => false,
          )
        admitted.set(note.inbox, sending)
      }
      if (!(await sending)) {
        if (admitted.get(note.inbox) === sending) {
          admitted.delete(note.inbox)
          sent.delete(note.inbox)
          save()
        }
        return false
      }
      cursor = Math.max(cursor, note.seq)
      save()
    }
    return true
  }

  // spent is whether the run has made every model request its cap allows,
  // or can no longer be held to it.
  const spent = () => capped && (unkept || turns >= cap)

  // capNow stops the run once, whichever path got there first, saying why.
  const capNow = async () => {
    if (aborting) return
    aborting = true
    close()
    await stop(session, unkept ? unkeptMessage : capMessage + cap + " model turns and was stopped")
  }

  // capAfterTool stops the run within the cap-th turn, as its tools answer:
  // AWAITED by Copilot before the turn goes on, where a turn_start event comes
  // to the extension only after the next request has gone (measured: the
  // event path let a fourth request through a cap of three).
  const capAfterTool = async (input) => {
    if (mainAgent(input) !== false && spent()) await capNow()
  }

  // handNested hands the checkouts cloned since the last of the main agent's
  // tools (A CHECKOUT CLONED DURING THE RUN, above) as input's tool result
  // with their listing appended; nothing where there is none.
  const handNested = (input) => {
    if (!handing || mainAgent(input) === false || aborting) return undefined
    let listing
    try {
      listing = execFileSync(helper, ["-adapter", "copilot-nested", "-config", nested], {
        encoding: "utf8",
        timeout: nestedTimeoutMs,
        maxBuffer: nestedMaxBuffer,
        stdio: ["ignore", "pipe", "ignore"],
      })
    } catch (error) {
      quietly(session.log(nestedFailedMessage + String(error?.message ?? error).split("\n")[0], { level: "warning" }))
      return undefined
    }
    if (listing === "") return undefined
    const result = input?.toolResult ?? {}
    return { modifiedResult: { ...result, textResultForLlm: (result.textResultForLlm ?? "") + "\n\n" + listing } }
  }

  // source is how Copilot started the session (sessionStart's), and answered
  // whether a model request of it was answered: a refusal deletes only a new
  // session no request reached.
  let source = null
  let answered = false
  let removing = false

  // storeKept writes the launch sets this attempt kept into its new session's
  // directory as the session starts, so a pack taken while it runs carries
  // them and a resume launches with exactly them (copilotrepo.go,
  // copilotKeptSets); a refused attempt's goes with its session.
  const storeKept = () => {
    const id = process.env.SESSION_ID
    if (typeof keptFile !== "string" || keptFile === "" || typeof sessions !== "string" || sessions === "") return
    try {
      if (typeof id !== "string" || id === "" || id.includes("/")) throw new Error("Copilot named no session")
      let names = ""
      try {
        names = readFileSync(keptFile, "utf8")
      } catch (error) {
        if (error?.code !== "ENOENT") throw error
      }
      const dir = sessions + "/" + id
      mkdirSync(dir, { recursive: true })
      writeFileSync(dir + "/crewlet-checkouts.tmp", names)
      renameSync(dir + "/crewlet-checkouts.tmp", dir + "/crewlet-checkouts")
    } catch (error) {
      quietly(session.log(keptFailedMessage + String(error?.message ?? error).split("\n")[0], { level: "warning" }))
    }
  }

  // removeRefused deletes this attempt's own session, Copilot's process being
  // the one it is joined to: the extension SDK offers no delete, so a second
  // client on the internal parent-process connection asks Copilot's own
  // sessions.bulkDelete for exactly its id (measured: the session's directory
  // and its store rows gone; the refused launch exits in about 4.4 s on the
  // pinned 1.0.114, as without it, and in about 65 s on 1.0.115, which waits
  // 60 s after the delete). It stops nothing else; the script checks it is gone
  // before the next attempt (copilot_storecheck.mjs). executor's package doc,
  // "Copilot's internal client", has why and what breaks without it.
  const removeRefused = async () => {
    if (removing) return
    removing = true
    life.abort()
    close()
    try {
      const id = process.env.SESSION_ID
      if (typeof id !== "string" || id === "") throw new Error("Copilot named no session")
      const sdk = await import("@microsoft/copilot-sdk")
      const client = new sdk.CopilotClient({ mode: "coding-agent", _internalConnection: { kind: "parent-process" } })
      await client.start()
      await client.rpc.sessions.bulkDelete({ sessionIds: [id] })
    } catch (error) {
      quietly(session.log(removeFailedMessage + String(error?.message ?? error).split("\n")[0], { level: "warning" }))
    }
  }

  // unbridged stops the run once for the agent agentId's server under a
  // bridge's name: the error that fails it, then the abort.
  const unbridged = async (agentId, server) => {
    if (aborting) return
    aborting = true
    close()
    const agent = agents.get(agentId) ?? "a custom agent"
    try {
      await session.log(
        `${unbridgedMessage}: ${agent} connected its own MCP server ${server}, most likely from its ` +
          "mcp-servers in its file under .github/agents/ in the repository, so the run was stopped",
        { level: "error" },
      )
    } finally {
      await session.abort()
    }
  }

  // JOINED LAST, every function it can call defined: the hook may fire as
  // soon as the session is joined.
  const session = await joinSession({
    hooks: {
      onSessionStart: async (input) => {
        source = input?.source ?? null
        // THE RUN'S MARK that this extension ran in the session, which the
        // script reads at a refusal and as the run ends (copilotrepo.go).
        if (typeof armed === "string" && armed !== "") {
          try {
            writeFileSync(armed, "")
          } catch {}
        }
        if (source === "new" && handing) storeKept()
      },
      onPreToolUse: async (input) => {
        if (typeof input?.agentId !== "string" || !taken.has(input.agentId)) return
        if (!guarded.some((b) => String(input.toolName).startsWith(b + "-"))) return
        return { permissionDecision: "deny", permissionDecisionReason: unbridgedMessage }
      },
      onPostToolUse: async (input) => {
        await capAfterTool(input)
        return handNested(input)
      },
      onPostToolUseFailure: async (input) => capAfterTool(input),
      onAgentStop: async (input) => {
        if (!polling || mainAgent(input) === false || life.signal.aborted || spent()) return
        await ask(life.signal, stopAskMs)
      },
    },
  })
  session.on((event) => {
    if (event === null || typeof event !== "object") return
    if (event.type === "assistant.message") answered = true
    if (event.type === "subagent.started" && typeof event.agentId === "string") {
      agents.set(event.agentId, named(event.data?.agentName))
    }
    if (event.type === "session.warning" && event.data?.warningType === blockedWarning && !answered && source === "new") {
      quietly(removeRefused())
      return
    }
    if (
      event.type === "session.mcp_server_status_changed" &&
      typeof event.agentId === "string" &&
      guarded.includes(event.data?.serverName)
    ) {
      taken.add(event.agentId)
      quietly(unbridged(event.agentId, event.data.serverName))
      return
    }
    if (mainAgent(event) === false) return
    switch (event.type) {
      case "assistant.turn_start":
        turns++
        if (!save()) unkept = capped
        if (unkept || (capped && turns > cap)) {
          quietly(capNow())
          return
        }
        open()
        return
      case "tool.execution_start":
        open()
        return
      case "assistant.idle":
        close()
        return
    }
  })
  quietly(session.log(armedMessage, { level: "info" }))
  if (unkept) quietly(capNow())
}

// stop ends a run past its cap: the line that labels the ending, then the
// abort that makes it, which happens whether or not the line was written.
async function stop(session, line) {
  try {
    await session.log(line, { level: "warning" })
  } finally {
    await session.abort()
  }
}

// attemptOf is this launch's key: run and the attempt the script wrote to the
// file attempt names, run alone where there is no file to read; null where it
// cannot be read, which counts as nothing known.
function attemptOf(run, attempt) {
  if (typeof attempt !== "string" || attempt === "") return run
  try {
    const n = readFileSync(attempt, "utf8").trim()
    return /^[0-9]+$/.test(n) ? run + "/" + n : null
  } catch {
    return null
  }
}

// named is an agent's name for the run's error: the characters of a name
// alone, at most 64, since a repository wrote it.
function named(name) {
  const clean = String(name ?? "").replace(/[^A-Za-z0-9_.:-]/g, "").slice(0, 64)
  return clean === "" ? "a custom agent" : `the custom agent ${clean}`
}

// resume is what the state file holds for run: nothing counted where there is
// no file or it is another launch's; broken where it exists and this process
// cannot read it whole, which counts as nothing known, never as nothing done.
function resume(file, run) {
  const none = { turns: 0, cursor: 0, sent: [] }
  if (typeof file !== "string" || file === "") return none
  let kept
  try {
    kept = JSON.parse(readFileSync(file, "utf8"))
  } catch (error) {
    return error?.code === "ENOENT" ? none : { ...none, broken: true }
  }
  if (kept?.run !== run) return kept !== null && typeof kept === "object" ? none : { ...none, broken: true }
  const whole =
    Number.isSafeInteger(kept.turns) &&
    Number.isSafeInteger(kept.cursor) &&
    Array.isArray(kept.sent) &&
    kept.sent.every((id) => typeof id === "string")
  return whole ? kept : { ...none, broken: true }
}

// keep writes the run's state to file whole (a write beside it, then a
// rename), so a process ended mid-write leaves the last state or this one:
// true once it is written, false where it could not be (or there is no file).
function keep(file, run, fields) {
  if (typeof file !== "string" || file === "") return false
  try {
    writeFileSync(file + ".tmp", JSON.stringify({ run, ...fields }))
    renameSync(file + ".tmp", file)
    return true
  } catch {
    return false
  }
}

// mainAgent is false for an event or a hook input of a subagent, which names
// its agent beside its type (an event's envelope, not its data); the main
// agent's name none.
function mainAgent(v) {
  return typeof v?.agentId !== "string"
}

function wellFormed(note) {
  return (
    note !== null &&
    typeof note === "object" &&
    Number.isSafeInteger(note.seq) &&
    note.seq > 0 &&
    typeof note.inbox === "string" &&
    note.inbox !== "" &&
    typeof note.message === "string" &&
    note.message !== ""
  )
}

// call makes one request of the route, bounded by ms and ended early by any of
// signals: refused for a 401 or 404, ok with the text for another success.
async function call(life, method, target, ms, ...signals) {
  const { signal, done } = deadline(ms, life.signal, ...signals)
  try {
    const response = await fetch(target, { method, signal })
    const text = await response.text()
    if (response.status === 401 || response.status === 404) return { refused: true }
    return { ok: response.ok, text }
  } catch {
    return {}
  } finally {
    done()
  }
}

function next(wait) {
  return wait === 0 ? backoffFirstMs : Math.min(wait * 2, backoffMostMs)
}

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

// deadline is a signal ending after ms or with any of signals, and done, which
// lets go of the timer and every listener (opencode_steer.js's).
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

function quietly(task) {
  task.catch(() => {})
}
