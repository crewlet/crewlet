// ensure.mjs: `node helper.mjs ensure`, the one check the engine runs before
// a box's harness starts. It holds the folder to the box's user, takes the
// box's one lock, installs the locked tree, chooses a Chromium, and leaves one
// supervisor running the browser as this launch needs it, found, started or
// restarted; then it prints one line of closed values, ready or withheld with
// the reason of the first check that failed. internal/browser/boxhelper's
// package doc carries the argument.

import { spawn } from "node:child_process"
import fs from "node:fs"
import fsp from "node:fs/promises"
import net from "node:net"
import os from "node:os"
import path from "node:path"

import {
  BROWSER_SOCKET,
  DIR_MODE,
  FILE_MODE,
  RELAY_SOCKET,
  Withheld,
  digest16,
  ensureLine,
  gone,
  prints,
  readConfig,
  realClock,
  running,
  runs,
  signal,
  sleep,
  writeCause,
  writeWhole,
} from "./common.mjs"
import { choose } from "./chromium.mjs"
import { install, sweep } from "./tree.mjs"

// SOCKET_PATH_MAX is the longest path a box's sockets may have: macOS's
// 104-byte sun_path less its NUL, the bound Playwright itself holds.
const SOCKET_PATH_MAX = 103

// LOCK_RECHECK_MS is how often ensure looks again at a lock it waits on
// besides each change the folder announces: a holder killed outright removes
// nothing and announces nothing, and one stat a second catches it, against a
// deadline and a takeover of minutes.
const LOCK_RECHECK_MS = 1000

// REAP_MS is how long ensure, its deadline passed during the install, waits
// for the install to reap the npm it killed and remove what it staged: a
// killed npm goes in milliseconds unless the kernel holds it, and the
// deadline's margin under the engine's bound holds the kill, this and the line.
const REAP_MS = 1000

// VERSION is a Chromium's full version, as ensure's line carries it.
const VERSION = /^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}\.[0-9]{1,6}$/

// SAID_MAX is the longest line ensure reads from a supervisor on fd 3, in
// bytes: the supervisor's is `ready <version>` or `failed <reason> <cause>`,
// under 100, so a longer one is no supervisor's, and it reads as none.
const SAID_MAX = 512

// ensure runs every check in folder, the helper's, and answers its one line.
// env is ensure's own environment, which npm gets whole and whose allowlisted
// part the supervisor gets.
export async function ensure(folder, { clock = realClock, env = process.env, home = os.homedir() } = {}) {
  const run = new Run(folder, clock, env, home)
  try {
    return await run.answer()
  } finally {
    await run.release()
  }
}

// stop ends the supervisor this folder's launch.json runs, and its browser,
// as ensure stops one it replaces, under the box's one lock: the engine's
// step for a box whose browser a launch withheld for its network.
export async function stop(folder, { clock = realClock } = {}) {
  const run = new Run(folder, clock, process.env, os.homedir())
  try {
    run.config = await readConfig(folder)
    await takeLock(folder, run.config, { clock })
    run.locked = true
    const live = await run.live(path.join(folder, "launch.json"))
    if (live !== undefined) await run.stop(live.pid)
  } finally {
    await run.release()
  }
}

// Run is one ensure: what it has learnt, for its line, the step it is on, and
// what it holds, to give back however it ends.
class Run {
  constructor(folder, clock, env, home) {
    this.folder = folder
    this.clock = clock
    this.env = env
    this.home = home
    this.helper = path.join(folder, "helper.mjs")
    this.known = { node: process.versions.node }
    this.locked = false
    this.child = undefined
    this.step = undefined
    this.deadlinePassed = false
    // Aborted, it kills an install's npm at once: ensure's deadline.
    this.installing = new AbortController()
  }

  // answer is the line: every check under ensure's own deadline, the line of
  // the deadline when it passes first.
  async answer() {
    try {
      await holdFolder(this.folder)
      this.config = await readConfig(this.folder)
    } catch (err) {
      if (err instanceof Withheld) return this.withheld(err)
      throw err
    }
    Object.assign(this.known, { helper: this.config.helper, pin: this.config.pin })
    let timer
    const deadline = new Promise((resolve) => {
      timer = this.clock.setTimeout(() => resolve("deadline"), this.config.ensure_deadline_ms)
    })
    const checks = this.checks()
    try {
      const outcome = await Promise.race([checks.then((line) => ({ line })), deadline])
      if (outcome !== "deadline") return outcome.line
      return await this.pastDeadline(checks)
    } finally {
      this.clock.clearTimeout(timer)
    }
  }

