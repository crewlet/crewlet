// A stand-in for `qwen --acp` that replays Qwen Code 0.28.4's own ACP
// messages, captured on the build VM (testdata/qwen/0.28.4/acp-*.jsonl), to
// the helper the suite drives (qwen_helper_test.go). Each capture line is the
// probe's log of one message: `dir` out (the probe's) or in-* (Qwen Code's).
// The stand-in answers each request of the helper's with the capture's answer
// to the same method, and on session/prompt replays the turn: every
// notification as captured, every request Qwen Code made of its host (the
// mid-turn drain) sent again and its answer awaited, and the prompt's answer
// last. The capture's session id is replaced with the one the helper creates.
//
// Driven by its environment:
//   QWEN_FAKE_START   the capture answering initialize, session/new and the
//                     MCP status, in order (the bridge disconnected, then
//                     connected)
//   QWEN_FAKE_TURN    the capture whose turn session/prompt replays
//   QWEN_FAKE_HOME    the home Qwen Code records sessions under
//   QWEN_FAKE_LOG     where the stand-in writes what it saw, a JSON line each,
//                     its argv first
//   QWEN_FAKE_NEVER_CONNECT  the MCP status never says connected
//   QWEN_FAKE_AWAIT_NOTES    asks again at a drain until it has been handed
//                     notes at that many drains (1 for any set value but a
//                     count), as later tool batches would
//   QWEN_FAKE_DIE_AFTER_NOTE exits at once after it is handed a note, before
//                     recording it
//   QWEN_FAKE_CHAT_SEED a chat file of Qwen Code's laid in place before the
//                     prompt is answered
//   QWEN_FAKE_COMPACT a file of one chat_compression record of Qwen Code's,
//                     recorded between the notes' records and the response
//                     after them
//   QWEN_FAKE_FAIL_AFTER_NOTE fails the request that carries the notes of
//                     that many-th drain to hand any (1 for any set value but a
//                     count) once they are recorded: no reply is recorded,
//                     and the prompt is answered with an error
//
// Like Qwen Code it records a drained note in the session's chat file as a
// `mid_turn_user_message` record, the captured record's shape with the note's
// text, at once, BEFORE the request that carries it (as 0.28.4 does); the
// reply to that request, the captured assistant record, only once the turn
// goes on; and exits once its stdin closes.
import fs from "node:fs"
import path from "node:path"

const env = process.env
const log = (entry) => fs.appendFileSync(env.QWEN_FAKE_LOG, JSON.stringify(entry) + "\n")
const capture = (file) => fs.readFileSync(file, "utf8").split("\n").filter((l) => l !== "").map((l) => JSON.parse(l))

log({ argv: process.argv.slice(2) })
const start = capture(env.QWEN_FAKE_START)
const turn = capture(env.QWEN_FAKE_TURN)
const chatLines = fs.readFileSync(path.join(path.dirname(env.QWEN_FAKE_TURN), "chat-note.jsonl"), "utf8").split("\n")
const chatTemplate = JSON.parse(chatLines.find((l) => l.includes('"mid_turn_user_message"')))
const replyTemplate = JSON.parse(chatLines.findLast((l) => l.includes('"type":"assistant"')))

for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) {
  process.on(sig, () => {
    log({ signal: sig })
    process.exit(128 + 15)
  })
}

// answersTo is every captured answer to method, in order, from capture.
function answersTo(capture, method) {
  const ids = new Set(capture.filter((m) => m.dir === "out" && m.method === method).map((m) => m.id))
  return capture.filter((m) => m.dir === "in-resp" && ids.has(m.id))
}
const sessionOf = (capture) => answersTo(capture, "session/new")[0]?.result?.sessionId ?? ""

let session = ""
// handed is how many drains have handed notes; count reads a variable that
// names a number of drains, 1 for any other set value.
let handed = 0
const count = (value) => (Number.parseInt(value ?? "", 10) > 0 ? Number.parseInt(value, 10) : 1)
const replace = (text, from) => (from === "" ? text : text.split(from).join(session))
const strip = (m) => {
  const { dir, ...message } = m
  return message
}

const status = answersTo(start, "qwen/status/workspace/mcp")
const disconnected = status.find((m) => JSON.stringify(m).includes('"disconnected"'))
const connected = status.find((m) => JSON.stringify(m).includes('"mcpStatus":"connected"'))
let statusAsks = 0

let nextID = 1000
const waiting = new Map()
let cancelled = null
const cancelArrived = new Promise((resolve) => (cancelled = resolve))

function send(message) { process.stdout.write(JSON.stringify(message) + "\n") }
function emit(m, from) { process.stdout.write(replace(JSON.stringify(strip(m)), from) + "\n") }
function answer(id, captured, from) {
  const { dir, id: _, ...rest } = captured
  send(JSON.parse(replace(JSON.stringify({ ...rest, id }), from)))
}
function ask(method, params) {
  const id = nextID++
  const asked = Date.now()
  return new Promise((resolve) => {
    waiting.set(id, (m) => resolve({ m, ms: Date.now() - asked }))
    send({ jsonrpc: "2.0", id, method, params })
  })
}

