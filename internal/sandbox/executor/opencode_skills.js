// crewlet.skills — the plugin an OpenCode run's server loads to report each
// skill of the run's own its model loads, to the run's tool bridge.
//
// The engine embeds this file (skills.go) and writes it into the run's work
// directory as index.js, a directory of its own that the run's configuration
// names with the bridge's skill route as its one option — a {file:} reference,
// so the configuration holds a path and never the route's token
// (opencode.go, OpenCode.WriteConfig). Plain ECMAScript with no imports:
// nothing is installed and nothing is fetched to load it, and it runs alike
// under OpenCode's Bun and under the node its suite drives it with
// (opencode_skills_test.go).
//
// WHY A REPORT. A run is handed skills of its own: written where OpenCode reads
// skills (the configuration's `skills`), each a directory named
// `crewlet:<name>`, and loaded by the run's model with OpenCode's own `skill`
// tool. Some of those skills are required: the bridge refuses a call to a tool
// such a skill covers until the run has loaded the skill. OpenCode's tool puts
// the skill's text in front of the model and tells nobody, so this plugin does:
// once the `skill` tool has answered, it posts the skill's id to the route,
// where the bridge records the load as the run's own (internal/api/mcpbridge,
// skills.go). A skill the run was not handed — the checkout's own, the
// image's — is posted too, and the bridge knows it for none of the run's.
//
// AWAITED, AND BOUNDED. OpenCode waits for every `execute.after` hook before it
// hands the tool's answer back to its model (core/src/tool.ts, 2.0.32), so a
// load is on record before the request that could call a tool the skill
// covers. A report is one request, abandoned after reportAbortMs; a failed one
// is not made again — the model's next call to a covered tool is refused with
// the skill to load, and that load is reported afresh. Its failure is never
// the tool's: the hook catches every error, and the skill stays loaded in the
// model's context whatever the bridge heard.
//
// A ROUTE THAT REFUSES ITS TOKEN (401) OR HAS NO SKILL ROUTE (404) ENDS THE
// PLUGIN'S REPORTS for the rest of the run: neither will answer otherwise
// later, and a request each time the model loads a skill would buy nothing.
//
// IT WRITES NOTHING, anywhere: OpenCode reloads a configured plugin whose
// directory changes.

// reportAbortMs bounds one report: a single write on the run's record, which
// the bridge answers in milliseconds, and well inside the time a model's next
// request would take anyway — the model waits on it.
const reportAbortMs = 5_000

// skillTool is OpenCode's tool a model loads a skill with.
const skillTool = "skill"

export default {
  id: "crewlet.skills",
  async setup(ctx) {
    const url = typeof ctx?.options?.url === "string" ? ctx.options.url : ""
    // A run its launch wired to no skill route — or one whose reference
    // OpenCode read as empty — has nowhere to report.
    if (url === "") return undefined
    let refused = false
    const registration = await ctx.tool.hook("execute.after", async (event) => {
      if (refused || event?.tool !== skillTool || event.status !== "completed") return
      const id = event.input?.id
      if (typeof id !== "string" || id === "") return
      refused = await report(url, id)
    })
    return async () => {
      refused = true
      await registration?.dispose?.()
    }
  },
}

// report posts one load to the route: true where the route refused it for good
// (its token no longer resolves, or the run has no skill route), false for an
// answer — or a failure — after which the next load is reported as usual.
async function report(url, skill) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), reportAbortMs)
  try {
    const response = await fetch(url, {
      method: "POST",
      body: JSON.stringify({ skill }),
      headers: { "content-type": "application/json" },
      signal: controller.signal,
      // Bun's own idle deadline on a request, off: this request's bound is
      // reportAbortMs.
      timeout: false,
    })
    await response.text()
    return response.status === 401 || response.status === 404
  } catch {
    return false
  } finally {
    clearTimeout(timer)
  }
}
