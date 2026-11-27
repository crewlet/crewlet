// screen.mjs: the screencast of the page in view, only while the relay's
// watch says on, paced to its rate with one frame in flight: at each slot the
// newest frame held leaves and the older are acknowledged to Chromium unsent,
// and a frame sent is acknowledged once its frame_ack came and the next slot
// opened. The last picture always arrives: from the screencast's flow pair
// at or above the flow major, below it from one capture once the stream goes
// quiet, and on either from one capture once the page resizes.
// internal/browser/boxhelper/live's package doc carries the argument.

import { on, send } from "./cdp.mjs"
import { jpegSize } from "./jpeg.mjs"
import { describe, guarded } from "./pages.mjs"

// Screen is one box's screen. frames is config.json's frames block; major()
// the Chromium major served; boot() the boot frames are numbered in; emit
// hands a frame's header and its JPEG to the relay; every timer is clock's.
export class Screen {
  constructor({ clock, log, frames, major, boot, emit }) {
    this.clock = clock
    this.log = log
    this.frames = frames
    this.major = major
    this.boot = boot
    this.emit = emit
    this.record = undefined
    this.watch = undefined
    this.run = undefined
    this.held = []
    this.unsent = []
    this.inFlight = undefined
    this.owed = undefined
    this.lastForward = -Infinity
    this.slotTimer = undefined
    this.quietTimer = undefined
    this.seq = 0
    this.seqBoot = undefined
  }

  // show is the page in view, a registry's record or undefined: told again
  // once its session opens.
  show(record) {
    this.record = record
    this.update()
  }

  // watched is the relay's newest watch, {on, fps}, or undefined once the
  // relay said bye or its connection closed: no frame_ack comes after it.
  watched(watch) {
    this.watch = watch
    if (watch === undefined) {
      this.inFlight = undefined
      this.clock.clearTimeout(this.slotTimer)
      this.slotTimer = undefined
    }
    this.update()
  }

  // acked is the relay's frame_ack: the frame in flight, its post answered.
  acked(boot, seq) {
    const f = this.inFlight
    if (f === undefined || f.boot !== boot || f.seq !== seq) return
    this.inFlight = undefined
    if (f.chromium !== undefined) this.owed = f.chromium
    this.slot()
  }

  close() {
    this.watched(undefined)
  }

  // flow is whether the browser served takes the screencast's flow pair.
  flow() {
    return this.major() >= this.frames.flow_major
  }

  // update starts the screencast where the page in view is watched and its
  // session open, on that page alone, and stops it anywhere else.
  update() {
    const record = this.watch?.on === true && this.record?.session !== undefined ? this.record : undefined
    if (this.run !== undefined && this.run.record === record && this.run.session === record?.session) return
    if (this.run !== undefined) this.stop(this.run)
    if (record !== undefined) this.background(this.start(record))
  }

  // start captures the page once, then starts its screencast: the first
  // frame a watcher gets is that capture, the page as it is, which no frame
  // of the screencast passes at its slot.
  async start(record) {
    const run = { record, session: record.session, stops: [], capturing: false, again: false }
    this.run = run
    await this.capture(run, this.frames.quality, 1, true)
    if (this.run !== run) return
    try {
      run.stops.push(on(run.session, "Page.screencastFrame", guarded(this.log, "screen_failed", (f) => this.frame(run, f))))
      run.stops.push(on(run.session, "Page.frameResized", guarded(this.log, "screen_failed", () => this.resized(run))))
      const params = {
        format: "jpeg",
        quality: this.frames.quality,
        maxWidth: this.frames.max_width,
        maxHeight: this.frames.max_height,
        everyNthFrame: 1,
      }
      if (this.flow()) Object.assign(params, { maxFramesInFlight: 1, sendLastFrame: true })
      await send(run.session, "Page.startScreencast", params)
    } catch (err) {
      this.log("screen_failed", { call: "start", error: describe(err) })
      if (this.run === run) this.stop(run)
    }
  }

  // stop ends run: its frames held are acknowledged to Chromium and
  // dropped, as is what Chromium is owed, and its screencast stopped.
  stop(run) {
    if (this.run !== run) return
    this.run = undefined
    for (const stop of run.stops) stop()
    this.quiet(false)
    for (const f of this.held) this.chromiumAck(f.chromium)
    this.held = []
    for (const chromium of this.unsent.splice(0)) this.chromiumAck(chromium)
    this.chromiumAck(this.owed)
    this.owed = undefined
    if (this.inFlight !== undefined) {
      this.chromiumAck(this.inFlight.chromium)
      this.inFlight.chromium = undefined
    }
    send(run.session, "Page.stopScreencast").catch((err) =>
      this.log("screen_failed", { call: "stop", error: describe(err) }),
    )
  }

