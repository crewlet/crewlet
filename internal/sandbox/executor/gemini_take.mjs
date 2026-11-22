// crewlet's Gemini CLI hook: the command both of a run's hooks run
// (geministeer.go), `node gemini-take.mjs <config>`. Plain node with no
// imports beyond its own: Gemini CLI needs node, so every box that runs it
// has one, and nothing is installed or fetched.
//
// AT AfterTool it reports a skill the model activated to the run's skill
// route. AT AfterAgent, at every finish, one continuing an earlier block
// included (0.70.0 honours each block), it takes the run's notes: no loop,
// since the bridge serves an item once and an empty take blocks nothing. One
// call of the bridge's unlisted steer_take tool over
// streamable HTTP (initialize, initialized, tools/call), whose text is the
// hook's whole answer, rendered by the engine in Gemini CLI's own JSON and
// printed as it came. Nothing here decides what a note is or whether it was read; the engine
// confirms a take by finding its text in the session.
//
// NOTHING FAILS THE RUN: Gemini CLI goes on whatever a hook does, and every
// failure here answers nothing, so the note waits for the next boundary.
import { createHash } from "node:crypto"
import fs from "node:fs"

// deadlineMs bounds the whole hook inside the ten seconds Gemini CLI gives it
// (geminiHookTimeout), leaving a second for node to start and print.
const deadlineMs = 9_000

// finishBoundary is the bridge's name for the boundary AfterAgent fires at:
// the main agent about to finish.
const finishBoundary = "Stop"

// takeField is the field of the engine's answer carrying the take's token
// (geminiTakeField): recorded beside the digest of the answer's text in
// the session's record the run names in CREWLET_GEMINI_TAKES, which its
// receipts are read against, and dropped from
// what Gemini CLI is handed, so it never reaches the model.
const takeField = "crewlet_take"

const signal = AbortSignal.timeout(deadlineMs)

let answer = ""
try {
  const config = JSON.parse(fs.readFileSync(process.argv[2], "utf8"))
  const input = JSON.parse(fs.readFileSync(0, "utf8"))
  answer = await hook(config, input)
} catch {
  answer = ""
}
process.stdout.write(answer === "" ? "{}" : answer)

// hook is the answer to one firing.
async function hook(config, input) {
  const event = input?.hook_event_name
  if (event === "AfterTool" && input.tool_name === "activate_skill" && config.skills) {
    await reportSkill(config, input)
  }
  if (event !== "AfterAgent" || !config.endpoint || !config.tool) return ""
  return recorded(config, await take(config, finishBoundary))
}

// recorded is answer as Gemini CLI is handed it, its take recorded first: a
// take recorded and then never delivered (the hook killed between) is told
// apart by the session, which never shows it.
function recorded(config, answer) {
  if (answer === "") return ""
  const parsed = JSON.parse(answer)
  const token = parsed[takeField]
  delete parsed[takeField]
  const takes = process.env.CREWLET_GEMINI_TAKES
  if (typeof token === "string" && token !== "" && takes && typeof parsed.reason === "string") {
    fs.appendFileSync(takes, JSON.stringify({ take: token, digest: itemDigest(parsed.reason) }) + "\n")
  }
  return JSON.stringify(parsed)
}

// reportSkill posts the activated skill to the skill route, by the name the
// engine handed it under: Gemini CLI lists `crewlet:x` as `crewlet-x`, and the
// run's skills are written so (geminiSkillName). A skill that is not the
// run's is posted too; the bridge knows it for none of the run's.
async function reportSkill(config, input) {
  const name = input.tool_input?.name
  if (typeof name !== "string" || name === "" || input.tool_response?.error) return
  const prefix = config.namespace + "-"
  const skill = name.startsWith(prefix) ? config.namespace + ":" + name.slice(prefix.length) : name
  try {
    await fetch(config.skills, {
      method: "POST",
      body: JSON.stringify({ skill }),
      headers: { "content-type": "application/json" },
      signal,
    })
  } catch {
    // Unreported: the model's next call to a covered tool is refused with the
    // skill to load, and that load is reported afresh.
  }
}

// take calls steer_take for boundary and is its text, "" for none.
async function take(config, boundary) {
  const headers = {
    ...(config.headers ?? {}),
    "content-type": "application/json",
    accept: "application/json, text/event-stream",
  }
  const init = await rpc(config.endpoint, headers, {
    jsonrpc: "2.0", id: 1, method: "initialize",
    params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "crewlet-gemini-take", version: "1" } },
  })
  if (init.session) headers["mcp-session-id"] = init.session
  try {
    await rpc(config.endpoint, headers, { jsonrpc: "2.0", method: "notifications/initialized" })
    const call = await rpc(config.endpoint, headers, {
      jsonrpc: "2.0", id: 2, method: "tools/call",
      params: { name: config.tool, arguments: { event: boundary, agent: "" } },
    })
    const content = call.message?.result?.content
    if (call.message?.result?.isError || !Array.isArray(content)) return ""
    return content.filter((part) => part?.type === "text").map((part) => part.text).join("")
  } finally {
    if (init.session) {
      await fetch(config.endpoint, { method: "DELETE", headers, signal }).catch(() => {})
    }
  }
}

// rpc posts one JSON-RPC message and reads its answer, a JSON body or the
// first `data:` line of an event stream; a notification's answer is empty.
async function rpc(endpoint, headers, body) {
  const response = await fetch(endpoint, { method: "POST", headers, body: JSON.stringify(body), signal })
  const text = await response.text()
  if (!response.ok) throw new Error(`the bridge answered ${response.status}`)
  const data = text.split("\n").find((line) => line.startsWith("data:"))
  const raw = data ? data.slice(5) : text
  return { session: response.headers.get("mcp-session-id"), message: raw.trim() === "" ? null : JSON.parse(raw) }
}

// itemDigest is text's digest as the engine takes one (executor.ItemDigest):
// SHA-256 of its UTF-8 bytes, in lowercase hex. The record keeps the digest,
// never the words: a note is sealed, and the session already holds its one
// copy, the one the model was handed.
function itemDigest(text) {
  return createHash("sha256").update(text, "utf8").digest("hex")
}
