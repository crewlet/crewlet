// crewlet's Qwen Code helper: the ACP host a run's script starts as its
// harness (qwen.go, QwenCode.Command), and the command its skill hook runs.
// Plain node with no imports beyond its own: Qwen Code is an npm package that
// needs node 22, so every box that runs it has one, and nothing is installed
// or fetched. It dials only out: Qwen Code's stdio, the run's notes route and
// skill route.
//
//   node qwen-helper.mjs run <config> <session id> <new|load>
//     Reads the brief on stdin and runs it as one ACP turn of
//     `qwen --acp --approval-mode yolo`: initialize;
//     session/new under the run's minted id, or session/load of the session
//     the run continues; the bridge connected (qwen/status/workspace/mcp);
//     session/prompt. Every session/update Qwen Code sends from then on is
//     written to
//     stdout as it came, a line each, with the helper's own lines
//     (`crewlet_qwen_*`) between them: the stream the runner reads
//     (qwenstream.go). Stops with session/cancel, never a signal: SIGTERM
//     tears the chat file Qwen Code is writing (measured on 0.28.4).
//
//   node qwen-helper.mjs skill <config>
//     The PostToolUse hook on the `skill` tool: reports the skill loaded to
//     the run's skill route by the name the engine handed it under.
//
//   node qwen-helper.mjs restore <config> <session id> <chat file>
//     Files a session carried from another box (qwencarry.go) under this
//     working directory's key, its first line's cwd rewritten to the
//     directory's realpath, the one field a resume checks (measured on
//     0.28.4), and removes every other key's copy of it.
//
//   node qwen-helper.mjs guard [--server <name>]... [--hook <name>]
//     Exits 3, naming the file, the key and the entry, where a settings file
//     the run's system tier cannot overrule takes one of Crewlet's own servers
//     or its tools away, or, for a run with a hook, where the settings or safe
//     mode switch every hook off (qwenguard.go). A file that does not read is
//     passed over, as Qwen Code reads it as empty.
//
// NOTES (the runner note contract's poll channel). While the turn runs, the
// helper polls the notes route and keeps what it fetched. Qwen Code asks for
// mid-turn messages after every tool batch of the main agent
// (craft/drainMidTurnQueue), gives each ask 2 s and stops asking for the rest
// of the session after three that time out; the helper answers every ask from
// what it already holds, at once, so it never meets either, handing every
// note it holds (drain). Qwen Code puts each before the next request as a
// text framed `\n[User message received during tool execution]: `, and
// records each in the session's chat file as its own `type:user`, subtype
// `mid_turn_user_message`, before that request goes out (0.28.4). A note is
// confirmed read on the notes route only once the chat file holds that record
// with its exact text, matched in order (two identical notes need two
// records, and one never recorded is passed over), and an assistant record
// after it, the reply to the request that carried it. A turn cancelled
// mid-reply records the streamed part as one, and a resume hands the note back
// before it (measured on 0.28.4), so it confirms; one with no reply (its
// request failed, or the run was killed before one) is never confirmed, and
// the engine serves it again. What the helper fetched and never handed dies
// with it: nothing of it is left in Qwen Code's session.
import { spawn } from "node:child_process"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"

// framing is what Qwen Code puts before a drained message, in the request and
// in its record (measured on 0.28.4).
const framing = "\n[User message received during tool execution]: "

// limitExit is what the helper exits with where Qwen Code failed the turn on
// its vendor's 429 after its own retries (the session/prompt error's code):
// the status mod 256, as Gemini CLI exits (geministream.go). The runner reads
// a limit only off a run that exited so (qwen.go, limitExit).
const limitExit = 429 % 256

// stoppedExit is what the helper exits with once a signal stopped the turn,
// as a process SIGTERM ended would.
const stoppedExit = 143

// mcpPollMs is the pause between two asks of the workspace's MCP status: the
// measured bridge went from disconnected to connected 0.4 to 0.7 s after
// session/new, so a quarter second costs at most that much of the start.
const mcpPollMs = 250

// pollAbortMs bounds one poll of the notes route: the engine holds an empty
// poll ten seconds (internal/api/mcpbridge, steerHold), so twenty is that
// hold and as much again for a relay and a slow network.
const pollAbortMs = 20_000

// readAbortMs bounds one confirmation: a single write on the run's record.
const readAbortMs = 10_000

// backoffFirstMs and backoffMostMs pace a failed request of the notes route:
// a second, doubling to thirty, reset by the next that succeeds, as
// OpenCode's plugin paces them (opencode_steer.js).
const backoffFirstMs = 1_000
const backoffMostMs = 30_000

// confirmEveryMs is how often the chat file is read for the records of the
// notes handed: a note is recorded within tens of milliseconds of the next
// request, and a second's delay in confirming it costs nothing.
const confirmEveryMs = 1_000

// stopWaitMs bounds Qwen Code's answer to session/cancel (measured within
// 60 ms) and to the window's ask once a turn is over, and stopExitMs its exit once its stdin closes on a stopped run (2.4 s
// measured): with the last confirmations (hurriedPostMs) they stay inside the
// five seconds the run's script gives the helper (internal/sandbox's
// JobStopGrace) in every measured case, and the helper kills Qwen Code
// itself before the script kills the helper.
const stopWaitMs = 2_000
const stopExitMs = 3_000

