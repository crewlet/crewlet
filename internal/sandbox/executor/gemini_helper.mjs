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
//   node gemini-helper.mjs guard [--server <name>]... [--hook <name>] [--tool <name>]...
//     Exits 3, saying which file and which list, where a settings file the
//     run's system tier cannot overrule switches off one of Crewlet's own
//     servers, its hook or a tool of the bridge's the run calls by name
//     (gemini.go's geminiGuard). A file that does not read is passed over, as
//     Gemini CLI reads it as empty.
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
} else if (mode === "guard") {
  const named = { server: [], hook: [], tool: [] }
  for (let i = 0; i + 1 < args.length; i += 2) named[args[i].replace(/^--/, "")]?.push(args[i + 1])
  const found = switchedOff(named)
  if (found.length > 0) {
    process.stderr.write("crewlet: Gemini CLI would start without what the run's notes and tools go through: " +
      found.join("; ") + ". Gemini CLI adds each such list up across its settings files, so no setting of " +
      "the run's undoes it; remove the entry. The run was not started.\n")
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

// switchedOff is what each settings file below the run's switches off of
// named's servers, hook and tools, a sentence a file: the home's settings, the
// working directory's unless it is the home (Gemini CLI reads it as no
// workspace there), and the system defaults. Matched as 0.70.0 matches: a
// server by its name lower-cased and trimmed, a hook and a tool exactly, a
// tool also by its qualified name (`mcp_<server>_<tool>`).
function switchedOff({ server, hook, tool }) {
  const home = os.homedir()
  const files = [path.join(home, ".gemini", "settings.json")]
  if (realpath(process.cwd()) !== realpath(home)) files.push(path.join(process.cwd(), ".gemini", "settings.json"))
  files.push(process.env.GEMINI_CLI_SYSTEM_DEFAULTS_PATH || "/etc/gemini-cli/system-defaults.json")
  const fold = (name) => String(name).toLowerCase().trim()
  const list = (value) => (Array.isArray(value) ? value : [])
  const found = []
  for (const file of files) {
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
    }
    for (const entry of list(settings.tools?.exclude)) {
      if (typeof entry !== "string") continue
      if (tool.includes(entry) || server.some((name) => entry.startsWith(`mcp_${name}_`))) {
        why.push(`its tools.exclude names the tool ${entry}`)
      }
    }
    for (const name of hook) {
      if (list(settings.hooksConfig?.disabled).includes(name)) why.push(`its hooksConfig.disabled names the hook ${name}`)
    }
    if (why.length > 0) found.push(`${file}: ${why.join(", ")}`)
  }
  return found
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
