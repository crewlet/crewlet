"use strict"
// drive.cjs: the hostreach suite's driver of reach.js. Run as
//
//   node drive.cjs <reach.js copied as a .cjs>
//
// with one JSON request on stdin, it answers one JSON line on stdout. Its
// check runs reach.js's own check against a fake io: the box's files, a
// connection per address that behaves as the request plans, lookups that
// answer, fail or never come, and a clock that moves only when nothing else
// is left to happen, so a bound is reached exactly and at once.
const { EventEmitter } = require("events")
const reach = require(process.argv[2])

const HTTP_OK = "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nConnection: close\r\n\r\nok\n"
const FORBIDDEN = "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"

let input = ""
process.stdin.setEncoding("utf8")
process.stdin.on("data", (chunk) => (input += chunk))
process.stdin.on("end", () => {
  run(JSON.parse(input)).then(
    (out) => process.stdout.write(JSON.stringify(out) + "\n"),
    (err) => {
      process.stdout.write(JSON.stringify({ error: String((err && err.stack) || err) }) + "\n")
      process.exitCode = 1
    },
  )
})

async function run(req) {
  switch (req.op) {
    case "parse":
      return {
        routes: reach.ROUTES,
        maxAddresses: reach.MAX_ADDRESSES,
        maxProxyAddresses: reach.MAX_PROXY_ADDRESSES,
        loopbackTargets: reach.LOOPBACK_TARGETS,
        gateways: reach.gateways(req.route4 || "", req.route6 || ""),
        addresses: reach.addresses(req.hosts || "", req.route4 || "", req.route6 || "", req.beyond || []),
        proxies: reach.proxies(req.env || {}),
      }
    case "check": {
      const io = fakeIO(req)
      let threw = ""
      const word = await io.settle(
        reach.check(req.env, io).catch((err) => {
          threw = String(err.message || err)
          return ""
        }),
      )
      return { word, threw, dials: io.dials, now: io.now() }
    }
  }
  throw new Error("no op " + req.op)
}

// fakeIO answers req.files (a path not there, or in req.absent, is ENOENT,
// one null unreadable, a list each read in turn, its last for good), req.dirs (a directory's names, none where it is not
// there) and req.links (a link's target), connects each address as req.plan
// names its behaviour (refuse unless named), and answers req.lookups (a list
// of addresses, "error" or "pending").
function fakeIO(req) {
  let now = 0
  let pending = 0
  const timers = []
  const dials = []
  const reads = {}
  const soon = (f) => {
    pending++
    setImmediate(() => {
      pending--
      f()
    })
  }
  const behaviours = {
    refuse(s) {
      soon(() => {
        s.emit("error", Object.assign(new Error("connect ECONNREFUSED"), { code: "ECONNREFUSED" }))
        s.emit("close")
      })
    },
    silent() {},
    "made-silent"(s) {
      soon(() => s.made())
    },
    "made-close"(s) {
      soon(() => s.made())
      s.onWrite = () => soon(() => s.emit("close"))
    },
    engine(s) {
      soon(() => s.made())
      s.onWrite = (data) => soon(() => s.emit("data", Buffer.from(data.startsWith("GET ") ? HTTP_OK : "ok\n")))
    },
    "proxy-open"(s) {
      soon(() => s.made())
      s.onWrite = (data) => {
        if (data.startsWith("GET ")) soon(() => s.emit("data", Buffer.from(HTTP_OK)))
        else if (data.startsWith("CONNECT ")) soon(() => s.emit("data", Buffer.from("HTTP/1.1 200 Connection established\r\n\r\n")))
        else soon(() => s.emit("data", Buffer.from("ok\n")))
      }
    },
    "proxy-refuse"(s) {
      soon(() => s.made())
      s.onWrite = () => soon(() => s.emit("data", Buffer.from(FORBIDDEN)))
    },
    // The terse proxies answer a status line with no reason phrase.
    "proxy-terse"(s) {
      soon(() => s.made())
      s.onWrite = (data) => {
        if (data.startsWith("GET ")) soon(() => s.emit("data", Buffer.from("HTTP/1.1 200\r\nContent-Length: 3\r\n\r\nok\n")))
        else if (data.startsWith("CONNECT ")) soon(() => s.emit("data", Buffer.from("HTTP/1.1 200\r\n\r\n")))
        else soon(() => s.emit("data", Buffer.from("ok\n")))
      }
    },
    "proxy-refuse-terse"(s) {
      soon(() => s.made())
      s.onWrite = () => soon(() => s.emit("data", Buffer.from("HTTP/1.1 403\r\n\r\n")))
    },
    // proxy-dest refuses a request for a loopback destination and dials any
    // other, which answers nothing: TEST-NET-1 is routed nowhere.
    "proxy-dest"(s) {
      soon(() => s.made())
      s.onWrite = (data) => {
        if (/^(GET http:\/\/|CONNECT )(127\.0\.0\.1|localhost|\[::1\]):/.test(data)) {
          soon(() => s.emit("data", Buffer.from(FORBIDDEN)))
        }
      }
    },
  }
  return {
    dials,
    now: () => now,
    read(path, absentIsEmpty) {
      const files = req.files || {}
      if (!(path in files) || (req.absent || []).includes(path)) return absentIsEmpty ? "" : null
      if (!Array.isArray(files[path])) return files[path]
      reads[path] = (reads[path] || 0) + 1
      return files[path][Math.min(reads[path], files[path].length) - 1]
    },
    list: (path) => (req.dirs || {})[path] || [],
    link: (path) => (req.links || {})[path] || "",
    connect(host, port) {
      const s = new EventEmitter()
      const log = { host, port, sent: [], made: -1, destroyed: -1 }
      dials.push(log)
      s.onWrite = () => {}
      s.made = () => {
        if (log.destroyed >= 0) return
        log.made = now
        s.emit("connect")
      }
      s.write = (data) => {
        log.sent.push(String(data))
        s.onWrite(String(data))
      }
      s.destroy = () => {
        if (log.destroyed < 0) log.destroyed = now
      }
      const behaviour = behaviours[(req.plan || {})[host] || "refuse"]
      if (!behaviour) throw new Error("no behaviour " + req.plan[host])
      behaviour(s)
      return s
    },
    lookup(host, cb) {
      const answer = (req.lookups || {})[host]
      if (answer === "pending") return
      soon(() => (Array.isArray(answer) ? cb(null, answer.map((address) => ({ address }))) : cb(new Error("ENOTFOUND"))))
    },
    after(ms, f) {
      const t = { at: now + ms, f, live: true }
      timers.push(t)
      return () => {
        t.live = false
      }
    },
    // settle runs the fake world until promise answers: every event already
    // due first, then the earliest timer, the clock moved to it.
    async settle(promise) {
      let done = false
      let answer
      promise.then((a) => {
        done = true
        answer = a
      })
      for (;;) {
        await new Promise((r) => setImmediate(r))
        if (done) return answer
        if (pending > 0) continue
        const next = timers.filter((t) => t.live).sort((a, b) => a.at - b.at)[0]
        if (!next) throw new Error("nothing is left to happen, and the check has not answered")
        now = next.at
        next.live = false
        next.f()
      }
    },
  }
}
