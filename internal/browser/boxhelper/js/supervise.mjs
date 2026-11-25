// supervise.mjs: `node helper.mjs supervise --launch <path>`, the box's one
// supervisor. It is the browser's parent: it launches Chromium through
// Playwright over a pipe, as launch.json says, kills a stale Chromium holding
// the profile first, holds a required sandbox to the command line the kernel
// holds, launches it again after a crash within a bound, serves it to the
// model through Playwright MCP on browser.sock, tells what attaches to it what
// it does, and on TERM closes it, killing its process group past a grace. Its
// process keeps a rejection policy of its own. internal/browser/boxhelper's
// package doc carries the argument.

import { randomBytes } from "node:crypto"
import fs from "node:fs"
import fsp from "node:fs/promises"
import os from "node:os"
import path from "node:path"

import { DIR_MODE, Withheld, carries, gone, realClock, running, signal, writeWhole } from "./common.mjs"
import { launchFailure } from "./chromium.mjs"
import { mcpConfig, serve } from "./serve.mjs"
import { load } from "./tree.mjs"

// LOG_MAX is supervisor.log's bound, with one rotation beside it, and
// ENTRY_MAX one entry's: the log is for a person debugging the box, and
// carries refusals by kind and errors by message, never a page's words.
const LOG_MAX = 1 << 20
const ENTRY_MAX = 512

// run is the supervisor as its own process, with attachments: its log in
// supervisor.log, its one line to ensure on fd 3, and its exit, 1 on a first
// launch that failed and 0 once stopped.
export async function run(file, attachments = []) {
  const clock = realClock
  const io = {
    clock,
    fs: fsp,
    log: fileLog(path.join(path.dirname(file), "supervisor.log"), clock),
    report: (line) => {
      try {
        fs.writeSync(3, `${line}\n`)
        fs.closeSync(3)
      } catch {
        // ensure gave up on the line and closed its end: nothing to tell.
      }
    },
    exit: (code) => process.exit(code),
  }
  const s = await supervise(file, io, attachments)
  if (s.state === "failed") io.exit(1)
  await s.stopped
  io.exit(0)
}

// supervise installs the process's policy, before anything is loaded, then
// starts the supervisor for the launch in file, and answers it once its first
// launch is ready or failed. io is {clock, fs, log, report, exit}: fs is the
// file system it reads a profile's lock through.
//
// Each of attachments is {attach(supervisor)}, which answers, once what it
// serves can be found and with nothing awaited of the browser, the hooks it
// is told by, each optional: onLaunched(context), each browser the supervisor
// serves, the first at once; onState(state); onModel(job), the model
// connection's, undefined once it closes; onTabs(job, action, answer), each
// browser_tabs call that moved the current tab (serve.mjs); and close(), first
// on a stop, waited for by nothing.
export async function supervise(file, io, attachments = []) {
  const s = new Supervisor(file, io, attachments)
  policy(s, io)
  await s.start()
  return s
}

// policy is the process's own: an unhandled rejection is logged and the
// process goes on, so one raised between jobs, when Playwright MCP listens
// for none, never ends the browser; an uncaught exception records the
// supervisor failed and exits 70, since nothing after one is to be trusted;
// TERM stops it; HUP and INT, which no one in its own session sends it, do
// nothing; and every file and socket it makes is the box user's alone.
function policy(s, io) {
  process.on("unhandledRejection", (reason) => io.log("unhandled_rejection", { reason: describe(reason) }))
  process.on("uncaughtException", (err) => {
    s.failSync("exited", err)
    io.exit(70)
  })
  process.on("SIGTERM", () => void s.stop())
  process.on("SIGHUP", () => {})
  process.on("SIGINT", () => {})
  process.umask(0o077)
}

// Supervisor is the box's browser and the record of it in supervisor.json.
// boot is drawn once and outlives every relaunch, so what an attachment counts
// under it survives a crash and starts again with a new supervisor.
export class Supervisor {
  constructor(file, io, attachments = []) {
    this.file = file
    this.folder = path.dirname(file)
    this.io = io
    this.clock = io.clock
    this.fs = io.fs
    this.boot = randomBytes(8).toString("hex")
    this.attachments = attachments
    this.attached = []
    this.started = this.clock.now()
    this.state = "starting"
    this.relaunches = []
    this.current = undefined
    this.relaunching = undefined
    this.failure = undefined
    this.writes = Promise.resolve()
    this.listeners = new Set()
    this.stopped = new Promise((resolve) => {
      this.markStopped = resolve
    })
  }