  // pastDeadline is the line once ensure's deadline has passed with checks
  // still running. During the install, npm is killed at once and given
  // REAP_MS, on the real clock it goes in, to be reaped with what it staged;
  // one the kernel holds longer keeps its directory and record for the next
  // ensure's reap. At any later step the browser was not up in time, and a
  // supervisor this ensure started is killed with its group.
  async pastDeadline(checks) {
    this.deadlinePassed = true
    if (this.step === "install") {
      this.installing.abort()
      await settled(checks, REAP_MS)
      return this.withheld(new Withheld("playwright_missing", "timeout", "ensure's deadline passed during the install"))
    }
    this.kill(this.child)
    return this.withheld(new Withheld("supervisor_failed", "timeout", "ensure's deadline passed"))
  }

  // checks runs every check after the folder's in order, and answers the line.
  async checks() {
    try {
      const major = Number(process.versions.node.split(".")[0])
      if (major < this.config.node_floor) {
        const detail = `Node ${process.versions.node} is below ${this.config.node_floor}`
        throw new Withheld("node_too_old", "below_floor", detail)
      }
      this.enter("lock")
      await takeLock(this.folder, this.config, { clock: this.clock })
      this.locked = true
      this.enter("install")
      const tree = await install(this.folder, this.config, { env: this.env, signal: this.installing.signal })
      this.enter("chromium")
      this.childEnv = this.supervisorEnv()
      const choice = await choose(this.config, { env: this.env, childEnv: this.childEnv, home: this.home })
      this.known.chromium = choice.version
      this.enter("supervisor")
      const { start, version } = await this.supervisor(tree.dir, choice)
      // Every supervisor alive is this helper's, on this tree, so no process
      // runs on any other entry of pw/.
      await sweep(this.folder, tree.name)
      return ensureLine("ready", {
        ...this.knownFields(),
        chromium: version,
        build: choice.build,
        sandbox: this.config.sandbox,
        supervisor: start,
      })
    } catch (err) {
      if (err instanceof Withheld) return this.withheld(err)
      throw err
    }
  }

  // enter starts step, unless ensure's deadline has passed: its line has been
  // answered, and nothing more is started for it.
  enter(step) {
    if (this.deadlinePassed) {
      throw new Withheld("supervisor_failed", "timeout", `ensure's deadline passed before the ${step}`)
    }
    this.step = step
  }

  knownFields() {
    return { helper: this.known.helper, pin: this.known.pin, node: this.known.node, chromium: this.known.chromium }
  }

  withheld(err) {
    process.stderr.write(`ensure: ${err.message}\n`)
    return ensureLine("withheld", { reason: err.reason, cause: err.cause, ...this.knownFields() })
  }

  // release gives back this ensure's lock, whichever way it ends.
  async release() {
    if (this.locked) await fsp.rm(path.join(this.folder, "ensure.lock"), { force: true }).catch(() => {})
  }

  // supervisorEnv is the environment ensure starts the supervisor in: the
  // names config.json's supervisor_env lists, as ensure's own environment
  // sets them, and nothing else.
  supervisorEnv() {
    const out = {}
    for (const name of this.config.supervisor_env) {
      if (Object.hasOwn(this.env, name)) out[name] = this.env[name]
    }
    return out
  }

