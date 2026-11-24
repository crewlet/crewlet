// crewlet: the extension every Pi run loads with `-e`, written into the run's
// box with the run's own settings in place of the `config` placeholder below
// (pi.go, Pi.WriteConfig; pisteer.go, piExtension). It imports Node's own
// `node:path` and `node:fs` alone, runs in the pi process, and writes no file.
//
// WHAT IT DOES, each from the settings it is written with:
//   - registers the seat's MCP servers (the tool bridge's), declared to the
//     model by name (`exposure: "direct"`), and at the first turn fails the run
//     LOUDLY where one it must hold offers no tool: pi starts a run whose server
//     is unreachable or refuses its token silently, with the tools absent; or
//     where an mcp.json entry (the home's or the repository's) shares one's
//     name, which pi would use in its place; and before every request where
//     a settings file's codemode mode leaves its tools out of the request;
//   - adds the seat's standing instructions as a prompt section of its own,
//     after pi's own prompt;
//   - holds the run to its turn cap: every turn_start is one model request,
//     and the one past the cap is aborted before its request is sent;
//   - polls the notes route while the agent works and hands every note it
//     admitted to the run at pi's own step boundaries (turn_end, and
//     agent_before_settle as the run is about to finish) as a custom message
//     carrying the note's inbox id, which pi records and never sends; the
//     runner confirms a note off that record (pitranscript.go), so this posts
//     no read;
//   - reports each load of one of the run's own skills (a `read` of its
//     SKILL.md) to the skill route, awaited before the model sees the file;
//   - records the context pi reports at each turn's end.
// What it records for the runner is a `custom` session entry (pi.appendEntry),
// which pi prints on its JSON stream and never sends to the model.
//
// Bounded, paced and refused as opencode_steer.js: every request is bounded,
// a failure waits a second doubling to thirty, an empty answer is never
// followed sooner than a second after its poll began, and a 401 or 404 ends
// that route's requests for the rest of the run.

import { readFileSync } from "node:fs"
import { resolve } from "node:path"

// config is the run's, written over the placeholder by the runner; null in
// the file as embedded, where the extension does nothing.
const config = /*crewlet:config*/ null

// The custom types this extension records and hands over (pisteer.go).
const noteType = "crewlet-note"
const armedType = "crewlet-armed"
const capType = "crewlet-cap"
const bridgeType = "crewlet-bridge"
const contextType = "crewlet-context"

// The prompt section the seat's standing instructions are added under.
const section = "crewlet"

// pollAbortMs bounds one poll: the route holds an empty one ten seconds
// (internal/api/mcpbridge, steerHold), and the same again is room for a relay.
const pollAbortMs = 20_000
// reportAbortMs bounds one skill report, which the model's next request waits on.
const reportAbortMs = 5_000
const backoffFirstMs = 1_000
const backoffMostMs = 30_000
const pollFloorMs = backoffFirstMs

export default function (pi) {
  if (config === null || typeof config !== "object") return
  arm(pi, config)
}

