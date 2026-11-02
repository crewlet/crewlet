// The probe opencode_steer_test.go drives the OpenCode notes plugin with: the
// plugin the engine embeds, imported from where a run's configuration names it
// (argv[2]), and set up against a stand-in of the context OpenCode hands a
// plugin — its options, its event subscription, its session's prompt. Nothing
// here decides anything: the Go suite plays the bridge's notes route over HTTP
// — or serves a real bridge's — and the server's events over this probe's
// standard input, and judges what the plugin did from the route's requests and
// this probe's reports.
//
// Commands, one JSON object a line on stdin:
//   {"op":"setup","name":N,"url":U,"session":S}  set a plugin up as N
//   {"op":"event","name":N,"event":E}             an event of the server's to N
//   {"op":"fail-prompts","name":N,"count":C}      N's next C admissions throw
//   {"op":"hold-prompts","name":N}                N's admissions wait for…
//   {"op":"release-prompts","name":N}             …this
//   {"op":"cleanup","name":N}                     run the cleanup N's setup returned
//   {"op":"watch-fetches"}                        report every request as it starts
//
// Reports, one JSON object a line on stdout: `setup` (with `ms`, how long the
// setup took, and `cleanup`, whether it returned one), `subscribed`,
// `prompt-started` and `prompt` (with the `input` the plugin passed, and `ok`),
// `cleaned`, `error`, and — once fetches are watched — `fetch` (with `method`,
// `url` and `ms`, the instant the request began on this process's monotonic
// clock).

import { createInterface } from "node:readline"
import { pathToFileURL } from "node:url"

const report = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")

const plugin = (await import(pathToFileURL(process.argv[2]).href)).default
const instances = new Map()
let fetchesWatched = false

// channel is an async iterable of the values pushed into it, ended by signal.
function channel(signal) {
  const queue = []
  let waiting = null
  let ended = false
  const end = () => {
    ended = true
    if (waiting !== null) {
      const w = waiting
      waiting = null
      w({ done: true, value: undefined })
    }
  }
  if (signal?.aborted) end()
  else signal?.addEventListener("abort", end, { once: true })
  return {
    push(value) {
      if (ended) return
      if (waiting !== null) {
        const w = waiting
        waiting = null
        w({ done: false, value })
      } else queue.push(value)
    },
    iterable: {
      [Symbol.asyncIterator]() {
        return {
          next() {
            if (queue.length > 0) return Promise.resolve({ done: false, value: queue.shift() })
            if (ended) return Promise.resolve({ done: true, value: undefined })
            return new Promise((resolve) => {
              waiting = resolve
            })
          },
          return() {
            end()
            return Promise.resolve({ done: true, value: undefined })
          },
        }
      },
    },
  }
}

// instance is the stand-in context one plugin is set up with.
function instance(name, url) {
  const self = { name, subscriptions: [], fail: 0, held: null, cleanup: undefined }
  self.ctx = {
    options: url === "" ? {} : { url },
    event: {
      subscribe({ signal } = {}) {
        const ch = channel(signal)
        self.subscriptions.push(ch)
        report({ op: "subscribed", name })
        return ch.iterable
      },
    },
    session: {
      async prompt(input) {
        report({ op: "prompt-started", name, input })
        if (self.held !== null) await self.held.promise
        if (self.fail > 0) {
          self.fail--
          report({ op: "prompt", name, input, ok: false })
          throw new Error("the stand-in refused this admission")
        }
        report({ op: "prompt", name, input, ok: true })
        return { id: input.id, delivery: input.delivery }
      },
    },
  }
  return self
}

function gate() {
  let open
  const promise = new Promise((resolve) => {
    open = resolve
  })
  return { promise, open }
}

const commands = {
  async setup({ name, url, session }) {
    process.env.CREWLET_STEER_SESSION = session
    const self = instance(name, url)
    instances.set(name, self)
    const started = performance.now()
    self.cleanup = await plugin.setup(self.ctx)
    report({ op: "setup", name, ms: performance.now() - started, cleanup: typeof self.cleanup === "function" })
  },
  event({ name, event }) {
    for (const ch of instances.get(name).subscriptions) ch.push(event)
  },
  "fail-prompts"({ name, count }) {
    instances.get(name).fail = count
  },
  "hold-prompts"({ name }) {
    instances.get(name).held = gate()
  },
  "release-prompts"({ name }) {
    const self = instances.get(name)
    const held = self.held
    self.held = null
    held?.open()
  },
  async cleanup({ name }) {
    await instances.get(name).cleanup?.()
    report({ op: "cleaned", name })
  },
  // ON THE PLUGIN'S OWN CLOCK: when a request BEGINS is what the plugin's
  // pacing bounds, and the route only sees it arrive — a first request that
  // pays for the client's setup and a connection arrives late by that much,
  // and the gap the route measures to the next one shrinks by as much.
  "watch-fetches"() {
    if (fetchesWatched) return
    fetchesWatched = true
    const real = globalThis.fetch
    globalThis.fetch = (input, init) => {
      report({ op: "fetch", method: init?.method ?? "GET", url: String(input), ms: performance.now() })
      return real(input, init)
    }
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
// Stdin closed: the suite is done with the probe, whatever the plugin still
// holds.
process.exit(0)
