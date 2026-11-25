// crewlet's Gemini CLI helper: what a run's script does around its harness
// that sh cannot do well (gemini.go, geminiScript). Plain node with no
// imports beyond its own: Gemini CLI needs node, so every box that runs it
// has one, and nothing is installed or fetched.
//
//   node gemini-helper.mjs pin <projects.json> <slug>
//     Pins the working directory's project slug before every run, so the run's
//     session is filed under tmp/<slug>/chats whatever the directory is called.
//     Gemini CLI keys the registry by the resolved working directory and drops
//     a slug whose .project_root names another directory (measured on 0.70.0:
//     a foreign one made the run exit 42), so a foreign one is removed and
//     Gemini CLI writes its own.
//
//   node gemini-helper.mjs ids <chats dir> <session id>
//     Lists the responses the session's chat file holds, an id a line: what a
//     continued session held as its run started.
//
//   node gemini-helper.mjs unshadowed <bridge>
//     Exits 0 only where no MCP server this box's Gemini CLI 0.70.0 could load
//     names a tool as it names the bridge's, mcp_<bridge>_<tool>, else 1 (a
//     reason a line on stdout): it spells a server S's tool T `mcp_S_T` with
//     every character outside [A-Za-z0-9_.:-] as `_`, a later registration
//     replacing the bridge's, so a server whose name sanitizes to `mcp`,
//     <bridge>, `mcp_<bridge>` or opens `<bridge>_` or `mcp_<bridge>_` may
//     answer the bridge's calls, and so may `mcp.serverCommand`'s server
//     `mcp`. It reads every source 0.70.0 loads a server from (both system
//     files at their variables' paths, the user's, the workspace's unless the
//     working directory is the home, each extension in the home, a link's at
//     its source), comments stripped as strip-json-comments strips them; one
//     it cannot read or parse fails, and so does a sign-in other than a key
//     or Vertex AI, whose remote admin controls add servers it cannot see, in
//     the user file (the override in its place) or any file 0.70.0 reads
//     after it. The user file's own <bridge> is the bridge.
//
//   node gemini-helper.mjs usage <chats dir> <session id> [<ids file>]
//     Prints one line of the run's stream: the tokens each request the chat
//     file records spent, but the responses the ids file names (an ids file
//     named and missing is a list that failed: the line says so and sums
//     nothing, never the whole session), split as
//     stream-json's result does not split its total (the reasoning, the tool
//     prompt; geministream.go's runSpend). 0.70.0 writes a response's entry
//     up to three times as it goes (its prose, its calls scheduled, its calls
//     run), each with the same id and counts, so the latest entry of each id
//     is the request's.
//
//   node gemini-helper.mjs settings <run's settings> <home's settings> <held copy>
//     Puts the run's settings into the home's own settings file, Gemini CLI's
//     user tier, the home's entries kept: its MCP servers beside the run's (one
//     by a run's name gives way), its hooks before the run's, and the run's
//     value of every other key over the home's. The home's own file is held
//     first, unless a held copy is there already (an earlier run's script
//     ended before it put the home's back), for the script to put back as it
//     ends; an empty copy stands for none. Exits 3, naming the file, where the
//     home's does not read as a JSON object, which Gemini CLI itself refuses
//     (exit 52, measured on 0.70.0).
//
//   node gemini-helper.mjs guard [--server <name>]... [--hook <name>] [--tool <name>]...
//                                [--auth <type>] [--turns <n>]
//     Exits 3, saying which file, key and entry, where a settings file or an
//     active home extension switches off one of Crewlet's own servers, its
//     hook or one of their tools (--tool, by its own name), or where a file Gemini CLI ranks above the home's (the
//     working directory's, the machine's system settings) replaces one of
//     those servers, switches hooks off, or sets another sign-in or round cap
//     than the run's (gemini.go's geminiGuard). A file that does not read is
//     passed over: Gemini CLI stops on it itself, naming it. So is a system
//     file Gemini CLI skips, one root does not keep (rootKept).
//
//   node gemini-helper.mjs shadows <bridge> [--own <name>]...
//     Exits 3, naming the file and the server, where a settings file or an
//     active home extension Gemini CLI 0.70.0 reads declares an MCP server
//     that would name a tool inside the bridge's names
//     (`mcp_<bridge>_<tool>`), or an extension's server takes its name; the
//     home's own settings may hold the run's servers (--own). Another server
//     of the bridge's name above the home's is the mode `guard`'s. The
//     sources are the mode `unshadowed`'s, read alike.
//
//   node gemini-helper.mjs bridged <bridge> <url sha-256> <marker>
//     The BeforeTool hook on every tool named inside the bridge's names:
//     stops the run, before the call runs, unless Gemini CLI says the
//     bridge's own server serves it, at the bridge's address, under the
//     bridge's own name for it; the reason is appended to the marker as a
//     line of the run's stream, which the run's script adds once Gemini CLI
//     exits (geministream.go's geminiUnbridgedEvent).
import crypto from "node:crypto"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"

const [mode, ...args] = process.argv.slice(2)

