// The probe toolenv_test.go drives the OpenCode tool-environment
// plugin with: the plugin the engine embeds, imported from where a run's
// configuration names it (argv[2]), set up against a stand-in of the context
// OpenCode hands a plugin with argv[3] (JSON) as its options. It then
// builds one shell command as OpenCode does, its environment a copy of the
// server's own, runs the `create.before` hooks the plugin registered on it,
// and prints one JSON object: `hooked` (the shell hooks registered), `command`
// (the command's environment after the hooks) and `server` (the server's own
// environment after them). Nothing here decides anything.

import { pathToFileURL } from "node:url"

const plugin = (await import(pathToFileURL(process.argv[2]).href)).default
const hooks = new Map()
await plugin.setup({
  options: JSON.parse(process.argv[3]),
  shell: {
    async hook(name, callback) {
      hooks.set(name, callback)
      return { async dispose() {} }
    },
  },
})
const command = { command: "env", cwd: process.cwd(), env: { ...process.env, TERM: "xterm-256color" } }
await hooks.get("create.before")?.(command)
process.stdout.write(JSON.stringify({ hooked: [...hooks.keys()], command: command.env, server: { ...process.env } }) + "\n")