// hurriedPostMs bounds the last confirmations of a run a signal stopped: a
// second, so the cancel, the confirmations and Qwen Code's exit together stay
// inside the five seconds the run's script gives the helper.
const hurriedPostMs = 1_000

// exitWaitMs bounds the wait for Qwen Code to exit once its turn is over and
// its stdin closed: the 2.4 s measured, and room on a busy box.
const exitWaitMs = 10_000

const [mode, configPath, sessionID, fourth] = process.argv.slice(2)

// ---- the skill hook -------------------------------------------------------

// skill reports one load of a skill. NOTHING FAILS THE RUN: Qwen Code goes on
// whatever a hook does, and an unreported load is reported afresh when the
// model's next call to a covered tool is refused with the skill to load.
async function skill() {
  try {
    const config = JSON.parse(fs.readFileSync(configPath, "utf8"))
    const input = JSON.parse(fs.readFileSync(0, "utf8"))
    const name = input?.tool_input?.skill
    if (config.skills && input?.tool_name === "skill" && typeof name === "string" && name !== "") {
      // Written as `crewlet-x` (qwenSkillName), reported as `crewlet:x`.
      const prefix = config.namespace + "-"
      const listed = name.startsWith(prefix) ? config.namespace + ":" + name.slice(prefix.length) : name
      await fetch(config.skills, {
        method: "POST",
        body: JSON.stringify({ skill: listed }),
        headers: { "content-type": "application/json" },
        signal: AbortSignal.timeout(readAbortMs),
      })
    }
  } catch {
    // Said above: the load is reported again.
  }
  process.stdout.write("{}")
}

// ---- the run --------------------------------------------------------------

// run is the whole turn, and the helper's exit status.
async function run() {
  const config = { ...JSON.parse(fs.readFileSync(configPath, "utf8")), session: sessionID, load: fourth === "load" }
  const brief = fs.readFileSync(0, "utf8")
  const out = (line) => process.stdout.write(JSON.stringify(line) + "\n")
  const host = new Host(config, out)
  return host.run(brief)
}

// Host is one turn of Qwen Code driven over ACP.
class Host {
  constructor(config, out) {
    this.config = config
    this.out = out
    this.nextID = 0
    this.pending = new Map()
    // failedRequest is the newest model request Qwen Code reported ending in
    // an error, by its execution id: the one a 429 failure's record names.
    this.failedRequest = ""
    // loading is set while session/load replays the continued session.
    this.loading = false
    this.exited = null
    this.outputClosed = false
    this.stopping = false
    this.inbox = new Inbox(config, out)
  }

  async run(brief) {
    const args = ["--acp", "--auth-type", this.config.authType, "--approval-mode", "yolo"]
    // ITS OWN PROCESS GROUP, so a signal meant for the job's processes
    // reaches the helper, which stops the turn with session/cancel, and never
    // Qwen Code itself; a helper killed outright closes Qwen Code's stdin,
    // and it exits.
    this.child = spawn("qwen", args, { stdio: ["pipe", "pipe", "inherit"], detached: true })
    this.exited = new Promise((resolve) => {
      this.child.on("exit", (code, signal) => resolve({ code, signal }))
      this.child.on("error", (err) => {
        // NOT STARTED (no `qwen` on PATH): no output will ever answer.
        process.stderr.write("crewlet: Qwen Code could not be started: " + err.message + "\n")
        this.closed("Qwen Code could not be started: " + err.message)
        resolve({ code: null, signal: null, error: err })
      })
    })
    this.child.stdin.on("error", () => {})
    this.read(this.child.stdout)
    for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) process.on(sig, () => this.stop())

