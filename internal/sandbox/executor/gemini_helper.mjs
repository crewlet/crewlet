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
//     reads it as empty.
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
} else if (mode === "usage") {
  const [chats, session, before] = args
  process.stdout.write(JSON.stringify(usage(chats, session, before)) + "\n")
} else if (mode === "shadows") {
  const [bridge, ...rest] = args
  const named = { own: [], allowed: [] }
  for (let i = 0; i + 1 < rest.length; i += 2) named[rest[i].replace(/^--/, "")]?.push(rest[i + 1])
  const found = shadows(bridge, named)
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

// shadows is each server a settings file declares that would take bridge's
// place or name a tool inside its names, a sentence a server. Gemini CLI
// 0.70.0 names a server's tool `<server>_<tool>` if that opens `mcp_`, else
// `mcp_<server>_<tool>`, writing each character outside `A-Za-z0-9_.:-` as
// `_`, and merges servers by name over its settings files, so: the bridge's
// own name outside the home's settings, a name that opens `<bridge>_`, or
// `mcp_<bridge>` or one opening `mcp_<bridge>_`. Read as 0.70.0 reads them:
// the system settings and defaults (each at its variable's path), the home's,
// and the working directory's unless it is the home. A server `mcp` (a
// file's, or the one `mcp.serverCommand` starts) names a tool there only by
// what it lists, which only the run's BeforeTool hook sees ([unbridged]).
function shadows(bridge, { own, allowed }) {
  const home = os.homedir()
  const system = process.env.GEMINI_CLI_SYSTEM_SETTINGS_PATH || "/etc/gemini-cli/settings.json"
  const files = [system, process.env.GEMINI_CLI_SYSTEM_DEFAULTS_PATH ||
    path.join(path.dirname(system), "system-defaults.json")]
  const user = path.join(home, ".gemini", "settings.json")
  files.push(user)
  if (realpath(process.cwd()) !== realpath(home)) files.push(path.join(process.cwd(), ".gemini", "settings.json"))
  const fold = (name) => String(name).toLowerCase().trim()
  const spelled = (name) => name.replace(/[^A-Za-z0-9_.:-]/g, "_")
  const found = []
  for (const file of files) {
    const servers = readSettings(file)?.mcpServers
    if (servers === null || typeof servers !== "object" || Array.isArray(servers)) continue
    for (const name of Object.keys(servers)) {
      if (file === user && own.includes(name)) continue
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

// readSettings is the settings object file holds, its comments stripped as
// Gemini CLI strips them; null for none, or one that does not read as one.
function readSettings(file) {
  try {
    const settings = JSON.parse(withoutComments(fs.readFileSync(file, "utf8")))
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
