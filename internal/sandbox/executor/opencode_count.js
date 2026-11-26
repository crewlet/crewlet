// crewlet.count — the plugin a capped OpenCode run's server loads so the
// run's model requests are recorded for its turn cap: a line for each request
// its server answered, every session's, a subagent's and a compaction's
// included, as the server's own `session.usage.updated` event says it
// (measured on 2.3.2 against a stand-in: one an answered request, a request
// retried after a 429 or a 500 once). It writes a start line as it loads, so
// a capped run whose requests were never recorded reads as such. A line holds
// its kind and the session alone, nothing the session said.
//
// The engine embeds this file (opencodecountplugin.go) and writes it as index.js
// in a directory of its own, which the run's configuration names with the
// record's path as its `file` option. Its one import is node's own fs.

import { appendFileSync } from "node:fs"

export default {
  id: "crewlet.count",
  async setup(ctx) {
    const file = ctx?.options?.file
    if (typeof file !== "string" || file === "") return undefined
    const write = (line) => {
      try {
        appendFileSync(file, JSON.stringify(line) + "\n")
      } catch {
        // Said by the count: a line never written is a request it reads as missing.
      }
    }
    write({ type: "start" })
    // LISTENS FOR THE SERVER'S WHOLE LIFE, subscribing again after a pause
    // where the subscription ends unasked. setup does not wait on it: OpenCode
    // holds every prompt until each plugin's setup has resolved.
    const listen = async () => {
      let wait = 100
      for (;;) {
        try {
          for await (const event of ctx.event.subscribe({})) {
            wait = 100
            if (event?.type !== "session.usage.updated") continue
            const session = event?.data?.sessionID
            write({ type: "request", session: typeof session === "string" ? session : "" })
          }
        } catch {
          // Said by what follows: the subscription is opened again.
        }
        await new Promise((resolve) => setTimeout(resolve, wait))
        wait = Math.min(wait * 2, 2000)
      }
    }
    listen()
    return undefined
  },
}
