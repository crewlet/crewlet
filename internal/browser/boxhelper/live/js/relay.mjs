// relay.mjs: `node helper.mjs relay --wiring <file>`, one job's relay. It
// reads its run's route from the wiring file, says its job's hello on
// relay.sock, and carries the box's frames, state and check-ins to the run's
// bridge, directly, holding one poll whose answer says whether anybody
// watches. Each answer is read as the contract says: retried, dropped, or the
// relay's end. It leaves on TERM within term_ms, once its parent script is
// gone, or once the bridge says its token or its browser is gone.
// internal/browser/boxhelper/live's package doc carries the argument.

import fs from "node:fs"
import net from "node:net"
import path from "node:path"

import { RELAY_SOCKET, isObject, readConfig, realClock } from "./common.mjs"
import { Failed, dialer } from "./dial.mjs"
import { FROM_SUPERVISOR, Reader, Refused, VERSION, Writer } from "./frames.mjs"

// The exits a relay's job script and its log read.
export const USAGE = 64
export const UNAVAILABLE = 69
export const SOFTWARE = 70

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

// LOG_ENTRY_MAX bounds one line of the relay's log.
const LOG_ENTRY_MAX = 512

// main is the relay as its own process: the wiring at the one flag's path
// read and held to its shape, the config read, and its policy installed, the
// relay run until it leaves.
export async function main(folder, flags) {
  const file = flags[1]
  if (flags.length !== 2 || flags[0] !== "--wiring" || !path.isAbsolute(file ?? "")) {
    return refuse(USAGE, `relay takes --wiring and a wiring file's absolute path, and was given ${flags.length} arguments`)
  }
  let config
  try {
    config = await readConfig(folder)
  } catch (err) {
    return refuse(UNAVAILABLE, `the helper's config cannot be read: ${err.message}`)
  }
  const wiring = await readWiring(file, config)
  if (typeof wiring === "string") return refuse(USAGE, `${file} is no wiring this relay takes: ${wiring}`)
  const clock = realClock
  const log = fileLog(clock, config.relay.log_bytes)
  const dial = dialer({ clock, bodyBytes: config.relay.body_bytes })
  const relay = new Relay({
    config,
    wiring,
    socket: path.join(folder, RELAY_SOCKET),
    io: {
      clock,
      log,
      exit: (code) => process.exit(code),
      ppid: () => process.ppid,
      connect: (socket) => net.connect(socket),
      request: (url, o) => dial.request(url, o),
      random: Math.random,
    },
  })
  process.on("unhandledRejection", (reason) => log("unhandled_rejection", { error: errorCode(reason) }))
  process.on("uncaughtException", (err) => {
    log("uncaught_exception", { error: errorCode(err) })
    process.exit(SOFTWARE)
  })
  for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) process.on(sig, () => relay.leave(sig))
  relay.start()
}

// refuse ends the relay before it starts, saying why in one line.
function refuse(code, why) {
  try {
    fs.writeSync(2, `relay: ${why}\n`)
  } catch {
    // Its log closed: the exit alone says it.
  }
  process.exit(code)
}

// readWiring is file's wiring, or why it is none this relay takes, in words
// that never repeat its content: its address holds the run's token.
export async function readWiring(file, config, io = fs.promises) {
  let w
  try {
    w = JSON.parse(await io.readFile(file, "utf8"))
  } catch (err) {
    return `it cannot be read as JSON (${errorCode(err)})`
  }
  if (!isObject(w)) return "it is no JSON object"
  if (w.v !== config.relay.wiring_v) return `its v is not ${config.relay.wiring_v}, the version this relay reads`
  if (typeof w.job !== "string" || !UUID.test(w.job)) return "its job is no lower-case launch id"
  if (!route(w.url)) return "its url is no absolute http or https address ending /browser"
  return { url: w.url, job: w.job }
}

// route is whether url is a route a wiring names: an absolute http or https
// address, no user, query or fragment, its path ending /browser.
function route(url) {
  if (typeof url !== "string" || !URL.canParse(url)) return false
  const u = new URL(url)
  return (u.protocol === "http:" || u.protocol === "https:") && u.username === "" && u.password === "" &&
    u.search === "" && u.hash === "" && u.pathname.endsWith("/browser")
}

