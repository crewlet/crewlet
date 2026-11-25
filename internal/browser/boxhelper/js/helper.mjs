// helper.mjs: the box helper's entry, `node helper.mjs <subcommand>`, run
// from the helper's folder in a box. ensure prints its one line; supervise
// runs the box's supervisor from the launch ensure wrote, the live view
// (live.mjs) attached to it; stop ends that supervisor and its browser; mcp
// is the shim, a job's browser server on stdio. An unknown subcommand or flag
// exits 64 with one line on stderr. internal/browser/boxhelper's package doc
// carries the argument.

import path from "node:path"

// usage refuses an invocation the helper does not take.
function usage(what) {
  process.stderr.write(`helper: ${what}\n`)
  process.exit(64)
}

// The folder as invoked, a link in its place unresolved, so ensure judges
// the path a box's user would.
const folder = path.dirname(path.resolve(process.argv[1]))
const [subcommand, ...flags] = process.argv.slice(2)

switch (subcommand) {
  case "ensure": {
    if (flags.length > 0) usage(`ensure takes no flags, and was given ${JSON.stringify(flags)}`)
    const { ensure } = await import("./ensure.mjs")
    let line
    try {
      line = await ensure(folder)
    } catch (err) {
      // No line: the engine reads ensure_unreadable, and this says why.
      process.stderr.write(`ensure: ${err?.stack ?? err}\n`)
      process.exit(1)
    }
    process.stdout.write(`${line}\n`, () => process.exit(0))
    break
  }
  case "stop": {
    if (flags.length > 0) usage(`stop takes no flags, and was given ${JSON.stringify(flags)}`)
    const { stop } = await import("./ensure.mjs")
    try {
      await stop(folder)
    } catch (err) {
      process.stderr.write(`stop: ${err?.stack ?? err}\n`)
      process.exit(1)
    }
    process.exit(0)
    break
  }
  case "supervise": {
    if (flags.length !== 2 || flags[0] !== "--launch" || !path.isAbsolute(flags[1])) {
      usage(`supervise takes --launch and launch.json's absolute path, and was given ${JSON.stringify(flags)}`)
    }
    const { run } = await import("./supervise.mjs")
    const { attachment } = await import("./live.mjs")
    await run(flags[1], [attachment()])
    break
  }
  case "mcp": {
    const job = flags[1]
    const root = flags[3]
    if (
      flags.length !== 4 ||
      flags[0] !== "--job" ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(job) ||
      flags[2] !== "--root" ||
      !path.isAbsolute(root)
    ) {
      usage(`mcp takes --job and the launch's id, then --root and the workspace's absolute path, and was given ${JSON.stringify(flags)}`)
    }
    const { shim } = await import("./shim.mjs")
    await shim(folder, job, root)
    break
  }
  default:
    usage(`unknown subcommand ${JSON.stringify(subcommand)}: ensure, supervise, stop or mcp`)
}
