// common.mjs: what the box helper's modules share. config.json, the document
// the engine writes beside them; the modes the helper writes at; its sockets'
// names; what the helper prints, the answer a failed check gives and the line
// that says it; the longest line the shim and the supervisor pass; the cause a
// failed write stands for; the clock every timer runs on, and a version asked
// on it; canonical JSON; and what the kernel says of a process, and which pid
// may be signalled. Node's built-ins alone, in the oldest Node the helper
// supports. internal/browser/boxhelper's package doc carries the argument.

import { execFile, spawn } from "node:child_process"
import { createHash } from "node:crypto"
import fsp from "node:fs/promises"
import { constants } from "node:os"
import path from "node:path"

// CONFIG_VERSION is the shape of config.json this helper reads. A document of
// a later shape is refused whole, never read short of what it says.
export const CONFIG_VERSION = 1

// FILE_MODE and DIR_MODE are what the helper writes at: the box's user alone.
export const FILE_MODE = 0o600
export const DIR_MODE = 0o700

// BROWSER_SOCKET and RELAY_SOCKET are the sockets the supervisor listens on
// in the helper's folder: the model's way to the browser, and the way a job's
// relay reaches what attaches to the supervisor.
export const BROWSER_SOCKET = "browser.sock"
export const RELAY_SOCKET = "relay.sock"

// LINE_PREFIX starts ensure's one line, the engine's to parse.
export const LINE_PREFIX = "crewlet-browser-ensure/1"

// LINE_MAX is the longest line the shim and the supervisor pass each other,
// in bytes, its newline aside: MCP's stdio framing, newline-delimited JSON,
// bounded where a full-page screenshot's answer, a few MB of base64, fits
// many times over.
export const LINE_MAX = 32 << 20

// VERSION_PROBE_MS bounds one --version the helper asks a program: a build or
// an npm answers in well under a second (npm in 0.13-0.18 s), so one silent
// for ten has hung, and a caller keeps room for several inside its deadline.
// askVersion holds a program to it on its caller's clock.
export const VERSION_PROBE_MS = 10_000

// isObject is whether v is a JSON object, never an array or null.
export function isObject(v) {
  return v !== null && typeof v === "object" && !Array.isArray(v)
}

// PRINTS is every reason the helper prints, with the causes it prints each
// with: with the ensure prelude's two, exactly the box's part of
// internal/browser's table, which TestTheHelperPrintsTheReasonsTheEngineReads
// holds both ways. A Withheld of a pair it lacks throws, so a misspelt cause
// fails where it is printed rather than reaching the engine.
export const PRINTS = Object.freeze({
  node_too_old: Object.freeze(["below_floor"]),
  folder_unusable: Object.freeze(["not_directory", "not_owned", "mode", "path_too_long", "no_space", "unwritable"]),
  playwright_missing: Object.freeze([
    "digest_mismatch",
    "npm_missing",
    "npm_too_old",
    "registry_unreachable",
    "registry_refused",
    "integrity_mismatch",
    "timeout",
    "no_space",
    "unwritable",
    "install_failed",
  ]),
  chromium_missing: Object.freeze(["not_found"]),
  chromium_sandbox_unavailable: Object.freeze(["root", "seccomp", "userns", "apparmor", "launcher", "unknown"]),
  chromium_start_failed: Object.freeze(["libraries", "profile_held", "exited", "timeout", "unknown"]),
  supervisor_failed: Object.freeze(["exited", "timeout"]),
})

// prints is whether the helper prints reason with cause.
export function prints(reason, cause) {
  return Object.hasOwn(PRINTS, reason) && PRINTS[reason].includes(cause)
}

// Withheld is a check that failed: the reason and the cause the helper
// prints, a pair of PRINTS, and a detail for its stderr, which the engine
// never reads.
export class Withheld extends Error {
  constructor(reason, cause, detail) {
    if (!prints(reason, cause)) {
      throw new TypeError(`the helper prints no ${reason} ${cause}: PRINTS lists every pair it may print`)
    }
    super(`${reason} ${cause}: ${detail}`)
    this.reason = reason
    this.cause = cause
    this.detail = detail
  }
}

