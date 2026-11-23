// crewlet.toolenv — the plugin an OpenCode run's server loads so the commands
// its model runs start without the variables the harness alone reads: the
// names it signs in by, the run's own wiring (its configuration, the session
// its notes plugin admits to) and its telemetry export's endpoint and headers.
//
// The engine embeds this file (opencodetoolenv.go) and writes it as index.js
// in a directory of its own, which the run's configuration names with the
// variables as its `names` option (opencode.go, OpenCode.WriteConfig). Plain
// ECMAScript with no imports, as the notes plugin is.
//
// OpenCode builds each shell command's environment as a copy of its own and
// hands it to a plugin's `create.before` shell hook before it spawns the
// command (measured on 2.3.2). Deleting a name there removes it from that
// command alone: the server's own environment, which its providers read their
// keys from, is untouched. setup awaits the registration, and OpenCode holds
// every prompt until every plugin's setup has resolved, so no command starts
// before it.

export default {
  id: "crewlet.toolenv",
  async setup(ctx) {
    const names = Array.isArray(ctx?.options?.names)
      ? ctx.options.names.filter((name) => typeof name === "string" && name !== "")
      : []
    if (names.length === 0) return undefined
    await ctx.shell.hook("create.before", (command) => {
      const env = command?.env
      if (env === null || typeof env !== "object") return
      for (const name of names) delete env[name]
    })
    return undefined
  },
}
