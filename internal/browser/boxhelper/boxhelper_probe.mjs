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
//   {"op":"proc","pid":P,"stat":S,"cmdline":C}
//                                          what running makes of P on Linux
//                                          where the kernel's stat line for it
//                                          reads S and its command line C:
//                                          alive, argv or command, and start
//   {"op":"signal","pid":P,"sig":S,"group":G}
//                                          send S, a signal's number or name,
//                                          to P, or to its group with G:
//                                          whether anyone was sent it
//   {"op":"record","body":B}               what the reap reads of a record
//                                          holding B: its pid and start, or
//                                          nothing
//   {"op":"prints"}                        every reason the modules print,
//                                          with its causes
//   {"op":"folder","folder":F,"fail":X}    ensure's hold of its folder F: ok,
//                                          or the reason and cause it gives
//   {"op":"lock","folder":F,"fail":X,"watch":E}
//                                          ensure's lock step in F, on the
//                                          probe's clock, the folder's watch
//                                          refused with errno E if set: ok and
//                                          what the lock holds, or the reason
//                                          and cause it gives
//   {"op":"takeover","folder":F}           two lock steps, B and C, in F at
//                                          once, on the probe's clock, a
//                                          holder live where it is the probe,
//                                          ordered as two ensures can run:
//                                          both judge the lock F holds stale,
//                                          C's next step waits for B's, B's
//                                          removal of the lock for C's next
//                                          step, and C's removal for B's own
//                                          lock. The order each took the lock
//                                          and the first gave it back
//   {"op":"sweep","folder":F,"name":N}     sweep F's pw/ of all but the tree
//                                          named N: the entries it removed
//   {"op":"sandbox_cause","uid":U,"files":{P:C}}
//                                          the cause a box with uid U and
//                                          those files' texts shows
//   {"op":"launch_failure","message":M,"name":N,"sandbox":S,"uid":U,"files":F}
//                                          the reason and cause of a launch
//                                          that failed with that error
//   {"op":"supervise","launch":L,"reject_first":R,"fail":X,"umask":U,"live":A,"at_once":O}
//                                          the supervisor of launch.json L in
//                                          this process, on the probe's own
//                                          clock, its profile's lock read
//                                          through X's file system, once it is
//                                          ready or failed, or with O at once;
//                                          with R an unhandled rejection is
//                                          raised just before it starts, with
//                                          U the probe's umask is U as it
//                                          starts, as a looser starter's would
//                                          be, and A names its attachments, in
//                                          order: "record", a stand-in
//                                          recording each call it is told;
//                                          "fail", one whose attach throws;
//                                          "fail_hooks", one whose every hook
//                                          throws or rejects; "stop", one that
//                                          stops the supervisor as it
//                                          attaches, its close recorded;
//                                          "hold", one whose attach waits for
//                                          the release op; "live", the live
//                                          view (live.mjs)
//   {"op":"attached","until":U}            answer once "record" has recorded
//                                          a call whose fields hold U's values
//   {"op":"advance","ms":N}                move the clock N ms, every timer due
//                                          run
//   {"op":"kill_chromium","until":S}       kill the supervised Chromium's group,
//                                          and answer once the supervisor has
//                                          it again or has crashed out, or
//                                          with S once its state is S
//   {"op":"release"}                       let every call a hold fault holds
//                                          run, and every later one at once
//   {"op":"freeze_chromium"}               stop the supervised Chromium's group
//   {"op":"context"}                       the supervisor's context, or its
//                                          refusal
//   {"op":"stop"}                          ask the supervisor to stop, and
//                                          answer once it is closing its
//                                          browser, the grace on the clock
//   {"op":"stopped"}                       answer once it has
//   {"op":"events"}                        the supervisor's state and events
//   {"op":"reject"}                        raise a rejection nobody handles
//   {"op":"fence","boundary":B,"text":T}   T fenced as a page's words under B
//   {"op":"edit","from":F,"message":M,"boundary":B}
//                                          what one model connection's edits
//                                          make of M from F ("model" or
//                                          "server"): the lines toward the
//                                          model, the messages toward the
//                                          server, a refusal, and its table
//                                          of tools/call in flight after it;
//                                          the connection is the probe's one,
//                                          drawn at its first edit with B
//   {"op":"edit_closed"}                   that connection closed: its table
//   {"op":"tools","launch":L}              the tools the pinned server lists
//                                          under launch.json L's config, no
//                                          edit between
// The live view's (internal/browser/boxhelper/live), on the box's config:
//   {"op":"config_read"}                   readConfig's answer for the box's
//                                          config.json: ok, or its refusal
//   {"op":"cdp"}                           the table: every command with its
//                                          parameters, every event, the flow
//                                          pair
//   {"op":"cdp_send","method":M,"params":P}
//                                          what send does with M and P on a
//                                          session the probe plays: sent, or
//                                          its refusal
//   {"op":"jpeg_size","jpeg_b64":J}        the width and height jpeg.mjs reads
//                                          of J, or none
//   {"op":"tabs_listed","answer":A}        what a browser_tabs answer A lists
//   {"op":"state_make","boot":B,"running":R,"tabs":T,"times":N}
//                                          the document made of T N times over
//                                          (default 1), the last, and its bytes
//                                          as the relay posts it
//   {"op":"tabs_overflow","next":N,"live":L}
//                                          a registry whose next number is N
//                                          numbers L pages, then one more: each
//                                          id, and the boot before and after
//   {"op":"screen","major":M,"fps":F,"shot_b64":S,"viewport":V}
//                                          a screen on the probe's clock over
//                                          a session the probe plays, at
//                                          Chromium major M, its page in view
//                                          and watched at F, its viewport V
//                                          ({width, height}, 1280 by 800 by
//                                          default), each capture answered
//                                          with S
//   {"op":"cdp_frame","jpeg_b64":J,"metadata":D,"session_id":I}
//                                          the session sends the screen a
//                                          frame
//   {"op":"cdp_resized","times":N}         the session says, N times at
//                                          once, the page resized
//   {"op":"frame_ack","seq":N}             the relay acknowledges frame N
//   {"op":"cdp_fail"}                      every later call of the session
//                                          fails
//   {"op":"page_closed"}                   no page is in view
//   {"op":"watch","on":O,"fps":F}          the relay's watch moves
//   {"op":"relay_gone"}                    the relay's connection closes
// Each screen op answers what the screen did so far, in order, each by the
// probe's own word for it, so a case spells no DevTools method: the session
// told metrics, captured (a capture), started {params} (the screencast),
// stopped, or chromium_ack {session_id}; forwarded {the frame's header and
// its bytes}; logged {log}; and the rejections nobody handled in the probe.
// A fault X is {method, errno, after, times, kill, uid, kill_holder,
// kill_reading, blank_reading, hold}: every call of the file system's method
// after the first `after`, or with times only that many, throws the error
// Node gives for errno, a name; with kill the first such call kills the
// probe, as SIGKILL would a box's helper part way; with uid it answers its
// stat with that uid; with kill_holder it reads the link, kills the group of
// the Chromium it names on its profile, and answers once that browser is gone
// and Playwright has said so, as a browser that dies while its launch is
// read; with kill_reading a read of a browser's command line does that to the
// browser, and answers what it read less --no-sandbox, as a sandboxed browser
// that dies as its command line is read; with blank_reading such a read
// answers nothing, as the kernel does once a browser's main thread exits;
// with hold it waits for the release op, then runs as itself.
// A supervisor's answers carry its state, every event it logged, and every
// call "record" recorded.
// A command that throws anything else answers {"op":"error","message":M}.