function arm(pi, { servers, required, system, notes, cap, skills, skillURL, listed, mcpFiles, settingsFiles }) {
  for (const s of Array.isArray(servers) ? servers : []) pi.registerMcpServer(s.name, s.config)
  const polling = typeof notes === "string" && notes !== ""
  const capped = Number.isSafeInteger(cap) && cap > 0
  const skillFiles = skills !== null && typeof skills === "object" ? skills : {}
  const reporting = typeof skillURL === "string" && skillURL !== ""

  // IN MEMORY ALONE, which holds because this process is the run's: a note
  // admitted and not yet handed over is lost with it, and the route serves an
  // unconfirmed note again to the next run.
  const life = new AbortController()
  let gate = null
  let cursor = 0
  const waiting = []
  const admitted = new Set()
  let turns = 0
  let armed = false
  let checked = false
  let stopped = false
  let skillsRefused = false

  const open = () => {
    if (!polling || gate !== null || life.signal.aborted || stopped) return
    gate = new AbortController()
    quietly(poll(gate.signal))
  }
  const close = () => {
    if (gate === null) return
    gate.abort()
    gate = null
  }

  const poll = async (gated) => {
    let wait = 0
    while (!gated.aborted && !life.signal.aborted) {
      const began = Date.now()
      const past = cursor
      const answer = await call("GET", notes + "/notes?after=" + cursor, undefined, pollAbortMs, life.signal, gated)
      if (answer.refused) {
        life.abort()
        return
      }
      if (answer.ok && admit(answer.text)) {
        wait = 0
        const early = pollFloorMs - (Date.now() - began)
        if (cursor === past && early > 0) await sleep(early, life.signal, gated)
        continue
      }
      if (gated.aborted || life.signal.aborted) return
      wait = wait === 0 ? backoffFirstMs : Math.min(wait * 2, backoffMostMs)
      await sleep(wait, life.signal, gated)
    }
  }

  // admit keeps each note of one answer, oldest first, moving the cursor past
  // it: false for an answer that is no list of well-formed notes.
  const admit = (text) => {
    let list
    try {
      list = JSON.parse(text)?.notes
    } catch {
      return false
    }
    if (!Array.isArray(list)) return false
    for (const note of list) {
      if (!wellFormed(note)) return false
      if (note.seq <= cursor) continue
      if (!admitted.has(note.inbox)) {
        admitted.add(note.inbox)
        waiting.push(note)
      }
      cursor = Math.max(cursor, note.seq)
    }
    return true
  }

  // spent is whether the run has made every model request its cap allows: a
  // note handed over then would need one more, which the cap refuses.
  const spent = () => capped && turns >= cap

  // handOver is every admitted note as the entries a boundary appends.
  const handOver = () =>
    waiting.splice(0).map((note) => ({
      type: "custom_message",
      customType: noteType,
      content: note.message,
      display: false,
      details: { inbox: note.inbox },
    }))

  // stop ends the run here: the entry that says why, a line in its error
  // stream, then the abort, which no request follows.
  const stop = (ctx, type, data, line) => {
    stopped = true
    close()
    pi.appendEntry(type, data)
    process.stderr.write("crewlet: " + line + "\n")
    ctx.abort()
  }

  pi.on("before_agent_start", (event) => {
    if (typeof system === "string" && system !== "") event.systemPromptOptions.sections[section] = system
    if (!armed) {
      armed = true
      pi.appendEntry(armedType, {})
    }
  })
  pi.on("agent_start", () => open())
  pi.on("agent_settled", () => close())
  pi.on("session_shutdown", () => {
    life.abort()
    close()
  })

  pi.on("turn_start", (_event, ctx) => {
    turns++
    if (!checked) {
      // THE SERVERS CONNECT BEFORE THE FIRST TURN (pi waits for them), so a
      // server that must hold tools and offers none is dead or refused us.
      checked = true
      const redefined = redefinitions(servers, mcpFiles)
      if (redefined.length > 0) {
        stop(ctx, bridgeType, { redefined }, "the seat's tools never reached this run: " + redefined.join("; ") +
          ", which pi uses in place of the server the run registers under that name; rename that entry")
        return
      }
      const names = pi.getAllTools().map((t) => t.name)
      const missing = (Array.isArray(required) ? required : []).filter(
        (server) => !names.some((n) => n.startsWith("mcp__" + server + "__")),
      )
      if (missing.length > 0) {
        stop(ctx, bridgeType, { missing }, "the seat's tools never reached this run: no tool of " +
          missing.join(", ") + " was offered, so its MCP server was unreachable or refused the run's token")
        return
      }
      const unlisted = listed === true ? unlistedBy(pi, settingsFiles) : ""
      if (unlisted !== "") {
        stop(ctx, bridgeType, { unlisted }, "the seat's skills never reached the model: " + unlisted)
        return
      }
    }
    const hidden = hiddenBy(pi, servers, settingsFiles)
    if (hidden !== "") {
      stop(ctx, bridgeType, { hidden }, "the seat's tools never reached the model: " + hidden)
      return
    }
    if (capped && turns > cap) stop(ctx, capType, { turns: cap }, "this run reached its turn cap of " + cap +
      " model turns and was stopped")
  })

  pi.on("turn_end", (event, ctx) => {
    const entries = [...event.entries]
    const usage = ctx.getContextUsage()
    if (usage && Number.isFinite(usage.contextWindow) && usage.contextWindow > 0) {
      entries.push({ type: "custom", customType: contextType,
        data: { tokens: Number.isFinite(usage.tokens) ? usage.tokens : null, contextWindow: usage.contextWindow } })
    }
    if (event.outcome !== "completed" || stopped || spent() || waiting.length === 0) return { entries }
    return { entries: [...entries, ...handOver()], continue: true }
  })

  pi.on("agent_before_settle", (event) => {
    if (stopped || spent() || waiting.length === 0) return
    return { entries: [...event.entries, ...handOver()], continue: true }
  })

  pi.on("tool_result", async (event, ctx) => {
    if (!reporting || skillsRefused || event.toolName !== "read" || event.isError) return
    const path = event.input?.path
    if (typeof path !== "string" || path === "") return
    const listed = skillFiles[resolve(ctx.cwd, path)]
    if (typeof listed !== "string") return
    const answer = await call("POST", skillURL, JSON.stringify({ skill: listed }), reportAbortMs, life.signal)
    if (answer.refused) skillsRefused = true
  })
}

