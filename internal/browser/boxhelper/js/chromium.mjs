// chromium.mjs: which Chromium a box runs, and what its failures stand for.
// It walks config.json's search in the order the build is trusted, asks each
// candidate its version, records whether it is the build the pinned
// Playwright expects, and reads a failed launch into ensure's closed reasons
// and causes. internal/browser/boxhelper's package doc carries the argument.

import fsp from "node:fs/promises"
import os from "node:os"
import path from "node:path"

import { Withheld, askVersion } from "./common.mjs"

// EXECUTABLE is where a Chrome for Testing build keeps its executable inside
// its directory, per platform: Playwright's own layout for its chromium.
const EXECUTABLE = {
  "linux-x64": ["chrome-linux64", "chrome"],
  "linux-arm64": ["chrome-linux-arm64", "chrome"],
  "darwin-x64": ["chrome-mac-x64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"],
  "darwin-arm64": ["chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"],
}

// MARKER is the file Playwright's installer writes last into a build's
// directory, so a directory without it holds a build not wholly installed.
const MARKER = "INSTALLATION_COMPLETE"

// choose is the Chromium the box runs, by config.json's search, first found:
// the build the pinned Playwright expects, at its revision in a Playwright
// browsers directory and answering exactly the expected version, launched
// through Playwright's chromium channel (build expected); a branded Chrome
// where Playwright's chrome channel finds it; a Chromium on PATH by name;
// then any other revision in a Playwright directory, newest first (each of
// those untested). A candidate that cannot say its version is passed over;
// none left throws chromium_missing not_found. env is ensure's own, for PATH
// and Playwright's variable; each candidate is asked under childEnv, the
// environment it would run in.
export async function choose(config, { env = process.env, childEnv = env, home = os.homedir(), probe = version } = {}) {
  const { search, expected_revision: revision, expected_version: expected } = config.chromium
  const dirs = playwrightDirs(search.playwright, env, home)
  const builds = []
  for (const dir of dirs) {
    for (const rev of await revisions(dir)) {
      const executable = path.join(dir, `chromium-${rev}`, ...executablePath())
      if (await installed(path.join(dir, `chromium-${rev}`), executable)) builds.push({ dir, rev, executable })
    }
  }
  for (const b of builds.filter((b) => b.rev === revision)) {
    if ((await probe(b.executable, childEnv)) === expected) {
      return { route: "channel", channel: "chromium", browsers_path: b.dir, executable: b.executable, version: expected, build: "expected" }
    }
  }
  const candidates = [
    ...(await existing(search.chrome)).map((executable) => ({ route: "channel", channel: "chrome", executable })),
    ...(await onPath(search.path, env)).map((executable) => ({ route: "executable", executable })),
    ...builds.sort((a, b) => Number(b.rev) - Number(a.rev)).map((b) => ({ route: "executable", executable: b.executable })),
  ]
  for (const c of candidates) {
    const v = await probe(c.executable, childEnv)
    if (v !== undefined) return { ...c, version: v, build: "untested" }
  }
  throw new Withheld("chromium_missing", "not_found", `no Chromium answered in ${JSON.stringify(search)}`)
}

// playwrightDirs is search's Playwright directories, each made absolute: the
// variable Playwright reads, where set to an absolute path, a path under the
// box's home, or the path as written.
function playwrightDirs(entries, env, home) {
  const out = []
  for (const entry of entries) {
    if (entry === "$PLAYWRIGHT_BROWSERS_PATH") {
      const v = env.PLAYWRIGHT_BROWSERS_PATH
      if (v && path.isAbsolute(v)) out.push(v)
    } else if (entry.startsWith("~/")) {
      out.push(path.join(home, entry.slice(2)))
    } else {
      out.push(entry)
    }
  }
  return out
}

// executablePath is this platform's executable inside a build's directory.
function executablePath() {
  const parts = EXECUTABLE[`${process.platform}-${process.arch}`]
  if (parts === undefined) throw new Withheld("chromium_missing", "not_found", `no Chrome for Testing for ${process.platform}-${process.arch}`)
  return parts
}

// revisions is every chromium-<revision> directory's revision in dir.
async function revisions(dir) {
  try {
    return (await fsp.readdir(dir)).map((e) => /^chromium-([0-9]{1,8})$/.exec(e)?.[1]).filter((r) => r !== undefined)
  } catch {
    return []
  }
}

// installed is whether a build's directory is wholly installed: its marker
// written, and its executable a file this user may run.
async function installed(dir, executable) {
  try {
    await fsp.access(path.join(dir, MARKER))
    return await executableFile(executable)
  } catch {
    return false
  }
}