  // start reads the launch, loads the tree, launches the browser and serves
  // it; a launch that fails is recorded and reported, and the browser served
  // never.
  async start() {
    const launch = JSON.parse(await fsp.readFile(this.file, "utf8"))
    if (launch?.v !== 1) throw new Error(`${this.file} is of version ${launch?.v}, and this supervisor reads 1`)
    this.launch = launch
    await this.record()
    const { core, mcp } = load(launch.tree)
    this.chromium = core.chromium
    if (this.stopping) return
    this.launching = this.launchBrowser()
    try {
      this.current = await this.launching
    } catch (err) {
      const { reason, cause } = err instanceof Withheld ? err : await launchFailure(err, launch.sandbox)
      this.failure = { reason, cause, at_ms: this.clock.now() }
      this.io.log("launch_failed", { reason, cause, error: describe(err) })
      if (this.stopping) return
      await this.setState("failed")
      this.io.report(`failed ${reason} ${cause}`)
      return
    } finally {
      this.launching = undefined
    }
    // A stop that came while the browser launched closes it: nothing is served.
    if (this.stopping) return
    await this.serveBrowser(launch, mcp)
    await this.setState("ready")
    this.io.report(`ready ${this.current.version}`)
  }

  // serveBrowser listens on browser.sock, then attaches each attachment,
  // before the supervisor says ready, so what it serves is there to be found
  // when a job starts on that answer, and tells it the browser served now. One
  // that fails is logged and left out: what attaches never costs the model its
  // browser.
  async serveBrowser(launch, mcp) {
    this.server = await serve({
      socket: launch.sockets.browser,
      protocol: launch.protocol,
      helper: launch.helper,
      helloMs: launch.hello_ms,
      clock: this.clock,
      log: this.io.log,
      state: () => this.state,
      createConnection: mcp.createConnection,
      context: () => this.context(),
      config: mcpConfig(launch),
      onModel: (job) => this.tell("onModel", job),
      onTabs: (job, action, answer) => this.tell("onTabs", job, action, answer),
    })
    for (const a of this.attachments) {
      let hooks
      try {
        hooks = await a.attach(this)
      } catch (err) {
        this.io.log("attachment_failed", { hook: "attach", error: describe(err) })
        continue
      }
      if (hooks === undefined) continue
      this.attached.push(hooks)
      if (this.current !== undefined) this.tellOne(hooks, "onLaunched", this.current.context)
    }
  }

  // tell calls hook on every attachment with args.
  tell(hook, ...args) {
    for (const hooks of this.attached) this.tellOne(hooks, hook, ...args)
  }

  // tellOne calls hooks' hook, if it has one, with args, waiting for nothing:
  // a hook that throws or rejects is logged, and costs no other attachment,
  // nor the supervisor, anything.
  tellOne(hooks, hook, ...args) {
    const failed = (err) => this.io.log("attachment_failed", { hook, error: describe(err) })
    try {
      const told = hooks[hook]?.(...args)
      if (typeof told?.then === "function") told.then(undefined, failed)
    } catch (err) {
      failed(err)
    }
  }

  // launchBrowser launches Chromium on the profile, as launch.json says, once
  // no stale Chromium holds it, and answers it with its pid and version: never
  // one that ended before its launch was read, nor, under a required sandbox,
  // one that runs without it, each a Withheld. Whether it ended is looked at
  // after the last read, so nothing is awaited between that look and its use.
  async launchBrowser() {
    const launch = this.launch
    await this.killStale()
    await fsp.rm(launch.artifacts, { recursive: true, force: true })
    await fsp.mkdir(launch.artifacts, { recursive: true, mode: DIR_MODE })
    await fsp.mkdir(launch.output, { recursive: true, mode: DIR_MODE })
    const options = {
      headless: true,
      chromiumSandbox: launch.sandbox === "required",
      viewport: launch.viewport,
      ignoreDefaultArgs: launch.switches.ignore_default_args,
      args: launch.switches.args,
      artifactsDir: launch.artifacts,
      timeout: launch.launch_timeout_ms,
      handleSIGINT: false,
      handleSIGTERM: false,
      handleSIGHUP: false,
    }
    if (launch.chromium.route === "channel") options.channel = launch.chromium.channel
    else options.executablePath = launch.chromium.executable
    const context = await this.chromium.launchPersistentContext(launch.profile, options)
    const browser = context.browser()
    const g = { context, browser, pid: undefined, version: browser.version() }
    // Before anything is awaited, so a browser that ends while its launch is
    // read is noticed.
    const onGone = () => this.crashed(g)
    context.once("close", onGone)
    browser.once("disconnected", onGone)
    g.pid = (await lockHolder(launch.profile, this.fs))?.pid
    // Under required, the command line the kernel holds for it, by that pid,
    // read through the module's io, so the probe can fault it.
    let proc
    if (launch.sandbox === "required") proc = g.pid === undefined ? { alive: false } : await running(g.pid, this.fs)
    if (g.gone || !browser.isConnected()) {
      await this.discard(g)
      throw new Withheld("chromium_start_failed", "exited", "the browser ended before its launch was read")
    }
    const unsandboxed = proc === undefined ? undefined : unsandboxedBy(g.pid, proc)
    if (unsandboxed !== undefined) {
      await this.discard(g)
      throw unsandboxed
    }
    this.io.log("launched", { pid: g.pid, version: g.version })
    return g
  }