if (mode === "pin") {
  const [registry, slug] = args
  const cwd = path.resolve(process.cwd())
  let projects = {}
  try {
    const read = JSON.parse(fs.readFileSync(registry, "utf8"))
    if (read && typeof read.projects === "object" && read.projects !== null) projects = read.projects
  } catch {
    // None yet, or one that does not read: the pin replaces it.
  }
  for (const [dir, named] of Object.entries(projects)) {
    if (named === slug && dir !== cwd) delete projects[dir]
  }
  projects[cwd] = slug
  fs.mkdirSync(path.dirname(registry), { recursive: true })
  fs.writeFileSync(registry, JSON.stringify({ projects }))
  const owner = path.join(path.dirname(registry), "tmp", slug, ".project_root")
  try {
    if (fs.readFileSync(owner, "utf8").trim() !== cwd) fs.rmSync(owner)
  } catch {
    // No owner yet: Gemini CLI writes it.
  }
} else if (mode === "ids") {
  const [chats, session] = args
  const file = sessionFile(chats, session)
  if (file !== "") process.stdout.write([...responses(file).keys()].map((id) => id + "\n").join(""))
} else if (mode === "unshadowed") {
  const why = shadows(args[0])
  for (const reason of why) process.stdout.write("no: " + reason + "\n")
  process.exit(why.length === 0 ? 0 : 1)
} else if (mode === "usage") {
  const [chats, session, before] = args
  process.stdout.write(JSON.stringify(usage(chats, session, before)) + "\n")
} else if (mode === "settings") {
  const [own, live, held] = args
  try {
    install(own, live, held)
  } catch (err) {
    process.stderr.write(`crewlet: ${err.message}. The run was not started.\n`)
    process.exit(3)
  }
} else if (mode === "guard") {
  const named = { server: [], hook: [], tool: [], auth: [], turns: [], skill: [] }
  for (let i = 0; i + 1 < args.length; i += 2) named[args[i].replace(/^--/, "")]?.push(args[i + 1])
  const found = switchedOff(named)
  if (found.length > 0) {
    process.stderr.write("crewlet: Gemini CLI would start without what the run's notes, tools, sign-in or " +
      "round cap rest on: " + found.join("; ") + ". Gemini CLI adds each such list up across its settings " +
      "files and ranks the working directory's and the machine's settings above the home's, which hold the " +
      "run's, so no setting of the run's undoes it; remove the entry. The run was not started.\n")
    process.exit(3)
  }
} else if (mode === "shadows") {
  const [bridge, ...rest] = args
  const named = { own: [] }
  for (let i = 0; i + 1 < rest.length; i += 2) named[rest[i].replace(/^--/, "")]?.push(rest[i + 1])
  const found = shadowing(bridge, named)
  if (found.length > 0) {
    process.stderr.write(`crewlet: a repository's or home's MCP server may not take a name inside Crewlet's own ` +
      `server's tool names (mcp_${bridge}_<tool>), or its place: ${found.join("; ")}. Rename or remove it. ` +
      "The run was not started.\n")
    process.exit(3)
  }
} else if (mode === "bridged") {
  const [bridge, digest, marker] = args
  const why = unbridged(bridge, digest, fs.readFileSync(0, "utf8"))
  if (why !== "") {
    fs.appendFileSync(marker, JSON.stringify({ type: "crewlet_gemini_unbridged", message: why }) + "\n")
    process.stdout.write(JSON.stringify({ continue: false, stopReason: why }) + "\n")
  } else {
    process.stdout.write("{}\n")
  }
} else {
  process.stderr.write(`crewlet: gemini-helper.mjs has no mode ${JSON.stringify(mode)}\n`)
  process.exit(2)
}

// usage is the usage line for session: the summed tokens of its requests the
// file listed in before does not name (the responses a continued session held
// as the run started, one id a line, which an earlier job's run made), or why
// there are none to read.
function usage(chats, session, before) {
  const line = { type: "crewlet_gemini_usage", session_id: session }
  const file = sessionFile(chats, session)
  if (file === "") return { ...line, missing: "no chat file in " + chats + " names the session" }
  const requests = responses(file)
  if (before) {
    let earlier
    try {
      earlier = fs.readFileSync(before, "utf8")
    } catch {
      return { ...line, missing: "the responses the session held as the run started were not listed, " +
        "so the run's own are not told from the earlier jobs'" }
    }
    for (const id of earlier.split("\n")) requests.delete(id)
  }
  const tokens = { input: 0, output: 0, cached: 0, thoughts: 0, tool: 0 }
  for (const counts of requests.values()) {
    for (const key of Object.keys(tokens)) {
      if (Number.isSafeInteger(counts[key]) && counts[key] > 0) tokens[key] += counts[key]
    }
  }
  return { ...line, requests: requests.size, tokens }
}

// responses is each response the chat file file holds, by its id: the latest
// entry of each id's tokens.
function responses(file) {
  const requests = new Map()
  for (const text of fs.readFileSync(file, "utf8").split("\n")) {
    let entry
    try {
      entry = JSON.parse(text)
    } catch {
      continue // a torn last line, or none
    }
    if (entry?.type === "gemini" && typeof entry.id === "string" && entry.tokens) requests.set(entry.id, entry.tokens)
  }
  return requests
}