  // supervisor leaves one supervisor running choice on tree: one already
  // running it as this launch would is found; one of another helper, launch
  // or a browser that crashed out is stopped and a new one started.
  async supervisor(tree, choice) {
    const f = (name) => path.join(this.folder, name)
    const launch = {
      v: 1,
      folder: this.folder,
      helper: this.config.helper,
      pin: this.config.pin,
      protocol: this.config.protocol,
      tree,
      chromium: choice,
      sandbox: this.config.sandbox,
      switches: this.config.switches,
      viewport: this.config.viewport,
      profile: f("profile"),
      output: f("output"),
      artifacts: f("artifacts"),
      sockets: { browser: f(BROWSER_SOCKET) },
      log: f("supervisor.log"),
      launch_timeout_ms: this.config.launch_timeout_ms,
      relaunch: this.config.relaunch,
      stop_grace_ms: this.config.stop_grace_ms,
      hello_ms: this.config.hello_ms,
      mcp: this.config.mcp,
    }
    const env = { ...this.childEnv }
    if (choice.route === "channel" && choice.browsers_path !== undefined) env.PLAYWRIGHT_BROWSERS_PATH = choice.browsers_path
    // The launch is named by everything its supervisor runs under: the whole
    // launch, its tree's path and every bound in it, and its environment.
    launch.digest = digest16({ launch, env })
    const launchFile = f("launch.json")
    const live = await this.live(launchFile)
    if (live !== undefined) {
      const found =
        (live.state === "ready" || live.state === "restarting") &&
        live.helper === launch.helper &&
        live.launch_digest === launch.digest &&
        (await this.listening(launch.sockets.browser))
      if (found) return { start: "found", version: live.chromium?.version ?? choice.version }
      await this.stop(live.pid)
    }
    try {
      await writeWhole(launchFile, JSON.stringify(launch, null, 1) + "\n")
    } catch (err) {
      throw new Withheld("folder_unusable", writeCause(err), `writing ${launchFile}: ${err.message}`)
    }
    const version = await this.start(launchFile, choice, env)
    return { start: live === undefined ? "started" : "restarted", version }
  }

  // live is the record of a supervisor running for this folder's launch.json,
  // or undefined: its supervisor.json naming a pid the kernel reports running
  // this helper's supervise on this folder, so a recycled pid is no supervisor.
  async live(launchFile) {
    const raw = await fsp.readFile(path.join(this.folder, "supervisor.json"), "utf8").catch(() => undefined)
    let record
    try {
      record = JSON.parse(raw)
    } catch {
      return undefined
    }
    if (!Number.isInteger(record?.pid)) return undefined
    if (!runs(await running(record.pid), [this.helper, "supervise", "--launch", launchFile])) return undefined
    return record
  }

  // listening is whether socket is a socket that takes a connection within
  // hello_ms; the connection is closed before it says a word, which the
  // supervisor reads as this look.
  async listening(socket) {
    try {
      if (!(await fsp.lstat(socket)).isSocket()) return false
    } catch {
      return false
    }
    return new Promise((resolve) => {
      const conn = net.connect(socket)
      const timer = this.clock.setTimeout(() => {
        conn.destroy()
        resolve(false)
      }, this.config.hello_ms)
      conn.once("connect", () => {
        this.clock.clearTimeout(timer)
        conn.destroy()
        resolve(true)
      })
      conn.once("error", () => {
        this.clock.clearTimeout(timer)
        resolve(false)
      })
    })
  }

  // stop ends a running supervisor: TERM, which stops its browser within its
  // grace, then its process group killed if it has not gone after twice that.
  async stop(pid) {
    signal(pid, "SIGTERM")
    if (await gone(pid, 2 * this.config.stop_grace_ms, this.clock)) return
    signal(pid, "SIGKILL", true)
    await gone(pid, this.config.stop_grace_ms, this.clock)
  }

  // kill ends a supervisor this ensure started and gave up on, with its group.
  kill(child) {
    if (child?.pid === undefined) return
    signal(child.pid, "SIGTERM")
    signal(child.pid, "SIGKILL", true)
  }