  // discard ends g, a browser launched and never to be served: its group
  // killed where its pid is known, then its context closed.
  async discard(g) {
    if (g.pid !== undefined) {
      try {
        signal(g.pid, "SIGKILL", true)
      } catch {
        // Not this user's to kill: the close below ends it.
      }
    }
    await g.context.close().catch(() => {})
  }

  // killStale kills the process group of a Chromium still holding the
  // profile, the orphan of a supervisor that died, before a launch: the one
  // its SingletonLock names, where that is this host and a process the kernel
  // reports running on this profile, so a recycled pid costs no other group
  // its life. A lock naming another host is left alone.
  async killStale() {
    const holder = await lockHolder(this.launch.profile, this.fs)
    if (holder === undefined || holder.host !== os.hostname()) return
    if (!carries(await running(holder.pid), `--user-data-dir=${this.launch.profile}`)) return
    this.io.log("stale_browser", { pid: holder.pid })
    if (!signal(holder.pid, "SIGKILL", true)) signal(holder.pid, "SIGKILL")
    await gone(holder.pid, this.launch.stop_grace_ms, this.clock)
  }

  // crashed is g gone without a stop: launched again on the same profile, at
  // most relaunch.max times in any relaunch.window_ms, and crashed out past
  // that.
  crashed(g) {
    if (g.gone) return
    g.gone = true
    if (this.stopping || this.current !== g) return
    this.current = undefined
    this.io.log("browser_gone", { pid: g.pid })
    this.again()
  }

  // again launches the browser again, or crashes out where the window holds
  // relaunch.max already; a relaunch that fails is one of them.
  again() {
    const now = this.clock.now()
    const recent = this.relaunches.filter((at) => now - at < this.launch.relaunch.window_ms)
    if (recent.length >= this.launch.relaunch.max) {
      this.relaunches = recent
      this.relaunching = undefined
      void this.setState("crashed_out")
      return
    }
    this.relaunches = [...recent, now]
    void this.setState("restarting")
    this.relaunching = this.launchBrowser().then(
      async (g) => {
        this.relaunching = undefined
        if (this.stopping) {
          await g.context.close().catch(() => {})
          return
        }
        this.current = g
        this.tell("onLaunched", g.context)
        await this.setState("ready")
      },
      (err) => {
        this.relaunching = undefined
        this.io.log("relaunch_failed", { error: describe(err) })
        if (!this.stopping) this.again()
      },
    )
  }

  // context is the browser context the model is served: the current one,
  // once any relaunch under way settles, or a refusal saying why there is
  // none.
  async context() {
    while (this.relaunching !== undefined) await this.relaunching
    if (this.state === "crashed_out") {
      const { max, window_ms: windowMs } = this.launch.relaunch
      throw new Error(
        `The browser kept crashing: it was started again ${max} times in ${windowMs / 60000} minutes, and starts again when this box's next job starts.`,
      )
    }
    if (this.current === undefined) throw new Error("The browser is not running.")
    return this.current.context
  }

  // stop closes every attachment, then its socket and every model
  // connection, then the browser through Playwright, Chromium's own close over
  // its pipe, so it writes its profile; past stop_grace_ms it kills
  // Chromium's process group. Then the record says stopped. Whatever a step
  // meets, the stop ends in stopped, which the process exits on.
  stop() {
    if (this.stopping) return this.stopped
    this.stopping = true
    void (async () => {
      try {
        this.tell("close")
        this.attached = []
        await this.server?.close()
        await this.launching?.catch(() => {})
        while (this.relaunching !== undefined) await this.relaunching
        const g = this.current
        if (g !== undefined) {
          let timer
          const graced = new Promise((resolve) => {
            timer = this.clock.setTimeout(() => resolve(false), this.launch.stop_grace_ms)
          })
          this.io.log("closing_browser", { pid: g.pid })
          const closed = g.context.close().then(
            () => true,
            () => true,
          )
          const done = await Promise.race([closed, graced])
          this.clock.clearTimeout(timer)
          if (!done && g.pid !== undefined) {
            // The kill and its record are one: Playwright kills what it
            // launched as this process exits, and its own close gives up after
            // 30 s, so the kill's own evidence is the record of it sent.
            this.io.log("browser_killed", { pid: g.pid, sent: signal(g.pid, "SIGKILL", true) })
            await gone(g.pid, this.launch.stop_grace_ms, this.clock)
          }
        }
        await this.setState("stopped")
      } finally {
        this.markStopped()
      }
    })()
    return this.stopped
  }