import fs from "node:fs"
import fsp from "node:fs/promises"
import { constants } from "node:os"
import path from "node:path"
import { createInterface } from "node:readline"
import { pathToFileURL } from "node:url"
import { getSystemErrorName } from "node:util"

const folder = process.argv[2]
const module = (name) => import(pathToFileURL(path.join(folder, name)).href)
const { PRINTS, carries, gone, readConfig, realClock, running, signal, Withheld } = await module("common.mjs")
const tree = await module("tree.mjs")
const chromium = await module("chromium.mjs")
const { holdFolder, takeLock } = await module("ensure.mjs")
const { supervise } = await module("supervise.mjs")
const { fence } = await module("fence.mjs")
const { Edits, mcpConfig } = await module("serve.mjs")
const cdp = await module("cdp.mjs")
const { jpegSize } = await module("jpeg.mjs")
const { States } = await module("state.mjs")
const { Pages, tabsListed } = await module("pages.mjs")
const { Screen } = await module("screen.mjs")
const { attachment: liveView } = await module("live.mjs")

const answer = (fields) => process.stdout.write(JSON.stringify(fields) + "\n")

// errorOf is the error Node gives for errno, a name: its negative number, and
// as its code libuv's name for it, which for EDQUOT is none
// (getSystemErrorName answers "Unknown system error -122" on Linux).
function errorOf(name, where) {
  const errno = -constants.errno[name]
  if (!Number.isInteger(errno)) throw new Error(`${name} is no errno this platform names`)
  const code = getSystemErrorName(errno)
  return Object.assign(new Error(`${code}: the probe's ${name} in ${where}`), { code, errno })
}