// sessionFile is the chat file naming session on its first line, "" for none.
// Gemini CLI names the file after the session's first eight characters, which
// two sessions may share, so the first line decides.
function sessionFile(chats, session) {
  let names = []
  try {
    names = fs.readdirSync(chats)
  } catch {
    return ""
  }
  const tail = "-" + session.slice(0, 8) + ".jsonl"
  for (const name of names.filter((n) => n.startsWith("session-") && n.endsWith(tail)).sort()) {
    const file = path.join(chats, name)
    try {
      const first = fs.readFileSync(file, "utf8").split("\n", 1)[0]
      if (JSON.parse(first).sessionId === session) return file
    } catch {
      // Not this session's.
    }
  }
  return ""
}

// install is the settings mode: own, the run's settings, merged over the
// home's own into live, the home's held at held first.
function install(own, live, held) {
  let home = ""
  if (fs.existsSync(held)) {
    home = fs.readFileSync(held, "utf8")
  } else {
    if (fs.existsSync(live)) home = fs.readFileSync(live, "utf8")
    if (fs.existsSync(live) && objectOf(home) === null) {
      throw new Error(`${live} does not read as a JSON object, so the run's settings cannot be added to it; ` +
        "Gemini CLI itself stops on it")
    }
    fs.mkdirSync(path.dirname(held), { recursive: true })
    fs.writeFileSync(held + ".part", home, { mode: 0o600 })
    fs.renameSync(held + ".part", held)
  }
  const run = objectOf(fs.readFileSync(own, "utf8"))
  if (run === null) throw new Error(`the run's settings, ${own}, do not read as a JSON object`)
  fs.mkdirSync(path.dirname(live), { recursive: true })
  fs.writeFileSync(live, JSON.stringify(over(objectOf(home) ?? {}, run), null, 2) + "\n", { mode: 0o600 })
}

// over is run's settings over home's, as Gemini CLI would rank a tier above
// the home's: MCP servers by name, hooks after the home's for each event, and
// every other key as [deep] merges it.
function over(home, run) {
  const out = deep(home, {})
  for (const [key, value] of Object.entries(run)) {
    if (key === "mcpServers" && isObject(home[key])) {
      out[key] = { ...home[key], ...value }
    } else if (key === "hooks" && isObject(home[key])) {
      out[key] = { ...home[key] }
      for (const [event, list] of Object.entries(value)) {
        out[key][event] = [...(Array.isArray(home[key][event]) ? home[key][event] : []), ...list]
      }
    } else {
      out[key] = deep(home[key], value)
    }
  }
  return out
}

// deep is value over base: two objects key by key, value's winning; else value.
function deep(base, value) {
  if (!isObject(value) || !isObject(base)) return value
  const out = { ...base }
  for (const [key, v] of Object.entries(value)) out[key] = deep(base[key], v)
  return out
}

// hooksOff reports whether 0.70.0 reads value of `hooksConfig.enabled` as
// false: false, 0, "" and "false" in any case (measured: "true", "no" and a
// missing or null value leave hooks on).
function hooksOff(value) {
  return value === false || value === 0 || value === "" || (typeof value === "string" && value.toLowerCase() === "false")
}

