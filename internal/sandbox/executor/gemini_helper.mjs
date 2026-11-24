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
//   node gemini-helper.mjs shadows <bridge> [--own <name>]... [--allowed <name>]...
//     Exits 3, naming the file and the server, where a settings file Gemini
//     CLI 0.70.0 reads declares an MCP server that would take the bridge's
//     place or name a tool inside its names (`mcp_<bridge>_<tool>`); the
//     home's own settings may hold the run's servers (--own). With --allowed,
//     a server the run does not admit by name is passed over, as Gemini CLI
//     drops it. A file that does not read is passed over, as Gemini CLI
//     reads it as empty, and so is an extension's, as the run loads none.
//     The sources are the mode `unshadowed`'s, read alike.
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
} else if (mode === "shadows") {
  const [bridge, ...rest] = args
  const named = { own: [], allowed: [] }
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

// shadowing is each server a settings file declares that would take bridge's
// place or name a tool inside its names, a sentence a server (the mode
// `shadows`). Gemini CLI 0.70.0 names a server's tool `<server>_<tool>` if
// that opens `mcp_`, else `mcp_<server>_<tool>`, writing each character
// outside `A-Za-z0-9_.:-` as `_`, and merges servers by name over its
// settings files, so: the bridge's own name outside the home's settings, a
// name that opens `<bridge>_`, or `mcp_<bridge>` or one opening
// `mcp_<bridge>_`. An extension's are passed over, as the run loads none, and
// so is a file that does not read, which Gemini CLI reads as empty. A server
// `mcp` (a file's, or `mcp.serverCommand`'s) names a tool there only by what
// it lists, which only the run's BeforeTool hook sees ([unbridged]).
function shadowing(bridge, { own, allowed }) {
  const fold = (name) => String(name).toLowerCase().trim()
  const spelled = (name) => name.replace(/[^A-Za-z0-9_.:-]/g, "_")
  const found = []
  for (const { file, role, settings } of settingsSources().sources) {
    if (role === "extension") continue
    for (const name of Object.keys(mcpServers(settings) ?? {})) {
      if (role === "user" && own.includes(name)) continue
      if (allowed.length > 0 && !allowed.some((a) => fold(a) === fold(name))) continue
      const s = spelled(name)
      if (name === bridge || s.startsWith(bridge + "_") || s === "mcp_" + bridge || s.startsWith("mcp_" + bridge + "_")) {
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

// realpath is dir resolved through its links, as given where it does not
// resolve.
function realpath(dir) {
  try {
    return fs.realpathSync(dir)
  } catch {
    return path.resolve(dir)
  }
}
