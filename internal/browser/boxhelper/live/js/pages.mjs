// pages.mjs: every page of every context of the box's browser, each with a
// tab id of its boot, a DevTools session of the helper's own, its title, read
// for every page at once on the clock and as a page loads, its address,
// whether it loads and whether it crashed; and the page in view, which
// follows the model's own tab calls, since no page's state says which is
// shown. internal/browser/boxhelper/live's package doc carries the argument.

import { on, send } from "./cdp.mjs"

// ID_MAX is the highest number a tab id of one boot takes: nine digits.
export const ID_MAX = 999_999_999

// TAB_LINE is one tab of a browser_tabs answer: its index, whether it is the
// server's current tab, and the rest, its title and address.
const TAB_LINE = /^- ([0-9]+):( \(current\))? \[/

// tabsListed is what a browser_tabs answer lists in its Result section: how
// many tabs, and the current one's index and line; undefined for an answer
// that lists them out of order or names two current.
export function tabsListed(answer) {
  const items = Array.isArray(answer?.content) ? answer.content : []
  const lines = items
    .filter((item) => item?.type === "text" && typeof item.text === "string")
    .flatMap((item) => item.text.split("\n"))
  const start = lines.indexOf("### Result")
  if (start < 0) return undefined
  let count = 0
  let current
  for (const line of lines.slice(start + 1)) {
    if (line.startsWith("### ")) break
    const m = TAB_LINE.exec(line)
    if (m === null) continue
    if (Number(m[1]) !== count) return undefined
    if (m[2] !== undefined) {
      if (current !== undefined) return undefined
      current = { index: count, line }
    }
    count += 1
  }
  return { count, current }
}

// Pages is the registry. boot is the supervisor's, and newBoot draws another
// once tab ids run out; titles are read every titleMs on clock; onChange is
// told every change of a page or of the view, as it happens.
export class Pages {
  constructor({ boot, newBoot, clock, titleMs, log, onChange }) {
    this.boot = boot
    this.newBoot = newBoot
    this.clock = clock
    this.titleMs = titleMs
    this.log = log
    this.onChange = onChange
    this.next = 1
    this.records = []
    this.byPage = new Map()
    this.view = undefined
    this.mirror = undefined
    this.persistent = undefined
    this.generation = 0
    this.stops = []
  }

  // launched starts the registry again on the browser context served, the
  // first or a relaunch's: the old browser's pages are gone, and a model
  // connection still live begins again on the new browser's first tab.
  launched(context) {
    this.reset()
    const generation = this.generation
    const browser = context.browser()
    this.persistent = context
    if (this.mirror !== undefined) {
      this.mirror = { job: this.mirror.job, order: [], current: undefined, contexts: new Set([context]) }
    }
    const watch = guarded(this.log, "pages_failed", (ctx) => this.watch(ctx))
    browser.on("context", watch)
    this.stops.push(() => browser.off("context", watch))
    for (const ctx of browser.contexts()) this.watch(ctx)
    this.titles(browser, generation).catch((err) => this.log("pages_failed", { error: describe(err) }))
  }

  // reset forgets the browser served: every page, its session and the view.
  reset() {
    this.generation += 1
    for (const stop of this.stops.splice(0)) stop()
    for (const record of this.records) {
      record.closed = true
      record.session?.detach().catch(() => {})
    }
    this.browserSession?.detach().catch(() => {})
    this.browserSession = undefined
    this.clock.clearTimeout(this.titleTimer)
    this.records = []
    this.byPage = new Map()
    this.view = undefined
    if (this.mirror !== undefined) this.mirror.order = []
    if (this.mirror !== undefined) this.mirror.current = undefined
    this.changed()
  }

  // watch registers ctx's pages and every page it opens; a context made
  // while a model connection is live is that connection's server's.
  watch(ctx) {
    if (this.mirror !== undefined && ctx !== this.persistent) this.mirror.contexts.add(ctx)
    const add = guarded(this.log, "pages_failed", (page) => this.add(page))
    ctx.on("page", add)
    this.stops.push(() => ctx.off("page", add))
    for (const page of ctx.pages()) this.add(page)
  }

  // titles opens one session on the browser, through which every page's
  // title is read in one call every titleMs, and as a page loads: Chromium
  // tells a title a script sets in no event, target discovery's included,
  // and Playwright has none.
  async titles(browser, generation) {
    let session
    try {
      session = await browser.newBrowserCDPSession()
    } catch (err) {
      return this.log("titles_unread", { error: describe(err) })
    }
    if (generation !== this.generation) return session.detach().catch(() => {})
    this.browserSession = session
    const tick = guarded(this.log, "pages_failed", () => {
      if (generation !== this.generation) return
      this.readTitles()
      this.titleTimer = this.clock.setTimeout(tick, this.titleMs)
    })
    tick()
  }

  // readTitles reads every page's title, one read at a time, a page's record
  // told its own; a read asked while one is under way comes after it.
  readTitles() {
    if (this.browserSession === undefined) return
    if (this.reading) {
      this.readAgain = true
      return
    }
    this.reading = true
    const session = this.browserSession
    send(session, "Target.getTargets", { filter: [{ type: "page" }] })
      .then(({ targetInfos }) => {
        if (session !== this.browserSession) return
        let changed = false
        for (const info of Array.isArray(targetInfos) ? targetInfos : []) {
          const record = this.records.find((r) => r.targetId === info?.targetId)
          const title = String(info?.title ?? "")
          if (record !== undefined && record.title !== title) {
            record.title = title
            changed = true
          }
        }
        if (changed) this.changed()
      })
      .catch((err) => {
        if (session === this.browserSession) this.log("titles_unread", { error: describe(err) })
      })
      .finally(() => {
        this.reading = false
        if (this.readAgain) {
          this.readAgain = false
          this.readTitles()
        }
      })
  }

  // add registers page, numbered in its boot, and opens its session.
  add(page) {
    if (this.byPage.has(page)) return
    const record = { page, id: this.nextID(), title: "", loading: false, crashed: false, closed: false }
    this.records.push(record)
    this.byPage.set(page, record)
    const guard = (f) => guarded(this.log, "pages_failed", f)
    page.on("close", guard(() => this.closed(record)))
    page.on(
      "crash",
      guard(() => {
        record.crashed = true
        this.changed()
      }),
    )
    page.on(
      "popup",
      guard((popup) => {
        const opened = this.byPage.get(popup)
        if (opened !== undefined) this.see(opened)
      }),
    )
    page.on(
      "framenavigated",
      guard((frame) => {
        if (frame === page.mainFrame()) this.changed()
      }),
    )
    const m = this.mirror
    if (m !== undefined && m.contexts.has(page.context())) {
      m.order.push(record)
      if (m.current === undefined) {
        m.current = record
        if (this.view === undefined) this.see(record)
      }
    }
    this.open(record, this.generation).catch((err) => this.log("pages_failed", { error: describe(err) }))
    this.changed()
  }

  // open opens record's own session: its target, its title so far, and its
  // main frame's loading. A page that closes meanwhile is passed over.
  async open(record, generation) {
    let session
    try {
      session = await record.page.context().newCDPSession(record.page)
    } catch (err) {
      if (!record.closed) this.log("page_session_failed", { error: describe(err) })
      return
    }
    if (record.closed || generation !== this.generation) return session.detach().catch(() => {})
    try {
      const { targetInfo } = await send(session, "Target.getTargetInfo")
      record.targetId = targetInfo.targetId
      record.title = String(targetInfo.title ?? "")
      const loading = (value) =>
        guarded(this.log, "pages_failed", ({ frameId }) => {
          if (frameId !== record.targetId) return
          record.loading = value
          if (!value) this.readTitles()
          this.changed()
        })
      on(session, "Page.frameStartedLoading", loading(true))
      on(session, "Page.frameStoppedLoading", loading(false))
      await send(session, "Page.enable")
    } catch (err) {
      if (!record.closed) this.log("page_session_failed", { error: describe(err) })
      return session.detach().catch(() => {})
    }
    if (record.closed || generation !== this.generation) return session.detach().catch(() => {})
    record.session = session
    this.changed()
  }

  // closed forgets record. A page in view hands the view to the tab the
  // model's server makes current then, as Playwright MCP does: the one now
  // at its place, or the last.
  closed(record) {
    if (record.closed) return
    record.closed = true
    this.records = this.records.filter((r) => r !== record)
    this.byPage.delete(record.page)
    record.session?.detach().catch(() => {})
    const m = this.mirror
    const at = m === undefined ? -1 : m.order.indexOf(record)
    if (at >= 0) {
      m.order.splice(at, 1)
      if (m.current === record) m.current = m.order[Math.min(at, m.order.length - 1)]
    }
    if (this.view === record) this.see(m?.current)
    this.changed()
  }

  // model is a model connection beginning, for job, on its first tab, as
  // Playwright MCP's own does, or ending, undefined.
  model(job) {
    if (job === undefined) {
      this.mirror = undefined
      return
    }
    const order = this.records.filter((r) => r.page.context() === this.persistent)
    this.mirror = { job, order, current: order[0], contexts: new Set([this.persistent]) }
    this.see(order[0])
  }

  // told is a browser_tabs call of job's that moved the server's current
  // tab, with its answer: the tab it lists current, at its index in the
  // connection's own order, comes into view, once the count and the address
  // agree; a disagreement leaves the view where it was.
  told(job, action, answer) {
    const m = this.mirror
    if (m === undefined || m.job !== job) return
    const listed = tabsListed(answer)
    if (listed === undefined || listed.count !== m.order.length) {
      return this.log("tabs_disagree", { action, listed: listed?.count ?? null, kept: m.order.length })
    }
    const record = listed.current === undefined ? undefined : m.order[listed.current.index]
    if (record !== undefined && !listed.current.line.includes(`](${record.page.url()})`)) {
      return this.log("tabs_disagree", { action, listed: listed.count, kept: m.order.length })
    }
    m.current = record
    this.see(record)
  }

  // see puts record in view, or none.
  see(record) {
    if (this.view === record) return
    this.view = record
    this.changed()
  }

  // nextID is the next tab id of the boot; past ID_MAX a new boot is drawn
  // and the live pages are numbered again from t1, no Chromium restarted.
  nextID() {
    if (this.next > ID_MAX) {
      this.boot = this.newBoot()
      this.next = 1
      for (const record of this.records) record.id = `t${this.next++}`
      this.log("tabs_renumbered", { tabs: this.records.length })
    }
    return `t${this.next++}`
  }

  // tabs is every page, in the order they appeared, as a state lists them.
  tabs() {
    return this.records.map((r) => ({
      id: r.id,
      title: r.title,
      url: r.page.url(),
      front: r === this.view,
      loading: r.loading,
      crashed: r.crashed,
    }))
  }

  changed() {
    this.onChange()
  }
}

// describe is an error's message, bounded for the log.
export function describe(err) {
  return String(err?.message ?? err).slice(0, 512)
}

// guarded is f as an event's or a timer's listener: what it throws is
// logged as kind, never thrown into Playwright's dispatch or the process,
// where it would end the supervisor and the model's browser with it.
export function guarded(log, kind, f) {
  return (...args) => {
    try {
      f(...args)
    } catch (err) {
      log(kind, { error: describe(err) })
    }
  }
}
