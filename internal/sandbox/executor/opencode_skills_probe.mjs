// The probe opencode_skills_test.go drives the OpenCode skill-load plugin
// with: the plugin the engine embeds, imported from where a run's
// configuration names it (argv[2]), and set up against a stand-in of the
// context OpenCode hands a plugin — its options and its tool hooks. Nothing
// here decides anything: the Go suite plays the bridge's skill route over HTTP,
// hands the plugin each tool's `execute.after` over this probe's standard
// input, and judges what the plugin did from the route's requests and this
// probe's reports.
//
// Commands, one JSON object a line on stdin:
//   {"op":"setup","url":U}   set the plugin up, its route U ("" for none)
//   {"op":"after","event":E} run its `execute.after` hook on E, as OpenCode
//                            does once a tool has answered, and wait for it
//   {"op":"cleanup"}         run the cleanup its setup returned
//
// Reports, one JSON object a line on stdout: `setup` (with `hooked`, the hooks
// it registered, and `cleanup`, whether it returned one), `after` (once the
// hook resolved, or at once where none is registered), `disposed` (a
// registration let go), `cleaned` and `error`.

import { createInterface } from "node:readline"
import { pathToFileURL } from "node:url"

const report = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")

const plugin = (await import(pathToFileURL(process.argv[2]).href)).default
const hooks = new Map()
let cleanup

const ctx = (url) => ({
  options: url === "" ? {} : { url },
  tool: {
    async hook(name, callback) {
      hooks.set(name, callback)
      return {
        async dispose() {
          hooks.delete(name)
          report({ op: "disposed", name })
        },
      }
    },
  },
})

const commands = {
  async setup({ url }) {
    cleanup = await plugin.setup(ctx(url))
    report({ op: "setup", hooked: [...hooks.keys()], cleanup: typeof cleanup === "function" })
  },
  async after({ event }) {
    const hook = hooks.get("execute.after")
    if (hook !== undefined) await hook(event)
    report({ op: "after" })
  },
  async cleanup() {
    await cleanup?.()
    report({ op: "cleaned" })
  },
}

for await (const line of createInterface({ input: process.stdin })) {
  if (line.trim() === "") continue
  try {
    const command = JSON.parse(line)
    await commands[command.op](command)
  } catch (error) {
    report({ op: "error", message: String(error?.stack ?? error) })
  }
}
// Stdin closed: the suite is done with the probe.
process.exit(0)