// errorCode is what of an error the log may carry: its code, never its
// message, which may hold the route and so the token.
export function errorCode(err) {
  return typeof err?.code === "string" ? err.code.slice(0, 64) : String(err?.name ?? "error").slice(0, 64)
}

// fileLog is the relay's log on stderr, which the job script appends to
// browser-relay.log: one line an entry, counted, and at bytes one line
// saying it stops, then nothing.
function fileLog(clock, bytes) {
  let written = 0
  let full = false
  return (kind, fields = {}) => {
    if (full) return
    const line = `${new Date(clock.now()).toISOString()} ${kind} ${JSON.stringify(fields).slice(0, LOG_ENTRY_MAX)}\n`
    const last = `${new Date(clock.now()).toISOString()} log_full {"bytes":${bytes}}\n`
    try {
      if (written + Buffer.byteLength(line) + Buffer.byteLength(last) > bytes) {
        full = true
        fs.writeSync(2, last)
        return
      }
      written += Buffer.byteLength(line)
      fs.writeSync(2, line)
    } catch {
      // A log that cannot be written stops nothing the relay does.
    }
  }
}

// Relay is one job's relay. io is {clock, log, exit, ppid, connect, request,
// random}: the bridge's requests and the supervisor's socket through it, so
// the probe runs a relay on its own clock and stand-ins.
export class Relay {
  constructor({ config, wiring, socket, io }) {
    this.config = config
    this.bounds = config.relay
    this.wiring = wiring
    this.socket = socket
    this.io = io
    this.clock = io.clock
    this.log = io.log
    this.hub = ""
    this.level = 0
    this.watch = undefined
    this.toldWatch = undefined
    this.refusedRate = undefined
    this.answering = false
    this.absentSince = io.clock.now()
    this.state = new Slot(this, "state")
    this.checkin = new Slot(this, "checkin")
    this.inFlight = new Set()
    this.leaving = false
    this.link = undefined
    this.linkAttempt = 0
  }

  // start connects to the supervisor, holds the first poll, and reads the
  // parent script's pid on the clock from now.
  start() {
    this.parent = this.io.ppid()
    this.watchParent()
    this.connect()
    this.poll().catch((err) => this.log("relay_failed", { verb: "poll", error: errorCode(err) }))
  }

  // watchParent reads the parent's pid every parent_ms and leaves once it
  // differs: the script that started the relay is gone, by its end, a trap
  // of its own or a kill.
  watchParent() {
    this.clock.setTimeout(() => {
      if (this.leaving) return
      if (this.io.ppid() !== this.parent) return this.leave("parent_gone")
      this.watchParent()
    }, this.bounds.parent_ms)
  }

  // leave ends the relay: bye to the supervisor, every request abandoned,
  // and exit 0 once each has settled and the bye is written, or at term_ms.
  leave(why) {
    if (this.leaving) return
    this.leaving = true
    this.log("relay_left", { why, abandoned: [...this.inFlight].map((r) => r.verb) })
    const bound = this.clock.setTimeout(() => this.io.exit(0), this.bounds.term_ms)
    const said = this.link?.bye() ?? Promise.resolve()
    for (const r of this.inFlight) r.abort.abort()
    Promise.allSettled([said, ...[...this.inFlight].map((r) => r.settled)]).then(() => {
      this.clock.clearTimeout(bound)
      this.io.exit(0)
    })
  }

  // connect opens the supervisor's socket and says the job's hello; a
  // socket absent, closed or not ready is tried again at the retry's pace.
  connect() {
    if (this.leaving) return
    const link = new Link(this, this.io.connect(this.socket))
    this.link = link
  }

  // linked is the supervisor's answer to the hello: the relay restates its
  // watch, and the supervisor its state and check-in.
  linked() {
    this.linkAttempt = 0
    this.answering = true
    this.toldWatch = undefined
    if (this.watch !== undefined) this.tellWatch(this.watch)
  }