    const session = this.config.session
    let ended
    try {
      await this.call("initialize", {
        protocolVersion: 1,
        clientCapabilities: { fs: { readTextFile: false, writeTextFile: false }, terminal: false },
      })
      if (this.config.load) {
        // THE LOADED SESSION'S HISTORY IS REPLAYED as session/update before
        // the load answers (ACP's loadSession), none of it this run's: it is
        // not written to the stream, which the runner reads as the run's own
        // live view and spend.
        this.loading = true
        try {
          await this.call("session/load", { sessionId: session, cwd: workingDir(), mcpServers: [] })
        } finally {
          this.loading = false
        }
      } else {
        await this.call("session/new", { cwd: workingDir(), mcpServers: [], _meta: { "qwen-code/sessionId": session } })
      }
      this.out({ type: "crewlet_qwen_session", session_id: session })
      const missing = await this.awaitServers(session)
      if (missing !== "") {
        this.out({ type: "crewlet_qwen_end", error: { message: missing } })
        process.stderr.write("crewlet: " + missing + "\n")
        await this.close()
        return 1
      }
      this.inbox.start(this.chatFile(session))
      this.prompting = true
      this.promptID = this.nextID + 1
      ended = await this.call("session/prompt", { sessionId: session, prompt: [{ type: "text", text: brief }] }, true)
    } catch (err) {
      ended = { error: { message: String(err?.message ?? err) } }
    }
    this.prompting = false
    await this.inbox.stop(this.stopping)
    if (!this.stopping && !this.outputClosed) {
      const usage = await this.call("qwen/status/session/context_usage", { sessionId: session }, true, stopWaitMs)
      const window = usage?.result?.usage?.contextWindowSize
      if (Number.isSafeInteger(window) && window > 0) this.out({ type: "crewlet_qwen_window", context_window: window })
    }
    let code = 0
    if (ended?.error) {
      const error = { code: ended.error.code, message: String(ended.error.message ?? "") }
      // THE PROVIDER'S WORDS where Qwen Code relays them (a prompt past the
      // window: -32603 "Internal error", the provider's sentence in details).
      if (typeof ended.error.data?.details === "string") error.details = ended.error.data.details
      code = 1
      if (error.code === 429) {
        code = limitExit
        const said = this.limitRecord(session)
        if (said !== "") this.out({ type: "crewlet_qwen_limit", status_code: 429, error_message: said })
      }
      this.out({ type: "crewlet_qwen_end", error })
    } else {
      this.out({ type: "crewlet_qwen_end", stop_reason: String(ended?.result?.stopReason ?? "") })
    }
    if (this.stopping) code = stoppedExit
    const gone = await this.close()
    if (!gone.ok) process.stderr.write("crewlet: Qwen Code did not exit once its turn was over; it was killed\n")
    return code
  }

  // awaitServers waits for the run's MCP servers to connect, and is why the
  // run cannot start: "" once the bridge is connected, or the wait is over
  // with only other servers left (said on stderr; the run goes on without
  // them, as Qwen Code would).
  async awaitServers(session) {
    const wanted = this.config.servers ?? []
    if (wanted.length === 0) return ""
    const deadline = Date.now() + this.config.mcpWaitMs
    let connected = new Set()
    for (;;) {
      const status = await this.call("qwen/status/workspace/mcp", { sessionId: session }, true)
      connected = new Set(
        (status?.result?.servers ?? []).filter((s) => s?.mcpStatus === "connected").map((s) => s.name),
      )
      if (wanted.every((name) => connected.has(name))) return ""
      if (this.outputClosed || this.stopping) throw new Error("Qwen Code stopped before its MCP servers connected")
      if (Date.now() >= deadline) break
      await sleep(mcpPollMs)
    }
    const left = wanted.filter((name) => !connected.has(name))
    const bridge = this.config.bridge
    if (bridge && left.includes(bridge)) {
      return `Qwen Code did not connect the run's tool bridge (${bridge}) within ` +
        `${this.config.mcpWaitMs / 1000}s, so the run was not started`
    }
    process.stderr.write(`crewlet: starting without the MCP servers that did not connect: ${left.join(", ")}\n`)
    return ""
  }

  // stop ends the turn as a signal asks: session/cancel, which Qwen Code
  // answers by ending the prompt with stopReason cancelled; the rest of the
  // run's end follows from there.
  stop() {
    if (this.stopping) return
    this.stopping = true
    if (!this.prompting) {
      this.close()
      return
    }
    this.notify("session/cancel", { sessionId: this.config.session })
    // A TURN THAT DOES NOT END ON THE CANCEL is ended here, so the helper still
    // closes Qwen Code's stdin inside the run's grace.
    setTimeout(() => {
      const settle = this.pending.get(this.promptID)
      this.pending.delete(this.promptID)
      settle?.({ error: { message: "Qwen Code did not end its turn on session/cancel" } })
    }, stopWaitMs).unref()
  }

  // close closes Qwen Code's stdin, its way to exit, and waits for it to go;
  // ok false where it had to be killed.
  async close() {
    try {
      this.child.stdin.end()
    } catch {
      // Gone already.
    }
    const wait = this.stopping ? stopExitMs : exitWaitMs
    const over = new AbortController()
    const gone = await Promise.race([this.exited, sleep(wait, over.signal).then(() => null)])
    over.abort()
    if (gone !== null) return { ok: true }
    try {
      process.kill(-this.child.pid, "SIGKILL")
    } catch {
      // Gone meanwhile.
    }
    await this.exited
    return { ok: false }
  }

  // chatFile is where Qwen Code records session: its home's projects
  // directory, keyed by the working directory's real path with every
  // character but a letter or digit made a dash (0.28.4).
  chatFile(session) {
    return chatPath(this.config.qwenHome, session)
  }

  // limitRecord is the provider's own words for the 429 Qwen Code failed the
  // turn on: the error record its chat file holds for the request that
  // failed last, "" where none names it. The file is the box's and the model's
  // shell can write it, so only the record of the failing request is read,
  // the first naming its execution id, and only after a 429 failure.
  limitRecord(session) {
    if (this.failedRequest === "") return ""
    let text = ""
    try {
      text = fs.readFileSync(this.chatFile(session), "utf8")
    } catch {
      return ""
    }
    for (const line of text.split("\n")) {
      let record
      try {
        record = JSON.parse(line)
      } catch {
        continue
      }
      const ev = record?.systemPayload?.uiEvent
      if (record?.type === "system" && record?.subtype === "ui_telemetry" && ev?.["event.name"] === "qwen-code.api_error" &&
        ev.execution_id === this.failedRequest && ev.status_code === 429 && typeof ev.error_message === "string") {
        return ev.error_message
      }
    }
    return ""
  }

  // read follows Qwen Code's stdout a JSON-RPC message a line.
  read(stream) {
    let buf = ""
    stream.setEncoding("utf8")
    stream.on("data", (chunk) => {
      buf += chunk
      for (let at = buf.indexOf("\n"); at >= 0; at = buf.indexOf("\n")) {
        const line = buf.slice(0, at)
        buf = buf.slice(at + 1)
        this.message(line)
      }
    })
    stream.on("end", () => this.closed("Qwen Code closed its output"))
  }

  // closed settles everything still waiting on an answer that will not come.
  closed(why) {
    this.outputClosed = true
    for (const [, settle] of this.pending) settle({ error: { message: why } })
    this.pending.clear()
  }

  // message handles one line of Qwen Code's.
  message(line) {
    let m
    try {
      m = JSON.parse(line)
    } catch {
      return
    }
    if (m.id !== undefined && m.method === undefined) {
      const settle = this.pending.get(m.id)
      this.pending.delete(m.id)
      settle?.(m)
      return
    }
    if (m.method === "session/update") {
      if (this.loading) return
      const life = m.params?.update?._meta?.executionLifecycle
      if (life?.kind === "request" && life.phase === "ended" && life.outcome === "error") {
        this.failedRequest = String(life.executionId ?? "")
      }
      process.stdout.write(line + "\n")
      return
    }
    if (m.id === undefined) return
    switch (m.method) {
      case "craft/drainMidTurnQueue":
        // AT ONCE, from what is held: the ask's 2 s are Qwen Code's.
        this.answer(m.id, { messages: this.inbox.drain() })
        return
      case "session/request_permission": {
        // Never asked under yolo (measured); a headless run cannot ask anybody.
        const option = (m.params?.options ?? []).find((o) => /^allow/.test(o?.kind ?? ""))
        this.answer(m.id, option ? { outcome: { outcome: "selected", optionId: option.optionId } }
          : { outcome: { outcome: "cancelled" } })
        return
      }
      default:
        this.send({ jsonrpc: "2.0", id: m.id, error: { code: -32601, message: "Method not found" } })
    }
  }

  send(message) {
    try {
      this.child.stdin.write(JSON.stringify(message) + "\n")
    } catch {
      // Gone: its exit ends the run.
    }
  }

  answer(id, result) { this.send({ jsonrpc: "2.0", id, result }) }

  notify(method, params) { this.send({ jsonrpc: "2.0", method, params }) }

  // call sends a request and resolves with Qwen Code's answer. An error
  // answer rejects, unless whole is set, which hands it back as it came; a
  // bound, where given, resolves with nothing once it passes.
  call(method, params, whole = false, boundMs = 0) {
    const id = ++this.nextID
    const answered = new Promise((resolve, reject) => {
      this.pending.set(id, (m) => {
        if (m.error && !whole) reject(new Error(`${method}: ${m.error.message ?? "refused"}`))
        else resolve(m)
      })
    })
    this.send({ jsonrpc: "2.0", id, method, params })
    if (boundMs <= 0) return answered
    let timer
    const bound = new Promise((resolve) => (timer = setTimeout(() => resolve(null), boundMs)))
    return Promise.race([answered, bound]).finally(() => clearTimeout(timer))
  }
}