  // start starts the supervisor, detached in a session of its own, in env,
  // its output appended to supervisor.log, and waits for the one line it
  // writes on fd 3: ready and its browser's version, or failed and why, a pair
  // the helper prints. It answers the version, or throws Withheld.
  async start(launchFile, choice, env) {
    const logFile = path.join(this.folder, "supervisor.log")
    let log
    try {
      log = fs.openSync(logFile, "a", FILE_MODE)
    } catch (err) {
      throw new Withheld("folder_unusable", writeCause(err), `opening ${logFile}: ${err.message}`)
    }
    let child
    try {
      child = spawn(process.execPath, [this.helper, "supervise", "--launch", launchFile], {
        detached: true,
        stdio: ["ignore", log, log, "pipe"],
        env,
      })
    } finally {
      fs.closeSync(log)
    }
    this.child = child
    child.on("error", () => {})
    const said = await this.readyLine(child)
    if (said === "timeout") {
      this.kill(child)
      throw new Withheld("supervisor_failed", "timeout", `no word from the supervisor in ${this.config.supervisor_wait_ms} ms`)
    }
    const words = said.split(" ")
    if (words[0] === "ready" && words.length === 2) {
      child.unref()
      this.child = undefined
      return VERSION.test(words[1]) ? words[1] : choice.version
    }
    if (words[0] === "failed" && words.length === 3 && prints(words[1], words[2])) {
      throw new Withheld(words[1], words[2], `the supervisor failed: ${said}`)
    }
    throw new Withheld("supervisor_failed", "exited", `the supervisor ended saying ${JSON.stringify(said)}`)
  }

  // readyLine is the supervisor's line on fd 3, "" where it ended it with
  // none, or "timeout" past supervisor_wait_ms. A signal, never a look at
  // its record.
  readyLine(child) {
    return new Promise((resolve) => {
      let text = ""
      let said = false
      const done = (line) => {
        if (said) return
        said = true
        this.clock.clearTimeout(timer)
        child.stdio[3].removeAllListeners("data")
        child.stdio[3].destroy()
        resolve(line)
      }
      const timer = this.clock.setTimeout(() => done("timeout"), this.config.supervisor_wait_ms)
      child.stdio[3].setEncoding("utf8")
      child.stdio[3].on("data", (chunk) => {
        text += chunk
        const nl = text.indexOf("\n")
        if (nl >= 0) done(text.slice(0, nl))
        else if (text.length > SAID_MAX) done("")
      })
      child.stdio[3].on("end", () => done(""))
      child.stdio[3].on("error", () => done(""))
    })
  }
}

// settled waits for promise to settle, at most ms on the real clock.
async function settled(promise, ms) {
  let timer
  const bound = new Promise((resolve) => {
    timer = realClock.setTimeout(resolve, ms)
  })
  try {
    await Promise.race([promise.then(noop, noop), bound])
  } finally {
    realClock.clearTimeout(timer)
  }
}

function noop() {}

// takeLock takes folder's ensure.lock, so one ensure or stop at a time reads
// and starts what the box runs: created exclusively, naming its holder. A
// holder that is neither of this helper, or one past lock_takeover_ms (killed
// first), is taken over, one ensure at a time (takeOver); a younger one is
// waited for, at each change in the folder that watch announces and every
// LOCK_RECHECK_MS besides, which alone wakes the wait where the kernel
// refuses the watch (inotify's limits, which the model's own watchers reach).
// A lock it cannot write or remove is folder_unusable, for the cause that
// write stands for. live is whether the ensure a holder names still runs.
export async function takeLock(folder, config, { clock = realClock, io = fsp, watch = fs.watch, live } = {}) {
  const file = path.join(folder, "ensure.lock")
  const helper = path.join(folder, "helper.mjs")
  live ??= async (holder) => {
    const proc = await running(holder.pid)
    return runs(proc, [helper, "ensure"]) || runs(proc, [helper, "stop"])
  }
  const me = JSON.stringify({ pid: process.pid, started_ms: clock.now() })
  const unusable = (err) => new Withheld("folder_unusable", writeCause(err), `taking ${file}: ${err.message}`)
  let wake
  let watcher
  try {
    watcher = watch(folder, () => wake?.())
    watcher.on("error", () => wake?.())
  } catch {
    // Refused: the recheck alone wakes the wait.
  }
  try {
    for (;;) {
      const changed = new Promise((resolve) => {
        wake = resolve
      })
      try {
        await io.writeFile(file, me, { flag: "wx", mode: FILE_MODE })
        return
      } catch (err) {
        if (err?.code !== "EEXIST") throw unusable(err)
      }
      const raw = await io.readFile(file, "utf8").catch(() => undefined)
      const at = { config, clock, io, me, unusable }
      if ((await stale(file, raw, { ...at, live })) && (await takeOver(file, raw, at))) continue
      await Promise.race([changed, sleep(clock, LOCK_RECHECK_MS)])
    }
  } finally {
    watcher?.close()
  }
}