  // setState records state, and tells whoever waits on one. A record it
  // cannot write is logged, never thrown: the state holds in memory, its
  // callers go on, and the next state's record is written whole.
  async setState(state) {
    this.state = state
    try {
      await this.record()
    } catch (err) {
      this.io.log("record_failed", { state, error: describe(err) })
    }
    for (const listener of this.listeners) listener(state)
    this.tell("onState", state)
  }

  // record writes supervisor.json whole, through the module's io, one write
  // at a time: ensure's one source for what runs here. Each is tried whatever
  // became of the one before, so a write that failed (a full disk) costs only
  // itself, and a later state still lands.
  record() {
    const body = () => {
      const g = this.current
      return `${JSON.stringify({
        v: 1,
        helper: this.launch?.helper,
        launch_digest: this.launch?.digest,
        pid: process.pid,
        boot: this.boot,
        started_ms: this.started,
        state: this.state,
        chromium:
          g === undefined
            ? undefined
            : {
                pid: g.pid,
                version: g.version,
                major: Number(g.version.split(".")[0]),
                build: this.launch.chromium.build,
                sandbox: this.launch.sandbox,
              },
        relaunches_ms: this.relaunches,
        failure: this.failure,
      })}\n`
    }
    const file = path.join(this.folder, "supervisor.json")
    this.writes = this.writes.catch(() => {}).then(() => writeWhole(file, body(), this.fs))
    return this.writes
  }

  // failSync records the supervisor failed with cause, as an uncaught
  // exception leaves it: synchronously, since the process ends next.
  failSync(cause, err) {
    this.io.log("uncaught_exception", { error: describe(err) })
    try {
      const file = path.join(this.folder, "supervisor.json")
      fs.writeFileSync(`${file}.failed`, `${JSON.stringify({ v: 1, pid: process.pid, state: "failed", failure: { reason: "supervisor_failed", cause, at_ms: this.clock.now() } })}\n`, { mode: 0o600 })
      fs.renameSync(`${file}.failed`, file)
    } catch {
      // Ensure finds no live supervisor of this launch either way.
    }
  }
}

// unsandboxedBy is the Withheld a browser launched under a required sandbox
// stands for, by proc, the command line the kernel holds for pid, its
// profile's lock's, or undefined where that carries no --no-sandbox: a
// launcher on PATH may add it (Debian's /usr/bin/chromium takes flags from
// /etc/chromium.d), and that browser is withheld as the launcher's, never run
// as required. One whose command line cannot be read, gone or not (the kernel
// holds none once its main thread exits), is held to nothing, so it is
// withheld too.
function unsandboxedBy(pid, proc) {
  if (proc.argv === undefined && proc.command === undefined) {
    return new Withheld("chromium_sandbox_unavailable", "unknown", `no command line to read for the browser, pid ${pid}`)
  }
  if (!carries(proc, "--no-sandbox")) return undefined
  return new Withheld("chromium_sandbox_unavailable", "launcher", `the browser, pid ${pid}, runs with --no-sandbox`)
}

// lockHolder is who the profile's SingletonLock names, {host, pid}, or
// undefined, read through io: the link Chromium's process singleton keeps,
// <host>-<pid>, which Chromium itself reads to tell whether a profile is in
// use. It needs no process table, which macOS does not have under /proc.
async function lockHolder(profile, io) {
  try {
    const target = await io.readlink(path.join(profile, "SingletonLock"))
    const at = target.lastIndexOf("-")
    const pid = Number(target.slice(at + 1))
    if (at <= 0 || !Number.isInteger(pid) || pid <= 0) return undefined
    return { host: target.slice(0, at), pid }
  } catch {
    return undefined
  }
}

// describe is an error or a rejection's reason, bounded for the log.
function describe(reason) {
  return String(reason?.message ?? reason).slice(0, ENTRY_MAX)
}

// fileLog is the supervisor's log: one line an entry on its standard error,
// which ensure opened onto supervisor.log for appending. At LOG_MAX the file
// is copied aside as supervisor.log.1 and the descriptor truncated, so its
// output keeps the file it was handed.
function fileLog(file, clock) {
  return (kind, fields = {}) => {
    const line = `${new Date(clock.now()).toISOString()} ${kind} ${JSON.stringify(fields).slice(0, ENTRY_MAX)}\n`
    try {
      if (fs.fstatSync(2).size + line.length > LOG_MAX) {
        fs.copyFileSync(file, `${file}.1`)
        fs.ftruncateSync(2, 0)
      }
      fs.writeSync(2, line)
    } catch {
      // A log that cannot be written stops nothing the box runs.
    }
  }
}