// Inbox is the run's notes: polled from the notes route while the turn runs,
// handed at the next ask, and confirmed read once the chat file records them.
class Inbox {
  constructor(config, out) {
    this.url = config.notes ?? ""
    this.out = out
    // loaded says the run continues a session, whose chat file exists.
    this.loaded = config.load === true
    this.cursor = 0
    // held is fetched and not yet handed; handed is handed and not yet
    // recorded, in the order handed; matched is recorded and not yet known
    // to have reached the model plain (record); owed is confirmed and not yet
    // posted.
    this.held = []
    this.handed = []
    this.matched = []
    this.owed = []
    this.life = new AbortController()
    this.refused = false
    this.offset = 0
    this.partial = ""
    // lost is set once the chat file reads shorter than what was read of it,
    // or a continued session's file cannot be measured as the turn begins.
    this.lost = false
  }

  start(chat) {
    if (this.url === "") return
    this.chat = chat
    // FROM WHERE THE TURN BEGINS: a loaded session holds an earlier job's
    // records of notes, which a note of the same text this turn hands was
    // never delivered as.
    // A NEW SESSION'S FILE does not exist yet and begins at 0; a CONTINUED
    // one's that cannot be measured confirms nothing this turn, since from 0
    // an earlier job's records would confirm a note of the same text.
    try {
      this.offset = fs.statSync(chat).size
    } catch {
      this.offset = 0
      this.lost = this.loaded
    }
    this.polling = this.poll()
    this.confirming = this.confirmLoop()
  }

  // drain is the answer to one ask: every note held, oldest first. Each
  // message of one answer becomes a record of its own holding its exact text,
  // in order (two in one answer measured on 0.28.4), which is what confirms
  // it.
  drain() {
    const notes = this.held.splice(0)
    for (const note of notes) {
      this.handed.push(note)
      this.out({ type: "crewlet_qwen_note", id: note.id, message: note.message })
    }
    return notes.map((note) => note.message)
  }