// ensureLine is ensure's answer: the verdict, then each field that has a
// value, in fields' order.
export function ensureLine(verdict, fields) {
  const parts = [LINE_PREFIX, verdict]
  for (const [key, value] of Object.entries(fields)) {
    if (value !== undefined && value !== "") parts.push(`${key}=${value}`)
  }
  return parts.join(" ")
}

// writeCause is the cause a failed write in the box stands for: a full disk
// or a spent quota is no_space, and anything else, a refused, read-only or
// vanished path among them, is unwritable. A quota is told by its errno:
// libuv names no EDQUOT, so its code is never that name.
export function writeCause(err) {
  return err?.code === "ENOSPC" || err?.errno === -constants.errno.EDQUOT ? "no_space" : "unwritable"
}

// realClock is the clock every timer of the helper runs on: a module takes a
// clock as an argument and reads time and sets timers through it alone, so
// the probe runs it on a clock of its own.
export const realClock = {
  now: () => Date.now(),
  setTimeout: (f, ms) => setTimeout(f, ms),
  clearTimeout: (handle) => clearTimeout(handle),
}

// sleep resolves after ms on clock.
export function sleep(clock, ms) {
  return new Promise((resolve) => clock.setTimeout(resolve, ms))
}

// canonical is value as JSON with every object's keys sorted and no space:
// one text for one value, so a digest over it names the value.
export function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`
  if (isObject(value)) {
    const keys = Object.keys(value).sort()
    return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(",")}}`
  }
  return JSON.stringify(value)
}

// digest16 is the first 16 hex digits of SHA-256 over value's canonical JSON.
export function digest16(value) {
  return createHash("sha256").update(canonical(value)).digest("hex").slice(0, 16)
}

// writeWhole writes body to file whole: staged beside it at FILE_MODE and
// renamed over it, so a reader finds the old file or the new one.
export async function writeWhole(file, body, io = fsp) {
  const staged = `${file}.${process.pid}.tmp`
  try {
    await io.writeFile(staged, body, { mode: FILE_MODE })
    await io.rename(staged, file)
  } catch (err) {
    await io.rm(staged, { force: true }).catch(() => {})
    throw err
  }
}

// grammar is what each text config.json carries must match. The pin takes a
// prerelease's tag, as npm's registry names one (@playwright/mcp's next is
// 0.0.79-alpha-1787869812000), bounded at PIN_MAX: a tree's directory name
// and the helper's answer carry it.
const grammar = {
  helper: /^[0-9a-f]{16}$/,
  pin: /^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/,
  pin_digest: /^[0-9a-f]{12}$/,
  installed_digest: /^[0-9a-f]{64}$/,
}
const PIN_MAX = 64

// shapes is what each of config.json's lists and Chromium values must match:
// a build's revision and four-number version; a Playwright browsers directory
// (the one variable Playwright reads for it, an absolute path, or one under
// the box's home); an absolute path; a program's name; a switch; a variable's
// name.
const shapes = {
  revision: /^[0-9]{1,8}$/,
  version: /^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}\.[0-9]{1,6}$/,
  playwright: /^(\$PLAYWRIGHT_BROWSERS_PATH|\/.+|~\/.+)$/,
  absolute: /^\/.+$/,
  name: /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/,
  switch: /^--[a-z0-9-]+(=[\x20-\x7e]+)?$/,
  env: /^[A-Za-z_][A-Za-z0-9_]{0,63}$/,
}

// BOUND_MAX is the longest bound config.json may carry, in milliseconds: an
// hour, past every bound the helper keeps, so a longer one is no engine's.
const BOUND_MAX = 3_600_000

// FLOOR_MAX is the highest major config.json may name as a floor: three
// digits, past any release of Node or npm for decades, so a larger one is no
// engine's.
const FLOOR_MAX = 999

// RETRIES_MAX is the most fetch retries config.json may ask of npm: five times
// npm's own two, where the engine asks one, so more is no engine's.
const RETRIES_MAX = 10