// redefinitions is every entry of files (read now, as pi reads them at the
// session's start) whose namespace is one of servers': pi 1.6.0 uses an
// mcp.json entry in place of a registered server of the same namespace, `-`
// and `_` alike. A file that is missing or holds no JSON object defines none.
function redefinitions(servers, files) {
  const space = (name) => "mcp__" + String(name).replace(/-/g, "_")
  const ours = new Set((Array.isArray(servers) ? servers : []).map((s) => space(s.name)))
  const out = []
  for (const file of Array.isArray(files) ? files : []) {
    if (typeof file !== "string" || file === "") continue
    const path = resolve(process.cwd(), file)
    let defined
    try {
      defined = JSON.parse(readFileSync(path, "utf8").replace(/^\uFEFF/, ""))?.mcpServers
    } catch {
      continue
    }
    if (defined === null || typeof defined !== "object") continue
    for (const name of Object.keys(defined)) {
      if (ours.has(space(name))) out.push(JSON.stringify(name) + " is defined again in " + path)
    }
  }
  return out
}

// hiddenBy says why pi leaves the servers' tools out of the next request, ""
// where it does not. Pi 1.6.0's codemode, while active with `codemode.mode`
// "only" in the merged settings, declares itself alone and hides every active
// `direct` tool, the servers' own (measured; codemode/tool.js). Checked at
// every turn, since codemode also turns on once an mcp.json server it serves
// connects. The file named is the last of files (home, then repository, as pi
// merges them) to set the mode.
function hiddenBy(pi, servers, files) {
  const spaces = (Array.isArray(servers) ? servers : []).map((s) => "mcp__" + String(s.name).replace(/-/g, "_") + "__")
  const active = pi.getActiveTools()
  if (!active.includes("codemode") || !active.some((n) => spaces.some((space) => n.startsWith(space)))) return ""
  if (pi.getSettings()?.codemode?.mode !== "only") return ""
  let from = "pi's settings"
  for (const file of Array.isArray(files) ? files : []) {
    if (typeof file !== "string" || file === "") continue
    const path = resolve(process.cwd(), file)
    try {
      const mode = JSON.parse(readFileSync(path, "utf8").replace(/^\uFEFF/, ""))?.codemode?.mode
      if (mode !== undefined) from = path
    } catch {}
  }
  return "codemode.mode \"only\" in " + from + " has pi declare only its codemode tool while codemode is active, " +
    "leaving every other tool out of the model's requests; set it to \"on\" or remove it"
}

// unlistedBy says why pi lists none of the run's skills, "" where it lists
// them. Pi 1.6.0 lists skills only while read or bash is among its tools, and
// takes its tools from the merged `defaultTools`, a plain list replacing the
// inherited one, a list of +name and -name entries changing it (measured:
// neither read nor bash dropped the whole listing, either alone kept it). The
// files named are each that sets the key, home then repository, with its value.
function unlistedBy(pi, files) {
  const active = pi.getActiveTools()
  if (active.includes("read") || active.includes("bash")) return ""
  const set = []
  for (const file of Array.isArray(files) ? files : []) {
    if (typeof file !== "string" || file === "") continue
    const path = resolve(process.cwd(), file)
    try {
      const tools = JSON.parse(readFileSync(path, "utf8").replace(/^\uFEFF/, ""))?.defaultTools
      if (tools !== undefined) set.push(path + " sets defaultTools to " + JSON.stringify(tools))
    } catch {}
  }
  if (set.length === 0) return ""
  return set.join(", and ") + ", which leaves both read and bash out of pi's tools, so pi lists none of the " +
    "run's skills; keep read or bash in it, or remove the key"
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

// call makes one request, bounded by ms and ended early by any of signals:
// refused for a 401 or 404, ok with the text for another success.
async function call(method, target, body, ms, ...signals) {
  const { signal, done } = deadline(ms, ...signals)
  try {
    const init = { method, signal }
    if (body !== undefined) {
      init.body = body
      init.headers = { "content-type": "application/json" }
    }
    const response = await fetch(target, init)
    const text = await response.text()
    if (response.status === 401 || response.status === 404) return { refused: true }
    return { ok: response.ok, text }
  } catch {
    return {}
  } finally {
    done()
  }
}

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
