// crewlet.checkouts — the plugin an OpenCode run's server loads to hand a
// repository checked out during the run below its working directory what a
// session started in it would have loaded: its instruction file and its
// skills.
//
// The engine embeds this file (opencodecheckout.go) and writes it as index.js
// in a directory of its own, which the run's configuration names with the
// note helper, its nested config and the firing's bounds as its options
// (opencode.go, OpenCode.WriteConfig).
//
// AFTER EACH TOOL CALL OF THE RUN'S OWN SESSION it runs the note helper's
// opencode-nested adapter (internal/notehelper's opencoderepo.go), which hands
// each checkout cloned since its last firing, once, and adds what the adapter
// prints to the tool's result as a text part, which OpenCode 2.3.2 sends to
// the model (measured; a string written into the result's output is not). The
// session is the one the run's script names, handed over as
// CREWLET_RUN_SESSION: a subagent's session discards its context with its
// answer, so a checkout claimed there would be handed to nobody.
//
// AWAITED, AND BOUNDED. OpenCode waits for every `execute.after` hook before
// it hands the tool's answer back to its model, and bounds that wait not at
// all, so the firing carries its own timeout and output bound
// (opencodecheckout.go). Its failure is never the tool's: the run's error
// stream says it, with the helper's own stderr, and the tool's result goes on
// unchanged.
//
// IT WRITES NOTHING in its own directory: OpenCode reloads a configured
// plugin whose directory changes.

import { execFile } from "node:child_process"

export default {
  id: "crewlet.checkouts",
  async setup(ctx) {
    const hand = ctx?.options ?? {}
    const session = globalThis.process?.env?.CREWLET_RUN_SESSION ?? ""
    if (typeof hand.helper !== "string" || hand.helper === "" || typeof hand.config !== "string" || session === "") {
      return undefined
    }
    await ctx.tool.hook("execute.after", async (event) => {
      const content = event?.result?.content
      if (event?.sessionID !== session || !Array.isArray(content)) return
      const handed = await handCheckouts(hand)
      if (handed.trim() !== "") content.push({ type: "text", text: handed })
    })
    return undefined
  },
}

// handCheckouts runs the note helper's opencode-nested adapter once: each
// checkout cloned since its last run, "" for none or on a failure, which the
// run's error stream says, with the helper's own stderr (its trouble, never
// what it left out, which is in its answer and in the run's skip log the
// script prints once OpenCode has stopped). Its input is closed at once.
function handCheckouts(hand) {
  return new Promise((resolve) => {
    const child = execFile(hand.helper, ["-adapter", "opencode-nested", "-config", hand.config],
      { encoding: "utf8", timeout: hand.timeoutMs, maxBuffer: hand.maxOutput }, (error, stdout, stderr) => {
        if (typeof stderr === "string" && stderr !== "") process.stderr.write(stderr)
        if (error === null) return resolve(stdout)
        let why = String(error?.message ?? error)
        if (error?.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER") why = "it printed past " + hand.maxOutput + " bytes"
        else if (error?.killed) why = "it ran past its bound of " + hand.timeoutMs + " ms and was stopped"
        process.stderr.write("crewlet: the note helper handing a checkout cloned during the run its instructions " +
          "and skills failed, so what it took is not handed: " + why + "\n")
        resolve("")
      })
    child.stdin?.end()
  })
}