  // unlinked is the supervisor's socket gone: the relay reports none
  // answers, and connects again.
  unlinked() {
    if (this.leaving) return
    this.link = undefined
    if (this.answering || this.checkin.newest === undefined) {
      this.answering = false
      this.absentSince = this.clock.now()
      this.checkin.put({ v: VERSION, job: this.wiring.job, connected: false, since_ms: this.absentSince, supervisor: "absent" })
    }
    this.linkAttempt += 1
    this.clock.setTimeout(() => this.connect(), this.retryWait(this.linkAttempt, undefined))
  }

  // received is a record the supervisor sent.
  received(header, payload) {
    const { t, ...rest } = header
    if (t === "frame") {
      return void this.postFrame(this.link, header, rest, payload).catch((err) =>
        this.log("relay_failed", { verb: "frame", error: errorCode(err) }),
      )
    }
    if (t === "state") return this.state.put(rest)
    if (t === "checkin") return this.checkin.put({ ...rest, supervisor: "answering" })
  }

  // tellWatch hands the supervisor watch, the newest the relay holds, once
  // it has answered the hello: a link not yet answered is told after it is.
  tellWatch(watch) {
    if (this.link?.hello !== true) return
    this.toldWatch = watch
    this.link.send({ t: "watch", v: VERSION, on: watch.on, fps: watch.fps })
  }

  // poll holds one poll at a time: a 200's hub and level are named by the
  // next, its watch handed on where its rate is one the settings name; a new
  // hub has the newest state and check-in posted again.
  async poll() {
    let attempt = 0
    while (!this.leaving) {
      const query = `hub=${encodeURIComponent(this.hub)}&level=${this.level}`
      const answer = await this.send("poll", "GET", { query, bound: this.bounds.poll_give_up_ms })
      if (this.leaving) return
      if (answer.status === 200) {
        const read = this.pollAnswer(answer.body)
        if (read !== undefined) {
          attempt = 0
          continue
        }
        this.log("poll_unread", {})
      } else if (answer.verdict === "leave") {
        return this.leave(`poll_${answer.status}`)
      } else if (answer.verdict === "gave_up") {
        attempt = 0
        continue
      }
      attempt += 1
      await this.wait(this.retryWait(attempt, answer.retryAfter))
    }
  }

  // pollAnswer takes a poll's 200, or answers undefined for one it cannot
  // read.
  pollAnswer(body) {
    let a
    try {
      a = JSON.parse(body.toString("utf8"))
    } catch {
      return undefined
    }
    const w = a?.watch
    if (!isObject(a) || typeof a.hub !== "string" || !Number.isInteger(a.level) || a.level < 0 || !isObject(w) ||
      typeof w.on !== "boolean" || !Number.isInteger(w.fps)) {
      return undefined
    }
    const newHub = a.hub !== this.hub
    this.hub = a.hub
    this.level = a.level
    const rates = [this.config.frames.watch_fps]
    if (w.on && !rates.includes(w.fps)) {
      if (this.refusedRate !== a.hub) this.log("watch_rate_unknown", { fps: w.fps })
      this.refusedRate = a.hub
    } else {
      const watch = { on: w.on, fps: w.on ? w.fps : rates[0] }
      this.watch = watch
      if (this.toldWatch?.on !== watch.on || this.toldWatch?.fps !== watch.fps) this.tellWatch(watch)
    }
    if (newHub) {
      this.state.restate()
      this.checkin.restate()
    }
    return a
  }

  // postFrame posts a frame the supervisor sent on link, its header the
  // record's less its type, and answers link's frame_ack once the post is
  // answered, given up at frame_give_up_ms, or, refused as too fast, once
  // its Retry-After has passed: the frame is never sent again. A link gone
  // meanwhile is acknowledged nothing: the next starts its screen afresh.
  async postFrame(link, header, meta, jpeg) {
    const until = this.clock.now() + this.bounds.frame_give_up_ms
    const ack = () => {
      if (this.link === link) link.send({ t: "frame_ack", v: VERSION, boot: header.boot, seq: header.seq })
    }
    const sent = JSON.stringify(meta)
    if (Buffer.byteLength(sent) > this.config.frames.meta_bytes) {
      this.log("frame_dropped", { why: "metadata" })
      return ack()
    }
    let attempt = 0
    while (!this.leaving) {
      const left = until - this.clock.now()
      if (left <= 0) {
        this.log("frame_given_up", {})
        return ack()
      }
      const answer = await this.send("frame", "POST", {
        headers: { "content-type": "image/jpeg", "x-crewlet-frame": sent },
        body: jpeg,
        bound: left,
      })
      if (this.leaving) return
      if (answer.status === 429) {
        await this.wait(answer.retryAfter ?? this.bounds.retry_base_ms)
        return ack()
      }
      if (answer.verdict === "done" || answer.verdict === "dropped") return ack()
      if (answer.verdict === "leave") return this.leave(`frame_${answer.status}`)
      attempt += 1
      await this.wait(Math.min(this.retryWait(attempt, answer.retryAfter), Math.max(0, until - this.clock.now())))
    }
  }