// HOLDER_GONE_MS bounds a kill_holder's or a kill_reading's wait for the
// browser it killed, which goes in milliseconds.
const HOLDER_GONE_MS = 30_000

// held is what a hold fault's calls wait for, until the release op settles it.
let release
const held = new Promise((resolve) => {
  release = resolve
})

// faulty is the file system with method failing as fail says, every call
// after the first fail.after, or fail.times of them; none fails with no fail.
function faulty(fail) {
  if (fail == null) return fsp
  const after = fail.after ?? 0
  const throws =
    !fail.kill && fail.uid === undefined && !fail.kill_holder && !fail.kill_reading && !fail.blank_reading && !fail.hold
  if (throws) errorOf(fail.errno, fail.method)
  let calls = 0
  return new Proxy(fsp, {
    get(target, method) {
      const real = target[method]
      if (method !== fail.method) return real
      return async (...args) => {
        const call = calls++
        if (call < after || (fail.times !== undefined && call >= after + fail.times)) return real.apply(target, args)
        if (fail.hold) {
          await held
          return real.apply(target, args)
        }
        if (fail.kill) process.kill(process.pid, "SIGKILL")
        if (fail.uid !== undefined) {
          const st = await real.apply(target, args)
          return Object.assign(Object.create(Object.getPrototypeOf(st)), st, { uid: fail.uid })
        }
        if (fail.kill_holder) return killHolder(await real.apply(target, args), args[0])
        if (fail.kill_reading) return killReading(String(args[0]), await real.apply(target, args))
        if (fail.blank_reading) return blankReading(String(args[0]), await real.apply(target, args))
        throw errorOf(fail.errno, method)
      }
    },
  })
}

// launched is the browser context the probe's supervisor launched last, as
// watchLaunches keeps it.
let launched

// watchLaunches keeps each context the supervisor of the launch in file
// launches in launched: its tree's chromium is the one the supervisor loads,
// one instance by the require cache, so a wrapper set on it here is the one
// the supervisor calls.
async function watchLaunches(file) {
  const { core } = tree.load(JSON.parse(await fsp.readFile(file, "utf8")).tree)
  const launch = core.chromium.launchPersistentContext
  core.chromium.launchPersistentContext = async function (...args) {
    launched = await launch.apply(this, args)
    return launched
  }
}

// killBrowser kills the group pid leads, and answers once the process has
// gone and the browser last launched has said it disconnected, so whatever
// Playwright tells of the death has been told.
async function killBrowser(pid) {
  const browser = launched?.browser()
  const told = browser?.isConnected() ? new Promise((resolve) => browser.once("disconnected", resolve)) : undefined
  signal(pid, "SIGKILL", true)
  await gone(pid, HOLDER_GONE_MS, realClock)
  await told
}

