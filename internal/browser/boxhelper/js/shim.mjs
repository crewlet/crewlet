// shim.mjs: `node helper.mjs mcp --job <id> --root <path>`, the box's browser
// as a harness's stdio MCP server. It says the job's hello on browser.sock,
// then relays newline-delimited JSON both ways, byte for byte but for two
// messages: an initialize gains the roots capability where the harness
// declared none, and the server's roots/list is answered here, with the job's
// root, never reaching the harness. It holds no token and reads no variable.
// internal/browser/boxhelper's package doc carries the argument.

import fs from "node:fs"
import net from "node:net"
import path from "node:path"
import { pathToFileURL } from "node:url"

import { BROWSER_SOCKET, LINE_MAX, isObject, readConfig, realClock } from "./common.mjs"

// ANSWER_MS bounds the wait for the supervisor's answer to the hello, which it
// gives from memory in milliseconds, and for the last bytes either way once
// one side has gone: past it the harness sees its server end rather than hang.
const ANSWER_MS = 5000

// ANSWER_MAX bounds the answer to the hello: the supervisor's is under 100
// bytes, so this bounds only what a peer that is no supervisor makes the shim
// hold before it gives up.
const ANSWER_MAX = 4096

// UNAVAILABLE is the exit of a shim that reached no browser: no socket, a
// refused hello or connection, or a peer that broke the framing.
const UNAVAILABLE = 69

// shim connects the job's MCP session on stdio to the browser of the box whose
// helper is in folder, and relays it until either side ends: stdin's end, after
// the socket is half-closed, or the socket's, after stdout is closed. Both
// exit 0.
export async function shim(folder, job, root) {
  let config
  try {
    config = await readConfig(folder)
  } catch (err) {
    fail(`the helper's config cannot be read: ${err.message}`)
  }
  const sock = await connect(path.join(folder, BROWSER_SOCKET))
  const rootURL = pathToFileURL(root).href
  sock.write(`${JSON.stringify({ crewlet: "browser-mcp", v: config.protocol, helper: config.helper, job, root: rootURL })}\n`)
  const { said, rest } = await answer(sock)
  if (said?.ok !== true) fail(`the browser refused this job's hello: ${String(said?.refused ?? "no reason").slice(0, 64)}`)
  relay(sock, rest, rootURL)
}

// fail ends the shim, saying why on stderr, which is the harness's log, before
// it exits.
function fail(why) {
  try {
    fs.writeSync(2, `browser: ${why}\n`)
  } catch {
    // A harness that closed our stderr reads the exit alone.
  }
  process.exit(UNAVAILABLE)
}

// connect is a connection to socket, or the shim's end: none there, or none
// it may make.
function connect(socket) {
  return new Promise((resolve) => {
    const sock = net.connect(socket)
    sock.once("connect", () => {
      sock.off("error", refused)
      resolve(sock)
    })
    const refused = (err) => fail(`no browser listens at ${socket} (${err.code ?? err.message})`)
    sock.once("error", refused)
  })
}

// answer is the supervisor's one line answering the hello, parsed, and the
// bytes after it, within ANSWER_MS.
function answer(sock) {
  return new Promise((resolve) => {
    const parts = []
    let size = 0
    const timer = realClock.setTimeout(() => fail(`the browser did not answer this job's hello in ${ANSWER_MS} ms`), ANSWER_MS)
    const onEnd = () => fail("the browser closed this job's connection before it answered")
    const onData = (chunk) => {
      const nl = chunk.indexOf(10)
      parts.push(nl < 0 ? chunk : chunk.subarray(0, nl))
      size += chunk.length
      if (nl < 0) {
        if (size > ANSWER_MAX) fail("the browser's answer to the hello runs past its bound")
        return
      }
      realClock.clearTimeout(timer)
      sock.off("data", onData)
      sock.off("close", onEnd)
      sock.pause()
      let said
      try {
        said = JSON.parse(Buffer.concat(parts).toString("utf8"))
      } catch {
        said = undefined
      }
      resolve({ said: isObject(said) && said.crewlet === "browser-mcp" ? said : undefined, rest: chunk.subarray(nl + 1) })
    }
    sock.on("data", onData)
    sock.on("close", onEnd)
  })
}