// PROTOCOL_MAX is the highest hello protocol config.json may name: the engine
// writes 1 and moves it only for a change an older peer cannot read, so three
// digits are past any it will write.
const PROTOCOL_MAX = 999

// VIEWPORT_MAX is the widest or tallest viewport config.json may ask, in CSS
// pixels: twice an 8K display's 7,680, where the engine asks 1280 by 800, so a
// larger one is no engine's.
const VIEWPORT_MAX = 16_384

// RELAUNCH_MAX is the most relaunches config.json may allow in a window, and
// WINDOW_MAX the longest window, in milliseconds: the engine allows three in
// ten minutes, and a hundred cold starts, or crashes a day apart counted as
// one cause, are past any policy that still gives the box its browser back.
const RELAUNCH_MAX = 100
const WINDOW_MAX = 86_400_000

// OUTPUT_MAX is the most bytes config.json may let the outputs a model leaves
// unnamed take: a tebibyte, past any box's disk, where the engine allows 256
// MiB, so a larger one is no engine's.
const OUTPUT_MAX = 2 ** 40

// BOUNDS is each of ensure's and the supervisor's bounds config.json carries.
const BOUNDS = [
  "ensure_deadline_ms",
  "lock_takeover_ms",
  "supervisor_wait_ms",
  "launch_timeout_ms",
  "stop_grace_ms",
  "hello_ms",
]

// readConfig is folder's config.json, held to the shape this helper reads:
// one it cannot read throws, naming what is wrong.
export async function readConfig(folder, io = fsp) {
  const file = path.join(folder, "config.json")
  let config
  try {
    config = JSON.parse(await io.readFile(file, "utf8"))
  } catch (err) {
    throw new Error(`${file} cannot be read: ${err.message}`, { cause: err })
  }
  const refuse = (key) => {
    throw new Error(`${file} has ${key} ${JSON.stringify(config?.[key])}`)
  }
  const count = (v, max) => Number.isInteger(v) && v > 0 && v <= max
  const list = (v, re) => Array.isArray(v) && v.every((s) => typeof s === "string" && re.test(s))
  if (!isObject(config)) throw new Error(`${file} is not an object`)
  if (!Number.isInteger(config.v) || config.v < 1) refuse("v")
  if (config.v > CONFIG_VERSION) {
    const reads = `this helper reads ${CONFIG_VERSION}: the engine writes both`
    throw new Error(`${file} is of version ${config.v}, and ${reads}`)
  }
  for (const [key, re] of Object.entries(grammar)) {
    if (typeof config[key] !== "string" || !re.test(config[key])) refuse(key)
  }
  if (config.pin.length > PIN_MAX) refuse("pin")
  if (!count(config.protocol, PROTOCOL_MAX)) refuse("protocol")
  if (!count(config.node_floor, FLOOR_MAX)) refuse("node_floor")
  if (!count(config.npm_floor, FLOOR_MAX)) refuse("npm_floor")
  const i = config.install
  if (
    !isObject(i) ||
    !count(i.timeout_ms, BOUND_MAX) ||
    !count(i.fetch_timeout_ms, BOUND_MAX) ||
    !Number.isInteger(i.fetch_retries) ||
    i.fetch_retries < 0 ||
    i.fetch_retries > RETRIES_MAX ||
    !count(i.fetch_retry_wait_ms, BOUND_MAX)
  ) {
    refuse("install")
  }
  const c = config.chromium
  const g = shapes
  if (
    !isObject(c) ||
    !g.revision.test(String(c.expected_revision)) ||
    !g.version.test(String(c.expected_version)) ||
    !isObject(c.search) ||
    !list(c.search.playwright, g.playwright) ||
    !list(c.search.chrome, g.absolute) ||
    !list(c.search.path, g.name)
  ) {
    refuse("chromium")
  }
  if (config.sandbox !== "required" && config.sandbox !== "waived") refuse("sandbox")
  const view = config.viewport
  if (!isObject(view) || !count(view.width, VIEWPORT_MAX) || !count(view.height, VIEWPORT_MAX)) {
    refuse("viewport")
  }
  const s = config.switches
  if (!isObject(s) || !list(s.ignore_default_args, g.switch) || !list(s.args, g.switch)) refuse("switches")
  if (!list(config.supervisor_env, g.env)) refuse("supervisor_env")
  for (const key of BOUNDS) {
    if (!count(config[key], BOUND_MAX)) refuse(key)
  }
  const r = config.relaunch
  if (!isObject(r) || !count(r.max, RELAUNCH_MAX) || !count(r.window_ms, WINDOW_MAX)) refuse("relaunch")
  const m = config.mcp
  if (
    !isObject(m) ||
    !count(m.output_max_bytes, OUTPUT_MAX) ||
    !count(m.action_ms, BOUND_MAX) ||
    !count(m.navigation_ms, BOUND_MAX)
  ) {
    refuse("mcp")
  }
  return config
}

