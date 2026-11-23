// crewlet.toolenv — the plugin an OpenCode run's server loads so the commands
// its model runs start without Crewlet's own variables: the run's wiring (its
// configuration, the session its notes plugin admits to), its telemetry and
// its trace context. The sign-in and the rest of the run's environment stay.
//
// The engine embeds this file (opencodetoolenv.go) and writes it as index.js
// in a directory of its own, which the run's configuration names with the
// variables as its `names` option and the prefixes of the names it also
// removes as `prefixes` (opencode.go, OpenCode.WriteConfig). Plain ECMAScript
// with no imports, as the notes plugin is.
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
    const strings = (list) => (Array.isArray(list) ? list.filter((n) => typeof n === "string" && n !== "") : [])
    const names = strings(ctx?.options?.names)
    const prefixes = strings(ctx?.options?.prefixes)
    if (names.length === 0 && prefixes.length === 0) return undefined
    await ctx.shell.hook("create.before", (command) => {
      const env = command?.env
      if (env === null || typeof env !== "object") return
      for (const name of Object.keys(env)) {
        if (names.includes(name) || prefixes.some((p) => name.startsWith(p))) delete env[name]
      }
    })
    return undefined
  },
}