function chatFile() {
  const key = process.cwd().replace(/[^a-zA-Z0-9]/g, "-")
  return path.join(env.QWEN_FAKE_HOME, "projects", key, "chats", session + ".jsonl")
}
function record(notes) {
  const lines = notes.map((note) => JSON.stringify({ ...chatTemplate, sessionId: session, message: { role: "user",
    parts: [{ text: "\n[User message received during tool execution]: " + note }] },
  systemPayload: { displayText: note } }))
  if (env.QWEN_FAKE_COMPACT) lines.push(fs.readFileSync(env.QWEN_FAKE_COMPACT, "utf8").trim())
  fs.mkdirSync(path.dirname(chatFile()), { recursive: true })
  fs.appendFileSync(chatFile(), lines.join("\n") + "\n")
}
// reply records the response to the request that carried the notes recorded
// last, as the turn goes on past it.
function reply() {
  fs.appendFileSync(chatFile(), JSON.stringify({ ...replyTemplate, sessionId: session }) + "\n")
}
let owedReply = false

async function replay(promptID) {
  const from = sessionOf(turn)
  const opened = turn.findIndex((m) => m.dir === "out" && m.method === "session/prompt")
  const promptCaptured = turn[opened].id
  for (const m of turn.slice(opened + 1)) {
    if (m.dir === "in-note") {
      if (owedReply) reply()
      owedReply = false
      emit(m, from)
    } else if (m.dir === "in-req") {
      for (let tries = 0; ; tries++) {
        const { m: got, ms } = await ask(m.method, JSON.parse(replace(JSON.stringify(m.params), from)))
        const notes = got?.result?.messages ?? []
        log({ drain: notes, ms })
        if (notes.length > 0) {
          handed++
          if (env.QWEN_FAKE_DIE_AFTER_NOTE) process.exit(1)
          record(notes)
          if (env.QWEN_FAKE_FAIL_AFTER_NOTE && handed === count(env.QWEN_FAKE_FAIL_AFTER_NOTE)) {
            send({ jsonrpc: "2.0", id: promptID, error: { code: -32603, message: "Internal error" } })
            return
          }
          owedReply = true
          break
        }
        if (!env.QWEN_FAKE_AWAIT_NOTES || handed >= count(env.QWEN_FAKE_AWAIT_NOTES) || tries >= 200) break
        await new Promise((resolve) => setTimeout(resolve, 50))
      }
    } else if (m.dir === "out" && m.method === "session/cancel") {
      log({ holding: true })
      await cancelArrived
    } else if (m.dir === "in-resp" && m.id === promptCaptured) {
      if (env.QWEN_FAKE_CHAT_SEED) {
        fs.mkdirSync(path.dirname(chatFile()), { recursive: true })
        fs.appendFileSync(chatFile(), replace(fs.readFileSync(env.QWEN_FAKE_CHAT_SEED, "utf8"), from))
      }
      if (owedReply) reply()
      owedReply = false
      answer(promptID, m, from)
      return
    }
  }
}

let buf = ""
process.stdin.setEncoding("utf8")
process.stdin.on("data", (chunk) => {
  buf += chunk
  for (let at = buf.indexOf("\n"); at >= 0; at = buf.indexOf("\n")) {
    const line = buf.slice(0, at)
    buf = buf.slice(at + 1)
    const m = JSON.parse(line)
    if (m.method === undefined) {
      waiting.get(m.id)?.(m)
      waiting.delete(m.id)
      continue
    }
    log({ method: m.method })
    switch (m.method) {
      case "initialize":
        answer(m.id, answersTo(start, "initialize")[0], "")
        break
      case "session/new":
        session = m.params?._meta?.["qwen-code/sessionId"] ?? ""
        answer(m.id, answersTo(start, "session/new")[0], sessionOf(start))
        break
      case "qwen/status/workspace/mcp":
        statusAsks++
        answer(m.id, env.QWEN_FAKE_NEVER_CONNECT || statusAsks === 1 ? disconnected : connected, sessionOf(start))
        break
      case "qwen/status/session/context_usage":
        answer(m.id, answersTo(turn, m.method)[0] ?? answersTo(capture(path.join(path.dirname(env.QWEN_FAKE_TURN),
          "acp-note.jsonl")), m.method)[0], sessionOf(turn))
        break
      case "session/prompt":
        replay(m.id)
        break
      case "session/cancel":
        cancelled()
        break
      default:
        send({ jsonrpc: "2.0", id: m.id, error: { code: -32601, message: "Method not found" } })
    }
  }
})
process.stdin.on("end", () => {
  log({ stdin: "closed" })
  process.exit(0)
})
