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
// (origin, token; the server binds 127.0.0.1) within startBoundMs; the guard
// asked of Kimi's own view of the run's MCP servers (guard); the session
// created under the run's agent profile in permission mode `auto`; its id
// reported ONCE, written beside and moved over the report, before any prompt
// (the seam's sessionMinter); the session's events subscribed; the brief
// posted in a request body, so no argv or process table holds it; then waited
// on with `:wait` until the session is idle. Its events reach stdout, a line
// each, for the live view and the decoder (kimievents.go); the driver's own
// lines are `crewlet.*`.
//
// THE GUARD. The workspace is trusted, as a person trusts their own, so a
// repository's MCP servers and permission rules load over the home's, and
// Kimi has no layer above them (2.7.2: the start directory's
// `.kimi-code/mcp.json` over its git root's `.mcp.json` over the home's
// `mcp.json`; every deny rule decided before any allow). A repository could
// so displace the run's own servers: one of its servers by their name, a file
// Kimi cannot read (no server loads at all), or a rule denying their tools.
// The first two end the run before its session is made, the third as Kimi
// denies a call, each naming what the repository holds (measured on 2.7.2).
// The home's config.toml and plugins hold the rest, which no layer the run
// writes undoes: a `[tools]` list leaving the run's tools inactive refuses the
// run before the brief (tools); a hook that keeps one of them from running
// (PreToolUse), or keeps the brief or a note from the model
// (UserPromptSubmit), ends it as Kimi reports that (denial, blocked), each
// naming the file and entry Kimi itself lists (hooksFor).
//
// HOW A NOTE ARRIVES. While the session works, the notes route is polled and
// each note posted to `/steer` under a prompt_id made of its inbox id
// (notePrefix), which KAP keeps as the message's id and answers, reused, with
// the prompt it holds (40927 where they conflict), so a note admitted twice
// lands once. Kimi hands it over as a user message opening a turn after the
// running tool's result; one sent while the model answers cuts that request
// short, and at idle a steer starts a turn (measured on 2.7.2). THIS DRIVER
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
import { createHash, randomUUID } from "node:crypto"
import fs from "node:fs"
import path from "node:path"

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

// toolsPollMs paces the look at the session's tools while the run's servers
// connect, which Kimi itself bounds by each server's startupTimeoutMs
// (settings.serverStartMs, kimi.go); toolsSlackMs is the time past that bound
// before the run goes on without them, as Kimi's session would.
const toolsPollMs = 100
const toolsSlackMs = 1_000

// endBoundMs bounds each step of an asked end — the cancel, then the server's
// stop — within the five seconds the script gives the driver (JobStopGrace).
const endBoundMs = 1_500

// stopBoundMs bounds the server's stop at the run's own end.
const stopBoundMs = 10_000

// notePrefix opens every prompt_id a note is steered under, and briefPrefix the
// brief's, made the run's own: a continued session already holds an earlier
// run's brief, and KAP answers a reused prompt_id with the prompt it holds
// (kimisteer.go, kimiNotePrefix).
const notePrefix = "crewlet-note-"
const briefPrefix = "crewlet-brief-"

// webTools is Kimi's own web tools, posted with the brief and every steered
// note as the session's disabled_tools on a web-denied run, which every agent
// of the session is held to, a subagent included; any other run posts none,
// so a session a denied run carried here gets its web back (kimi.go,
// kimiWebTools).
const webTools = ["FetchURL", "WebSearch"]

// disabledTools is the session's disabled_tools for this run ([webTools]).
function disabledTools() {
  return settings.webDenied ? webTools : []
}

// bindBoundMs bounds the wait for a fresh session's bound profile on disk, as a
// continued session's instructions are renewed from it (renew): written as the
// session is created (measured on 2.7.2), so ten seconds is room.
const bindBoundMs = 10_000

// kept is every event type the stream keeps: a turn's start, step and end, an
// item's start and end, the context's size and a notice. Deltas and the
// session's bookkeeping frames are left out, as nothing reads them, and so is
// a subscription's `turns` page, which on a continued session repeats an
// earlier run's turns (measured on 2.7.2).
const kept = new Set(["turn.started", "turn.updated", "turn.ended", "item.started", "item.ended", "context", "notice"])

