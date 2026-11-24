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
  const named = { server: [], hook: [], tool: [], auth: [], turns: [] }
  for (let i = 0; i + 1 < args.length; i += 2) named[args[i].replace(/^--/, "")]?.push(args[i + 1])
  const found = switchedOff(named)
  if (found.length > 0) {
    process.stderr.write("crewlet: Gemini CLI would start without what the run's notes, tools, sign-in or " +
      "round cap rest on: " + found.join("; ") + ". Gemini CLI adds each such list up across its settings " +
      "files and ranks the working directory's and the machine's settings above the home's, which hold the " +
      "run's, so no setting of the run's undoes it; remove the entry. The run was not started.\n")
    process.exit(3)
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

// switchedOff is what the settings files switch off or replace of named's
// servers, hook, sign-in and round cap, a sentence a file: the home's settings,
// which hold the run's, the working directory's unless it is the home (Gemini
// CLI reads it as no workspace there), and the system defaults and settings
// where Gemini CLI reads them ([rootKept]).
// Matched as 0.70.0 matches: a server in a list by its name lower-cased and
// trimmed, in mcpServers by its key, a hook exactly, a tool as [excludes] reads.
function switchedOff({ server, hook, tool, auth, turns }) {
  const home = os.homedir()
  const files = [{ file: path.join(home, ".gemini", "settings.json") }]
  if (realpath(process.cwd()) !== realpath(home)) {
    files.push({ file: path.join(process.cwd(), ".gemini", "settings.json"), above: true })
  }
  const defaults = process.env.GEMINI_CLI_SYSTEM_DEFAULTS_PATH || "/etc/gemini-cli/system-defaults.json"
  const system = process.env.GEMINI_CLI_SYSTEM_SETTINGS_PATH || "/etc/gemini-cli/settings.json"
  if (rootKept(defaults)) files.push({ file: defaults })
  if (rootKept(system)) files.push({ file: system, above: true })
  const fold = (name) => String(name).toLowerCase().trim()
  const list = (value) => (Array.isArray(value) ? value : [])
  const found = []
  for (const { file, above } of files) {
    const settings = readSettings(file)
    if (settings === null) continue
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
    for (const name of hook) {
      if (list(settings.hooksConfig?.disabled).includes(name)) why.push(`its hooksConfig.disabled names the hook ${name}`)
      if (above && settings.hooksConfig?.enabled === false) {
        why.push(`its hooksConfig.enabled is false, which switches the hook ${name} off`)
      }
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
  for (const { file, excludeTools } of activeExtensions(home)) {
    const why = list(excludeTools).filter((entry) => excludes(entry, server, tool))
    if (why.length > 0) found.push(`${file}: its excludeTools names the tool ${why.join(", ")}`)
  }
  return found
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
  const enablement = readSettings(path.join(dir, "extension-enablement.json")) ?? {}
  const here = slashed(process.cwd())
  const out = []
  for (const entry of entries.sort()) {
    const file = path.join(dir, entry, "gemini-extension.json")
    const manifest = readSettings(file)
    if (manifest === null || typeof manifest.name !== "string") continue
    let enabled = true
    for (const rule of enablement[manifest.name]?.overrides ?? []) {
      if (typeof rule !== "string") continue
      const disable = rule.startsWith("!")
      let base = disable ? rule.slice(1) : rule
      const subdirs = base.endsWith("*")
      base = subdirs ? base.slice(0, -1) : base
      const glob = `${base}${subdirs ? "*" : ""}`
      const pattern = glob.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/(\/?)\*/g, "($1.*)?")
      if (new RegExp("^" + pattern + "$").test(here)) enabled = !disable
    }
    if (enabled) out.push({ file, excludeTools: manifest.excludeTools })
  }
  return out
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
    const settings = JSON.parse(withoutComments(text))
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

// shadows is every reason a server Gemini CLI 0.70.0 could load may name a
// tool as it names bridge's, empty where none can (the mode `unshadowed`).
function shadows(bridge) {
  const why = []
  if (!bridge) return ["no bridge named"]
  const home = process.env.GEMINI_CLI_HOME || os.homedir()
  const userFile = path.join(home, ".gemini", "settings.json")
  const sysFile = process.env.GEMINI_CLI_SYSTEM_SETTINGS_PATH || "/etc/gemini-cli/settings.json"
  const sysDefFile = process.env.GEMINI_CLI_SYSTEM_DEFAULTS_PATH ||
    path.join(path.dirname(sysFile), "system-defaults.json")
  const workFile = path.join(process.cwd(), ".gemini", "settings.json")
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
  const spells = (key) => {
    const s = key.replace(/[^A-Za-z0-9_.:-]/g, "_")
    return s === "mcp" || s === bridge || s.startsWith(bridge + "_") || s === "mcp_" + bridge ||
      s.startsWith("mcp_" + bridge + "_")
  }
  const servers = (where, v, own) => {
    if (!v) return
    const m = v.mcpServers
    if (m !== undefined && (m === null || typeof m !== "object" || Array.isArray(m))) {
      why.push(where + ": mcpServers is no object")
      return
    }
    for (const key of Object.keys(m ?? {})) {
      if (spells(key) && !(own && key === bridge)) why.push(where + ": server " + JSON.stringify(key))
    }
    if (v.mcp && typeof v.mcp === "object" && v.mcp.serverCommand) {
      why.push(where + ": mcp.serverCommand starts a server \"mcp\"")
    }
  }
  const sys = read(sysFile)
  const sysDef = read(sysDefFile)
  const user = read(userFile)
  const work = path.resolve(process.cwd()) === path.resolve(home) ? undefined : read(workFile)
  servers(sysFile, sys, false)
  servers(sysDefFile, sysDef, false)
  servers(userFile, user, true)
  servers(workFile, work, false)
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
    const manifest = path.join(from, "gemini-extension.json")
    servers(manifest, read(manifest), false)
  }
  // THE SIGN-IN, WHICHEVER LATER FILES 0.70.0 SKIPS: it merges system
  // defaults < the user's (the override in its place) < the workspace's < the
  // system's, and skips a system file that is not root-owned (measured) and an
  // untrusted workspace, so the user's must name one and every later one that
  // names one counts. The defaults sit below the user's and never decide.
  const pick = (v) => v?.security?.auth?.selectedType
  const signIns = [process.env.GEMINI_CLI_AUTH_OVERRIDE || pick(user)]
  for (const v of [work, sys]) if (pick(v) !== undefined) signIns.push(pick(v))
  for (const auth of signIns) {
    if (auth !== "gemini-api-key" && auth !== "vertex-ai") {
      why.push("sign-in " + JSON.stringify(auth ?? null) + ": a login's remote admin controls can add servers")
    }
  }
  return why
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
