// tree.mjs: the locked Playwright tree in a box. It installs the lock the
// engine wrote beside it with the box's own npm, into a directory of its own,
// checks what npm installed against the lock and the files a whole install
// holds, publishes it by rename, stops what an install a dead helper left
// still runs, loads a published tree through @playwright/mcp's own resolver,
// and sweeps every other entry of pw/ once nothing runs on one.
// internal/browser/boxhelper's package doc carries the argument.

import { spawn } from "node:child_process"
import { createHash } from "node:crypto"
import fsp from "node:fs/promises"
import { createRequire } from "node:module"
import path from "node:path"

import {
  DIR_MODE,
  FILE_MODE,
  Withheld,
  addressable,
  askVersion,
  gone,
  isObject,
  realClock,
  running,
  signal,
  writeCause,
  writeWhole,
} from "./common.mjs"

// LAYOUT is the version of how this module lays a tree out: the directory npm
// installs it into, its .complete, and what makes one ready. A tree's name
// carries it, so a tree laid out otherwise is never read as one laid out this
// way. It moves with a change to any of those and with nothing else.
export const LAYOUT = 1

// MANIFEST and LOCK are the lock's two files in the helper's folder, as the
// engine writes them: names neither npm nor Node reads as the folder's own.
export const MANIFEST = "tree-package.json"
export const LOCK = "tree-package-lock.json"

// RECORD is an install's record of the npm it started, in the directory npm
// installs into, and RECORD_MAX its bound: a pid and a start take under 128
// bytes, so a longer file is no record the helper wrote.
const RECORD = "install.json"
const RECORD_MAX = 256

// STDOUT_MAX is how much of npm's stdout an install keeps, its --json answer:
// an error is about 1 KiB, and npm 11's list of three packages about as much.
// STDERR_TAIL is how much of the end of its stderr a failure's detail
// carries: a refused connection with its stack runs about 1.5 KiB. The rest
// is read and dropped, so npm never blocks on a full pipe.
const STDOUT_MAX = 64 << 10
const STDERR_TAIL = 4 << 10

// NPM_VERSION is npm's answer to --version: three numbers, then a
// prerelease's tag if any (12.0.0-pre.1), a version of its major.
const NPM_VERSION = /^([0-9]+)\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/

const MISSING = "playwright_missing"

// ABORTED is the detail of an install its caller aborted at its deadline.
const ABORTED = "the install's caller aborted it at its deadline"

// treeName is the directory a config's tree is published at under pw/: named
// by what decides its bytes, the release, the lock's digest and LAYOUT.
export function treeName(config) {
  return `${config.pin}-${config.pin_digest}-l${LAYOUT}`
}

// install makes config's tree ready in folder's pw/ and answers its name,
// where it is and whether this call installed it, or throws Withheld,
// playwright_missing with its cause. A published tree is reused with nothing
// read. env is the environment npm runs in, the helper's whole; io the file
// system; clock what npm's bound runs on; signal an AbortSignal whose abort
// kills npm at once (a caller's deadline); spawned is handed each npm's pid.
export async function install(folder, config, options = {}) {
  const { env = process.env, io = fsp, clock = realClock, signal: abort, spawned = () => {} } = options
  // Whatever npm writes is the box's user's alone.
  process.umask(0o077)
  const pw = path.join(folder, "pw")
  await reapInstalls(pw, config.stop_grace_ms, { io, clock })
  const name = treeName(config)
  const dir = path.join(pw, name)
  if (await complete(dir, config.pin_digest, io)) return { name, dir, installed: false }
  const { manifest, lock } = await readLock(folder, config.pin_digest, io)
  await askNPM(env, config.npm_floor, { clock, abort })

  let staging
  try {
    await hold(pw, io)
    staging = await io.mkdtemp(path.join(pw, `${name}.`))
    await io.writeFile(path.join(staging, "package.json"), manifest, { mode: FILE_MODE, flag: "wx" })
    await io.writeFile(path.join(staging, "package-lock.json"), lock, { mode: FILE_MODE, flag: "wx" })
  } catch (err) {
    await discard(staging, io)
    throw new Withheld(MISSING, writeCause(err), `staging the install in ${pw}: ${err.message}`)
  }
  try {
    judge(await runNPM(staging, config, { env, io, clock, abort, spawned }), config)
    const problems = await checkInstalled(staging, lock, config.installed_digest, io)
    if (problems.length > 0) {
      const words = problems.map((p) => p.words).join("; ")
      throw new Withheld(MISSING, "install_failed", `npm ci answered 0, and ${words}`)
    }
    try {
      await io.rm(path.join(staging, RECORD), { force: true })
      await io.writeFile(path.join(staging, ".complete"), `${config.pin_digest}\n`, { mode: FILE_MODE, flag: "wx" })
      await io.rm(dir, { recursive: true, force: true })
      await io.rename(staging, dir)
    } catch (err) {
      throw new Withheld(MISSING, writeCause(err), `publishing the tree at ${dir}: ${err.message}`)
    }
    staging = undefined
  } finally {
    // What a failure staged goes with it, npm reaped first, so a later install
    // starts clean. The failure is the answer, and a staged directory that
    // will not go is no tree: only the rename publishes one.
    await discard(staging, io)
  }
  return { name, dir, installed: true }
}