// killHolder kills the Chromium the SingletonLock at link names, target, where
// that is a process on link's profile (killBrowser), and answers target.
async function killHolder(target, link) {
  const pid = Number(target.slice(target.lastIndexOf("-") + 1))
  if (carries(await running(pid), `--user-data-dir=${path.dirname(link)}`)) await killBrowser(pid)
  return target
}

// killReading kills the browser whose command line it read at file, body,
// where that is a process on a profile (killBrowser), and answers body less
// --no-sandbox, the command line of a browser its own sandbox holds; anything
// else it read it answers as it is.
async function killReading(file, body) {
  const m = /^\/proc\/([0-9]+)\/cmdline$/.exec(file)
  if (m === null || !String(body).includes("--user-data-dir=")) return body
  await killBrowser(Number(m[1]))
  return String(body).replace(/[\0 ]--no-sandbox(?=[\0 ]|$)/g, "")
}

// blankReading answers nothing for the command line of a process on a
// profile it read at file, body, as the kernel answers once that browser's
// main thread exits; anything else it read it answers as it is.
function blankReading(file, body) {
  const browser = /^\/proc\/[0-9]+\/cmdline$/.test(file) && String(body).includes("--user-data-dir=")
  return browser ? "" : body
}

// clock is the probe's own: time moves only when a case advances it, and
// then every timer due runs, in order.
const clock = (() => {
  let now = Date.now()
  let seq = 0
  const timers = []
  return {
    now: () => now,
    setTimeout: (f, ms) => {
      const t = { at: now + Math.max(0, ms), f, seq: seq++ }
      timers.push(t)
      return t
    },
    clearTimeout: (t) => {
      const i = timers.indexOf(t)
      if (i >= 0) timers.splice(i, 1)
    },
    advance(ms) {
      const to = now + ms
      for (;;) {
        timers.sort((a, b) => a.at - b.at || a.seq - b.seq)
        if (timers.length === 0 || timers[0].at > to) break
        const t = timers.shift()
        now = t.at
        t.f()
      }
      now = to
    },
  }
})()

// files is a reader of fixed texts, as sandboxCause takes one.
const files = (texts = {}) => async (file) => (Object.hasOwn(texts, file) ? String(texts[file]).trim() : undefined)

// refused is a Withheld's answer.
const refused = (err) => ({ reason: err.reason, cause: err.cause, detail: err.detail })

let supervisor
let edits
const events = []
const reports = []
const logged = new Set() // each called with every event as it is logged
const attached = [] // each call "record" was told, as it recorded it
const recorded = new Set() // each called with every call as it is recorded

// record is a call "record" was told.
function record(call) {
  attached.push(call)
  for (const listener of recorded) listener(call)
}

// texts is an answer's text items, joined by newlines.
const texts = (answer) =>
  (Array.isArray(answer?.content) ? answer.content : []).map((item) => item?.text ?? "").join("\n")

// closed records a close of an attachment to s, as s shows then.
const closed = (s) => () =>
  record({ hook: "close", state: s.state, socket: fs.existsSync(s.launch.sockets.browser) })

// attachments are the stand-ins the supervise op attaches, by name; each
// hook a recording one is told is recorded, with what the supervisor showed
// then.
const attachments = {
  record: {
    attach(s) {
      record({ hook: "attach", state: s.state, socket: fs.existsSync(s.launch.sockets.browser), boot: s.boot })
      return {
        onLaunched: (context) =>
          record({ hook: "launched", current: context === s.current?.context, pages: context.pages().length }),
        onState: (state) => record({ hook: "state", state }),
        onModel: (job) => record({ hook: "model", job: job ?? null }),
        onTabs: (job, action, answer) => record({ hook: "tabs", job, action, text: texts(answer) }),
        close: closed(s),
      }
    },
  },
  fail: {
    attach() {
      throw new Error("an attachment that fails to attach")
    },
  },
  fail_hooks: {
    attach() {
      const throws = () => {
        throw new Error("a hook that throws")
      }
      const rejects = () => Promise.reject(new Error("a hook that rejects"))
      return { onLaunched: throws, onState: rejects, onModel: throws, onTabs: rejects, close: throws }
    },
  },
  stop: {
    attach(s) {
      void s.stop()
      return { close: closed(s) }
    },
  },
  hold: {
    async attach() {
      await held
      return undefined
    },
  },
  live: {
    attach: (s) => liveView().attach(s),
  },
}