const settings = JSON.parse(fs.readFileSync(process.argv[2], "utf8"))
// mode is `rekey <session> <dir>`, run before the run to file a session under
// this run's working directory; `resume <session> <bind|brief>`, a run that
// continues it; or none, a run that starts a session of its own.
const [mode, continued, dir] = process.argv.slice(3)
if (mode === "rekey") process.exit(rekey(continued, dir))
const life = new AbortController()
let server = null
let api = null
let sid = ""
let asked = false
let socket = null
let ending = false
// refused is why the guard ends the run, "" while it may go on, or a hook's
// block (denial), named as the run ends (hookBlock).
let refused = ""
// ran is every tool call Kimi reported running: an executing call's item is
// updated with its `fields` before it ends, and one a hook vetoed never is
// (measured on 2.7.2: the bridge's error, its transport failing, a hook that
// allows). ours is every prompt this run posted, brief and notes.
const ran = new Set()
const ours = new Set()

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
// refusal is a guard's end of the run: said on the stream as well, where the
// decoder reads it as the run's failure (kimievents.go), since a turn the
// guard cut short may already hold an answer.
const refusal = (why) => {
  say({ type: "crewlet.refused", why })
  return fail(why)
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
  const unguarded = await guard()
  if (unguarded !== "") return refusal(unguarded)

  const config = { permission_mode: "auto", profile: settings.profile }
  if (settings.model) config.model = settings.model
  if (settings.thinking) config.thinking = settings.thinking
  if (mode === "resume") {
    // A CONTINUED SESSION REPORTS NOTHING: its id is the one the run was
    // started on, and the seam refuses any other.
    const renewed = await renew(continued, config)
    if (renewed !== "") return fail("the continued session's standing instructions could not be renewed: " + renewed)
    const opened = await call("GET", `/api/v1/sessions/${continued}`)
    if (!opened.ok) return fail(`Kimi could not open the continued session ${continued}: ` + opened.why)
    sid = continued
  } else {
    const created = await call("POST", "/api/v1/sessions", { cwd: process.cwd(), agent_config: config })
    if (!created.ok || typeof created.data?.id !== "string") return fail("Kimi refused the session: " + created.why)
    sid = created.data.id
    // THE ID REPORTED ONCE, BEFORE THE BRIEF: a reader finds the whole id or none.
    fs.writeFileSync(settings.reportStaging, sid + "\n")
    fs.renameSync(settings.reportStaging, settings.report)
  }
  if (asked) return 143
  const inactive = await tools()
  if (inactive !== "") return refusal(inactive)
  if (asked) return 143

  await subscribe()
  const briefID = briefPrefix + randomUUID()
  ours.add(briefID)
  const posted = await call("POST", `/api/v1/sessions/${sid}/prompts`, { prompt_id: briefID, content: [{ type: "text", text: brief }], disabled_tools: disabledTools() })
  if (refused !== "") return refuse()
  if (!posted.ok) return fail("Kimi refused the brief: " + posted.why)

  const notes = typeof settings.notes === "string" && settings.notes !== "" ? poller(settings.notes) : null
  notes?.open()
  for (;;) {
    if (asked) return end()
    if (refused !== "") return refuse()
    const waited = await call("POST", `/api/v1/sessions/${sid}:wait`, { timeout_ms: waitMs }, waitBoundMs)
    if (asked) return end()
    if (refused !== "") return refuse()
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
    const stopped = await blocked()
    if (stopped !== "") return refusal(stopped)
    return session.last_turn_outcome === "completed" ? 0 : 1
  }
}