// complete is whether dir is a published tree of digest: a directory, never a
// link, holding a .complete that names digest, written after every other file.
async function complete(dir, digest, io) {
  try {
    if (!(await io.lstat(dir)).isDirectory()) return false
    const marker = path.join(dir, ".complete")
    if (!(await io.lstat(marker)).isFile()) return false
    return (await io.readFile(marker, "utf8")) === `${digest}\n`
  } catch {
    return false
  }
}

// readLock is the lock's two files the engine wrote into folder, held to the
// digest config.json names: missing, or other bytes, is digest_mismatch, which
// the engine's next write repairs; any other failure to read, its cause.
async function readLock(folder, digest, io) {
  const read = async (name) => {
    try {
      return await io.readFile(path.join(folder, name))
    } catch (err) {
      if (err?.code === "ENOENT" || err?.code === "ENOTDIR") {
        throw new Withheld(MISSING, "digest_mismatch", `${name} is not in the helper's folder`)
      }
      throw new Withheld(MISSING, writeCause(err), `reading ${name}: ${err.message}`)
    }
  }
  const manifest = await read(MANIFEST)
  const lock = await read(LOCK)
  if (pinDigest(manifest, lock) !== digest) {
    throw new Withheld(MISSING, "digest_mismatch", `${MANIFEST} and ${LOCK} are not the lock config.json names`)
  }
  return { manifest, lock }
}

// pinDigest names the lock's two files as the engine does (boxhelper's
// pinDigest): the first 12 hex digits of SHA-256 over each file's hex SHA-256
// and a newline.
function pinDigest(manifest, lock) {
  const hex = (body) => createHash("sha256").update(body).digest("hex")
  return createHash("sha256").update(`${hex(manifest)}\n${hex(lock)}\n`).digest("hex").slice(0, 12)
}

// askNPM holds the box's npm to floor, asked on clock (askVersion): one the
// box's PATH lacks, or that it cannot run, is npm_missing; one older than
// floor, or that answers --version with no version or not within
// VERSION_PROBE_MS, npm_too_old. --loglevel=error rides along, since an
// unknown level in the box's npm settings makes npm answer nothing at all.
async function askNPM(env, floor, { clock, abort }) {
  const args = ["--version", "--loglevel=error"]
  const { said, error, aborted } = await askVersion("npm", args, { env, clock, signal: abort })
  if (aborted) throw new Withheld(MISSING, "timeout", ABORTED)
  if (error?.code === "ENOENT" || error?.code === "EACCES") {
    throw new Withheld(MISSING, "npm_missing", `no npm the box can run: ${error.message}`)
  }
  const m = error === undefined ? NPM_VERSION.exec(said) : null
  if (m === null) {
    const how = error === undefined ? `answered ${JSON.stringify(said)}` : `failed: ${error.message}`
    throw new Withheld(MISSING, "npm_too_old", `npm --version ${how}, no version`)
  }
  if (Number(m[1]) < floor) {
    const detail = `npm ${said} is older than npm ${floor}, the oldest the helper installs with`
    throw new Withheld(MISSING, "npm_too_old", detail)
  }
  return said
}