// listed is a server's answer to tools/list under config, over a transport of
// the probe's own, with nothing between them: no browser, which tools/list
// never reaches.
async function listed(createConnection, config) {
  const server = await createConnection(config)
  const answers = new Map()
  const transport = {
    start: async () => {},
    send: async (m) => answers.get(m.id)?.(m),
    close: async () => {},
  }
  const ask = (id, method, params) =>
    new Promise((resolve) => {
      answers.set(id, resolve)
      transport.onmessage({ jsonrpc: "2.0", id, method, params })
    })
  await server.connect(transport)
  await ask(1, "initialize", { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "probe", version: "1" } })
  transport.onmessage({ jsonrpc: "2.0", method: "notifications/initialized" })
  const answer = await ask(2, "tools/list", {})
  await server.close()
  return answer.result.tools
}

// settled answers once the supervisor's state is one of states.
function settled(states) {
  return new Promise((resolve) => {
    const listener = (state) => {
      if (!states.includes(state)) return
      supervisor.listeners.delete(listener)
      resolve(state)
    }
    supervisor.listeners.add(listener)
  })
}

// supervised is a supervisor's state, every event it logged and every call
// "record" recorded.
const supervised = () => ({ state: supervisor?.state ?? "starting", events, reports, attached })

// gate is a promise and the function that settles it.
function gate() {
  let open
  const opened = new Promise((resolve) => {
    open = resolve
  })
  return { opened, open }
}

// paced is the file system of one of the takeover op's lock steps, each call
// run through hooks: before(call) is awaited ahead of it and after(call,
// done) runs once it has settled, done {got}, its answer, or undefined where
// it threw. A call is {method, file, judged}: file is its path's base name,
// and judged whether the step had read the lock before it, and so judged it.
function paced(hooks) {
  let judged = false
  return new Proxy(fsp, {
    get(target, method) {
      const real = target[method]
      if (typeof real !== "function") return real
      return async (...args) => {
        const call = { method, file: path.basename(String(args[0])), judged }
        await hooks.before(call)
        let done
        try {
          done = { got: await real.apply(target, args) }
          return done.got
        } finally {
          if (method === "readFile" && call.file === "ensure.lock") judged = true
          hooks.after(call, done)
        }
      }
    },
  })
}

// takeovers are the takeover op's two lock steps in folder, ordered by gates
// as its line says, and answer {steps, readHolder}: each step's promise, by
// name, and for each a gate it opens once it has read a live holder's lock,
// the other's, which it waits on.
function takeovers(folder, config, planted) {
  const g = { bNext: gate(), cNext: gate(), bTook: gate(), B: gate(), C: gate() }
  const live = (holder) => holder.pid === process.pid
  const step = (name) => {
    let next = false
    return paced({
      async before(call) {
        if (name === "C" && call.judged && !next) await g.bNext.opened
        if (call.method === "rm" && call.file === "ensure.lock") await (name === "B" ? g.cNext : g.bTook).opened
      },
      after(call, done) {
        if (call.judged && !next) {
          next = true
          g[name === "B" ? "bNext" : "cNext"].open()
        }
        if (name === "B" && call.method === "writeFile" && call.file === "ensure.lock" && done !== undefined) {
          g.bTook.open()
        }
        if (call.method === "readFile" && call.file === "ensure.lock" && done !== undefined && done.got !== planted) {
          g[name].open()
        }
      },
    })
  }
  const steps = {}
  for (const name of ["B", "C"]) steps[name] = takeLock(folder, config, { clock, io: step(name), live })
  return { steps, readHolder: { B: g.B, C: g.C } }
}

// unhandled counts the rejections nobody handled in the probe, from the
// first screen op on.
let unhandled = 0
let countingUnhandled = false

