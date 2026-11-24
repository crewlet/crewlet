// The stand-in of pi's extension API the suite drives the rendered
// pi_extension.mjs with (pi_extension_internal_test.go, pi_contract_test.go):
// `node pi_extension_probe.mjs <extension.mjs>`. Nothing here decides
// anything: the suite plays pi's events over this probe's stdin and judges what
// the extension did from the routes' requests and these reports.
//
// Commands, one JSON object a line on stdin:
//   {"op":"tools","names":[...]}           what pi.getAllTools() and getActiveTools() name from now on
//   {"op":"settings","settings":S}         what pi.getSettings() answers
//   {"op":"usage","usage":U}               what ctx.getContextUsage() answers
//   {"op":"event","name":N,"event":E}      every handler of N, awaited in order
//
// Reports, one JSON object a line on stdout: `loaded`, `register` (name,
// config), `tool` (name), `entry` (customType, data), `abort`, `handled`
// (name, result, the event as the handlers left it), `error`.

import { createInterface } from "node:readline"
import { pathToFileURL } from "node:url"

const report = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")
const handlers = new Map()
let tools = []
let usage
let settings = {}

const pi = {
  on(name, handler) {
    if (!handlers.has(name)) handlers.set(name, [])
    handlers.get(name).push(handler)
  },
  registerMcpServer(name, config) {
    report({ op: "register", name, config })
  },
  registerTool(tool) {
    report({ op: "tool", name: tool.name })
  },
  appendEntry(customType, data) {
    report({ op: "entry", customType, data })
  },
  getAllTools() {
    return tools.map((name) => ({ name }))
  },
  getActiveTools() {
    return [...tools]
  },
  getSettings() {
    return structuredClone(settings)
  },
}

const ctx = {
  cwd: process.cwd(),
  abort() {
    report({ op: "abort" })
  },
  getContextUsage() {
    return usage
  },
}

try {
  const extension = await import(pathToFileURL(process.argv[2]).href)
  extension.default(pi)
  report({ op: "loaded", events: [...handlers.keys()].sort() })
} catch (error) {
  report({ op: "error", message: String(error?.stack ?? error) })
}

for await (const line of createInterface({ input: process.stdin })) {
  if (line.trim() === "") continue
  try {
    const command = JSON.parse(line)
    if (command.op === "tools") tools = command.names
    if (command.op === "usage") usage = command.usage
    if (command.op === "settings") settings = command.settings
    if (command.op === "event") {
      const event = command.event ?? {}
      let result
      for (const handler of handlers.get(command.name) ?? []) {
        const out = await handler(event, ctx)
        if (out !== undefined) result = out
      }
      report({ op: "handled", name: command.name, result: result ?? null, event })
    }
  } catch (error) {
    report({ op: "error", message: String(error?.stack ?? error) })
  }
}
process.exit(0)
