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
//   node gemini-helper.mjs usage <chats dir> <session id>
//     Prints one line of the run's stream: the tokens each request the chat
//     file records spent, split as stream-json's result does not split its
//     total (the reasoning, the tool prompt; geministream.go's runSpend).
//     0.70.0 writes a response's entry up to three times as it goes (its
//     prose, its calls scheduled, its calls run), each with the same id and
//     counts, so the latest entry of each id is the request's.
import fs from "node:fs"
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
} else if (mode === "usage") {
  const [chats, session] = args
  process.stdout.write(JSON.stringify(usage(chats, session)) + "\n")
} else {
  process.stderr.write(`crewlet: gemini-helper.mjs has no mode ${JSON.stringify(mode)}\n`)
  process.exit(2)
}

// usage is the usage line for session: its requests' summed tokens, or why
// there are none to read.
function usage(chats, session) {
  const line = { type: "crewlet_gemini_usage", session_id: session }
  const file = sessionFile(chats, session)
  if (file === "") return { ...line, missing: "no chat file in " + chats + " names the session" }
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
  const tokens = { input: 0, output: 0, cached: 0, thoughts: 0, tool: 0 }
  for (const counts of requests.values()) {
    for (const key of Object.keys(tokens)) {
      if (Number.isSafeInteger(counts[key]) && counts[key] > 0) tokens[key] += counts[key]
    }
  }
  return { ...line, requests: requests.size, tokens }
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