// guard holds the run's own MCP servers to the file the run wrote them in,
// as Kimi itself reports where each server it loads for this directory comes
// from: "" where every one does, else why not. A server of the repository's by
// one of their names replaces it, and a repository file Kimi cannot read
// leaves no server loaded, its error naming the file.
async function guard() {
  const servers = settings.servers ?? []
  if (servers.length === 0) return ""
  const listed = await call("GET", "/api/v1/mcp/servers?cwd=" + encodeURIComponent(process.cwd()))
  if (!listed.ok || !Array.isArray(listed.data)) return "Kimi could not load the MCP servers the run is served by: " + listed.why
  for (const name of servers) {
    const found = listed.data.find((s) => s?.name === name)
    if (found === undefined) return `Kimi did not load the MCP server ${name}, which the run is served by`
    if (found.origin !== settings.mcpConfig) return `${found.origin} names an MCP server ${name}, which Kimi loads over the one the run is served by; rename it there`
  }
  return ""
}

// tools is why the session would offer the model none of some tool of the
// run's own servers, "" for none: Kimi lists each tool of the session with
// whether its tool policy leaves it active, and the run's profile and the
// session's disabled tools leave every MCP tool so, which leaves the home's
// config.toml `[tools]` (measured on 2.7.2: `disabled` naming one, `enabled`
// leaving it out). Asked until every server lists a tool or Kimi's own bound
// for their start has passed.
async function tools() {
  const servers = settings.servers ?? []
  if (servers.length === 0) return ""
  const own = (name) => typeof name === "string" && servers.some((s) => name.startsWith(`mcp__${s}__`))
  const until = Date.now() + (settings.serverStartMs ?? 0) + toolsSlackMs
  let mine = []
  for (;;) {
    const listed = await call("GET", "/api/v1/tools?session_id=" + encodeURIComponent(sid))
    if (!listed.ok) return "Kimi could not list the session's tools: " + listed.why
    mine = (listed.data?.tools ?? []).filter((t) => own(t?.name))
    if (servers.every((s) => mine.some((t) => t.name.startsWith(`mcp__${s}__`))) || Date.now() >= until || asked) break
    await sleep(toolsPollMs, life.signal)
  }
  const off = mine.filter((t) => t.active === false).map((t) => t.name)
  if (off.length === 0) return ""
  const config = await call("GET", "/api/v1/config")
  const policy = (config.ok ? config.data?.tools : undefined) ?? {}
  const says = []
  const disabled = (policy.disabled ?? []).filter((p) => typeof p === "string" && p.startsWith("mcp__"))
  if (disabled.length > 0) says.push(`its [tools] disabled lists ${disabled.map((p) => JSON.stringify(p)).join(", ")}`)
  if ((policy.enabled ?? []).length > 0) says.push(`its [tools] enabled lists only ${policy.enabled.map((p) => JSON.stringify(p)).join(", ")}`)
  const where = says.length > 0 ? `${path.join(home(), "config.toml")}: ${says.join("; ")}` : "Kimi's tool policy"
  return `Kimi would offer the model none of the run's own tools ${off.join(", ")}: ${where}; it must leave them active`
}

// denial is why a session event ends the run, "" for none: Kimi denying a call
// of one of the run's own servers' tools by a permission rule, which no rule
// the run could add overrides; or a call of one Kimi ended unrun (ran), a
// hook's veto, named as the run ends (hookBlock).
function denial(event) {
  const item = event?.item
  if (event?.type !== "item.ended" || item?.type !== "tool_call" || item.outcome !== "failed") return ""
  if (typeof item.name !== "string" || typeof item.error !== "string") return ""
  if (!(settings.servers ?? []).some((name) => item.name.startsWith(`mcp__${name}__`))) return ""
  if (item.error.includes("denied by permission rule")) return `Kimi denied the run's own tool ${item.name} by a permission rule (${item.error}); a deny rule matching it, as a repository's .kimi-code/local.toml may hold, must not name it`
  if (ran.has(item.item_id)) return ""
  return { event: "PreToolUse", value: item.name, what: `Kimi did not run the run's own tool ${item.name}`, says: item.error }
}