  // frame is one screencast frame of run: held for its slot once its bytes
  // are a JPEG the hub takes and its metadata within the wire's bounds;
  // past the byte bound it is replaced by one smaller capture. A frame not
  // sent is acknowledged at the next slot as an older one is, so Chromium
  // is paced however its frames fare; acknowledged at once, a stream of
  // frames past the bound ran Chromium flat out, and it shrank them.
  frame(run, f) {
    const chromium = { session: run.session, sessionId: f?.sessionId }
    if (this.run !== run) return this.chromiumAck(chromium)
    this.quiet(false)
    const jpeg = Buffer.from(String(f?.data ?? ""), "base64")
    if (jpeg.length > this.frames.max_bytes) {
      this.unsend(chromium)
      this.log("frame_oversize", { bytes: jpeg.length })
      return this.background(this.capture(run, this.frames.oversize_quality, this.frames.oversize_scale))
    }
    const m = f?.metadata
    const at = typeof m?.timestamp === "number" && m.timestamp > 0 ? Math.round(m.timestamp * 1000) : this.clock.now()
    const meta = this.within(jpeg, {
      at_ms: at,
      device_width: Math.round(m?.deviceWidth),
      device_height: Math.round(m?.deviceHeight),
      offset_top: Math.round(m?.offsetTop),
      page_scale: m?.pageScaleFactor,
    })
    if (meta === undefined) return this.unsend(chromium)
    this.hold({ capture: "screencast", chromium, record: run.record, jpeg, meta })
  }

  // resized is run's page resized: one capture is taken, the newest frame
  // then, since after a scaled capture the screencast's own frames of a
  // resized page were seen not to come, or to come under its old size
  // (2026-11-25). Resizes while one is under way take one more after it.
  resized(run) {
    if (this.run !== run) return
    if (run.capturing) {
      run.again = true
      return
    }
    run.capturing = true
    this.background(
      this.capture(run, this.frames.quality, 1).finally(() => {
        run.capturing = false
        if (run.again && this.run === run) {
          run.again = false
          this.resized(run)
        }
      }),
    )
  }

  // unsend keeps a frame not sent for its acknowledgement at the next slot.
  unsend(chromium) {
    this.unsent.push(chromium)
    this.slot()
  }

  // capture takes one capture of run's page, of its visual viewport fitted
  // to the frame's bound and scaled by scale, at quality: an ordinary frame,
  // its metadata from the same layout metrics, opening its run where opens.
  // It is scaled, by a clip, only where it must be: a scaled capture runs
  // through device emulation, after which the screencast was seen to lose a
  // resize. One past the byte bound is replaced by one at the oversize
  // quality and scale, held only if it fits.
  async capture(run, quality, scale, opens = false) {
    let metrics
    let shot
    try {
      metrics = await send(run.session, "Page.getLayoutMetrics")
      if (this.run !== run) return
      const view = metrics.cssVisualViewport
      const fit = Math.min(1, this.frames.max_width / view.clientWidth, this.frames.max_height / view.clientHeight)
      const params = { format: "jpeg", quality, captureBeyondViewport: false }
      if (fit * scale < 1) {
        params.clip = { x: view.pageX, y: view.pageY, width: view.clientWidth, height: view.clientHeight, scale: fit * scale }
      }
      shot = await send(run.session, "Page.captureScreenshot", params)
    } catch (err) {
      return this.log("screen_failed", { call: "capture", error: describe(err) })
    }
    if (this.run !== run) return
    const jpeg = Buffer.from(String(shot?.data ?? ""), "base64")
    if (jpeg.length > this.frames.max_bytes) {
      this.log("frame_oversize", { bytes: jpeg.length })
      if (quality === this.frames.oversize_quality && scale === this.frames.oversize_scale) return
      return this.capture(run, this.frames.oversize_quality, this.frames.oversize_scale, opens)
    }
    const meta = this.within(jpeg, {
      at_ms: this.clock.now(),
      device_width: Math.round(metrics.cssLayoutViewport?.clientWidth),
      device_height: Math.round(metrics.cssLayoutViewport?.clientHeight),
      offset_top: 0,
      page_scale: metrics.cssVisualViewport?.scale,
    })
    if (meta !== undefined) this.hold({ capture: "screenshot", chromium: undefined, record: run.record, jpeg, meta, opens })
  }

