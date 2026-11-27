// serve.mjs: browser.sock, where the shim reaches Playwright MCP embedded in
// the supervisor. A connection says its hello first, within hello_ms, and is
// answered ok or refused with why; an accepted one is the box's one model
// connection, closing the one before it, and is handed to Playwright MCP's
// createConnection over newline-delimited JSON, its context the supervisor's
// browser, every message through the edits on its way: the allowed tools
// alone, and a page's words inside a fence. internal/browser/boxhelper's
// package doc carries the argument.

import fsp from "node:fs/promises"
import net from "node:net"

import { LINE_MAX, isObject } from "./common.mjs"
import { fence, neutralise, newBoundary } from "./fence.mjs"

// HELLO_MAX bounds a hello, which a few hundred bytes hold.
const HELLO_MAX = 4096

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

// NAME_MAX is how many bytes of a tool name the model sent an Unknown tool
// answer repeats: more than five times the longest the pin lists, 24, so a
// longer name is no tool's, and the model's own bytes come back bounded.
const NAME_MAX = 128

// ALLOWED is every tool the model may call and the only tools it is shown: the
// pinned server's core tools, less browser_run_code_unsafe, which runs code
// in the supervisor's own process with Playwright's API in hand. A list of
// what may pass, since the server also lists and calls by name the tools a
// page registers (WebMCP), and a later pin may add more.
export const ALLOWED = new Set([
  "browser_click",
  "browser_close",
  "browser_console_messages",
  "browser_drag",
  "browser_drop",
  "browser_emulate_media",
  "browser_evaluate",
  "browser_file_upload",
  "browser_fill_form",
  "browser_find",
  "browser_handle_dialog",
  "browser_hover",
  "browser_navigate",
  "browser_navigate_back",
  "browser_network_request",
  "browser_network_requests",
  "browser_press_key",
  "browser_resize",
  "browser_select_option",
  "browser_snapshot",
  "browser_tabs",
  "browser_take_screenshot",
  "browser_type",
  "browser_wait_for",
])

// mcpConfig is Playwright MCP's config for launch: the browser the getter
// hands it, never one it launches or isolates; no capability past the core
// tools; WebMCP off, which this path turns on by default; the browser's
// context shared, so browser_close refuses; and the outputs a model leaves
// unnamed, and each call's bounds, as launch.json states them.
export function mcpConfig(launch) {
  return {
    browser: { browserName: "chromium", isolated: false },
    capabilities: [],
    webmcp: false,
    sharedBrowserContext: true,
    outputDir: launch.output,
    outputMaxSize: launch.mcp.output_max_bytes,
    timeouts: { action: launch.mcp.action_ms, navigation: launch.mcp.navigation_ms },
  }
}

