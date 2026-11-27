// relaysock.mjs: relay.sock, where a job's relay reaches the live view in
// the supervisor: a filesystem socket in the helper's folder at 0600, a stale
// one removed first. A connection says its hello within hello_ms and is
// answered hello, or refused, helper_mismatch or not_ready before the first
// launch is served; the newest accepted one closes the one before. A record
// past its bounds or off its end's types, a watch of a rate the settings do
// not name, or any other malformed record ends the connection, logged.
// internal/browser/boxhelper/live's package doc carries the argument.

import fsp from "node:fs/promises"
import net from "node:net"

import { FROM_RELAY, Reader, Refused, VERSION, Writer } from "./frames.mjs"
import { describe } from "./pages.mjs"

const HELPER = /^[0-9a-f]{16}$/
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
const BOOT = /^[0-9a-f]{16}$/

// listen listens on o.socket and answers {close}, which stops it listening,
// removing the socket, and ends every connection. o is {socket, helper,
// helloMs, headerBytes, payloadBytes, rates, clock, log, ready, answer,
// relay, watch, ack, bye, gone}: ready() is whether a relay may be served,
// answer() the hello's boot and chromium; relay(conn) is told each accepted
// connection, conn {job, send, close}, and watch(conn, {on, fps}), ack(conn,
// boot, seq), bye(conn) and gone(conn) what it says and its end.
export async function listen(o) {
  await fsp.rm(o.socket, { force: true })
  const sockets = new Set()
  let current
  const server = net.createServer((sock) => {
    sockets.add(sock)
    sock.on("error", () => {})
    const conn = connection(o, sock, (accepted) => {
      const prior = current
      current = accepted
      prior?.close()
      safely(o, () => o.relay(accepted))
    })
    sock.on("close", () => {
      sockets.delete(sock)
      if (conn.accepted && current === conn) current = undefined
      if (conn.accepted) safely(o, () => o.gone(conn))
    })
  })
  await new Promise((resolve, reject) => {
    server.once("error", reject)
    server.listen(o.socket, () => {
      server.off("error", reject)
      resolve()
    })
  })
  await fsp.chmod(o.socket, 0o600)
  return {
    close: () => {
      server.close()
      for (const sock of sockets) sock.destroy()
    },
  }
}

// connection reads one connection's records: its hello, judged, and then
// what an accepted relay says; accept is told it once it is answered hello.
function connection(o, sock, accept) {
  const writer = new Writer(sock)
  const conn = {
    accepted: false,
    job: undefined,
    send: (header, payload) => writer.send(header, payload),
    close: () => sock.destroy(),
  }
  let heard = 0
  const timer = o.clock.setTimeout(() => {
    if (conn.accepted) return
    if (heard > 0) o.log("relay_refused", { refused: "hello_timeout" })
    sock.destroy()
  }, o.helloMs)
  const end = (why) => {
    o.log("relay_refused", { refused: why })
    sock.destroy()
  }
  // failed ends the connection on anything its reading threw, which no
  // record of a relay's may turn into the supervisor's exit.
  const failed = (err) => {
    if (err instanceof Refused) return end("framing")
    o.log("relay_failed", { error: describe(err) })
    sock.destroy()
  }
  const reader = new Reader({
    types: FROM_RELAY,
    headerBytes: o.headerBytes,
    payloadBytes: o.payloadBytes,
    onRecord: (header) => {
      if (!conn.accepted) return hello(header)
      switch (header.t) {
        case "watch": {
          const fps = header.fps
          if (typeof header.on !== "boolean" || !Number.isInteger(fps) || fps < 1) return end("bad_watch")
          if (header.on && !o.rates.includes(fps)) return end("watch_rate")
          return o.watch(conn, { on: header.on, fps: header.on ? fps : o.rates[0] })
        }
        case "frame_ack":
          if (!BOOT.test(String(header.boot)) || !Number.isInteger(header.seq) || header.seq < 1) return end("bad_ack")
          return o.ack(conn, header.boot, header.seq)
        case "bye":
          return o.bye(conn)
        default:
          return end("not_after_hello")
      }
    },
  })
  const hello = (h) => {
    o.clock.clearTimeout(timer)
    if (h.t !== "hello" || !HELPER.test(String(h.helper)) || !UUID.test(String(h.job))) return end("bad_hello")
    if (h.helper !== o.helper) return refuse("helper_mismatch")
    if (!o.ready()) return refuse("not_ready")
    conn.accepted = true
    conn.job = h.job
    conn.send({ t: "hello", v: VERSION, helper: o.helper, ...o.answer() })
    accept(conn)
  }
  const refuse = (reason) => {
    o.log("relay_refused", { refused: reason })
    conn.send({ t: "refused", v: VERSION, reason })
    sock.end()
  }
  sock.on("data", (chunk) => {
    heard += chunk.length
    try {
      reader.push(chunk)
    } catch (err) {
      failed(err)
    }
  })
  sock.on("close", () => o.clock.clearTimeout(timer))
  return conn
}

// safely runs f, logging what it throws rather than letting a socket's
// event turn it into the supervisor's exit.
function safely(o, f) {
  try {
    f()
  } catch (err) {
    o.log("relay_failed", { error: describe(err) })
  }
}