// takeOver removes the lock file holding raw, judged stale, under
// ensure.lock.takeover, made exclusively: two ensures that judged one lock
// at once would otherwise both remove it, the later removing the lock the
// first took meanwhile, and both hold it. Only the lock judged is removed,
// read again under the takeover. It answers false while another ensure's
// takeover is under way, which its few file operations end in milliseconds;
// a takeover file older than lock_takeover_ms is a dead ensure's, removed.
async function takeOver(file, raw, { config, clock, io, me, unusable }) {
  const takeover = `${file}.takeover`
  try {
    await io.writeFile(takeover, me, { flag: "wx", mode: FILE_MODE })
  } catch (err) {
    if (err?.code !== "EEXIST") throw unusable(err)
    const written = await io.stat(takeover).then(
      (st) => st.mtimeMs,
      () => undefined,
    )
    if (written !== undefined && clock.now() - written <= config.lock_takeover_ms) return false
    await io.rm(takeover, { force: true }).catch((err) => {
      throw unusable(err)
    })
    return true
  }
  let failed
  try {
    if ((await io.readFile(file, "utf8").catch(() => undefined)) === raw) await io.rm(file, { force: true })
  } catch (err) {
    failed = err
  }
  await io.rm(takeover, { force: true }).catch((err) => {
    failed ??= err
  })
  if (failed !== undefined) throw unusable(failed)
  return true
}

// stale is whether the lock file holds, as raw, is to be taken over: its
// holder no ensure of this helper (not live: gone, or its pid another
// process's now), or past lock_takeover_ms, which is killed first; or, where
// it names no holder yet, a file that old. A younger holder is waited for.
async function stale(file, raw, { config, clock, io, live }) {
  const holder = parseLock(raw)
  if (holder === undefined) {
    const written = await io.stat(file).then(
      (st) => st.mtimeMs,
      () => undefined,
    )
    return written === undefined || clock.now() - written > config.lock_takeover_ms
  }
  if (!(await live(holder))) return true
  if (clock.now() - holder.started_ms <= config.lock_takeover_ms) return false
  signal(holder.pid, "SIGKILL")
  await gone(holder.pid, config.stop_grace_ms, clock)
  return true
}

// parseLock is a lock's holder, or undefined for a lock not yet written or
// written by no ensure.
function parseLock(raw) {
  try {
    const holder = JSON.parse(raw)
    if (Number.isInteger(holder?.pid) && Number.isInteger(holder?.started_ms)) return holder
  } catch {
    // Not yet written, or garbled: read as a holder still writing it.
  }
  return undefined
}

// holdFolder holds folder to the box's user, before anything in it is read:
// a directory, theirs, at DIR_MODE, every file directly in it at FILE_MODE
// (the engine's writes take the modes the box's upload gives), and short
// enough a path that its sockets' paths fit. A failure throws
// folder_unusable with its cause.
export async function holdFolder(folder, io = fsp) {
  const unusable = (cause, detail) => new Withheld("folder_unusable", cause, detail)
  let info
  try {
    info = await io.lstat(folder)
  } catch (err) {
    throw unusable("not_directory", `${folder}: ${err.message}`)
  }
  if (!info.isDirectory()) throw unusable("not_directory", `${folder} is no directory`)
  if (info.uid !== process.getuid()) throw unusable("not_owned", `${folder} is user ${info.uid}'s`)
  try {
    await io.chmod(folder, DIR_MODE)
    for (const entry of await io.readdir(folder, { withFileTypes: true })) {
      if (entry.isFile()) await io.chmod(path.join(folder, entry.name), FILE_MODE)
    }
  } catch (err) {
    throw unusable("mode", err.message)
  }
  for (const name of [BROWSER_SOCKET, RELAY_SOCKET]) {
    const socket = path.join(folder, name)
    if (Buffer.byteLength(socket) > SOCKET_PATH_MAX) {
      throw unusable("path_too_long", `${socket} is ${Buffer.byteLength(socket)} bytes, past ${SOCKET_PATH_MAX}`)
    }
  }
}