  // send is one request of verb, its answer read as the contract says:
  // {status, verdict, retryAfter, body}, verdict done, dropped, retry, leave
  // or gave_up; the request is tracked while it is in flight, so a leave
  // abandons it.
  async send(verb, method, { query, headers, body, bound }) {
    const abort = new AbortController()
    const tracked = { verb, abort, settled: undefined }
    let url = `${this.wiring.url}/${verb}`
    if (query !== undefined) url += `?${query}`
    const answered = this.io.request(url, { method, headers, body, bound, signal: abort.signal })
    tracked.settled = answered.then(
      () => {},
      () => {},
    )
    this.inFlight.add(tracked)
    let answer
    try {
      answer = await answered
    } catch (err) {
      const gaveUp = err instanceof Failed && err.code === "bound" && verb === "poll"
      if (!this.leaving) this.log("request_failed", { verb, error: errorCode(err) })
      return { status: 0, verdict: gaveUp ? "gave_up" : "retry" }
    } finally {
      this.inFlight.delete(tracked)
    }
    return { ...answer, verdict: this.verdict(verb, answer) }
  }

  // verdict is what an answer makes the relay do: done on a 2xx; leave on
  // 401, or a 404 the hub says not_found; dropped, logged, on 400, 405,
  // 413 or 415; and retry on anything else, a 404 of a route this node's
  // build lacks among it.
  verdict(verb, answer) {
    const s = answer.status
    if (s >= 200 && s < 300) return "done"
    if (s === 401) return "leave"
    if (s === 404 && codeOf(answer.body) === "not_found") return "leave"
    if (s === 400 || s === 405 || s === 413 || s === 415) {
      this.log("refused", { verb, status: s, code: codeOf(answer.body) })
      return "dropped"
    }
    if (!this.leaving) this.log("retried", { verb, status: s, code: codeOf(answer.body) })
    return "retry"
  }

  // retryWait is the wait before attempt: doubling from retry_base_ms to
  // retry_ceiling_ms, spread by retry_jitter, and never under a Retry-After.
  retryWait(attempt, retryAfter) {
    let delay = this.bounds.retry_base_ms
    for (let i = 1; i < attempt && delay < this.bounds.retry_ceiling_ms; i++) delay *= 2
    delay = Math.min(delay, this.bounds.retry_ceiling_ms)
    const spread = Math.round(delay * (1 + (this.io.random() * 2 - 1) * this.bounds.retry_jitter))
    return Math.max(spread, retryAfter ?? 0)
  }

  // wait resolves after ms on the relay's clock.
  wait(ms) {
    return new Promise((resolve) => this.clock.setTimeout(resolve, ms))
  }
}

// codeOf is the code a refusal's body names, as the bridge writes it, or "".
function codeOf(body) {
  try {
    const parsed = JSON.parse(body.toString("utf8"))
    return typeof parsed?.error === "string" ? parsed.error.slice(0, 64) : ""
  } catch {
    return ""
  }
}

// Slot posts the newest document of one kind, a state or a check-in: one
// post at a time, at least post_gap_ms after the last, retried until it is
// taken or a newer one takes its place; one past the bound the hub takes it
// within is dropped, logged, never posted. Its backoff is the verb's, never a
// document's: a post answered to retry holds back the next of its kind,
// newer or not, until the backoff's wait or the answer's Retry-After has
// passed, and only a post taken or dropped starts the doubling again.
class Slot {
  constructor(relay, verb) {
    this.relay = relay
    this.verb = verb
    this.pending = undefined
    this.newest = undefined
    // busy is a post in flight, or a wait for the next one's time.
    this.busy = false
    this.lastPost = -Infinity
    this.attempt = 0
    this.notBefore = -Infinity
  }