// hold makes pw/ a directory of the box's user alone: one there is held at
// DIR_MODE, one missing made, and a link or a file in its place replaced.
async function hold(pw, io) {
  let st
  try {
    st = await io.lstat(pw)
  } catch (err) {
    if (err?.code !== "ENOENT") throw err
  }
  if (st?.isDirectory()) return io.chmod(pw, DIR_MODE)
  if (st !== undefined) await io.unlink(pw)
  await io.mkdir(pw, { mode: DIR_MODE })
}

// sweep removes every entry of folder's pw/ but the tree named name: trees of
// another release, lock or layout, and whatever an install left. Its caller
// holds that no browser runs on any of them, and the install's reap killed
// every npm a record names; one that would not go (the kernel holds it) keeps
// its directory, which the next reap removes once it has gone, since npm is
// reaped before its directory goes. An entry that will not go stays for the
// next sweep, since the browser it follows is up. It answers what it removed.
export async function sweep(folder, name, io = fsp) {
  const pw = path.join(folder, "pw")
  let entries
  try {
    entries = await io.readdir(pw)
  } catch {
    return []
  }
  const removed = []
  for (const entry of entries) {
    if (entry === name) continue
    if (await stillRuns(await readRecord(path.join(pw, entry), io))) continue
    try {
      await io.rm(path.join(pw, entry), { recursive: true, force: true })
      removed.push(entry)
    } catch {
      // Left for the next sweep.
    }
  }
  return removed
}

// discard removes what an install staged, if anything; one that will not go
// stays in pw/, under a name no reuse reads as a tree, until the sweep.
async function discard(staging, io) {
  if (staging !== undefined) await io.rm(staging, { recursive: true, force: true }).catch(() => {})
}

// npmArgs is the command line npm ci runs with in staging, under config.json's
// install bounds. Every setting that decides what is installed is on it, since
// the command line beats the environment and every npmrc: a dry or lock-only
// run installs nothing; the linked layout or bin links leave links; global, in
// either spelling, makes npm ci refuse; never sends the lock's addresses past
// the box's registry; prefer-online refetches a warm cache; no audit, fund,
// update check or progress, so no request but the tarballs; no log file. The
// log level decides what the helper reads of npm, its --json answer. Where
// the bytes come from stays the box's: no --cache, --offline or --registry,
// nor --prefix, under which npm reads its global npmrc from the prefix. npm
// installs into staging, its working directory, which holds the lock, where
// --no-workspaces ends its search for the project.
function npmArgs(bounds) {
  const wait = bounds.fetch_retry_wait_ms
  return [
    "ci",
    "--ignore-scripts",
    "--install-strategy=hoisted",
    "--no-bin-links",
    "--no-workspaces",
    "--no-global",
    "--location=user",
    "--no-dry-run",
    "--no-package-lock-only",
    "--no-prefer-online",
    "--replace-registry-host=npmjs",
    "--no-audit",
    "--no-fund",
    "--no-update-notifier",
    "--no-progress",
    "--logs-max=0",
    "--loglevel=error",
    "--json",
    `--fetch-timeout=${bounds.fetch_timeout_ms}`,
    `--fetch-retries=${bounds.fetch_retries}`,
    `--fetch-retry-mintimeout=${wait}`,
    `--fetch-retry-maxtimeout=${wait}`,
  ]
}