  // stop ends the polling and confirms what the chat file shows. A match no
  // reply followed is never confirmed: Qwen Code records a note before the
  // request carrying it goes out, and that request may have failed, been
  // cancelled or been cut off by the stop. Hurried where a signal stopped the
  // run.
  async stop(hurried) {
    if (this.url === "") return
    this.done = true
    this.life.abort()
    await this.polling
    await this.confirming
    this.scan()
    await this.post(hurried ? hurriedPostMs : readAbortMs)
  }

  async poll() {
    let wait = 0
    while (!this.done && !this.refused) {
      const began = Date.now()
      const answer = await request("GET", this.url + "/notes?after=" + this.cursor, undefined, pollAbortMs, this.life.signal)
      if (this.done) return
      if (answer.refused) {
        this.refused = true
        return
      }
      if (answer.ok && this.admit(answer.text)) {
        wait = 0
        // AN EMPTY ANSWER CUT SHORT of the engine's hold waits out a second
        // before the next poll, so no answer drives the helper faster.
        const early = backoffFirstMs - (Date.now() - began)
        if (early > 0) await sleep(early, this.life.signal)
        continue
      }
      wait = wait === 0 ? backoffFirstMs : Math.min(wait * 2, backoffMostMs)
      await sleep(wait, this.life.signal)
    }
  }

  // admit holds each new note of one poll's answer, advancing the cursor.
  // False for an answer that is not one.
  admit(text) {
    let notes
    try {
      notes = JSON.parse(text)?.notes
    } catch {
      return false
    }
    if (!Array.isArray(notes)) return false
    for (const note of notes) {
      if (!wellFormed(note) || note.seq <= this.cursor) continue
      this.held.push({ id: note.id, message: note.message })
      this.cursor = note.seq
    }
    return true
  }

  async confirmLoop() {
    while (!this.done) {
      await sleep(confirmEveryMs, this.life.signal)
      if (this.done) return
      this.scan()
      await this.post(readAbortMs)
    }
  }

  // scan reads the chat file from where it last stopped and confirms, in
  // order, each note handed whose record it finds.
  scan() {
    if (this.lost || (this.handed.length === 0 && this.matched.length === 0)) return
    let fd
    try {
      fd = fs.openSync(this.chat, "r")
    } catch {
      return
    }
    try {
      const size = fs.fstatSync(fd).size
      if (size < this.offset) {
        // A FILE SHORTER THAN WHAT WAS READ OF IT was rewritten under the
        // run, and its start holds a continued session's earlier records,
        // which a note this run lost would match: it confirms nothing more,
        // and what it has not confirmed is served again.
        this.lost = true
        this.matched = []
        this.handed = []
        return
      }
      const chunk = Buffer.alloc(Math.min(size - this.offset, 8 << 20))
      const n = chunk.length === 0 ? 0 : fs.readSync(fd, chunk, 0, chunk.length, this.offset)
      this.offset += n
      const lines = (this.partial + chunk.subarray(0, n).toString("utf8")).split("\n")
      this.partial = lines.pop()
      for (const line of lines) this.record(line)
    } finally {
      fs.closeSync(fd)
    }
  }

  // record matches, where line is a note's record, the first note handed with
  // its text, the ones handed before it left unconfirmed (a note Qwen Code
  // never recorded must not keep every later one from its record), and
  // confirms the matches an assistant record follows: a response to a request
  // that carried them plain. A compaction before it keeps a match only where
  // its text survives plain past the summary (ADR-0177; qwenconfirm.go's
  // qwenNotesHeld, the same rule); one it drops is served again. A line that
  // does not parse drops every match: it may have been a compaction that
  // summarized them, so their notes are served again rather than confirmed.
  record(line) {
    if (line.trim() === "") return
    let r
    try {
      r = JSON.parse(line)
    } catch {
      this.matched = []
      return
    }
    if (r?.type === "assistant") {
      this.owed.push(...this.matched.splice(0).map((note) => note.id))
    } else if (r?.type === "system" && r?.subtype === "chat_compression") {
      const tail = preserved(r.systemPayload)
      this.matched = this.matched.filter((note) => {
        const n = tail.get(note.message) ?? 0
        tail.set(note.message, n - 1)
        return n > 0
      })
    } else if (r?.type === "user" && r?.subtype === "mid_turn_user_message") {
      const at = this.handed.findIndex((note) => r?.message?.parts?.[0]?.text === framing + note.message)
      if (at >= 0) this.matched.push(this.handed.splice(0, at + 1)[at])
    }
  }

  // post confirms what is owed, asked again after a failure until bound.
  async post(boundMs) {
    const until = Date.now() + boundMs
    let wait = 0
    while (this.owed.length > 0 && !this.refused && Date.now() < until) {
      const ids = this.owed.slice()
      const answer = await request("POST", this.url + "/read", JSON.stringify({ ids }), Math.max(1, until - Date.now()))
      if (answer.refused) {
        this.refused = true
        return
      }
      if (answer.ok) {
        this.owed = this.owed.filter((id) => !ids.includes(id))
        wait = 0
        continue
      }
      wait = wait === 0 ? backoffFirstMs : Math.min(wait * 2, backoffMostMs)
      await sleep(Math.min(wait, Math.max(0, until - Date.now())))
    }
  }
}