  // within is meta with jpeg's own width and height, or undefined, logged,
  // where jpeg is no JPEG the hub takes or a field is past its bound.
  within(jpeg, meta) {
    const f = this.frames
    const size = jpegSize(jpeg, f.jpeg_header_bytes)
    if (size === undefined || size.width < 1 || size.width > f.max_width || size.height < 1 || size.height > f.max_height) {
      this.log("frame_dropped", { why: "not_jpeg" })
      return undefined
    }
    const device = (n, least) => Number.isInteger(n) && n >= least && n <= f.device_max
    const scaled = typeof meta.page_scale === "number" && meta.page_scale >= f.scale_min && meta.page_scale <= f.scale_max
    if (!device(meta.device_width, 1) || !device(meta.device_height, 1) || !device(meta.offset_top, 0) || !scaled) {
      this.log("frame_dropped", { why: "metadata" })
      return undefined
    }
    return { ...meta, width: size.width, height: size.height }
  }

  // hold keeps a frame for the next slot.
  hold(frame) {
    this.held.push(frame)
    this.slot()
  }

  // slot opens once 1000/fps ms have passed since the last frame sent and
  // none awaits its frame_ack: what Chromium is owed is acknowledged, and the
  // newest frame held is sent, the older acknowledged unsent; a run's opening
  // capture is sent first, and what came after it waits for the next slot.
  slot() {
    if (this.inFlight !== undefined || this.watch === undefined) return
    const wait = this.lastForward + 1000 / this.watch.fps - this.clock.now()
    if (wait > 0) {
      this.slotTimer ??= this.clock.setTimeout(
        guarded(this.log, "screen_failed", () => {
          this.slotTimer = undefined
          this.slot()
        }),
        wait,
      )
      return
    }
    for (const chromium of this.unsent.splice(0)) this.chromiumAck(chromium)
    if (this.owed !== undefined) {
      this.chromiumAck(this.owed)
      this.owed = undefined
      this.quiet(!this.flow())
    }
    if (this.held[0]?.opens === true) return this.forward(this.held.shift())
    const newest = this.held.pop()
    for (const older of this.held) this.chromiumAck(older.chromium)
    this.held = []
    if (newest !== undefined) this.forward(newest)
  }

  // forward sends frame to the relay, numbered in its boot.
  forward(frame) {
    const boot = this.boot()
    if (boot !== this.seqBoot) {
      this.seqBoot = boot
      this.seq = 0
    }
    this.seq += 1
    const { at_ms, width, height, device_width, device_height, offset_top, page_scale } = frame.meta
    const header = { t: "frame", v: 1, boot, seq: this.seq, tab: frame.record.id, capture: frame.capture, at_ms, width,
      height, device_width, device_height, offset_top, page_scale }
    this.inFlight = { boot, seq: this.seq, chromium: frame.chromium }
    this.lastForward = this.clock.now()
    this.emit(header, frame.jpeg)
  }

  // quiet arms, or disarms, the capture that ends a quiet stream on a build
  // below the flow major: armed once a screencast frame sent has had its
  // slot, it takes one capture after quiet_ms, and any frame that comes
  // first disarms it, so a still page after its capture takes none.
  quiet(arm) {
    this.clock.clearTimeout(this.quietTimer)
    this.quietTimer = undefined
    if (!arm || this.run === undefined) return
    const run = this.run
    this.quietTimer = this.clock.setTimeout(
      guarded(this.log, "screen_failed", () => {
        this.quietTimer = undefined
        if (this.run === run) this.background(this.capture(run, this.frames.quality, 1))
      }),
      this.frames.quiet_ms,
    )
  }

  // background runs promise unawaited, what it rejects with logged: a
  // rejection nobody handles would reach the model's next answer.
  background(promise) {
    promise.catch((err) => this.log("screen_failed", { error: describe(err) }))
  }

  // chromiumAck acknowledges a screencast frame to Chromium, a failure
  // logged: the session of a page gone answers none.
  chromiumAck(chromium) {
    if (chromium === undefined) return
    send(chromium.session, "Page.screencastFrameAck", { sessionId: chromium.sessionId }).catch((err) =>
      this.log("screen_failed", { call: "ack", error: describe(err) }),
    )
  }
}