// executableFile is whether file is a regular file, through links, that this
// user may run.
async function executableFile(file) {
  try {
    if (!(await fsp.stat(file)).isFile()) return false
    await fsp.access(file, fsp.constants.X_OK)
    return true
  } catch {
    return false
  }
}

// existing is each of files that is a runnable file, in order.
async function existing(files) {
  const out = []
  for (const f of files) if (await executableFile(f)) out.push(f)
  return out
}

// onPath is each of names found on env's PATH, in order, each at the first
// directory holding it; only absolute directories are searched.
async function onPath(names, env) {
  const dirs = String(env.PATH ?? "").split(":").filter((d) => path.isAbsolute(d))
  const out = []
  for (const name of names) {
    for (const dir of dirs) {
      if (await executableFile(path.join(dir, name))) {
        out.push(path.join(dir, name))
        break
      }
    }
  }
  return out
}

// FOUR_NUMBERS is a Chromium's version, a word of its answer to --version:
// Chrome for Testing ends its answer with it, and a distribution's build says
// more after it (Debian's: Chromium 119.0.6045.123 built on Debian 12.2).
const FOUR_NUMBERS = /(?:^|\s)([0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}\.[0-9]{1,6})(?=\s|$)/

// version is the first four-number version word executable answers to
// --version, or undefined for one that does not answer one within
// VERSION_PROBE_MS (askVersion).
export async function version(executable, env) {
  const { said } = await askVersion(executable, ["--version"], { env })
  const m = said === undefined ? null : FOUR_NUMBERS.exec(said)
  return m === null ? undefined : m[1]
}

// sandboxCause is what the box shows of why Chromium's own sandbox would not
// start, the first that holds: run as root; Ubuntu's AppArmor restricting
// unprivileged user namespaces; user namespaces turned off; a seccomp filter
// already on the process, as a container runtime's default profile installs;
// or unknown. read is a file's text, trimmed, or undefined.
export async function sandboxCause({ uid = process.getuid(), read = readTrimmed } = {}) {
  if (uid === 0) return "root"
  if ((await read("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")) === "1") return "apparmor"
  if (
    (await read("/proc/sys/user/max_user_namespaces")) === "0" ||
    (await read("/proc/sys/kernel/unprivileged_userns_clone")) === "0"
  ) {
    return "userns"
  }
  if (/^Seccomp:\t2$/m.test((await read("/proc/self/status")) ?? "")) return "seccomp"
  return "unknown"
}

async function readTrimmed(file) {
  try {
    return (await fsp.readFile(file, "utf8")).trim()
  } catch {
    return undefined
  }
}

// MARKERS is the text each cause of a failed launch is told by, in the order
// read, each one a launch here can carry alone: the dynamic loader's words
// for a library it cannot load, and dlerror's for one Chromium loads itself;
// Chromium's for a profile whose lock it cannot take, and for one another
// Chromium of this host serves, each of which Playwright's rewrite keeps; and
// the exit code Chromium gives a profile in use, in Playwright's log of the
// exit. Playwright MCP's own launcher reads three of them. A browser that ends
// before Playwright reaches it over its pipe reads as a closed target.
const MARKERS = [
  ["libraries", ["error while loading shared libraries", "cannot open shared object file"]],
  ["profile_held", ["ProcessSingleton", "Opening in existing browser session", "exitCode=21"]],
]
const EXITED = ["Target page, context or browser has been closed"]

// launchFailure is a failed launch read as the Withheld it stands for, under
// the cell's sandbox setting: Playwright's rewrite of Chromium's sandbox
// failure, under required, is chromium_sandbox_unavailable with the cause the
// box shows (cause, sandboxCause's answer); anything else is
// chromium_start_failed, by the markers above, Playwright's timeout, its words
// for a browser that ended, or unknown.
export async function launchFailure(err, sandbox, cause = sandboxCause) {
  const message = String(err?.message ?? err)
  const failed = (reason, c) => new Withheld(reason, c, message)
  if (sandbox === "required" && message.includes("Chromium sandboxing failed!")) {
    return failed("chromium_sandbox_unavailable", await cause())
  }
  for (const [c, markers] of MARKERS) {
    if (markers.some((m) => message.includes(m))) return failed("chromium_start_failed", c)
  }
  if (err?.name === "TimeoutError") return failed("chromium_start_failed", "timeout")
  if (EXITED.some((m) => message.includes(m))) return failed("chromium_start_failed", "exited")
  return failed("chromium_start_failed", "unknown")
}