// bootID is the boot the kernel's tick counts belong to, read once: ticks
// restart at every boot and a record outlives one, which is why
// internal/procgroup pairs them too. undefined where the kernel does not say,
// and then no start is told, so no record matches a process.
let bootID
function linuxBoot() {
  bootID ??= fsp.readFile("/proc/sys/kernel/random/boot_id", "utf8").then(
    (s) => s.trim() || undefined,
    () => undefined,
  )
  return bootID
}

// running is what the kernel says of pid: whether it can still run (it is
// reported, and a thread of it has not exited); its command line, which the
// kernel holds until the main thread exits, exactly on Linux and as one
// string elsewhere, or where a process rewrote its title into one, as
// Chromium does in its new headless mode; and its start, which a pid handed
// out again never shares: field 22 of its stat on Linux, with the boot, and
// ps's lstart elsewhere. A pid it does not report, or will not describe, is
// gone. io is the file system /proc is read through.
export async function running(pid, io = fsp) {
  if (!Number.isInteger(pid) || pid <= 0) return { alive: false }
  if (process.platform === "linux") {
    try {
      const stat = await io.readFile(`/proc/${pid}/stat`, "utf8")
      // The fields after the command, which may hold any byte but its last
      // ")": the state first, so field n is fields[n - 3].
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ")
      // The state is the MAIN THREAD's: Z is the process over only where
      // num_threads, field 20, counts that thread alone, since every other is
      // released as it exits, and Node's main thread reads Z for milliseconds
      // while another finishes its exit; X is the reap, as is x on kernels
      // 2.6.33 to 3.13. internal/procgroup's parseProcStat reads them so, and
      // says why.
      const over = fields[0] === "X" || fields[0] === "x" || (fields[0] === "Z" && Number(fields[17]) <= 1)
      if (over) return { alive: false }
      const boot = await linuxBoot()
      const start = boot !== undefined && /^[0-9]+$/.test(fields[19] ?? "") ? `${boot}:${fields[19]}` : undefined
      const cmdline = await io.readFile(`/proc/${pid}/cmdline`, "utf8")
      if (cmdline === "") return { alive: true, start }
      const argv = cmdline.split("\0").slice(0, -1)
      if (argv.length === 1 && argv[0].includes(" ")) return { alive: true, command: argv[0], start }
      return { alive: true, argv, start }
    } catch {
      return { alive: false }
    }
  }
  const out = await new Promise((resolve) => {
    const env = { ...process.env, LC_ALL: "C" }
    execFile("/bin/ps", ["-ww", "-o", "stat=,lstart=,args=", "-p", String(pid)], { env }, (err, stdout) =>
      resolve(err ? "" : stdout),
    )
  })
  // lstart is five words under the C locale: Sat Nov 21 20:40:01 2026.
  const m = /^\s*(\S+)\s+(\S+\s+\S+\s+\d+\s+[\d:]+\s+\d+)\s+(.*?)\s*$/s.exec(out)
  if (m === null || m[1].startsWith("Z")) return { alive: false }
  return { alive: true, command: m[3], start: m[2].split(/\s+/).join(" ") }
}

// runs is whether proc, as running read it, was started with args after its
// executable: exactly on Linux, and as the end of its command elsewhere,
// which is exact for arguments that hold no space. Without a command line,
// gone or not, it runs nothing.
export function runs(proc, args) {
  if (proc.argv) return proc.argv.length === args.length + 1 && args.every((a, i) => proc.argv[i + 1] === a)
  return proc.command !== undefined && proc.command.endsWith(` ${args.join(" ")}`)
}

