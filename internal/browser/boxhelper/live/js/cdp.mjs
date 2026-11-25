// cdp.mjs: every DevTools command the live view sends, with the parameters
// it may give each, and every event it reads, as a table: send and on refuse
// a call off it, so what the helper asks a browser is what the suite holds to
// each pinned Chromium's own description. internal/browser/boxhelper/live's
// package doc carries the argument.

// FLOW is the screencast's flow pair, which a build below the screen's flow
// major ignores, so the screen gives it only at or above that major.
export const FLOW = Object.freeze(["maxFramesInFlight", "sendLastFrame"])

// SENDS is every command the live view sends, with every parameter it may
// give it.
export const SENDS = Object.freeze({
  "Page.enable": Object.freeze([]),
  "Page.startScreencast": Object.freeze(["format", "quality", "maxWidth", "maxHeight", "everyNthFrame", ...FLOW]),
  "Page.stopScreencast": Object.freeze([]),
  "Page.screencastFrameAck": Object.freeze(["sessionId"]),
  "Page.getLayoutMetrics": Object.freeze([]),
  "Page.captureScreenshot": Object.freeze(["format", "quality", "clip", "captureBeyondViewport"]),
  "Target.getTargetInfo": Object.freeze([]),
  "Target.getTargets": Object.freeze(["filter"]),
})

// READS is every event the live view reads.
export const READS = Object.freeze([
  "Page.screencastFrame",
  "Page.frameResized",
  "Page.frameStartedLoading",
  "Page.frameStoppedLoading",
])

// OffTable is a call the table does not list, refused before it reaches a
// browser.
export class OffTable extends Error {}

// send is session's answer to method with params, once the table lists the
// method and each parameter; a call off it throws OffTable and sends nothing.
export async function send(session, method, params = {}) {
  if (!Object.hasOwn(SENDS, method)) throw new OffTable(`the live view sends no ${method}: SENDS lists every command`)
  for (const key of Object.keys(params)) {
    if (!SENDS[method].includes(key)) throw new OffTable(`the live view gives ${method} no ${key}: SENDS lists each`)
  }
  return session.send(method, params)
}

// on hands listener each event of session's the table reads, and answers
// the function that stops it; an event off the table throws OffTable.
export function on(session, event, listener) {
  if (!READS.includes(event)) throw new OffTable(`the live view reads no ${event}: READS lists every event`)
  session.on(event, listener)
  return () => session.off(event, listener)
}
