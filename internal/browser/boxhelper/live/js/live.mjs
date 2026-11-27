// live.mjs: the live view, as what attaches to the supervisor (supervise.mjs):
// relay.sock, listening before the supervisor says ready, and, told what the
// supervisor does, the pages of each browser it launches, the page in view,
// the screen while the relay's watch says on, the state, and the check-in of
// the relay's job. helper.mjs composes it with the supervisor, which imports
// nothing of it. internal/browser/boxhelper/live's package doc carries the
// argument.

import { randomBytes } from "node:crypto"

import { Pages, guarded } from "./pages.mjs"
import { listen } from "./relaysock.mjs"
import { Screen } from "./screen.mjs"
import { RUNNING, States } from "./state.mjs"

// attachment is the live view as the supervisor takes an attachment.
export function attachment() {
  return {
    attach: async (s) => {
      const view = new Live(s)
      await view.listen()
      return view.hooks()
    },
  }
}

// Live is one supervisor's live view.
class Live {
  constructor(s) {
    this.s = s
    this.launch = s.launch
    this.clock = s.clock
    this.log = s.io.log
    this.states = new States(this.launch.state)
    this.running = undefined
    this.major = 0
    this.version = undefined
    this.relay = undefined
    this.modelJob = undefined
    this.modelSince = this.clock.now()
    this.left = undefined
    this.attached = this.clock.now()
    this.dirty = false
    this.pages = new Pages({
      boot: s.boot,
      newBoot: () => randomBytes(8).toString("hex"),
      clock: this.clock,
      titleMs: this.launch.state.title_ms,
      log: this.log,
      onChange: () => this.changed(),
    })
    this.screen = new Screen({
      clock: this.clock,
      log: this.log,
      frames: this.launch.frames,
      major: () => this.major,
      boot: () => this.pages.boot,
      emit: (header, jpeg) => this.relay?.send(header, jpeg),
    })
  }

  // listen opens relay.sock, and awaits nothing of the browser.
  async listen() {
    const launch = this.launch
    this.server = await listen({
      socket: launch.sockets.relay,
      helper: launch.helper,
      helloMs: launch.hello_ms,
      headerBytes: launch.relay.header_bytes,
      payloadBytes: launch.frames.max_bytes,
      rates: [launch.frames.watch_fps],
      clock: this.clock,
      log: this.log,
      ready: () => this.s.state !== "starting",
      answer: () => ({ boot: this.pages.boot, chromium: this.chromium() }),
      relay: (conn) => this.relayed(conn),
      watch: (conn, watch) => conn === this.relay && this.screen.watched(watch),
      ack: (conn, boot, seq) => conn === this.relay && this.screen.acked(boot, seq),
      bye: (conn) => conn === this.relay && this.screen.watched(undefined),
      gone: (conn) => {
        if (conn !== this.relay) return
        this.relay = undefined
        this.screen.watched(undefined)
      },
    })
  }

  hooks() {
    return {
      onLaunched: (context) => {
        this.version = context.browser().version()
        this.major = Number(this.version.split(".")[0])
        this.pages.launched(context)
      },
      onState: (state) => this.state(state),
      onModel: (job) => this.model(job),
      onTabs: (job, action, answer) => this.pages.told(job, action, answer),
      close: () => this.close(),
    }
  }

  // state is the supervisor's state: what the browser is doing, and, once
  // no browser is served, no tab.
  state(state) {
    if (!Object.hasOwn(RUNNING, state)) return
    if (state !== "ready" && this.s.current === undefined) this.pages.reset()
    this.running = RUNNING[state]
    this.changed()
  }

  // model is the model connection's job, or undefined once it closed.
  model(job) {
    const now = this.clock.now()
    if (this.modelJob !== undefined && this.modelJob !== job) this.left = { job: this.modelJob, at: now }
    this.modelJob = job
    this.modelSince = now
    this.pages.model(job)
    const relay = this.relay
    if (relay !== undefined && (job === relay.job) !== relay.connected) {
      relay.connected = job === relay.job
      relay.since = now
      this.checkin()
    }
  }

  // relayed is a relay's connection, accepted: told the newest state and its
  // job's check-in, its screen waiting for its own watch.
  relayed(conn) {
    this.relay = conn
    this.screen.watched(undefined)
    conn.connected = this.modelJob === conn.job
    if (conn.connected) conn.since = this.modelSince
    else conn.since = this.left?.job === conn.job ? this.left.at : this.attached
    this.publish()
    this.checkin()
  }

  checkin() {
    const r = this.relay
    if (r !== undefined) r.send({ t: "checkin", v: 1, job: r.job, connected: r.connected, since_ms: r.since })
  }

  // changed publishes what changed once the current task is done, so a
  // burst of changes makes one state.
  changed() {
    if (this.dirty) return
    this.dirty = true
    queueMicrotask(
      guarded(this.log, "live_failed", () => {
        this.dirty = false
        this.publish()
      }),
    )
  }

  // publish shows the screen the page in view, and the relay the state.
  publish() {
    this.screen.show(this.pages.view)
    if (this.relay === undefined || this.running === undefined) return
    const tabs = this.pages.tabs()
    if (tabs.length > this.launch.state.max_tabs) this.log("tabs_left_out", { tabs: tabs.length - this.launch.state.max_tabs })
    this.relay.send({ t: "state", ...this.states.make(this.pages.boot, this.running, tabs) })
  }

  // chromium is the browser served, as the relay's hello is answered.
  chromium() {
    const launch = this.launch
    return { version: this.version, major: this.major, build: launch.chromium.build, sandbox: launch.sandbox }
  }

  close() {
    this.server?.close()
    this.relay = undefined
    this.screen.close()
    this.pages.reset()
  }
}