// workingDir is the run's working directory as the helper names it: its
// realpath. Qwen Code keys and records the cwd session/new and session/load
// name as given, unresolved (measured over ACP on 0.28.4), so the helper names
// the realpath every time.
function workingDir() {
  return fs.realpathSync(process.cwd())
}

// chatPath is where Qwen Code records session from this working directory: its
// home's projects directory, under the realpath with every character but a
// letter or a digit a dash.
function chatPath(qwenHome, session) {
  return path.join(qwenHome, "projects", workingDir().replace(/[^a-zA-Z0-9]/g, "-"), "chats", session + ".jsonl")
}

// restore files the carried chat file under this directory's key (the mode
// above), and is the exit status: 1, said on stderr, where the file is not the
// session's or cannot be filed.
function restore() {
  try {
    const config = JSON.parse(fs.readFileSync(configPath, "utf8"))
    const text = fs.readFileSync(fourth, "utf8")
    const end = text.indexOf("\n")
    const head = JSON.parse(end < 0 ? text : text.slice(0, end))
    if (head?.sessionId !== sessionID) throw new Error("its first line names another session")
    head.cwd = workingDir()
    const target = chatPath(config.qwenHome, sessionID)
    fs.mkdirSync(path.dirname(target), { recursive: true })
    fs.writeFileSync(target + ".restoring", JSON.stringify(head) + (end < 0 ? "\n" : text.slice(end)))
    fs.renameSync(target + ".restoring", target)
    const projects = path.join(config.qwenHome, "projects")
    for (const key of fs.readdirSync(projects)) {
      const other = path.join(projects, key, "chats", sessionID + ".jsonl")
      if (other !== target) fs.rmSync(other, { force: true })
    }
    return 0
  } catch (err) {
    process.stderr.write("crewlet: the carried Qwen Code session could not be filed: " + String(err?.message ?? err) + "\n")
    return 1
  }
}

// preserved counts, by text, what a chat_compression record keeps plain: each
// text part of a user entry of its history past the prefix, the framing cut.
// The prefix is the larger of summaryPrefixLength and contextPrefixLength,
// which no capture tells apart, so a summarized note is never kept
// (qwenconfirm.go's qwenPreserved, the same rule). A summary prefix that does
// not read keeps nothing.
function preserved(payload) {
  const kept = new Map()
  const summary = payload?.summaryPrefixLength
  const context = payload?.contextPrefixLength
  const history = payload?.compressedHistory
  if (!Number.isSafeInteger(summary) || summary < 0 || !Array.isArray(history)) return kept
  const prefix = Number.isSafeInteger(context) ? Math.max(summary, context) : summary
  for (const entry of history.slice(prefix)) {
    if (entry?.role !== "user") continue
    for (const part of Array.isArray(entry?.parts) ? entry.parts : []) {
      if (typeof part?.text !== "string") continue
      const text = part.text.startsWith(framing) ? part.text.slice(framing.length) : part.text
      kept.set(text, (kept.get(text) ?? 0) + 1)
    }
  }
  return kept
}

// wellFormed says whether a poll's note is one the helper can hand.
function wellFormed(note) {
  return note !== null && typeof note === "object" && Number.isSafeInteger(note.seq) && note.seq > 0 &&
    typeof note.id === "string" && note.id !== "" && typeof note.message === "string" && note.message !== ""
}

// request makes one request of the notes route within ms, ended early by
// signal: refused for a token the route no longer resolves or a run with no
// notes route, ok with the answer's text for any other success.
async function request(method, target, body, ms, signal) {
  const signals = [AbortSignal.timeout(ms)]
  if (signal) signals.push(signal)
  try {
    const response = await fetch(target, {
      method,
      body,
      headers: body === undefined ? undefined : { "content-type": "application/json" },
      signal: AbortSignal.any(signals),
    })
    const text = await response.text()
    if (response.status === 401 || response.status === 404) return { refused: true }
    return { ok: response.ok, text }
  } catch {
    return {}
  }
}

// sleep resolves after ms, or as soon as signal ends.
function sleep(ms, signal) {
  return new Promise((resolve) => {
    if (signal?.aborted) return resolve()
    const timer = setTimeout(done, ms)
    function done() {
      clearTimeout(timer)
      signal?.removeEventListener("abort", done)
      resolve()
    }
    signal?.addEventListener("abort", done, { once: true })
  })
}

// ---- the guard -------------------------------------------------------------

// guard is the mode above: 0 where nothing switches the servers args name off,
// else 3, said on stderr.
function guard(args) {
  const servers = []
  const hooks = []
  const skills = []
  for (let i = 0; i + 1 < args.length; i += 2) {
    if (args[i] === "--server") servers.push(args[i + 1])
    if (args[i] === "--hook") hooks.push(args[i + 1])
    if (args[i] === "--skill") skills.push(args[i + 1])
  }
  const found = switchedOff(servers)
  if (hooks.length > 0) found.push(...hooksOff(hooks))
  if (skills.length > 0) found.push(...skillsOff(skills))
  if (found.length === 0) return 0
  process.stderr.write("crewlet: Qwen Code would start without what the run's tools or skill loads go through: " +
    found.join("; ") + ". No setting of the run's undoes it, as Qwen Code adds each such list up across its " +
    "settings files and the run never switches on hooks the box switched off; remove the entry. The run was " +
    "not started.\n")
  return 3
}