// relay passes stdin to the socket and the socket to stdout, a line at a
// time, each as it came but for the two messages the shim owns, each side
// paused while the other cannot take more.
function relay(sock, rest, root) {
  const stdin = process.stdin
  const stdout = process.stdout
  let ending = false
  const end = (finish) => {
    if (ending) return
    ending = true
    finish(() => process.exit(0))
    realClock.setTimeout(() => process.exit(0), ANSWER_MS)
  }
  const tooLong = (from) => () => fail(`a line from ${from} runs past ${LINE_MAX} bytes, which the browser refuses`)

  const toServer = new Lines((line) => {
    if (!sock.write(withRoots(line) ?? Buffer.concat([line, NEWLINE]))) {
      stdin.pause()
      sock.once("drain", () => stdin.resume())
    }
  }, tooLong("the harness"))
  const toModel = new Lines((line) => {
    const id = rootsRequest(line)
    if (id !== undefined) {
      sock.write(`${JSON.stringify({ jsonrpc: "2.0", id, result: { roots: [{ uri: root, name: "workspace" }] } })}\n`)
      return
    }
    if (!stdout.write(Buffer.concat([line, NEWLINE]))) {
      sock.pause()
      stdout.once("drain", () => sock.resume())
    }
  }, tooLong("the browser"))

  stdin.on("data", (chunk) => toServer.push(chunk))
  stdin.on("end", () => {
    const left = toServer.rest()
    if (left.length > 0) sock.write(left)
    end((exit) => sock.end(exit))
  })
  stdin.on("error", () => end((exit) => sock.end(exit)))
  sock.on("data", (chunk) => toModel.push(chunk))
  sock.on("close", () => {
    const left = toModel.rest()
    if (left.length > 0) stdout.write(left)
    end((exit) => stdout.end(exit))
  })
  sock.on("error", () => {})
  stdout.on("error", () => end((exit) => sock.end(exit)))
  toModel.push(rest)
  sock.resume()
}

const NEWLINE = Buffer.from("\n")

// withRoots is line, an initialize of the harness's that declares no roots,
// with the roots capability added; or undefined for any other line, which is
// relayed as it came.
function withRoots(line) {
  const m = parse(line)
  if (!isObject(m) || m.method !== "initialize" || !("id" in m) || !isObject(m.params)) return undefined
  const caps = m.params.capabilities
  if (caps !== undefined && !isObject(caps)) return undefined
  if (isObject(caps) && Object.hasOwn(caps, "roots")) return undefined
  return Buffer.from(`${JSON.stringify({ ...m, params: { ...m.params, capabilities: { ...caps, roots: {} } } })}\n`)
}

// rootsRequest is the id of line, the server's roots/list request, which the
// shim answers; or undefined for any other line.
function rootsRequest(line) {
  const m = parse(line)
  if (isObject(m) && m.method === "roots/list" && (typeof m.id === "string" || Number.isInteger(m.id))) return m.id
  return undefined
}

function parse(line) {
  try {
    return JSON.parse(line.toString("utf8"))
  } catch {
    return undefined
  }
}

// Lines cuts a stream into lines, each handed to onLine without its newline;
// a line past LINE_MAX is tooLong's instead.
class Lines {
  constructor(onLine, tooLong) {
    this.onLine = onLine
    this.tooLong = tooLong
    this.parts = []
    this.size = 0
  }

  push(chunk) {
    let start = 0
    for (;;) {
      const nl = chunk.indexOf(10, start)
      if (nl < 0) {
        const tail = chunk.subarray(start)
        if (tail.length > 0) {
          this.parts.push(tail)
          this.size += tail.length
          if (this.size > LINE_MAX) this.tooLong()
        }
        return
      }
      const line = Buffer.concat([...this.parts, chunk.subarray(start, nl)])
      this.parts = []
      this.size = 0
      start = nl + 1
      if (line.length > LINE_MAX) return this.tooLong()
      this.onLine(line)
    }
  }

  // rest is what came after the last newline, as it came.
  rest() {
    const left = Buffer.concat(this.parts)
    this.parts = []
    this.size = 0
    return left
  }
}