// switchedOff is what the settings files switch off or replace of named's
// servers, hook, sign-in and round cap, a sentence a file: the home's settings,
// which hold the run's, the working directory's unless it is the home (Gemini
// CLI reads it as no workspace there), and the system defaults and settings
// where Gemini CLI reads them ([rootKept]). `hooksConfig.enabled` is the one
// the highest tier setting it holds: system defaults, home, working
// directory, system settings (measured: a working directory's true over the
// home's false keeps hooks on; the run's settings no longer set it).
// Matched as 0.70.0 matches: a server or a skill in a list by its name (a
// skill's as listed) lower-cased and trimmed, in mcpServers by its key, a hook
// exactly, a tool as [excludes] reads; `skills.enabled` false lists no skill.
function switchedOff({ server, hook, tool, auth, turns, skill = [] }) {
  const home = os.homedir()
  const files = [{ file: path.join(home, ".gemini", "settings.json"), tier: 1 }]
  if (realpath(process.cwd()) !== realpath(home)) {
    files.push({ file: path.join(process.cwd(), ".gemini", "settings.json"), above: true, tier: 2 })
  }
  const defaults = process.env.GEMINI_CLI_SYSTEM_DEFAULTS_PATH || "/etc/gemini-cli/system-defaults.json"
  const system = process.env.GEMINI_CLI_SYSTEM_SETTINGS_PATH || "/etc/gemini-cli/settings.json"
  if (rootKept(defaults)) files.push({ file: defaults, tier: 0 })
  if (rootKept(system)) files.push({ file: system, above: true, tier: 3 })
  const fold = (name) => String(name).toLowerCase().trim()
  const list = (value) => (Array.isArray(value) ? value : [])
  const found = []
  let enabled = null
  let skills = null
  for (const { file, above, tier } of files) {
    const settings = readSettings(file)
    if (settings === null) continue
    const value = settings.hooksConfig?.enabled
    if (value !== undefined && value !== null && (enabled === null || tier >= enabled.tier)) {
      enabled = { file, tier, value }
    }
    const on = settings.skills?.enabled
    // A null sets it too, and so does a value it reads as false ("false", 0;
    // measured on 0.70.0: a working directory's null over a home's false listed
    // the skills, a home's "false" or 0 listed none).
    if (on !== undefined && (skills === null || tier >= skills.tier)) skills = { file, tier, value: on }
    const why = []
    for (const name of server) {
      if (list(settings.mcp?.excluded).some((entry) => fold(entry) === fold(name))) {
        why.push(`its mcp.excluded names the server ${name}`)
      }
      if (Array.isArray(settings.mcp?.allowed) && !settings.mcp.allowed.some((entry) => fold(entry) === fold(name))) {
        why.push(`its mcp.allowed leaves the server ${name} out`)
      }
      if (above && isObject(settings.mcpServers) && Object.hasOwn(settings.mcpServers, name)) {
        why.push(`its mcpServers.${name} replaces the run's server ${name}`)
      }
    }
    for (const entry of list(settings.tools?.exclude)) {
      if (excludes(entry, server, tool)) why.push(`its tools.exclude names the tool ${entry}`)
    }
    for (const name of skill) {
      if (list(settings.skills?.disabled).some((entry) => fold(entry) === fold(name))) {
        why.push(`its skills.disabled names the run's skill ${name}`)
      }
    }
    for (const name of hook) {
      if (list(settings.hooksConfig?.disabled).includes(name)) why.push(`its hooksConfig.disabled names the hook ${name}`)
    }
    const selected = settings.security?.auth?.selectedType
    for (const type of auth) {
      if (above && selected !== undefined && selected !== type) {
        why.push(`its security.auth.selectedType is ${JSON.stringify(selected)}, not the run's ${type}`)
      }
    }
    // A LOWER CAP ONLY ENDS THE RUN SOONER; 0.70.0 caps a session at a
    // value above 0 alone, so one at 0 or below lets the run go on uncapped.
    const cap = settings.model?.maxSessionTurns
    for (const n of turns) {
      if (above && typeof cap === "number" && (cap <= 0 || cap > Number(n))) {
        why.push(`its model.maxSessionTurns is ${JSON.stringify(cap)}, above the run's round cap ${n}`)
      }
    }
    if (why.length > 0) found.push(`${file}: ${why.join(", ")}`)
  }
  if (skill.length > 0 && skills !== null && hooksOff(skills.value)) {
    found.push(`${skills.file}: its skills.enabled is ${JSON.stringify(skills.value)}, which lists none of the ` +
      `run's skills ${skill.join(", ")}`)
  }
  if (hook.length > 0 && enabled !== null && hooksOff(enabled.value)) {
    found.push(`${enabled.file}: its hooksConfig.enabled is ${JSON.stringify(enabled.value)}, which switches every ` +
      `hook off, the run's ${hook.join(", ")} among them`)
  }
  for (const { file, excludeTools } of activeExtensions(home)) {
    const why = list(excludeTools).filter((entry) => excludes(entry, server, tool))
    if (why.length > 0) found.push(`${file}: its excludeTools names the tool ${why.join(", ")}`)
  }
  found.push(...policyDenies(home, server, tool))
  return found
}

// policyDenies is each tool of the run's servers a policy file of the home's
// (~/.gemini/policies, the run's own crewlet.toml aside) or the machine's
// (/etc/gemini-cli/policies, where root keeps it) denies, a sentence a tool:
// the matching rule of the highest priority, in a yolo run's mode, decides,
// and a deny leaves the call "not found" without a word (measured on 0.70.0:
// `toolName` "*", the qualified name, or `mcpName` with "*" or the tool's own
// name; a higher-priority allow after it, a rule for another mode or another
// server let it run). The run's admin-tier allow would override it, so the
// run is refused instead; an `ask_user` it does override, as a headless run
// cannot ask (measured: denied under yolo without it). Reimplemented, only as
// measured: 0.70.0 ships its policy engine in hashed bundle chunks alone. A
// rule it cannot read whole refuses only where it could reach a run's tool.
function policyDenies(home, server, tool) {
  const dirs = [path.join(home, ".gemini", "policies"), "/etc/gemini-cli/policies"]
  const rules = []
  const unread = []
  const skipped = []
  const each = (fn) => server.some((s) => tool.some((t) => fn(s, t, `mcp_${s}_${t}`)))
  for (const dir of dirs) {
    let names = []
    try {
      names = fs.readdirSync(dir).filter((n) => n.endsWith(".toml")).sort()
    } catch {}
    for (const name of names) {
      const file = path.join(dir, name)
      if (dir === dirs[0] && name === "crewlet.toml") continue
      if (dir === dirs[1] && !rootKept(file)) continue
      let text = ""
      try {
        text = fs.readFileSync(file, "utf8")
      } catch {
        continue
      }
      for (const rule of tomlRules(text)) {
        if (rule.unread.length === 0) {
          rules.push({ ...rule, file, tier: dir === dirs[0] ? 2 : 3 })
          continue
        }
        const why = rule.unread.map((w) => `${file}:${w}`).join("; ")
        if (each((s, t, qualified) => mayHit(rule, s, t, qualified))) unread.push(why)
        else skipped.push(why)
      }
    }
  }
  if (skipped.length > 0) {
    process.stderr.write("crewlet: Gemini CLI policy rules the guard cannot read whole were left out of its " +
      `check, as what it reads of each rules out every tool of the run's servers: ${skipped.join("; ")}\n`)
  }
  const found = []
  if (unread.length > 0) {
    found.push(`a policy rule the guard cannot read exactly, which Gemini CLI may read as a deny of the run's ` +
      `tools: ${unread.join("; ")}`)
  }
  for (const s of server) {
    for (const t of tool) {
      const qualified = `mcp_${s}_${t}`
      let best = null
      for (const r of rules) {
        if (Array.isArray(r.modes) && !r.modes.includes("yolo")) continue
        if (!hits(r.mcpName, r.toolName, s, t, qualified)) continue
        // RANKED BY TIER, THEN PRIORITY, as 0.70.0's transformPriority does
        // (tier + priority / 1000: the home's policies the user tier, 2, the
        // machine's the admin tier, 3; read from its bundle).
        const priority = Number.isFinite(r.priority) ? r.priority : 0
        const rank = r.tier + priority / 1000
        if (best === null || rank > best.rank || (rank === best.rank && r.decision === "deny")) {
          best = { ...r, priority, rank }
        }
      }
      if (best !== null && best.decision === "deny") {
        found.push(`${best.file}: its rule ${JSON.stringify({ toolName: best.toolName, mcpName: best.mcpName })} ` +
          `denies the tool ${qualified} (priority ${best.priority}), which the run never allows back over it`)
      }
    }
  }
  return found
}