// runNPM runs npm ci in staging, in a process group of its own, and records
// its pid and start in staging's RECORD for a later reap. It is bounded: TERM
// at install.timeout_ms and the group killed stop_grace_ms later, since npm
// defers its exit to an outstanding fetch, or killed at once on an abort. It
// answers once npm is reaped, its pipes are closed and its record is written,
// so nothing of it lands after the caller removes staging.
function runNPM(staging, config, { env, io, clock, abort, spawned }) {
  return new Promise((resolve) => {
    if (abort?.aborted) return resolve({ aborted: true })
    let child
    try {
      child = spawn("npm", npmArgs(config.install), {
        cwd: staging,
        env,
        stdio: ["ignore", "pipe", "pipe"],
        detached: true,
      })
    } catch (err) {
      return resolve({ spawnError: err })
    }
    const run = { bounded: false, aborted: false }
    const stdout = []
    let kept = 0
    let stderr = Buffer.alloc(0)
    let term
    let kill
    let recorded = Promise.resolve()
    const group = (sig) => {
      try {
        signal(child.pid, sig, true)
      } catch {
        // Nobody left to stop, or nobody this user may stop: the close says which.
      }
    }
    const onAbort = () => {
      run.aborted = true
      group("SIGKILL")
    }
    child.stdout.on("data", (chunk) => {
      if (kept >= STDOUT_MAX) return
      stdout.push(chunk.subarray(0, STDOUT_MAX - kept))
      kept += Math.min(chunk.length, STDOUT_MAX - kept)
    })
    child.stderr.on("data", (chunk) => {
      stderr = Buffer.concat([stderr, chunk])
      if (stderr.length > STDERR_TAIL) stderr = stderr.subarray(stderr.length - STDERR_TAIL)
    })
    child.once("error", (err) => {
      if (child.pid === undefined) run.spawnError = err
    })
    child.once("spawn", () => {
      spawned(child.pid)
      recorded = (async () => {
        try {
          const { start } = await running(child.pid)
          await writeWhole(path.join(staging, RECORD), JSON.stringify({ pid: child.pid, start }), io)
        } catch (err) {
          run.recordError = err
          group("SIGKILL")
        }
      })()
      term = clock.setTimeout(() => {
        run.bounded = true
        group("SIGTERM")
        kill = clock.setTimeout(() => group("SIGKILL"), config.stop_grace_ms)
      }, config.install.timeout_ms)
      abort?.addEventListener("abort", onAbort, { once: true })
      if (abort?.aborted) onAbort()
    })
    child.once("close", async (status, sig) => {
      clock.clearTimeout(term)
      clock.clearTimeout(kill)
      abort?.removeEventListener("abort", onAbort)
      await recorded
      run.status = status
      run.signal = sig
      run.stdout = Buffer.concat(stdout).toString("utf8")
      run.stderr = stderr.toString("utf8").trim()
      resolve(run)
    })
  })
}

// judge throws the Withheld a run of npm stands for, if any: npm that would
// not start, a record that could not be written, npm stopped by its bound or
// an abort, or npm's own failure, read by npmFailure. A run that answered 0 is
// judged by what it installed (checkInstalled), never by its JSON.
function judge(run, config) {
  if (run.spawnError !== undefined) {
    const code = run.spawnError.code
    const cause = code === "ENOENT" || code === "EACCES" ? "npm_missing" : "install_failed"
    throw new Withheld(MISSING, cause, `starting npm: ${run.spawnError.message}`)
  }
  if (run.recordError !== undefined) {
    throw new Withheld(MISSING, writeCause(run.recordError), `recording npm's pid: ${run.recordError.message}`)
  }
  if (run.status === 0) return
  if (run.aborted) throw new Withheld(MISSING, "timeout", ABORTED)
  if (run.bounded) {
    throw new Withheld(MISSING, "timeout", `npm had not installed the tree within ${config.install.timeout_ms} ms`)
  }
  const { code, cause } = npmFailure(run.stdout)
  const how = run.status === null ? `ended by ${run.signal}` : `exited ${run.status}`
  throw new Withheld(MISSING, cause, `npm ci ${how} (${code ?? "no code"}): ${run.stderr}`)
}

