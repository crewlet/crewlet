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