// hooksOff is why 0.28.4 would run none of hooks, a sentence a cause, each
// measured: `disableAllHooks` truthy as JavaScript reads it ("false" too) in
// the highest of the system defaults, the home's and the working directory's
// settings that set it; and QWEN_CODE_SAFE_MODE 1, true, yes or on in any
// case, from the first `.env` it finds ([envFile]), else the settings' `env`,
// the working directory's over the home's (a variable set in one of those
// wins over each after it). The run's environment never holds it: the wrapper
// unsets it, as it does Claude Code's (qwen.go, QwenCode.Output).
function hooksOff(hooks) {
  const run = `the run's hook ${hooks.join(", ")}`
  const found = []
  let all = null
  let safe = null
  for (const file of settingsFiles().reverse()) {
    const s = readSettings(file)
    if (s === null) continue
    if (all === null && s.disableAllHooks !== undefined && s.disableAllHooks !== null) all = { file, value: s.disableAllHooks }
    const value = s.env?.QWEN_CODE_SAFE_MODE
    if (safe === null && typeof value === "string") safe = { where: `${file}'s env.QWEN_CODE_SAFE_MODE`, value }
  }
  if (all !== null && all.value) {
    found.push(`${all.file}: its disableAllHooks is ${JSON.stringify(all.value)}, which switches every hook off, ` +
      `${run} among them`)
  }
  const env = envFile()
  if (env !== null) {
    const value = envValue(env, "QWEN_CODE_SAFE_MODE")
    if (value !== null) safe = { where: `${env}'s QWEN_CODE_SAFE_MODE`, value }
  }
  if (safe !== null && /^(1|true|yes|on)$/i.test(safe.value)) {
    found.push(`${safe.where} is ${JSON.stringify(safe.value)}: safe mode switches every hook off, ${run} among them`)
  }
  return found
}

// skillsOff is what of the settings files below the run's switches one of
// skills off, a sentence an entry, each measured on 0.28.4: `skills.disabled`
// or `skills.defaultDisabled` naming it by its listed name in any case, or
// `skills.disabledLevels` holding "user", the level the run's skills directory
// is read at; any file's entry counts.
function skillsOff(skills) {
  const fold = (name) => String(name).toLowerCase().trim()
  const list = (value) => (Array.isArray(value) ? value : [])
  const found = []
  for (const file of settingsFiles()) {
    const s = readSettings(file)
    if (s === null) continue
    for (const key of ["disabled", "defaultDisabled"]) {
      for (const name of skills) {
        if (list(s.skills?.[key]).some((entry) => fold(entry) === fold(name))) {
          found.push(`${file}: its skills.${key} names the run's skill ${name}`)
        }
      }
    }
    if (list(s.skills?.disabledLevels).includes("user")) {
      found.push(`${file}: its skills.disabledLevels holds "user", the level of the run's skills ${skills.join(", ")}`)
    }
  }
  return found
}

// settingsFiles is the settings files below the run's system tier, lowest
// first: the system defaults, the home's, and the working directory's unless
// it is the home (measured on 0.28.4: a higher one's value wins).
function settingsFiles() {
  const home = os.homedir()
  const files = [process.env.QWEN_CODE_SYSTEM_DEFAULTS_PATH || path.join(path.dirname(
    process.env.QWEN_CODE_SYSTEM_SETTINGS_PATH || "/etc/qwen-code/settings.json"), "system-defaults.json"),
  path.join(home, ".qwen", "settings.json")]
  if (realpath(process.cwd()) !== realpath(home)) files.push(path.join(process.cwd(), ".qwen", "settings.json"))
  return files
}

// envFile is the one `.env` 0.28.4 loads: the first of the working
// directory's `.qwen/.env` and `.env`, then the home's (measured: each loads,
// the working directory's first; `.gemini/.env` and a `.env` above the home
// never), or null.
function envFile() {
  const home = os.homedir()
  const dirs = realpath(process.cwd()) === realpath(home) ? [home] : [process.cwd(), home]
  for (const dir of dirs) {
    for (const file of [path.join(dir, ".qwen", ".env"), path.join(dir, ".env")]) {
      try {
        if (fs.statSync(file).isFile()) return file
      } catch {
        // not there: the next one
      }
    }
  }
  return null
}