// The codes npm's --json answer gives for a registry, mirror or proxy it could
// not reach or that failed on its side: a connection's own, @npmcli/agent's
// timeouts and its refused proxy, make-fetch-happen's redirects, a miss
// offline, and the TLS layer failing against the peer: EPROTO, a host name the
// peer's certificate does not name, every refusal of the certificate itself
// (CERTIFICATE), and every OpenSSL code (TLS_FAILURE), as an https address
// that answers plain HTTP gives ERR_SSL_WRONG_VERSION_NUMBER.
const UNREACHABLE = new Set([
  "ECONNREFUSED",
  "ECONNRESET",
  "ENOTFOUND",
  "EAI_AGAIN",
  "ETIMEDOUT",
  "ENETUNREACH",
  "EHOSTUNREACH",
  "ENETDOWN",
  "ECONNABORTED",
  "EPIPE",
  "FETCH_ERROR",
  "ENOTCACHED",
  "EIDLETIMEOUT",
  "ECONNECTIONTIMEOUT",
  "ERESPONSETIMEOUT",
  "ETRANSFERTIMEOUT",
  "EINVALIDPROXY",
  "EMAXREDIRECT",
  "EINVALIDREDIRECT",
  "ENOREDIRECT",
  "EPROTO",
  "ERR_TLS_CERT_ALTNAME_INVALID",
])
const TLS_FAILURE = /^ERR_SSL_[A-Z0-9_]+$/

// CERTIFICATE is every code Node gives a certificate its TLS check refused
// (its X509 codes), OUT_OF_MEM aside, which is the box's own memory.
const CERTIFICATE = new Set([
  "UNABLE_TO_GET_ISSUER_CERT",
  "UNABLE_TO_GET_CRL",
  "UNABLE_TO_DECRYPT_CERT_SIGNATURE",
  "UNABLE_TO_DECRYPT_CRL_SIGNATURE",
  "UNABLE_TO_DECODE_ISSUER_PUBLIC_KEY",
  "CERT_SIGNATURE_FAILURE",
  "CRL_SIGNATURE_FAILURE",
  "CERT_NOT_YET_VALID",
  "CERT_HAS_EXPIRED",
  "CRL_NOT_YET_VALID",
  "CRL_HAS_EXPIRED",
  "ERROR_IN_CERT_NOT_BEFORE_FIELD",
  "ERROR_IN_CERT_NOT_AFTER_FIELD",
  "ERROR_IN_CRL_LAST_UPDATE_FIELD",
  "ERROR_IN_CRL_NEXT_UPDATE_FIELD",
  "DEPTH_ZERO_SELF_SIGNED_CERT",
  "SELF_SIGNED_CERT_IN_CHAIN",
  "UNABLE_TO_GET_ISSUER_CERT_LOCALLY",
  "UNABLE_TO_VERIFY_LEAF_SIGNATURE",
  "CERT_CHAIN_TOO_LONG",
  "CERT_REVOKED",
  "INVALID_CA",
  "PATH_LENGTH_EXCEEDED",
  "INVALID_PURPOSE",
  "CERT_UNTRUSTED",
  "CERT_REJECTED",
  "HOSTNAME_MISMATCH",
])

// UNWRITABLE is the codes of a path npm was refused, a read-only image cache
// that misses an entry among them.
const UNWRITABLE = new Set(["EACCES", "EPERM", "EROFS"])

// NOT_PUBLISHED is the codes of a tarball whose bytes are not the published
// ones: the lock's SHA-512 refusing them, or zlib refusing to inflate them,
// which the published bytes, installed whole by the suite, never meet. npm
// inflates as it hashes, so which of the two it reports is a race inside npm,
// whose fetcher (pacote's isDataCorruptionError) takes both for corrupt data.
const NOT_PUBLISHED = new Set(["EINTEGRITY", "Z_DATA_ERROR"])

// npmFailure reads npm's --json answer, stdout whole, into its code and the
// cause it stands for. An HTTP status code is read as docs/concepts/
// code-sandbox.md reads one: 408, 429 and 5xx a registry failing on its side,
// any other 4xx (EOTP and EAUTHIP too) one refusing, a mirror lacking the
// release or wanting a credential. A code it does not read, or none (npm's
// JSON carries no errno, so a spent quota is UNKNOWN), is install_failed.
export function npmFailure(stdout) {
  let code
  try {
    code = JSON.parse(stdout)?.error?.code
  } catch {
    // No JSON answer, so no code.
  }
  if (typeof code !== "string") return { code: undefined, cause: "install_failed" }
  const status = /^E([0-9]{3})$/.exec(code)
  if (status !== null) {
    const n = Number(status[1])
    if (n === 408 || n === 429 || (n >= 500 && n <= 599)) return { code, cause: "registry_unreachable" }
    if (n >= 400 && n <= 499) return { code, cause: "registry_refused" }
    return { code, cause: "install_failed" }
  }
  if (NOT_PUBLISHED.has(code)) return { code, cause: "integrity_mismatch" }
  if (code === "EOTP" || code === "EAUTHIP") return { code, cause: "registry_refused" }
  if (UNREACHABLE.has(code) || CERTIFICATE.has(code) || TLS_FAILURE.test(code)) {
    return { code, cause: "registry_unreachable" }
  }
  if (code === "ENOSPC") return { code, cause: "no_space" }
  if (UNWRITABLE.has(code)) return { code, cause: "unwritable" }
  return { code, cause: "install_failed" }
}