// session is a DevTools session the probe plays: each call recorded and
// answered as the browser would, failing where a case says, and each event
// the case raises handed to its listeners.
class Session {
  constructor(shot, viewport) {
    this.shot = shot
    this.viewport = viewport
    this.listeners = new Map()
    this.failing = false
  }
  async send(method, params) {
    const told = {
      "Page.getLayoutMetrics": () => ({ kind: "metrics" }),
      "Page.captureScreenshot": () => ({ kind: "captured", params }),
      "Page.startScreencast": () => ({ kind: "started", params }),
      "Page.stopScreencast": () => ({ kind: "stopped" }),
      "Page.screencastFrameAck": () => ({ kind: "chromium_ack", session_id: params.sessionId }),
    }[method]
    screened.push({ ...(told?.() ?? { kind: "called", method }), at: clock.now() })
    if (this.failing) throw new Error(`the probe fails ${method}`)
    if (method === "Page.getLayoutMetrics") {
      const viewport = { pageX: 0, pageY: 0, clientWidth: this.viewport.width, clientHeight: this.viewport.height }
      return { cssLayoutViewport: viewport, cssVisualViewport: { ...viewport, offsetX: 0, offsetY: 0, scale: 1, zoom: 1 } }
    }
    if (method === "Page.captureScreenshot") return { data: this.shot }
    return {}
  }
  on(event, f) {
    this.listeners.set(event, [...(this.listeners.get(event) ?? []), f])
  }
  off(event, f) {
    this.listeners.set(event, (this.listeners.get(event) ?? []).filter((g) => g !== f))
  }
  emit(event, params) {
    for (const f of this.listeners.get(event) ?? []) f(params)
  }
}

// screened is everything the probe's screen did, in order; screen is it,
// and stage the page it shows, on its session.
const screened = []
let screen
let stage

// settled answers once the screen's promises of the moment have run.
const settledScreen = () => new Promise((resolve) => setImmediate(resolve))

// screenAnswer is what the screen did so far.
const screenAnswer = async () => {
  await settledScreen()
  return { screen: screened, unhandled }
}

