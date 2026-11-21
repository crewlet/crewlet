// The probe the box helper's Go suite drives the helper's modules with, through
// boxhelpertest's Probe: the modules the engine wrote into a box, imported
// from that box's folder (argv[2]), so a case drives the bytes a box runs, in
// the box's environment. Nothing here decides anything: the Go cases judge
// what it reports.
//
// Commands, one JSON object a line on stdin, each answered by one line on
// stdout naming its op, in order:
//   {"op":"install","folder":F,"fail":X,"abort":A}
//                                          install config.json's tree in F,
//                                          with the probe's whole environment:
//                                          where and whether installed, or the
//                                          reason and cause it is withheld,
//                                          and either way the pid of each npm
//                                          it started; A true aborts its
//                                          signal as npm starts, as a
//                                          caller's deadline would
//   {"op":"npm_cause","stdout":S}          the code and cause npm's --json
//                                          answer S stands for
//   {"op":"load","dir":T}                  require an installed tree's
//                                          packages: what the server is
//                                          handed, and every coreBundle.js
//                                          loaded
//   {"op":"identify","pid":P}              what the kernel says of P as the
//                                          reap reads it: alive, and its start
//   {"op":"signal","pid":P,"sig":S,"group":G}
//                                          send S, a signal's number or name,
//                                          to P, or to its group with G:
//                                          whether anyone was sent it
//   {"op":"record","body":B}               what the reap reads of a record
//                                          holding B: its pid and start, or
//                                          nothing
// A fault X is {method, errno, after, kill}: every call of the file system's
// method after the first `after` throws the error Node gives for errno, a
// name, or with kill the first such call kills the probe, as SIGKILL would a
// box's helper part way.
// A command that throws anything else answers {"op":"error","message":M}.

import fsp from "node:fs/promises"
import { constants } from "node:os"
import path from "node:path"
import { createInterface } from "node:readline"
import { pathToFileURL } from "node:url"
import { getSystemErrorName } from "node:util"

const folder = process.argv[2]
const module = (name) => import(pathToFileURL(path.join(folder, name)).href)
const { readConfig, running, signal, Withheld } = await module("common.mjs")
const tree = await module("tree.mjs")

const answer = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")

// faulty is the file system with method failing as fail says, every call
// after the first fail.after, with the error Node gives for fail.errno: its
// negative number, and as its code libuv's name for it, which for EDQUOT is
// none (getSystemErrorName answers "Unknown system error -122" on Linux).
function faulty(fail) {
  if (fail === undefined) return fsp
  const after = fail.after ?? 0
  const errno = -constants.errno[fail.errno]
  if (!fail.kill && !Number.isInteger(errno)) throw new Error(`${fail.errno} is no errno this platform names`)
  const code = fail.kill ? "" : getSystemErrorName(errno)
  let calls = 0
  return new Proxy(fsp, {
    get(target, method) {
      const real = target[method]
      if (method !== fail.method) return real
      return async (...args) => {
        if (calls++ < after) return real.apply(target, args)
        if (fail.kill) process.kill(process.pid, "SIGKILL")
        throw Object.assign(new Error(`${code}: the probe's ${fail.errno} in ${method}`), { code, errno })
      }
    },
  })
}

const commands = {
  async install({ folder, fail, abort }) {
    const pids = []
    const caller = new AbortController()
    const spawned = (pid) => {
      pids.push(pid)
      if (abort) caller.abort()
    }
    try {
      const config = await readConfig(folder)
      return { ...(await tree.install(folder, config, { io: faulty(fail), signal: caller.signal, spawned })), pids }
    } catch (err) {
      if (!(err instanceof Withheld)) throw err
      return { reason: err.reason, cause: err.cause, detail: err.detail, pids }
    }
  },
  npm_cause({ stdout }) {
    return tree.npmFailure(stdout)
  },
  load({ dir }) {
    const { mcp, core, require } = tree.load(dir)
    return {
      create_connection: typeof mcp.createConnection,
      launch_persistent_context: typeof core.chromium?.launchPersistentContext,
      bundles: Object.keys(require.cache).filter((file) => path.basename(file) === "coreBundle.js"),
    }
  },
  async identify({ pid }) {
    const { alive, start } = await running(pid)
    return { alive, start }
  },
  signal({ pid, sig, group }) {
    return { sent: signal(pid, sig, group) }
  },
  record({ body }) {
    return tree.recordOf(body)
  },
}

for await (const line of createInterface({ input: process.stdin, crlfDelay: Infinity })) {
  const command = JSON.parse(line)
  try {
    answer({ op: command.op, ...(await commands[command.op](command)) })
  } catch (err) {
    answer({ op: "error", message: `${command.op}: ${err?.stack ?? err}` })
  }
}
process.exit(0)