// hits is whether a rule's mcpName and toolName (either undefined where the
// rule has none) match the tool t of the server s, qualified mcp_<s>_<t>.
function hits(mcpName, toolName, s, t, qualified) {
  const names = Array.isArray(toolName) ? toolName : [toolName]
  return mcpName !== undefined
    ? mcpName === s && names.some((n) => n === undefined || n === "*" || n === t || n === qualified)
    : names.some((n) => n === "*" || n === qualified || (typeof n === "string" && n.endsWith("*") &&
      qualified.startsWith(n.slice(0, -1))))
}

// mayHit is whether a rule read only in part could still deny the tool t of
// the server s in a yolo run. A key it read stands (TOML takes a key once):
// a decision other than deny, modes without yolo, or a mcpName or toolName
// naming other tools rules the tool out. A key whose value it could not
// read, or any key where a line's key is unreadable (lost), may hold any
// value; a rule with neither name may match every tool.
function mayHit(rule, s, t, qualified) {
  const unknown = (k) => rule.lost || rule.unknown.includes(k)
  if (rule.decision !== undefined && rule.decision !== "deny") return false
  if (rule.modes !== undefined && !(Array.isArray(rule.modes) && rule.modes.includes("yolo"))) return false
  if (rule.toolName === undefined && rule.mcpName === undefined && !unknown("toolName") && !unknown("mcpName")) {
    return true
  }
  const mcp = rule.mcpName !== undefined ? [rule.mcpName] : unknown("mcpName") ? [undefined, s] : [undefined]
  const toolName = rule.toolName !== undefined ? rule.toolName : unknown("toolName") ? "*" : undefined
  return mcp.some((m) => hits(m, toolName, s, t, qualified))
}

