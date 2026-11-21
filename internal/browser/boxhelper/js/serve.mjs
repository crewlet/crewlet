// serve.mjs: browser.sock, where a job's browser server reaches Playwright
// MCP embedded in the supervisor. A connection says its hello first, within
// hello_ms, and is answered ok or refused with why; an accepted one is the
// box's one model connection, closing the one before it, and is handed to
// Playwright MCP's createConnection over newline-delimited JSON, its context
// the supervisor's browser. internal/browser/boxhelper's package doc carries
// the argument.

import fsp from "node:fs/promises"
import net from "node:net"

// LINE_MAX is the longest line either side may send, in bytes, its newline
// aside: MCP's stdio framing, newline-delimited JSON, bounded where a
// full-page screenshot's answer, a few MB of base64, fits many times over.
export const LINE_MAX = 32 << 20

// HELLO_MAX bounds a hello, which a few hundred bytes hold.
const HELLO_MAX = 4096

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

// serve listens on socket, mode 0600, and answers {close}, which stops it
// listening, which removes the socket (libuv unlinks a listening pipe's path
// as it closes it), and ends every connection. o is {socket,
// protocol, helper, helloMs, clock, log, state, createConnection, context,
// config}: state() is the supervisor's, context() the browser's context, and
// config Playwright MCP's.
export async function serve(o) {
  await fsp.rm(o.socket, { force: true })
  const connections = new Set()
  let model
  const server = net.createServer((sock) => {
    connections.add(sock)
    sock.on("close", () => connections.delete(sock))
    sock.on("error", () => {})
    hello(o, sock).then(
      async (said) => {
        if (said === undefined) return
        model?.close()
        const transport = new LineTransport(sock, said.rest, o.log)
        const mcp = await o.createConnection(o.config, o.context)
        const current = { close: () => void mcp.close().catch(() => {}) }
        model = current
        sock.on("close", () => {
          if (model === current) model = undefined
        })
        o.log("model_connected", { job: said.job })
        await mcp.connect(transport)
      },
      (err) => {
        o.log("connection_failed", { error: String(err?.message ?? err).slice(0, 512) })
        sock.destroy()
      },
    )
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
    close: async () => {
      model?.close()
      server.close()
      for (const sock of connections) sock.destroy()
    },
  }
}

// hello reads sock's hello and answers it: ok, and {job, rest}, the bytes
// after it; or refused (helper_mismatch, bad_hello, not_ready before the first
// launch is served), and undefined, the connection ended. One that says
// nothing in helloMs is closed; one that closes before its first byte is
// ensure looking, and is not logged.
function hello(o, sock) {
  return new Promise((resolve) => {
    const parts = []
    let size = 0
    const answer = (fields) => `${JSON.stringify({ crewlet: "browser-mcp", v: o.protocol, ...fields })}\n`
    const finish = (said) => {
      o.clock.clearTimeout(timer)
      sock.off("data", onData)
      sock.off("close", onClose)
      resolve(said)
    }
    const refuse = (why) => {
      o.log("hello_refused", { refused: why })
      sock.end(answer({ ok: false, refused: why }))
      finish(undefined)
    }
    const timer = o.clock.setTimeout(() => {
      if (size > 0) o.log("hello_timeout", {})
      sock.destroy()
      finish(undefined)
    }, o.helloMs)
    const onClose = () => finish(undefined)
    const onData = (chunk) => {
      const nl = chunk.indexOf(10)
      parts.push(nl < 0 ? chunk : chunk.subarray(0, nl))
      size += chunk.length
      if (nl < 0) {
        if (size > HELLO_MAX) refuse("bad_hello")
        return
      }
      sock.pause()
      const why = judge(o, Buffer.concat(parts).toString("utf8"))
      if (typeof why === "string") return refuse(why)
      sock.write(answer({ ok: true }))
      finish({ job: why.job, rest: chunk.subarray(nl + 1) })
    }
    sock.on("data", onData)
    sock.on("close", onClose)
  })
}

// judge is a hello's refusal, or {job, root}: the shape first, then the
// helper's version, then whether the browser was ever up to be served. One
// during a relaunch is accepted: ensure answers found then, and the context
// getter awaits the relaunch, so refusing it would leave a harness started on
// that answer with no browser for its whole job.
function judge(o, line) {
  let h
  try {
    h = JSON.parse(line)
  } catch {
    return "bad_hello"
  }
  if (
    h === null ||
    typeof h !== "object" ||
    h.crewlet !== "browser-mcp" ||
    h.v !== o.protocol ||
    typeof h.helper !== "string" ||
    !/^[0-9a-f]{16}$/.test(h.helper) ||
    typeof h.job !== "string" ||
    !UUID.test(h.job) ||
    !fileRoot(h.root)
  ) {
    return "bad_hello"
  }
  if (h.helper !== o.helper) return "helper_mismatch"
  if (o.state() === "starting") return "not_ready"
  return { job: h.job, root: h.root }
}

// fileRoot is whether root is an absolute file: URL.
function fileRoot(root) {
  try {
    const u = new URL(root)
    return u.protocol === "file:" && u.pathname.startsWith("/") && u.host === ""
  } catch {
    return false
  }
}

// LineTransport is an MCP transport over a socket: one JSON value a line,
// LINE_MAX at most. A line past it, or one that is not JSON, ends the
// connection.
class LineTransport {
  constructor(sock, rest, log) {
    this.sock = sock
    // rest is what came after the hello, read as the first chunk once the
    // server has started, so a message sent with the hello is answered.
    this.rest = rest
    this.parts = []
    this.size = 0
    this.log = log
  }

  async start() {
    this.sock.on("data", (chunk) => this.feed(chunk))
    this.sock.on("close", () => this.onclose?.())
    this.sock.on("error", (err) => this.onerror?.(err))
    const rest = this.rest
    this.rest = undefined
    this.feed(rest)
    this.sock.resume()
  }

  feed(chunk) {
    let rest = chunk
    for (;;) {
      const nl = rest.indexOf(10)
      if (nl < 0) {
        if (rest.length > 0) this.parts.push(rest)
        this.size += rest.length
        if (this.size > LINE_MAX) this.end("line_too_long")
        return
      }
      const line = Buffer.concat([...this.parts, rest.subarray(0, nl)])
      this.parts = []
      this.size = 0
      rest = rest.subarray(nl + 1)
      if (line.length > LINE_MAX) return this.end("line_too_long")
      if (line.length === 0) continue
      let message
      try {
        message = JSON.parse(line.toString("utf8"))
      } catch {
        return this.end("not_json")
      }
      this.onmessage?.(message)
    }
  }

  end(why) {
    this.log("connection_refused", { refused: why })
    this.sock.destroy()
  }

  async send(message) {
    if (this.sock.destroyed) return
    if (!this.sock.write(`${JSON.stringify(message)}\n`)) {
      await new Promise((resolve) => {
        this.sock.once("drain", resolve)
        this.sock.once("close", resolve)
      })
    }
  }

  async close() {
    this.sock.destroy()
  }
}