const commands = {
  async config_read() {
    try {
      await readConfig(folder)
      return { ok: true }
    } catch (err) {
      return { refused: err.message }
    }
  },
  cdp() {
    return { sends: cdp.SENDS, reads: cdp.READS, flow: cdp.FLOW }
  },
  async cdp_send({ method, params }) {
    const sent = []
    try {
      await cdp.send({ send: async (m, p) => sent.push({ method: m, params: p }) }, method, params)
      return { sent }
    } catch (err) {
      return { refused: err.message, off_table: err instanceof cdp.OffTable, sent }
    }
  },
  async jpeg_size({ jpeg_b64: jpeg }) {
    const config = await readConfig(folder)
    return { size: jpegSize(Buffer.from(jpeg, "base64"), config.frames.jpeg_header_bytes) ?? null }
  },
  tabs_listed({ answer }) {
    return { listed: tabsListed(answer) ?? null }
  },
  async state_make({ boot, running, tabs, times = 1 }) {
    const config = await readConfig(folder)
    const states = new States(config.state)
    let doc
    for (let i = 0; i < times; i++) doc = states.make(boot, running, tabs)
    return { doc, bytes: Buffer.byteLength(JSON.stringify(doc)) }
  },
  tabs_overflow({ next, live }) {
    const logged = []
    const boots = ["00000000000000a1", "00000000000000b2"]
    const pages = new Pages({
      boot: "00000000000000f0",
      newBoot: () => boots.shift(),
      log: (kind, fields) => logged.push({ kind, ...fields }),
      onChange: () => {},
    })
    pages.next = next
    const before = pages.boot
    for (let i = 0; i < live; i++) pages.records.push({ id: pages.nextID() })
    const numbered = pages.records.map((r) => r.id)
    const added = pages.nextID()
    return { numbered, ids: pages.records.map((r) => r.id), added, boot_before: before, boot: pages.boot, logged }
  },
  async screen({ major, fps, shot_b64: shot, viewport = { width: 1280, height: 800 } }) {
    if (!countingUnhandled) {
      countingUnhandled = true
      process.on("unhandledRejection", () => {
        unhandled += 1
      })
    }
    const config = await readConfig(folder)
    stage = { id: "t1", session: new Session(shot, viewport) }
    screen = new Screen({
      clock,
      log: (kind, fields = {}) => screened.push({ kind: "logged", log: kind, ...fields, at: clock.now() }),
      frames: config.frames,
      major: () => major,
      boot: () => "0123456789abcdef",
      emit: (header, jpeg) => screened.push({ kind: "forwarded", ...header, bytes: jpeg.length, at: clock.now() }),
    })
    screen.show(stage)
    screen.watched({ on: true, fps })
    return screenAnswer()
  },
  cdp_frame({ jpeg_b64: jpeg, metadata, session_id: sessionId }) {
    stage.session.emit("Page.screencastFrame", { data: jpeg, metadata, sessionId })
    return screenAnswer()
  },
  cdp_resized({ times }) {
    for (let i = 0; i < times; i++) stage.session.emit("Page.frameResized", {})
    return screenAnswer()
  },
  frame_ack({ seq }) {
    screen.acked("0123456789abcdef", seq)
    return screenAnswer()
  },
  cdp_fail() {
    stage.session.failing = true
    return screenAnswer()
  },
  page_closed() {
    screen.show(undefined)
    return screenAnswer()
  },
  watch({ on, fps }) {
    screen.watched({ on, fps })
    return screenAnswer()
  },
  relay_gone() {
    screen.watched(undefined)
    return screenAnswer()
  },
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
      return { ...refused(err), pids }
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
  async proc({ pid, stat, cmdline }) {
    const texts = { [`/proc/${pid}/stat`]: stat, [`/proc/${pid}/cmdline`]: cmdline }
    const readFile = async (file) => {
      if (typeof texts[file] !== "string") throw errorOf("ENOENT", file)
      return texts[file]
    }
    const { alive, argv, command, start } = await running(pid, { readFile })
    return { alive, argv, command, start }
  },
  signal({ pid, sig, group }) {
    return { sent: signal(pid, sig, group) }
  },
  record({ body }) {
    return tree.recordOf(body)
  },
  prints() {
    return { prints: PRINTS }
  },
  async folder({ folder, fail }) {
    try {
      await holdFolder(folder, faulty(fail))
      return { ok: true }
    } catch (err) {
      if (!(err instanceof Withheld)) throw err
      return refused(err)
    }
  },
  async lock({ folder, fail, watch }) {
    const refuse = () => {
      throw errorOf(watch, "watch")
    }
    try {
      const config = await readConfig(folder)
      await takeLock(folder, config, { clock, io: faulty(fail), watch: watch === undefined ? undefined : refuse })
      return { ok: true, holder: await fsp.readFile(path.join(folder, "ensure.lock"), "utf8") }
    } catch (err) {
      if (!(err instanceof Withheld)) throw err
      return refused(err)
    }
  },
  async takeover({ folder }) {
    const config = await readConfig(folder)
    const file = path.join(folder, "ensure.lock")
    const { steps, readHolder } = takeovers(folder, config, await fsp.readFile(file, "utf8"))
    const order = []
    const taken = {}
    for (const name of ["B", "C"]) {
      taken[name] = steps[name].then(() => {
        order.push(`${name} took`)
        return name
      })
    }
    const first = await Promise.race([taken.B, taken.C])
    const other = first === "B" ? "C" : "B"
    // The other has taken the lock too, or waits on the first's.
    await Promise.race([taken[other], readHolder[other].opened])
    if (!order.includes(`${other} took`)) {
      order.push(`${first} gave it back`)
      await fsp.rm(file)
      await taken[other]
    }
    return { order }
  },
  async sweep({ folder, name }) {
    return { removed: await tree.sweep(folder, name) }
  },
  async sandbox_cause({ uid, files: texts }) {
    return { cause: await chromium.sandboxCause({ uid, read: files(texts) }) }
  },
  async launch_failure({ message, name, sandbox, uid, files: texts }) {
    const err = Object.assign(new Error(message), name === undefined ? {} : { name })
    const failed = await chromium.launchFailure(err, sandbox, () => chromium.sandboxCause({ uid, read: files(texts) }))
    return { reason: failed.reason, cause: failed.cause }
  },
  async supervise({ launch, reject_first: rejectFirst, fail, umask, live, at_once: atOnce }) {
    const io = {
      clock,
      fs: faulty(fail),
      log: (kind, fields = {}) => {
        const e = { kind, ...fields, at: clock.now() }
        events.push(e)
        for (const listener of logged) listener(e)
      },
      report: (line) => reports.push(line),
      exit: (code) => process.exit(code),
    }
    if (fail?.kill_holder || fail?.kill_reading) await watchLaunches(launch)
    if (umask !== undefined) process.umask(umask)
    if (rejectFirst) Promise.reject(new Error("a rejection raised as the supervisor starts"))
    const names = live ?? []
    const unknown = names.find((name) => !Object.hasOwn(attachments, name))
    if (unknown !== undefined) throw new Error(`no attachment is named ${unknown}`)
    const started = supervise(launch, io, names.map((name) => attachments[name])).then((s) => {
      supervisor = s
    })
    if (!atOnce) await started
    else started.catch((err) => events.push({ kind: "supervise_failed", error: String(err?.message ?? err) }))
    return supervised()
  },
  async attached({ until = {} }) {
    const matches = (call) => Object.entries(until).every(([key, value]) => (call[key] ?? null) === value)
    if (!attached.some(matches)) {
      await new Promise((resolve) => {
        const listener = (call) => {
          if (!matches(call)) return
          recorded.delete(listener)
          resolve()
        }
        recorded.add(listener)
      })
    }
    return supervised()
  },
  async advance({ ms }) {
    clock.advance(ms)
    if (screen === undefined) return { now: clock.now() }
    return { now: clock.now(), ...(await screenAnswer()) }
  },
  async kill_chromium({ until }) {
    const pid = supervisor.current?.pid
    const back = settled(until === undefined ? ["ready", "crashed_out"] : [until])
    process.kill(-pid, "SIGKILL")
    await back
    return { ...supervised(), killed: pid, pid: supervisor.current?.pid }
  },
  release() {
    release()
    return {}
  },
  freeze_chromium() {
    const pid = supervisor.current?.pid
    process.kill(-pid, "SIGSTOP")
    return { frozen: pid }
  },
  async context() {
    try {
      const context = await supervisor.context()
      return { pages: context.pages().length }
    } catch (err) {
      return { refused: err.message }
    }
  },
  async stop() {
    const closing = new Promise((resolve) => {
      const listener = (e) => {
        if (e.kind !== "closing_browser") return
        logged.delete(listener)
        resolve()
      }
      logged.add(listener)
    })
    await Promise.race([closing, supervisor.stop()])
    return supervised()
  },
  async stopped() {
    await supervisor.stopped
    return supervised()
  },
  events() {
    return supervised()
  },
  reject() {
    Promise.reject(new Error("a rejection nobody handles, raised between jobs"))
    return {}
  },
  fence({ boundary, text }) {
    return { fenced: fence(boundary, text) }
  },
  edit({ from, message, boundary }) {
    edits ??= new Edits({ boundary, log: (kind, fields = {}) => events.push({ kind, ...fields }) })
    const out = from === "model" ? edits.fromModel(message) : edits.fromServer(message)
    return { ...out, in_flight: edits.inFlight, events }
  },
  edit_closed() {
    edits.closed()
    return { in_flight: edits.inFlight }
  },
  async tools({ launch }) {
    const l = JSON.parse(await fsp.readFile(launch, "utf8"))
    const { mcp } = tree.load(l.tree)
    return { tools: (await listed(mcp.createConnection, mcpConfig(l))).map((tool) => tool.name) }
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