// checkInstalled is every way the install in dir falls short of the lock
// (its bytes) and of digest, the files a whole install of it holds, and none
// when npm installed exactly the lock: npm's own record of what it installed
// names the lock's packages, each at the lock's version, address and SHA-512;
// nothing in dir is a link, or anything but a file or a directory; and
// installedDigest of its node_modules is digest. Each problem is {words}; the
// one that the files' digest is not digest also carries files, the digest
// they have, since alone it is no failure of npm's: a file npm could not
// write, or a digest of another lock's, which the suite tree's recipe tells a
// bump.
export async function checkInstalled(dir, lock, digest, io = fsp) {
  let want
  try {
    want = JSON.parse(String(lock)).packages
  } catch (err) {
    return [{ words: `the lock is no JSON: ${err.message}` }]
  }
  const problems = []
  try {
    const record = JSON.parse(await io.readFile(path.join(dir, "node_modules", ".package-lock.json"), "utf8"))
    const odd = differing(isObject(want) ? want : {}, isObject(record?.packages) ? record.packages : {})
    if (odd.length > 0) problems.push({ words: `npm's record of what it installed is not the lock: ${few(odd)}` })
  } catch (err) {
    problems.push({ words: `npm left no record of what it installed: ${err.message}` })
  }
  try {
    const { others } = await entries(dir, io)
    if (others.length > 0) problems.push({ words: `it holds what is neither a file nor a directory: ${few(others)}` })
    const got = await installedDigest(path.join(dir, "node_modules"), io)
    if (got !== digest) {
      problems.push({ words: `the files it installed have the digest ${got}, not ${digest}`, files: got })
    }
  } catch (err) {
    problems.push({ words: `reading what it installed: ${err.message}` })
  }
  return problems
}

// differing is every package the lock and npm's record of what it installed
// disagree on: in one alone, or of another version, address or SHA-512. The
// lock's root, "", is no package npm installs.
function differing(want, got) {
  const locked = { ...want }
  delete locked[""]
  const keys = [...new Set([...Object.keys(locked), ...Object.keys(got)])].sort()
  const fields = ["version", "resolved", "integrity"]
  return keys.filter((key) => fields.some((f) => locked[key]?.[f] === undefined || locked[key][f] !== got[key]?.[f]))
}

// FEW is how many names a failure's words give: enough to see what differs,
// a package or a file, with the rest counted, so a tree npm left empty reads
// as a line rather than every file it lacks.
const FEW = 5

// few is the first FEW of names, for a failure's words.
function few(names) {
  const shown = names.slice(0, FEW).join(", ")
  return names.length > FEW ? `${shown} and ${names.length - FEW} more` : shown
}

// installedDigest is the digest of the files an install left in nodeModules:
// the hex SHA-256 over one line per regular file, npm's own record
// (.package-lock.json) aside, `<path>\0<hex SHA-256 of its bytes>\n`, the
// path relative to nodeModules and slash-separated, the lines in byte order.
// Modes and directories are no part of it: the umask decides the one, and the
// files the other. The package doc says how a bump regenerates the digest of
// the lock's whole install, tree/installed.sha256.
export async function installedDigest(nodeModules, io = fsp) {
  const lines = []
  for (const rel of (await entries(nodeModules, io)).files) {
    if (rel === ".package-lock.json") continue
    const body = await io.readFile(path.join(nodeModules, ...rel.split("/")))
    lines.push(Buffer.from(`${rel}\0${createHash("sha256").update(body).digest("hex")}\n`))
  }
  lines.sort(Buffer.compare)
  return createHash("sha256").update(Buffer.concat(lines)).digest("hex")
}