// tomlRules is each `[[rule]]` table of a policy file, as far as a rule
// needs: bare keys, basic and literal strings, integers, booleans and arrays
// of those, one line or several, with comments and a trailing comma. A line
// in a rule it cannot read exactly (a quoted key, `1_000`, a multi-line
// string, an inline table), or a rule with no decision or no toolName and no
// mcpName, is a reason in the rule's unread ("line N: ..."); its key goes in
// unknown, or lost is set where the key is unreadable too.
function tomlRules(text) {
  const rules = []
  let rule = null
  let pending = null
  const lines = text.split(/\r?\n/)
  const close = () => {
    if (rule === null) return
    if (rule.decision === undefined || (rule.toolName === undefined && rule.mcpName === undefined)) {
      rule.unread.push(`line ${rule.line}: a rule with no decision, or no toolName and no mcpName`)
    }
  }
  for (const [i, raw] of lines.entries()) {
    const line = pending === null ? raw.trim() : pending.text + "\n" + raw.trim()
    if (pending === null && (line === "" || line.startsWith("#"))) continue
    if (pending === null && /^\[\[\s*rule\s*\]\]\s*(#.*)?$/.test(line)) {
      close()
      rule = { line: i + 1, unread: [], unknown: [], lost: false }
      rules.push(rule)
      continue
    }
    if (pending === null && line.startsWith("[")) {
      close()
      rule = null
      continue
    }
    if (rule === null) continue
    const m = /^([A-Za-z0-9_-]+)[ \t]*=[ \t]*([\s\S]*)$/.exec(line)
    if (m === null) {
      rule.unread.push(`line ${i + 1}: ${JSON.stringify(raw.trim().slice(0, 80))}`)
      rule.lost = true
      pending = null
      continue
    }
    const value = tomlValue(m[2])
    if (value === undefined) {
      if (arrayOpen(m[2]) && i + 1 < lines.length) {
        pending = { text: line, line: pending?.line ?? i + 1 }
        continue
      }
      const at = pending?.line ?? i + 1
      rule.unknown.push(m[1])
      rule.unread.push(arrayOpen(m[2]) ? `line ${at}: an array never closed`
        : `line ${at}: ${JSON.stringify(line.slice(0, 80))}`)
      pending = null
      continue
    }
    pending = null
    rule[m[1]] = value
  }
  close()
  return rules
}

// arrayOpen is whether text opens an array it does not close, its strings
// and comments aside: the value goes on to the next line.
function arrayOpen(text) {
  let depth = 0
  for (let at = 0; at < text.length; at++) {
    const c = text[at]
    if (c === "#") {
      const end = text.indexOf("\n", at)
      if (end < 0) break
      at = end
    } else if (c === '"' || c === "'") {
      at++
      while (at < text.length && text[at] !== c && text[at] !== "\n") at += c === '"' && text[at] === "\\" ? 2 : 1
    } else if (c === "[") {
      depth++
    } else if (c === "]") {
      depth--
    }
  }
  return depth > 0
}

// tomlValue is text, a TOML value with an optional comment after it, as a
// basic or literal string, an integer, a boolean or an array of those (over
// lines, with comments and a trailing comma); undefined where it is anything
// else.
function tomlValue(text) {
  let at = 0
  const space = (lines) => {
    for (;;) {
      while (at < text.length && (text[at] === " " || text[at] === "\t" || (lines && text[at] === "\n"))) at++
      if (!lines || text[at] !== "#") return
      const end = text.indexOf("\n", at)
      at = end < 0 ? text.length : end
    }
  }
  const one = () => {
    space(true)
    if (text[at] === '"') {
      let end = at + 1
      while (end < text.length && text[end] !== '"' && text[end] !== "\n") end += text[end] === "\\" ? 2 : 1
      if (end >= text.length || text[end] !== '"' || text.startsWith('"""', at)) return undefined
      try {
        const v = JSON.parse(text.slice(at, end + 1))
        at = end + 1
        return v
      } catch {
        return undefined
      }
    }
    if (text[at] === "'") {
      const end = text.indexOf("'", at + 1)
      if (end < 0 || text.slice(at, end).includes("\n") || text.startsWith("\'\'\'", at)) return undefined
      const v = text.slice(at + 1, end)
      at = end + 1
      return v
    }
    const m = /^(-?[0-9]+|true|false)(?=[\s,\]#]|$)/.exec(text.slice(at))
    if (m === null) return undefined
    at += m[1].length
    return m[1] === "true" ? true : m[1] === "false" ? false : Number(m[1])
  }
  let value
  space(false)
  if (text[at] === "[") {
    at++
    value = []
    for (;;) {
      space(true)
      if (text[at] === "]") {
        at++
        break
      }
      const v = one()
      if (v === undefined) return undefined
      value.push(v)
      space(true)
      if (text[at] === ",") {
        at++
        continue
      }
      if (text[at] !== "]") return undefined
      at++
      break
    }
  } else {
    value = one()
    if (value === undefined) return undefined
  }
  space(false)
  return at === text.length || text[at] === "#" ? value : undefined
}

// excludes reports whether entry, a tools.exclude or an extension's
// excludeTools entry, takes a tool of the run's own servers away as 0.70.0's
// ToolRegistry.isActiveTool reads it: the tool's qualified name
// (`mcp_<server>_<tool>`), its own name, or `DiscoveredMCPTool`, the class
// every MCP tool is (measured: each hides the bridge's tool from the request).
function excludes(entry, server, tool) {
  if (typeof entry !== "string") return false
  return entry === "DiscoveredMCPTool" || tool.includes(entry) ||
    server.some((name) => entry.startsWith(`mcp_${name}_`))
}

// activeExtensions is each extension under the home's ~/.gemini/extensions
// (a link followed) that 0.70.0 loads for the working directory, its manifest
// and excludeTools: every one, but one extension-enablement.json disables
// there, the last of its rules matching the directory deciding (Gemini CLI's
// ExtensionEnablementManager.isEnabled). A manifest that does not read is
// passed over, as Gemini CLI passes it over.
function activeExtensions(home) {
  const dir = path.join(home, ".gemini", "extensions")
  let entries = []
  try {
    entries = fs.readdirSync(dir)
  } catch {
    return []
  }
  const out = []
  for (const entry of entries.sort()) {
    const file = path.join(dir, entry, "gemini-extension.json")
    const manifest = readSettings(file)
    if (manifest === null || typeof manifest.name !== "string") continue
    if (extensionEnabled(home, manifest.name)) out.push({ file, excludeTools: manifest.excludeTools })
  }
  return out
}

// extensionEnabled reports whether the home's extension-enablement.json leaves
// the extension name on for the working directory: the last of its rules
// matching the directory decides, and none leaves it on.
function extensionEnabled(home, name) {
  const enablement = readSettings(path.join(home, ".gemini", "extensions", "extension-enablement.json")) ?? {}
  const here = slashed(process.cwd())
  let enabled = true
  for (const rule of enablement[name]?.overrides ?? []) {
    if (typeof rule !== "string") continue
    const disable = rule.startsWith("!")
    let base = disable ? rule.slice(1) : rule
    const subdirs = base.endsWith("*")
    base = subdirs ? base.slice(0, -1) : base
    const glob = `${base}${subdirs ? "*" : ""}`
    const pattern = glob.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/(\/?)\*/g, "($1.*)?")
    if (new RegExp("^" + pattern + "$").test(here)) enabled = !disable
  }
  return enabled
}

// slashed is dir as Gemini CLI matches an enablement rule against it, with a
// leading and a trailing slash.
function slashed(dir) {
  let out = dir.replace(/\\/g, "/")
  if (!out.startsWith("/")) out = "/" + out
  if (!out.endsWith("/")) out += "/"
  return out
}

// rootKept reports whether Gemini CLI 0.70.0 reads the system file at file
// rather than skip it with a warning: the file and every directory above it,
// by the path named and by the one it resolves to, owned by root and writable
// by no group or other, a link on the way owned by root too.
function rootKept(file) {
  const kept = (p) => {
    const link = fs.lstatSync(p)
    const s = fs.statSync(p)
    return (!link.isSymbolicLink() || link.uid === 0) && s.uid === 0 && (s.mode & 0o22) === 0
  }
  const up = (p) => {
    const out = [p]
    for (let d = path.dirname(p); ; d = path.dirname(d)) {
      out.push(d)
      if (d === path.dirname(d)) return out
    }
  }
  try {
    const named = path.resolve(file)
    return [...up(named), ...up(fs.realpathSync(named))].every(kept)
  } catch {
    return false
  }
}

// isObject reports whether value is a JSON object.
function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value)
}

