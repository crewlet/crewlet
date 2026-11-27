// dial.mjs: the relay's requests to its run's bridge, through Node's http and
// https and agents of its own, kept alive and given no proxy, so neither a
// proxy variable nor a Node that honours NODE_USE_ENV_PROXY sends one through
// a proxy: the relay reaches its bridge directly, as the harness does. Each
// request is bounded on the relay's clock, and its answer read to a bound.
// internal/browser/boxhelper/live's package doc carries the argument.

import http from "node:http"
import https from "node:https"

// Failed is a request that got no answer: refused, reset, cut at its bound,
// or answered past what the relay reads. code names which, and never the
// address, which holds the run's token.
export class Failed extends Error {
  constructor(code) {
    super(`no answer: ${code}`)
    this.code = code
  }
}

// dialer is the relay's way to its bridge: request(url, {method, headers,
// body, bound, signal}) answers {status, retryAfter, body} once the whole
// answer is read, at most bodyBytes of it, or throws Failed; close ends its
// connections.
export function dialer({ clock, bodyBytes }) {
  const agents = {
    "http:": new http.Agent({ keepAlive: true }),
    "https:": new https.Agent({ keepAlive: true }),
  }
  return {
    request: (url, o) => request(agents, clock, bodyBytes, url, o),
    close: () => {
      for (const agent of Object.values(agents)) agent.destroy()
    },
  }
}

// request settles once: answered as its whole answer is read, or failed at
// the first of a refusal, a reset, its bound, its signal, or an answer past
// bodyBytes, whether or not its answer had begun, since a request destroyed
// mid-answer raises no event that would settle it later.
function request(agents, clock, bodyBytes, url, { method, headers = {}, body, bound, signal }) {
  return new Promise((resolve, reject) => {
    const u = new URL(url)
    const transport = u.protocol === "https:" ? https : http
    let settled = false
    const settle = (answer, failed) => {
      if (settled) return
      settled = true
      clock.clearTimeout(timer)
      signal?.removeEventListener("abort", onAbort)
      if (failed === undefined) resolve(answer)
      else reject(failed)
    }
    const fail = (code) => {
      settle(undefined, new Failed(code))
      req.destroy()
    }
    const req = transport.request(u, { method, headers, agent: agents[u.protocol] }, (res) => {
      const parts = []
      let read = 0
      res.on("data", (chunk) => {
        read += chunk.length
        if (read > bodyBytes) return fail("answer_too_long")
        parts.push(chunk)
      })
      res.on("end", () =>
        settle({ status: res.statusCode, retryAfter: retryAfter(res.headers["retry-after"]), body: Buffer.concat(parts) }),
      )
      res.on("error", () => fail("reset"))
      res.on("close", () => {
        if (!res.complete) fail("reset")
      })
    })
    const timer = clock.setTimeout(() => fail("bound"), bound)
    const onAbort = () => fail("abandoned")
    req.on("error", (err) => fail(typeof err?.code === "string" ? err.code : "error"))
    if (signal?.aborted) return onAbort()
    signal?.addEventListener("abort", onAbort, { once: true })
    req.end(body)
  })
}

// retryAfter is a Retry-After header's seconds in ms, or undefined: the
// bridge writes whole seconds.
function retryAfter(value) {
  if (typeof value !== "string" || !/^[0-9]{1,6}$/.test(value)) return undefined
  return Number(value) * 1000
}
