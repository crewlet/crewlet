// The probe opencodecountplugin_test.go drives the OpenCode count plugin with: the
// plugin the engine embeds, imported from where a run's configuration names
// it (argv[2]), set up against a stand-in of the context OpenCode hands a
// plugin with argv[3] (JSON) as its options and an event subscription that
// yields argv[4] (a JSON list of events) and then ends. It waits for the
// plugin to read them and prints one JSON object: `file`, what the record it
// names holds ("" where it names none). Nothing here decides anything.

import { pathToFileURL } from "node:url"
import { readFileSync } from "node:fs"

const plugin = (await import(pathToFileURL(process.argv[2]).href)).default
const options = JSON.parse(process.argv[3])
const events = JSON.parse(process.argv[4])
let served = false
let done
const read = new Promise((resolve) => {
  done = resolve
})
await plugin.setup({
  options,
  event: {
    subscribe() {
      if (served) return (async function* () {})()
      served = true
      return (async function* () {
        for (const event of events) yield event
        done()
      })()
    },
  },
})
// The generator resumes past its last event only once the plugin has read
// it; a plugin given no file subscribes to nothing.
if (served) await read
let file = ""
try {
  file = typeof options.file === "string" ? readFileSync(options.file, "utf8") : ""
} catch {
  file = ""
}
process.stdout.write(JSON.stringify({ file }) + "\n")
process.exit(0)