// objectOf is the settings object text holds, its comments stripped as Gemini
// CLI strips them; null for none.
function objectOf(text) {
  try {
    const settings = JSON.parse(stripComments(text))
    return isObject(settings) ? settings : null
  } catch {
    return null
  }
}

// readSettings is the settings object file holds ([objectOf]); null for none,
// or one that does not read as one.
function readSettings(file) {
  try {
    return objectOf(fs.readFileSync(file, "utf8"))
  } catch {
    return null
  }
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

// settingsSources is every source Gemini CLI 0.70.0 loads a server from,
// each read as it reads them: both system files at their variables' paths,
// the user's, the workspace's unless the working directory is the home, and
// each extension in the home (a link's at its source). A source is its file,
// its role and its settings, undefined where it is absent or does not read;
// why is each one present that did not read.
function settingsSources() {
  const why = []
  const home = process.env.GEMINI_CLI_HOME || os.homedir()
  const read = (file) => {
    let text
    try {
      text = fs.readFileSync(file, "utf8")
    } catch (e) {
      if (e.code !== "ENOENT") why.push("unreadable " + file)
      return undefined
    }
    try {
      const v = JSON.parse(stripComments(text))
      return v && typeof v === "object" && !Array.isArray(v) ? v : {}
    } catch {
      why.push("unparsable " + file)
      return undefined
    }
  }
  const source = (file, role) => ({ file, role, settings: read(file) })
  const sysFile = process.env.GEMINI_CLI_SYSTEM_SETTINGS_PATH || "/etc/gemini-cli/settings.json"
  const sources = [
    source(sysFile, "system"),
    source(process.env.GEMINI_CLI_SYSTEM_DEFAULTS_PATH || path.join(path.dirname(sysFile), "system-defaults.json"),
      "defaults"),
    source(path.join(home, ".gemini", "settings.json"), "user"),
  ]
  if (realpath(process.cwd()) !== realpath(home)) {
    sources.push(source(path.join(process.cwd(), ".gemini", "settings.json"), "workspace"))
  }
  const extensions = path.join(home, ".gemini", "extensions")
  let dirs = []
  try {
    dirs = fs.readdirSync(extensions)
  } catch (e) {
    if (e.code !== "ENOENT") why.push("unreadable " + extensions)
  }
  for (const d of dirs) {
    if (d.endsWith(".lock")) continue
    const dir = path.join(extensions, d)
    try {
      if (!fs.statSync(dir).isDirectory()) continue
    } catch {
      why.push("unreadable " + dir)
      continue
    }
    const install = read(path.join(dir, ".gemini-extension-install.json"))
    const from = install && install.type === "link" && typeof install.source === "string" ? install.source : dir
    sources.push(source(path.join(from, "gemini-extension.json"), "extension"))
  }
  return { sources, why }
}

// mcpServers is the servers settings declares by name, null where its
// mcpServers is no object.
function mcpServers(settings) {
  const m = settings?.mcpServers
  if (m === undefined) return {}
  return m === null || typeof m !== "object" || Array.isArray(m) ? null : m
}

// shadows is every reason a server Gemini CLI 0.70.0 could load may name a
// tool as it names bridge's, empty where none can (the mode `unshadowed`).
function shadows(bridge) {
  if (!bridge) return ["no bridge named"]
  const { sources, why } = settingsSources()
  const spells = (key) => {
    const s = key.replace(/[^A-Za-z0-9_.:-]/g, "_")
    return s === "mcp" || s === bridge || s.startsWith(bridge + "_") || s === "mcp_" + bridge ||
      s.startsWith("mcp_" + bridge + "_")
  }
  for (const { file, role, settings } of sources) {
    if (!settings) continue
    const m = mcpServers(settings)
    if (m === null) {
      why.push(file + ": mcpServers is no object")
      continue
    }
    for (const key of Object.keys(m)) {
      if (spells(key) && !(role === "user" && key === bridge)) why.push(file + ": server " + JSON.stringify(key))
    }
    if (settings.mcp && typeof settings.mcp === "object" && settings.mcp.serverCommand) {
      why.push(file + ": mcp.serverCommand starts a server \"mcp\"")
    }
  }
  // THE SIGN-IN, WHICHEVER LATER FILES 0.70.0 SKIPS: it merges system
  // defaults < the user's (the override in its place) < the workspace's < the
  // system's, and skips a system file that is not root-owned (measured) and an
  // untrusted workspace, so the user's must name one and every later one that
  // names one counts. The defaults sit below the user's and never decide.
  const pick = (role) => sources.find((s) => s.role === role)?.settings?.security?.auth?.selectedType
  const signIns = [process.env.GEMINI_CLI_AUTH_OVERRIDE || pick("user")]
  for (const role of ["workspace", "system"]) if (pick(role) !== undefined) signIns.push(pick(role))
  for (const auth of signIns) {
    if (auth !== "gemini-api-key" && auth !== "vertex-ai") {
      why.push("sign-in " + JSON.stringify(auth ?? null) + ": a login's remote admin controls can add servers")
    }
  }
  return why
}

// shadowing is each server a settings file or an active home extension
// declares under a name inside bridge's tool names, a sentence a server (the
// mode `shadows`). Gemini CLI 0.70.0 names a server's tool `<server>_<tool>`
// if that opens `mcp_`, else `mcp_<server>_<tool>`, writing each character
// outside `A-Za-z0-9_.:-` as `_`, so: a name that opens `<bridge>_`, or
// `mcp_<bridge>` or one opening `mcp_<bridge>_`, and an extension's own
// <bridge>. Another <bridge> in a file ranked above the home's, which holds
// the run's, is the switch-off guard's ([switchedOff]). A file that does not
// read is passed over, as Gemini CLI reads it as empty, and so are a system
// file it skips ([rootKept]) and an extension off here ([extensionEnabled]).
// A server `mcp` names a tool there only by what it lists, which only the
// run's BeforeTool hook sees ([unbridged]).
function shadowing(bridge, { own }) {
  const spelled = (name) => name.replace(/[^A-Za-z0-9_.:-]/g, "_")
  const home = process.env.GEMINI_CLI_HOME || os.homedir()
  const found = []
  for (const { file, role, settings } of settingsSources().sources) {
    if ((role === "system" || role === "defaults") && !rootKept(file)) continue
    if (role === "extension" && (typeof settings?.name !== "string" || !extensionEnabled(home, settings.name))) continue
    for (const name of Object.keys(mcpServers(settings) ?? {})) {
      if (role === "user" && own.includes(name)) continue
      const s = spelled(name)
      if ((role === "extension" && name === bridge) || s.startsWith(bridge + "_") || s === "mcp_" + bridge ||
        s.startsWith("mcp_" + bridge + "_")) {
        found.push(`${file} declares the server ${JSON.stringify(name)} under mcpServers`)
      }
    }
  }
  return found
}

// unbridged is why the call a BeforeTool hook input (text) describes is not
// the bridge's, "" where it is: Gemini CLI hands the hook the server that
// serves the call as it resolved it (mcp_context, from its live servers), so
// another server of the bridge's name, or one whose tool took a name of the
// bridge's, or an admin's server no file shows, is seen as it is called. The
// address is matched by its digest, so the bridge's token is on no command line.
function unbridged(bridge, digest, text) {
  let input
  try {
    input = JSON.parse(text)
  } catch {
    return "the hook that checks a call to Crewlet's own server was handed no call it could read"
  }
  const name = String(input?.tool_name ?? "")
  const served = input?.mcp_context
  if (!name.startsWith(`mcp_${bridge}_`) && served?.server_name !== bridge) return ""
  const url = typeof served?.url === "string" ? served.url : ""
  if (served?.server_name !== bridge || served.command != null ||
    crypto.createHash("sha256").update(url).digest("hex") !== digest || name !== `mcp_${bridge}_${served.tool_name}`) {
    const by = served ? `the server ${JSON.stringify(String(served.server_name))}` : "a server no settings file names"
    return `Gemini CLI would run ${JSON.stringify(name)}, a name of Crewlet's own server ${JSON.stringify(bridge)}, ` +
      `on ${by}: a repository's or home's MCP server may not take a name inside Crewlet's own server's tool ` +
      "names, or its place. Rename or remove it. The call was not run."
  }
  return ""
}

// stripComments is text with its `//` and `/* */` comments outside strings
// removed, as strip-json-comments, which 0.70.0 reads its settings through,
// removes them.
function stripComments(text) {
  let out = ""
  let inString = false
  for (let i = 0; i < text.length; ) {
    const c = text[i]
    if (inString) {
      out += c
      if (c === "\\") {
        out += text[i + 1] ?? ""
        i += 2
        continue
      }
      if (c === '"') inString = false
      i++
      continue
    }
    if (c === '"') {
      inString = true
      out += c
      i++
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++
    } else if (c === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2)
      i = end < 0 ? text.length : end + 2
    } else {
      out += c
      i++
    }
  }
  return out
}