// serve listens on socket, mode 0600, and answers {close}, which stops it
// listening, which removes the socket (libuv unlinks a listening pipe's path
// as it closes it), and ends every connection. o is {socket, protocol,
// helper, helloMs, clock, log, state, createConnection, context, config,
// onModel, onTabs}: state() is the supervisor's, context() the browser's
// context, and config Playwright MCP's; onModel(job) is told the model
// connection's job as it is accepted, and undefined as it closes, and
// onTabs(job, action, answer) each browser_tabs call that moved the current
// tab (Edits).
export async function serve(o) {
  await fsp.rm(o.socket, { force: true })
  const connections = new Set()
  const box = { model: undefined }
  const server = net.createServer((sock) => {
    connections.add(sock)
    sock.on("close", () => connections.delete(sock))
    sock.on("error", () => {})
    hello(o, sock)
      .then((said) => said === undefined || accept(o, sock, said, box))
      .catch((err) => {
        o.log("connection_failed", { error: String(err?.message ?? err).slice(0, 512) })
        sock.destroy()
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
    close: async () => {
      box.model?.close()
      server.close()
      for (const sock of connections) sock.destroy()
    },
  }
}

// accept makes sock, whose hello said said, the box's one model connection,
// closing the one before it, and hands it to Playwright MCP through the
// edits. It is the model connection from this call on, before any await, so
// of two hellos accepted together the later one alone stays.
async function accept(o, sock, said, box) {
  const current = {
    mcp: undefined,
    close() {
      sock.destroy()
      void this.mcp?.close().catch(() => {})
    },
  }
  const prior = box.model
  box.model = current
  o.onModel(said.job)
  prior?.close()
  sock.on("close", () => {
    if (box.model !== current) return
    box.model = undefined
    o.onModel(undefined)
  })
  const onTabs = (action, answer) => o.onTabs(said.job, action, answer)
  const transport = new ModelTransport(sock, said.rest, new Edits({ log: o.log, onTabs }), o.log)
  current.mcp = await o.createConnection(o.config, o.context)
  if (sock.destroyed) return void current.mcp.close().catch(() => {})
  o.log("model_connected", { job: said.job })
  await current.mcp.connect(transport)
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
    !isObject(h) ||
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

// kind is what a JSON-RPC 2.0 message is, as the server's SDK tells one, or
// undefined for anything else.
function kind(m) {
  if (!isObject(m) || m.jsonrpc !== "2.0") return undefined
  const id = typeof m.id === "string" || Number.isInteger(m.id)
  if (typeof m.method === "string") {
    if (!("id" in m)) return "notification"
    return id ? "request" : undefined
  }
  return id && ("result" in m) !== ("error" in m) ? "response" : undefined
}

// TAB_MOVES is every browser_tabs action that can move which tab is current,
// as the server makes them: new, select and close, but never list.
const TAB_MOVES = new Set(["new", "select", "close"])

// Edits is what the supervisor does to one model connection's messages, in
// each one's order of travel (the package doc's edits), with the table of the
// model's requests the server holds and has not answered. onTabs(action,
// answer) is told each browser_tabs call of TAB_MOVES the server answered
// without error, with its result as the server gave it, before any edit.
export class Edits {
  constructor({ boundary = newBoundary(), allowed = ALLOWED, log = () => {}, onTabs = () => {} } = {}) {
    this.boundary = boundary
    this.allowed = allowed
    this.log = log
    this.onTabs = onTabs
    // pending is each request of the model's handed to the server and not
    // yet answered, by id, as {method}, and a tools/call's {tool, action}.
    this.pending = new Map()
  }

  // inFlight is the ids of the model's tools/call the server is running: in
  // at its request, out at its answer, at the model's notifications/cancelled
  // for it, after which the server never answers it, or as the connection
  // closes.
  get inFlight() {
    return [...this.pending].filter(([, call]) => call.method === "tools/call").map(([id]) => id)
  }

  // fromModel is what becomes of a message from the model's side: {toServer,
  // toModel}, the messages each way, or {refused} for one that ends the
  // connection: no JSON-RPC message, or a request reusing an id in flight.
  fromModel(m) {
    const k = kind(m)
    if (k === undefined) return { refused: "not_jsonrpc" }
    if (k === "request") {
      if (this.pending.has(m.id)) return { refused: "id_in_flight" }
      const call = { method: m.method }
      if (m.method === "tools/call") {
        const name = m.params?.name
        if (typeof name !== "string" || !this.allowed.has(name)) {
          this.log("tool_refused", {})
          return this.answerInstead(m, { error: { code: -32602, message: `Unknown tool: ${this.named(name)}` } })
        }
        // Playwright's CLI daemon's own channel into a call, read around the
        // tool's schema, which drops every other key it does not declare.
        if (isObject(m.params.arguments) && Object.hasOwn(m.params.arguments, "_meta")) {
          delete m.params.arguments._meta
          this.log("call_meta_dropped", {})
        }
        const action = isObject(m.params.arguments) ? m.params.arguments.action : undefined
        call.tool = name
        if (typeof action === "string") call.action = action
      }
      this.pending.set(m.id, call)
    } else if (k === "notification" && m.method === "notifications/cancelled") {
      this.pending.delete(m.params?.requestId)
    }
    return { toServer: [m], toModel: [] }
  }

  // answerInstead is the one place a request of the model's is answered in
  // the server's place, with reply, and never handed to it, so it never
  // enters the table.
  answerInstead(m, reply) {
    return { toServer: [], toModel: [JSON.stringify({ jsonrpc: "2.0", id: m.id, ...reply })] }
  }

  // fromServer is what becomes of a message from the server: {toModel,
  // toServer}, the lines toward the model and the messages back.
  fromServer(m) {
    const none = { toModel: [], toServer: [] }
    const k = kind(m)
    if (k === "response") {
      if (!this.pending.has(m.id)) {
        // One the model cancelled, or none of its requests asked for.
        this.log("answer_dropped", {})
        return none
      }
      const call = this.pending.get(m.id)
      const method = call.method
      this.pending.delete(m.id)
      if (call.tool === "browser_tabs" && TAB_MOVES.has(call.action) && "result" in m && m.result?.isError !== true) {
        this.onTabs(call.action, m.result)
      }
      let out = m
      if (method === "tools/call") out = "error" in m ? this.callError(m) : this.fenced(m)
      else if (method === "tools/list" && "result" in m) out = this.listed(m)
      return { toModel: this.lines(out, method), toServer: [] }
    }
    if (k === "request") {
      if (m.method === "roots/list" || m.method === "ping") return { toModel: this.lines(m), toServer: [] }
      this.log("request_refused", { method: m.method.slice(0, 64) })
      return { toModel: [], toServer: [{ jsonrpc: "2.0", id: m.id, error: { code: -32601, message: "Method not found" } }] }
    }
    if (k === "notification") {
      switch (m.method) {
        case "notifications/progress": {
          // Nothing fences a progress message, so the model is told how far,
          // never in whose words.
          const { message: _, ...params } = isObject(m.params) ? m.params : {}
          return { toModel: this.lines({ ...m, params }), toServer: [] }
        }
        case "notifications/cancelled":
        case "notifications/tools/list_changed":
          return { toModel: this.lines(m), toServer: [] }
      }
      this.log("notification_dropped", { method: m.method.slice(0, 64) })
      return none
    }
    this.log("server_message_dropped", {})
    return none
  }

  // closed forgets every request in flight: the connection ended, and the
  // server answers none of them.
  closed() {
    this.pending.clear()
  }

  // fenced is a tools/call result as the model reads it: each text item
  // inside the connection's fence, each image as it is, any other item
  // dropped, and nothing of the result but those and isError.
  fenced(m) {
    const r = isObject(m.result) ? m.result : {}
    const content = []
    let dropped = 0
    for (const item of Array.isArray(r.content) ? r.content : []) {
      if (item?.type === "text" && typeof item.text === "string") {
        content.push({ type: "text", text: fence(this.boundary, item.text) })
      } else if (item?.type === "image" && typeof item.data === "string" && typeof item.mimeType === "string") {
        content.push({ type: "image", data: item.data, mimeType: item.mimeType })
      } else {
        dropped += 1
      }
    }
    if (dropped > 0) this.log("content_dropped", { items: dropped })
    return { jsonrpc: "2.0", id: m.id, result: { content, ...(r.isError === true ? { isError: true } : {}) } }
  }

  // callError is a tools/call the server answered with an error: its code and
  // its words, neutralised, since no fence wraps them.
  callError(m) {
    const { code, message } = isObject(m.error) ? m.error : {}
    return {
      jsonrpc: "2.0",
      id: m.id,
      error: { code: Number.isInteger(code) ? code : -32603, message: neutralise(this.boundary, String(message ?? "")) },
    }
  }

  // listed is a tools/list result with every tool outside the list removed.
  listed(m) {
    const tools = Array.isArray(m.result?.tools) ? m.result.tools : []
    const shown = tools.filter((tool) => this.allowed.has(tool?.name))
    if (shown.length < tools.length) this.log("tools_hidden", { tools: tools.length - shown.length })
    return { jsonrpc: "2.0", id: m.id, result: { tools: shown } }
  }

  // named is a tool name the model sent, as an Unknown tool answer repeats
  // it: neutralised, then cut to NAME_MAX bytes, never inside a character.
  named(name) {
    const text = neutralise(this.boundary, typeof name === "string" ? name : (JSON.stringify(name) ?? ""))
    let out = ""
    let bytes = 0
    for (const ch of text) {
      bytes += Buffer.byteLength(ch)
      if (bytes > NAME_MAX) break
      out += ch
    }
    return out
  }

  // lines is m as the lines toward the model: past LINE_MAX, an answer to a
  // tools/call is replaced by an error result saying so, an answer to any
  // other request by an error, and anything else is dropped.
  lines(m, method) {
    const text = JSON.stringify(m)
    const n = Buffer.byteLength(text)
    if (n <= LINE_MAX) return [text]
    this.log("answer_too_long", { bytes: n })
    const words = `The browser's answer was ${n} bytes, more than this box passes on. Ask for less: a smaller screenshot or snapshot.`
    if (method === "tools/call") {
      return [JSON.stringify({ jsonrpc: "2.0", id: m.id, result: { content: [{ type: "text", text: words }], isError: true } })]
    }
    if (method !== undefined) return [JSON.stringify({ jsonrpc: "2.0", id: m.id, error: { code: -32603, message: words } })]
    return []
  }
}

// ModelTransport is the server's transport over one model connection:
// newline-delimited JSON of at most LINE_MAX a line, each message through
// edits on its way. A line past the bound, one that is not JSON, or one the
// edits refuse ends the connection.
class ModelTransport {
  constructor(sock, rest, edits, log) {
    this.sock = sock
    // rest is what came after the hello, read as the first chunk once the
    // server has started, so a message sent with the hello is answered.
    this.rest = rest
    this.parts = []
    this.size = 0
    this.edits = edits
    this.log = log
  }

  async start() {
    this.sock.on("data", (chunk) => this.feed(chunk))
    this.sock.on("close", () => {
      this.edits.closed()
      this.onclose?.()
    })
    this.sock.on("error", (err) => this.onerror?.(err))
    const rest = this.rest
    this.rest = undefined
    this.feed(rest)
    this.sock.resume()
  }

  feed(chunk) {
    let rest = chunk
    for (;;) {
      if (this.sock.destroyed) return
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
      const out = this.edits.fromModel(message)
      if (out.refused !== undefined) return this.end(out.refused)
      for (const l of out.toModel) void this.write(l)
      for (const m of out.toServer) this.onmessage?.(m)
    }
  }

  end(why) {
    this.log("connection_refused", { refused: why })
    this.sock.destroy()
  }

  // send is the server's message on its way to the model, through the edits;
  // an answer the edits give the server instead reaches it on a later tick,
  // never inside the send that asked.
  async send(message) {
    const out = this.edits.fromServer(message)
    for (const m of out.toServer) queueMicrotask(() => this.onmessage?.(m))
    for (const l of out.toModel) await this.write(l)
  }

  async write(line) {
    if (this.sock.destroyed) return
    if (!this.sock.write(`${line}\n`)) {
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