// carries is whether proc, as running read it, holds arg among its
// arguments: exactly on Linux, as one space-delimited word elsewhere; never
// without a command line.
export function carries(proc, arg) {
  if (proc.argv) return proc.argv.slice(1).includes(arg)
  return proc.command !== undefined && ` ${proc.command} `.includes(` ${arg} `)
}

// signal sends sig to pid, or to the process group it leads with group, and
// reports whether there was anyone to send it to: nobody at a pid that is not
// addressable.
export function signal(pid, sig, group = false) {
  if (!addressable(pid)) return false
  try {
    process.kill(group ? -pid : pid, sig)
    return true
  } catch (err) {
    if (err?.code === "ESRCH") return false
    throw err
  }
}

// addressable is whether pid names one process, or one group, the helper may
// signal. kill(-1) is every process the box's user may signal, which a group
// signal to pid 1 would send, and kill(0) the sender's own group; pid 1 itself
// is init's for the box's life, never a process the helper started. So a pid
// of 1 or less is nobody's: internal/procgroup's rule, held here because a pid
// the helper signals is often read back from a file the box's user can write.
export function addressable(pid) {
  return Number.isInteger(pid) && pid > 1
}

// POLL_MS is how often the helper looks again at a process it waits to see
// go, which nothing announces to a process that is not its parent: a
// signalled process goes within a tick or two, and every such wait is
// bounded far above this.
export const POLL_MS = 50

// gone waits until pid no longer runs, at most ms on clock, and answers
// whether it went. The kernel is looked at again on the real clock, since a
// process goes in real time whatever clock the bound is held to.
export async function gone(pid, ms, clock) {
  const until = clock.now() + ms
  for (;;) {
    if (!(await running(pid)).alive) return true
    if (clock.now() >= until) return false
    await sleep(realClock, POLL_MS)
  }
}

// VERSION_MAX is the longest answer to --version the helper reads, in bytes:
// npm's and a build's are under 100, so a longer one is no version, and the
// whole of one fits a failure's words.
const VERSION_MAX = 1 << 10

// askVersion is what file, run in env with args, answers to a --version, its
// stdout trimmed: {said} once it exits 0 within VERSION_PROBE_MS on clock;
// {aborted} once signal aborts; otherwise {error}, its spawn's (ENOENT or
// EACCES), its exit's, an answer past VERSION_MAX or the bound's. Killed at
// the bound or the abort, it answers only once the program is reaped, and its
// bound is the clock's, as every other is, never a timer of child_process's.
export function askVersion(file, args, { env = process.env, clock = realClock, signal: abort } = {}) {
  return new Promise((resolve) => {
    if (abort?.aborted) return resolve({ aborted: true })
    let child
    try {
      child = spawn(file, args, { env, stdio: ["ignore", "pipe", "ignore"] })
    } catch (err) {
      return resolve({ error: err })
    }
    const out = []
    let read = 0
    let ended
    const end = (how) => {
      ended ??= how
      child.kill("SIGKILL")
    }
    child.stdout.on("data", (chunk) => {
      read += chunk.length
      if (read > VERSION_MAX) return end({ error: new Error(`it answered more than ${VERSION_MAX} bytes`) })
      out.push(chunk)
    })
    const silent = () => end({ error: new Error(`no answer within ${VERSION_PROBE_MS} ms`) })
    const bound = clock.setTimeout(silent, VERSION_PROBE_MS)
    const onAbort = () => end({ aborted: true })
    abort?.addEventListener("abort", onAbort, { once: true })
    child.on("error", (err) => {
      ended ??= { error: err }
    })
    child.once("close", (status, sig) => {
      clock.clearTimeout(bound)
      abort?.removeEventListener("abort", onAbort)
      if (ended === undefined && status !== 0) ended = { error: new Error(`it exited ${status ?? sig}`) }
      resolve(ended ?? { said: Buffer.concat(out).toString("utf8").trim() })
    })
  })
}