// entries is what lies under root, by slash path relative to it: every
// regular file, and everything that is neither a file nor a directory, a link
// among them, never followed.
async function entries(root, io) {
  const files = []
  const others = []
  const walk = async (dir, rel) => {
    for (const d of await io.readdir(dir, { withFileTypes: true })) {
      const at = rel === "" ? d.name : `${rel}/${d.name}`
      if (d.isDirectory()) await walk(path.join(dir, d.name), at)
      else if (d.isFile()) files.push(at)
      else others.push(at)
    }
  }
  await walk(root, "")
  return { files, others }
}

// reapInstalls stops what an install a dead helper left in pw/ still runs,
// before anything is reused: for each directory holding a record, it kills the
// recorded npm's group while the kernel still reports that pid with the start
// the record names, so a pid handed out again, whatever it runs, is left
// alone; waits up to graceMs for it to go; then removes the directory. One
// whose npm will not go is kept for a later reap, and one with no record is
// the sweep's. It answers the directories it removed.
export async function reapInstalls(pw, graceMs, { io = fsp, clock = realClock } = {}) {
  let names
  try {
    if (!(await io.lstat(pw)).isDirectory()) return []
    names = await io.readdir(pw)
  } catch {
    // Nothing to reap in a pw/ that is not there, or that the install itself
    // will fail to use, naming why.
    return []
  }
  const removed = []
  for (const entry of names) {
    const dir = path.join(pw, entry)
    const record = await readRecord(dir, io)
    if (record === undefined) continue
    if (await stillRuns(record)) {
      try {
        signal(record.pid, "SIGKILL", true)
      } catch {
        // Not this user's to stop: the wait below keeps its directory.
      }
      if (!(await gone(record.pid, graceMs, clock))) continue
    }
    try {
      await io.rm(dir, { recursive: true, force: true })
      removed.push(entry)
    } catch {
      // Kept for a later reap: nothing it recorded runs.
    }
  }
  return removed
}

// stillRuns is whether the npm record names still runs: the kernel reports
// its pid with the start the record names, so a pid handed out again, whatever
// it runs, is none.
async function stillRuns(record) {
  if (record?.pid === undefined) return false
  const proc = await running(record.pid)
  return proc.alive && proc.start !== undefined && proc.start === record.start
}

// readRecord is the record of the npm an install started in dir: undefined
// where dir is no directory or holds none, {} for a file longer than any the
// helper writes, and otherwise what recordOf reads in it.
async function readRecord(dir, io) {
  let body
  try {
    if (!(await io.lstat(dir)).isDirectory()) return undefined
    const file = path.join(dir, RECORD)
    const st = await io.lstat(file)
    if (!st.isFile() || st.size > RECORD_MAX) return {}
    body = await io.readFile(file, "utf8")
  } catch {
    return undefined
  }
  return recordOf(body)
}

// recordOf is what a record's body names: its npm's pid and start where it is
// one the helper wrote, and {} for one it never wrote, which names no process.
// A pid that is not addressable is none it wrote: a record lies where any
// process of the box's user may write, and one naming pid 1 would turn the
// reap's kill of a group into a kill of every process.
export function recordOf(body) {
  try {
    const r = JSON.parse(body)
    if (isObject(r) && addressable(r.pid) && typeof r.start === "string") return { pid: r.pid, start: r.start }
  } catch {
    // Not JSON: no record the helper wrote.
  }
  return {}
}

// load is an installed tree's two packages: @playwright/mcp from the tree, and
// playwright-core through mcp's own resolver, anchored at its package.json,
// which its exports allow, so the core the helper drives is the one mcp's
// server runs on: one instance by construction, not by hoisting.
export function load(dir) {
  const fromTree = createRequire(path.join(dir, "index.js"))
  const fromMCP = createRequire(fromTree.resolve("@playwright/mcp/package.json"))
  return { mcp: fromTree("@playwright/mcp"), core: fromMCP("playwright-core"), require: fromMCP }
}