// envValue is name's value in the `.env` file, its last assignment, quotes
// stripped; null where it sets none or cannot be read.
function envValue(file, name) {
  let text
  try {
    text = fs.readFileSync(file, "utf8")
  } catch {
    return null
  }
  let value = null
  for (const line of text.split(/\r?\n/)) {
    const m = line.match(/^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$/)
    if (m === null || m[1] !== name) continue
    const q = m[2].match(/^(["'`])(.*)\1$/)
    value = q !== null ? q[2] : m[2].replace(/\s+#.*$/, "")
  }
  return value
}

// switchedOff is what the settings files below the run's take away of
// servers, a sentence each: the home's, the working directory's unless it is
// the home, and the system defaults, read as 0.28.4 reads them (its settings
// of an older shape, `excludeMCPServers`, `allowMCPServers` and
// `excludeTools`, included). `mcp.excluded` names a server by its name or a
// glob of `*` and `?`, case kept; `permissions.deny` and `tools.exclude` deny
// a server's tools by `mcp__<server>`, a tool's qualified name or a trailing
// `*`, a rule with a specifier never; `tools.disabled` hides a tool by its
// qualified name alone; and the `mcp.allowed` lists, joined across the files,
// must name the server where any file holds one (each measured on 0.28.4).
function switchedOff(servers) {
  const home = os.homedir()
  const files = [path.join(home, ".qwen", "settings.json")]
  if (realpath(process.cwd()) !== realpath(home)) files.push(path.join(process.cwd(), ".qwen", "settings.json"))
  files.push(process.env.QWEN_CODE_SYSTEM_DEFAULTS_PATH || path.join(path.dirname(
    process.env.QWEN_CODE_SYSTEM_SETTINGS_PATH || "/etc/qwen-code/settings.json"), "system-defaults.json"))
  const list = (...values) =>
    values.flatMap((v) => (Array.isArray(v) ? v : [])).filter((e) => typeof e === "string" && e !== "")
  const found = []
  const allowed = []
  for (const file of files) {
    const s = readSettings(file)
    if (s === null) continue
    const why = []
    for (const name of servers) {
      const qualified = `mcp__${name}__`
      for (const entry of list(s.mcp?.excluded, s.excludeMCPServers)) {
        if (globMatch(name, entry)) why.push(`its mcp.excluded entry "${entry}" takes the server ${name} away`)
      }
      for (const [key, entries] of [["permissions.deny", list(s.permissions?.deny)],
        ["tools.exclude", list(s.tools?.exclude, s.excludeTools)]]) {
        for (const entry of entries) {
          if (entry.includes("(")) continue
          const prefix = entry.endsWith("*") ? entry.slice(0, -1) : null
          const hits = prefix === null
            ? entry === `mcp__${name}` || entry.startsWith(qualified)
            : qualified.startsWith(prefix) || prefix.startsWith(qualified)
          if (hits) why.push(`its ${key} entry "${entry}" denies the tools of the server ${name}`)
        }
      }
      for (const entry of list(s.tools?.disabled)) {
        if (entry.startsWith(qualified) && !entry.includes("*")) {
          why.push(`its tools.disabled entry "${entry}" hides a tool of the server ${name}`)
        }
      }
    }
    if (Array.isArray(s.mcp?.allowed) || Array.isArray(s.allowMCPServers)) {
      allowed.push({ file, entries: list(s.mcp?.allowed, s.allowMCPServers) })
    }
    if (why.length > 0) found.push(`${file}: ${why.join(", ")}`)
  }
  for (const name of servers) {
    if (allowed.length === 0 || allowed.some(({ entries }) => entries.some((e) => globMatch(name, e)))) continue
    for (const { file, entries } of allowed) {
      found.push(`${file}: its mcp.allowed ${JSON.stringify(entries)} leaves the server ${name} out`)
    }
  }
  return found
}

// globMatch is Qwen Code's own match of a server name against an
// `mcp.excluded` or `mcp.allowed` entry: equal, or a glob of `*` and `?`.
function globMatch(name, pattern) {
  if (!pattern.includes("*") && !pattern.includes("?")) return name === pattern
  const re = pattern.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".")
  return new RegExp("^" + re + "$", "s").test(name)
}

// readSettings is the settings object file holds, its comments and a leading
// byte-order mark stripped as Qwen Code strips them; null for none, or one
// that does not read as one.
function readSettings(file) {
  try {
    const text = fs.readFileSync(file, "utf8").replace(/^\uFEFF/, "")
    const settings = JSON.parse(withoutComments(text))
    return settings !== null && typeof settings === "object" && !Array.isArray(settings) ? settings : null
  } catch {
    return null
  }
}

// withoutComments is text less its `//` and `/* */` comments outside strings.
function withoutComments(text) {
  let out = ""
  for (let i = 0; i < text.length; i++) {
    const c = text[i]
    if (c === '"') {
      let j = i + 1
      while (j < text.length && text[j] !== '"') j += text[j] === "\\" ? 2 : 1
      out += text.slice(i, j + 1)
      i = j
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++
      out += "\n"
    } else if (c === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2)
      i = end < 0 ? text.length : end + 1
    } else {
      out += c
    }
  }
  return out
}

// realpath is dir resolved through its links, as given where it does not
// resolve.
function realpath(dir) {
  try {
    return fs.realpathSync(dir)
  } catch {
    return path.resolve(dir)
  }
}

// LAST, once every class above is defined: a class is not hoisted, and the
// run's Host would not yet exist where the mode is read.
if (mode === "skill") {
  await skill()
} else if (mode === "run") {
  process.exitCode = await run()
} else if (mode === "restore") {
  process.exitCode = restore()
} else if (mode === "guard") {
  process.exitCode = guard(process.argv.slice(3))
} else {
  process.stderr.write(`crewlet: qwen-helper.mjs has no mode ${JSON.stringify(mode)}\n`)
  process.exitCode = 2
}