// blocked is why a prompt of this run never reached the model, "" for none:
// Kimi's record of the main agent's turns holds a turn of one of the run's
// prompts that ended `blocked` before any step, as a UserPromptSubmit hook
// blocking it ends one (measured on 2.7.2, the brief's and a note's alike).
async function blocked() {
  const page = await call("GET", `/api/v1/sessions/${sid}/agents/main/turns?page_size=200`)
  for (const turn of page.ok ? page.data?.turns ?? [] : []) {
    if (turn?.end_reason !== "blocked" || (turn.steps ?? []).length > 0) continue
    const prompt = (turn.items ?? []).find((i) => i?.type === "user_message" && ours.has(i.item_id))
    if (prompt === undefined) continue
    const said = (turn.items ?? []).find((i) => i?.type === "assistant_message")?.text ?? ""
    const what = prompt.item_id.startsWith(briefPrefix) ? "the run's brief" : `the note ${prompt.item_id.slice(notePrefix.length)}`
    return hookBlock({ event: "UserPromptSubmit", value: prompt.text ?? "", what: `Kimi kept ${what} from the model`, says: said.replace(/<\/?hook_result[^>]*>/g, "").trim() })
  }
  return ""
}

// hookBlock is why a hook's block ends the run: what it kept, what it said,
// and each hook Kimi lists for that event whose matcher matches (hooksFor).
async function hookBlock(block) {
  const sources = await hooksFor(block.event, block.value)
  const where = sources.length > 0 ? sources.join("; ") : `no ${block.event} hook Kimi lists matches it`
  return `${block.what}: a ${block.event} hook blocked it (${block.says}); ${where}; such a hook must not block the run's own tools, brief or notes`
}

// hooksFor is each hook of event whose matcher matches value, as Kimi lists
// them: config.toml's (`GET /api/v1/config`) and each installed plugin's, off
// the manifest Kimi names for it (`GET /api/v1/plugins/{id}`). A matcher is
// matched as Kimi matches it: none matches all, else a RegExp tested on value.
async function hooksFor(event, value) {
  const matches = (m) => {
    if (typeof m !== "string" || m === "") return true
    try {
      return new RegExp(m).test(value)
    } catch {
      return false
    }
  }
  const entry = (h) => `event ${event}, matcher ${JSON.stringify(h.matcher ?? "")}, command ${JSON.stringify(h.command ?? "")}`
  const out = []
  const config = await call("GET", "/api/v1/config", undefined, callBoundMs, true)
  for (const h of config.ok ? config.data?.hooks ?? [] : []) {
    if (h?.event === event && matches(h.matcher)) out.push(`${path.join(home(), "config.toml")}: its [[hooks]] entry ${entry(h)}`)
  }
  const plugins = await call("GET", "/api/v1/plugins", undefined, callBoundMs, true)
  for (const p of plugins.ok ? plugins.data?.plugins ?? [] : []) {
    if (!(p?.hook_count > 0) || p.enabled === false) continue
    const info = await call("GET", "/api/v1/plugins/" + encodeURIComponent(p.id), undefined, callBoundMs, true)
    const manifest = info.ok ? info.data?.manifest_path : undefined
    let hooks
    try {
      hooks = JSON.parse(fs.readFileSync(manifest, "utf8")).hooks
    } catch {}
    if (!Array.isArray(hooks)) {
      out.push(`the plugin ${p.id} (${manifest ?? "its manifest"}) holds ${p.hook_count} hooks`)
      continue
    }
    for (const h of hooks) if (h?.event === event && matches(h.matcher)) out.push(`${manifest} (the plugin ${p.id}): its hooks entry ${entry(h)}`)
  }
  return out
}

