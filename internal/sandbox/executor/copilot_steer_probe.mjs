// The stand-in of Copilot's extension SDK copilot_steer_internal_test.go drives
// the notes extension with: installed by the suite as
// `node_modules/@microsoft/copilot-sdk/extension.mjs` beside the extension the
// engine renders, so the extension's one import resolves here. Nothing here
// decides anything: the suite plays the session's events and hooks over this
// probe's stdin and judges what the extension did from the notes route's
// requests and these reports.
//
// Commands, one JSON object a line on stdin:
//   {"op":"event","event":E}            a session event to every handler
//   {"op":"hook","name":N,"input":I}    call the extension's hook N
//   {"op":"hold-sends"}                 sends wait, once reported, for…
//   {"op":"release-sends","fail":F}     …this, failing where F is true
//
// Reports, one JSON object a line on stdout: `joined` (the hooks registered),
// `holding` (hold-sends applied), `send` (the options the extension passed),
// `abort`, `log` (message, level), `hooked` (name, output), `error`.

import { createInterface } from "node:readline"

const report = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")
const handlers = []
let hooks = {}
let held = null

const session = {
  on(handler) {
    handlers.push(handler)
    return () => {}
  },
  async send(options) {
    report({ op: "send", input: options })
    if (held !== null && !(await held.promise)) throw new Error("the probe failed this send")
    return "message-" + handlers.length
  },
  async abort() {
    report({ op: "abort" })
  },
  async log(message, options) {
    report({ op: "log", message, level: options?.level ?? "info" })
  },
}

async function hook(name, input) {
  try {
    const output = await hooks[name]?.(input, { sessionId: "probe" })
    report({ op: "hooked", name, output: output ?? null })
  } catch (error) {
    report({ op: "error", message: String(error?.stack ?? error) })
  }
}

export async function joinSession(config) {
  hooks = config?.hooks ?? {}
  report({ op: "joined", hooks: Object.keys(hooks).sort() })
  listen()
  return session
}

let listening = false
function listen() {
  if (listening) return
  listening = true
  ;(async () => {
    for await (const line of createInterface({ input: process.stdin })) {
      if (line.trim() === "") continue
      try {
        const command = JSON.parse(line)
        if (command.op === "event") for (const h of handlers) h(command.event)
        if (command.op === "hold-sends") {
          let open
          held = { promise: new Promise((resolve) => (open = resolve)), open: (pass) => open(pass) }
          report({ op: "holding" })
        }
        if (command.op === "release-sends") {
          const h = held
          held = null
          h?.open(command.fail !== true)
        }
        // NOT AWAITED HERE, so a hook waiting on a held send leaves the
        // probe reading the command that releases it.
        if (command.op === "hook") hook(command.name, command.input)
      } catch (error) {
        report({ op: "error", message: String(error?.stack ?? error) })
      }
    }
    process.exit(0)
  })()
}