  // put makes doc the newest, to be posted.
  put(doc) {
    this.newest = doc
    this.pending = doc
    this.pump()
  }

  // restate posts the newest again, as a new hub has none of it.
  restate() {
    if (this.newest === undefined || this.pending !== undefined) return
    this.pending = this.newest
    this.pump()
  }

  // pump posts what is pending once its time has come: post_gap_ms after
  // the last post, and not before the backoff's wait.
  pump() {
    const r = this.relay
    if (this.busy || this.pending === undefined || r.leaving) return
    const wait = Math.max(this.lastPost + r.bounds.post_gap_ms, this.notBefore) - r.clock.now()
    if (wait > 0) {
      this.busy = true
      r.clock.setTimeout(() => {
        this.busy = false
        this.pump()
      }, wait)
      return
    }
    this.post(this.pending).catch((err) => r.log("relay_failed", { verb: this.verb, error: errorCode(err) }))
  }

  async post(doc) {
    const r = this.relay
    this.pending = undefined
    const body = Buffer.from(JSON.stringify(doc))
    const most = this.verb === "checkin" ? r.bounds.checkin_bytes : r.config.state.max_bytes
    if (body.length > most) {
      r.log("dropped", { verb: this.verb, why: "too_long", bytes: body.length })
      return this.pump()
    }
    this.busy = true
    this.lastPost = r.clock.now()
    const answer = await r.send(this.verb, "POST", {
      headers: { "content-type": "application/json" },
      body,
      bound: r.bounds.post_give_up_ms,
    })
    this.busy = false
    if (r.leaving) return
    if (answer.verdict === "leave") return r.leave(`${this.verb}_${answer.status}`)
    if (answer.verdict === "retry") {
      this.attempt += 1
      this.notBefore = r.clock.now() + r.retryWait(this.attempt, answer.retryAfter)
      this.pending ??= doc
    } else {
      this.attempt = 0
      this.notBefore = -Infinity
    }
    this.pump()
  }
}

// Link is the relay's connection to the supervisor: its hello, then the
// supervisor's records, each handed to the relay; one that refuses the hello
// as another helper's ends the relay, which no retry in its job changes.
class Link {
  constructor(relay, sock) {
    this.relay = relay
    this.sock = sock
    this.writer = new Writer(sock)
    this.hello = false
    const config = relay.config
    const reader = new Reader({
      types: FROM_SUPERVISOR,
      headerBytes: relay.bounds.header_bytes,
      payloadBytes: config.frames.max_bytes,
      onRecord: (header, payload) => this.record(header, payload),
    })
    sock.on("connect", () => {
      this.send({ t: "hello", v: VERSION, helper: config.helper, job: relay.wiring.job })
    })
    sock.on("data", (chunk) => {
      try {
        reader.push(chunk)
      } catch (err) {
        relay.log("supervisor_refused", { why: err instanceof Refused ? "framing" : errorCode(err) })
        sock.destroy()
      }
    })
    sock.on("error", () => {})
    sock.on("close", () => {
      if (relay.link === this) relay.unlinked()
    })
  }

  record(header, payload) {
    const relay = this.relay
    if (!this.hello) {
      if (header.t === "refused") {
        relay.log("hello_refused", { reason: String(header.reason).slice(0, 64) })
        if (header.reason === "helper_mismatch") {
          relay.leaving = true
          return relay.io.exit(UNAVAILABLE)
        }
        return this.sock.destroy()
      }
      if (header.t !== "hello") return this.sock.destroy()
      this.hello = true
      return relay.linked()
    }
    relay.received(header, payload)
  }

  send(header) {
    this.writer.send(header)
  }

  // bye tells the supervisor the relay is ending, and answers once it is
  // written or the socket is gone.
  bye() {
    if (!this.hello || this.sock.destroyed) return Promise.resolve()
    return new Promise((resolve) => {
      this.sock.once("close", resolve)
      this.send({ t: "bye", v: VERSION })
      this.sock.end(resolve)
    })
  }
}