// refuse ends a run the guard stopped: its turn cancelled, and why said.
async function refuse() {
  await end()
  return refusal(typeof refused === "string" ? refused : await hookBlock(refused))
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

// renew renews the standing instructions bound into the continued session
// session, which Kimi renders once, as it creates a session, and restores on a
// resume (profile.bind): a throwaway session is created with this run's profile
// in this run's directory, its bind read off its record and appended to the
// continued session's, and the throwaway deleted. A resume renders from the
// record's last bind (measured on 2.7.2). "" once renewed, else why.
async function renew(session, config) {
  const made = await call("POST", "/api/v1/sessions", { cwd: process.cwd(), agent_config: config })
  if (!made.ok || typeof made.data?.id !== "string" || typeof made.data?.workspace_id !== "string") return "Kimi refused the session it renders them in: " + made.why
  const wire = path.join(home(), "sessions", made.data.workspace_id, made.data.id, "agents", "main", "wire.jsonl")
  let bind = ""
  for (const until = Date.now() + bindBoundMs; bind === "" && Date.now() < until && !asked; ) {
    try {
      bind = fs.readFileSync(wire, "utf8").split("\n").find((l) => l.startsWith('{"type":"profile.bind"')) ?? ""
    } catch {}
    if (bind === "") await sleep(100, life.signal)
  }
  const deleted = await call("POST", `/api/v1/sessions/${made.data.id}:delete`, {})
  if (bind === "") return `the session it renders them in bound no profile within ${bindBoundMs / 1000} seconds`
  if (!deleted.ok) return "the session it renders them in could not be deleted: " + deleted.why
  try {
    fs.appendFileSync(path.join(sessionDir(session), "agents", "main", "wire.jsonl"), bind + "\n")
  } catch (error) {
    return "the continued session's record could not be written: " + error.message
  }
  return ""
}

// rekey files the session's directory dir under this run's working directory,
// as Kimi files a session it creates there (`wd_<name>_<hash>`, workDirKey),
// and records that directory as its cwd: a continued session works in the cwd
// its state.json names, and a box's path differs from the one it was written in
// (measured on 2.7.2). 0 once filed, else 1, saying why.
function rekey(session, dir) {
  try {
    const dest = sessionDir(session)
    if (path.resolve(dir) !== dest) {
      fs.mkdirSync(path.dirname(dest), { recursive: true })
      fs.rmSync(dest, { recursive: true, force: true })
      fs.renameSync(dir, dest)
    }
    const file = path.join(dest, "state.json")
    const state = JSON.parse(fs.readFileSync(file, "utf8"))
    if (state === null || typeof state !== "object" || state.id !== session) throw new Error("its state.json names another session")
    state.cwd = process.cwd()
    fs.writeFileSync(file + ".next", JSON.stringify(state))
    fs.renameSync(file + ".next", file)
    return 0
  } catch (error) {
    process.stderr.write("crewlet: the session could not be filed under this run's directory: " + error.message + "\n")
    return 1
  }
}

// home is the run's KIMI_CODE_HOME, which the wrapper pins.
function home() {
  return process.env.KIMI_CODE_HOME || path.join(process.env.HOME ?? "", ".kimi-code")
}

// sessionDir is where Kimi files session for this run's working directory.
function sessionDir(session) {
  return path.join(home(), "sessions", workDirKey(process.cwd()), session)
}

// workDirKey is Kimi's own name for a working directory's sessions
// (encodeWorkDirKey in 2.7.2's bundle): `wd_`, the directory's
// name made a slug of at most 40 characters, and 12 hex digits of SHA-256 of
// its path.
function workDirKey(dir) {
  const normalized = dir.replaceAll("\\", "/").replace(/\/+$/, "")
  let slug = (normalized.split("/").pop() ?? normalized).toLowerCase().replaceAll(/[^a-z0-9._-]+/g, "-").replaceAll(/^-+|-+$/g, "").slice(0, 40).replaceAll(/^-+|-+$/g, "")
  if (slug === "" || slug === "." || slug === "..") slug = "workspace"
  return "wd_" + slug + "_" + createHash("sha256").update(normalized).digest("hex").slice(0, 12)
}

// subscribe streams the session's events to stdout, again after a drop while
// the run goes on.
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
        let event
        try {
          event = JSON.parse(text)
        } catch {
          return
        }
        if (kept.has(event?.type)) process.stdout.write(text.replace(/\n/g, " ") + "\n")
        if (event?.type === "item.updated" && event.set?.fields !== undefined) ran.add(event.item_id)
        const why = refused === "" ? denial(event) : ""
        if (why !== "") {
          refused = why
          life.abort()
        }
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
        ours.add(notePrefix + note.inbox)
        const steered = await call("POST", `/api/v1/sessions/${sid}/steer`, { prompt_id: notePrefix + note.inbox, content: [{ type: "text", text: note.message }], disabled_tools: disabledTools() })
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
    const end = () => (done(), resolve())
    // An ended signal fires no listener added later (opencode_steer.js's).
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
