"use strict"
// reach.js: the box's half of the launch's check of its network, which the
// shell half runs with node -e. internal/browser/hostreach's package doc
// carries the argument. CommonJS and Node's built-ins alone, so any node a
// box holds runs it.
const fs = require("fs")
const net = require("net")
const dns = require("dns")

const LINE = "crewlet-browser-reach/1"
// ROUTES is every route an address is dialed under, in the order an address
// two of them name is dialed under the first; proxy, the last, is dialed
// through each proxy.
const ROUTES = ["loopback", "gateway", "host_alias", "rootless_gateway"]
const PROXY_VARS = ["http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"]
const MAX_ADDRESSES = 64
const MAX_PROXY_ADDRESSES = 8
// MAX_HEARD is the most of an answer a dial reads before it settles it as
// answered: the engine's runs 3 bytes raw and 64 to a proxy's GET, and a
// proxy's own headers stay well under 4 KiB.
const MAX_HEARD = 4096
const IDENTITY_MAP = /^\s*0\s+0\s+4294967295\s*$/
// LOOPBACK_TARGETS is each spelling of the engine's loopback a proxy is
// asked for: a name the proxy resolves, and either address.
const LOOPBACK_TARGETS = ["127.0.0.1", "localhost", "[::1]"]
// ELSEWHERE is what a proxy of this host that refused every loopback ask is
// asked for at the same port: TEST-NET-1 (RFC 5737), routed nowhere, so a
// proxy that refuses it alike refused the port, not loopback destinations.
const ELSEWHERE = "192.0.2.1"
// LOOPBACK_SOCKETS is the box's own loopback as /proc/net's tables print a
// socket's address, in the kernel's byte order: 127.0.0.1, ::1 and
// ::ffff:127.0.0.1.
const LOOPBACK_SOCKETS = new Set(["0100007F", "00000000000000000000000001000000", "0000000000000000FFFF00000100007F"])

// gateways is every default route's next hop in the box's routing tables, an
// IPv6 link-local one with its interface.
function gateways(route4, route6) {
  const out = []
  for (const line of route4.split("\n").slice(1)) {
    const f = line.trim().split(/\s+/)
    if (f[1] === "00000000" && /^[0-9A-Fa-f]{8}$/.test(f[2] || "") && f[2] !== "00000000") {
      out.push([6, 4, 2, 0].map((i) => parseInt(f[2].substr(i, 2), 16)).join("."))
    }
  }
  const zero = "0".repeat(32)
  for (const line of route6.split("\n")) {
    const f = line.trim().split(/\s+/)
    if (f.length === 10 && f[0] === zero && f[1] === "00" && /^[0-9a-f]{32}$/.test(f[4]) && f[4] !== zero) {
      const hop = f[4].match(/.{4}/g).join(":")
      out.push(hop.startsWith("fe80:") ? hop + "%" + f[9] : hop)
    }
  }
  return out
}

// named is every address the box's hosts file names.
function named(hosts) {
  const out = []
  for (const line of hosts.split("\n")) {
    const words = line.replace(/#.*/, "").trim().split(/\s+/)
    if (words.length > 1) out.push(words[0])
  }
  return out
}

// addresses is every address the box dials, each once, under the first route
// that names it.
function addresses(hosts, route4, route6, beyond) {
  const found = {
    loopback: ["127.0.0.1", "::1"],
    gateway: gateways(route4, route6),
    host_alias: named(hosts),
    rootless_gateway: beyond,
  }
  const out = []
  const seen = new Set()
  for (const route of ROUTES) {
    for (const address of found[route]) {
      if (net.isIP(address.split("%")[0]) && !seen.has(address)) {
        seen.add(address)
        out.push({ route, address })
      }
    }
  }
  return out
}

// proxies is every proxy the box's Chromium may use, read as Chromium reads
// its environment: the scheme and any user dropped, an HTTP proxy at the host
// and port named, 80 where none is.
function proxies(env) {
  const out = []
  for (const name of PROXY_VARS) {
    let v = (env[name] || "").trim()
    if (v.includes("://")) v = v.slice(v.indexOf("://") + 3)
    if (v.includes("@")) v = v.slice(v.indexOf("@") + 1)
    const hostPort = v.split("/")[0]
    const m = /^\[([^\]]+)\](?::(\d+))?$/.exec(hostPort) || /^([^:[\]]+)(?::(\d+))?$/.exec(hostPort)
    const port = m && m[2] ? Number(m[2]) : 80
    if (m && port > 0 && port < 65536 && !out.some((p) => p.host === m[1] && p.port === port)) {
      out.push({ host: m[1], port })
    }
  }
  return out
}

// canon is an address in one spelling, as a URL's host holds it, so a proxy's
// address and this host's compare however either was written.
function canon(address) {
  try {
    const host = new URL("http://" + (address.includes(":") ? "[" + address + "]" : address) + "/").hostname
    return host.replace(/^\[|\]$/g, "")
  } catch (err) {
    return address
  }
}

// dial is one connection walking steps. Not made within bounds.route, or
// failed before it was, it leads nowhere. Once made it is answered by its last
// step's answer, another answer its peer ends with, or its close; and held
// when none comes within bounds.answer. It settles with how, and the status
// of the first HTTP answer it heard, 0 for none.
function dial(io, host, port, steps, bounds) {
  return new Promise((settle) => {
    let sock
    try {
      sock = io.connect(host, port)
    } catch (err) {
      settle({ how: "nowhere", status: 0 })
      return
    }
    let heard = Buffer.alloc(0)
    let at = 0
    let made = false
    let settled = false
    let status = 0
    let cancel = io.after(bounds.route, () => done("nowhere"))
    function done(how) {
      if (settled) return
      settled = true
      cancel()
      sock.destroy()
      settle({ how, status })
    }
    sock.on("connect", () => {
      made = true
      cancel()
      cancel = io.after(bounds.answer, () => done("held"))
      sock.write(steps[0].send)
    })
    sock.on("data", (chunk) => {
      heard = Buffer.concat([heard, chunk])
      if (heard.length > MAX_HEARD) return done("answered")
      while (!settled && at < steps.length && steps[at].want(heard)) {
        if (status === 0) status = statusOf(heard)
        if (steps[at].go && !steps[at].go(heard)) return done("answered")
        heard = Buffer.alloc(0)
        at++
        if (at === steps.length) return done("answered")
        sock.write(steps[at].send)
      }
    })
    sock.on("error", () => done(made ? "answered" : "nowhere"))
    sock.on("close", () => done(made ? "answered" : "nowhere"))
  })
}

// resolve is each proxy at every address its host has, none for a name that
// fails, or held where a name does not resolve within bound.
function resolve(io, list, bound) {
  return Promise.all(
    list.map(
      (p) =>
        new Promise((settle) => {
          if (net.isIP(p.host)) return settle({ found: [p] })
          const cancel = io.after(bound, () => settle({ held: true, found: [] }))
          io.lookup(p.host, (err, all) => {
            cancel()
            settle({ found: err ? [] : all.map((a) => ({ host: a.address, port: p.port })) })
          })
        }),
    ),
  )
}

const line = (b) => b.includes(10)
const headers = (b) => b.includes("\r\n\r\n")
const ok200 = (b) => /^HTTP\/1\.[01] 200[ \r]/.test(b.toString("latin1"))
const statusOf = (b) => Number((/^HTTP\/1\.[01] ([0-9]{3})[ \r]/.exec(b.toString("latin1")) || [0, 0])[1])
const refused = (o) => o.status >= 400 && o.status < 500
const nowhere = (o) => o.how === "nowhere"

// asks is what a proxy is asked of target: a GET of the proxy route's token
// there, and a tunnel there that carries the token once it opens.
function asks(target, token) {
  const get = "GET http://" + target + "/" + token + " HTTP/1.1\r\nHost: " + target + "\r\nConnection: close\r\n\r\n"
  const tunnel = "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
  return [
    [{ send: get, want: headers }],
    [{ send: tunnel, want: headers, go: ok200 }, { send: token + "\n", want: line }],
  ]
}

// loopbackSockets is the inode of every socket bound on the box's own
// loopback, as its tables list them: TCP listening, UDP unconnected; null
// where a table cannot be read, the IPv6 ones absent aside.
function loopbackSockets(io) {
  const out = new Set()
  const tables = [["/proc/net/tcp", "0A"], ["/proc/net/tcp6", "0A"], ["/proc/net/udp", "07"], ["/proc/net/udp6", "07"]]
  for (const [path, state] of tables) {
    const table = io.read(path, path.endsWith("6"))
    if (table === null) return null
    for (const row of table.split("\n").slice(1)) {
      const f = row.trim().split(/\s+/)
      if (f.length > 9 && f[3] === state && LOOPBACK_SOCKETS.has(f[1].split(":")[0].toUpperCase()) && f[9] !== "0") {
        out.add(f[9])
      }
    }
  }
  return out
}

// heldSockets is the inode of every socket a process the box can see holds,
// read off each of its threads' descriptor tables: a process's own listing,
// /proc/<pid>/fd, is its main thread's, and is empty once that thread exits
// while the others hold the table on.
function heldSockets(io) {
  const out = new Set()
  for (const pid of io.list("/proc")) {
    if (!/^[0-9]+$/.test(pid)) continue
    for (const tid of io.list("/proc/" + pid + "/task")) {
      if (!/^[0-9]+$/.test(tid)) continue
      const fds = "/proc/" + pid + "/task/" + tid + "/fd"
      for (const fd of io.list(fds)) {
        const m = /^socket:\[([0-9]+)\]$/.exec(io.link(fds + "/" + fd))
        if (m) out.add(m[1])
      }
    }
  }
  return out
}

// forwarded is whether a socket stayed bound on the box's own loopback while
// no process the box can see held it: something outside the box bound it
// there. Null where a table cannot be read.
function forwarded(io) {
  const before = loopbackSockets(io)
  if (before === null || before.size === 0) return before === null ? null : false
  const held = heldSockets(io)
  const after = loopbackSockets(io)
  if (after === null) return null
  return [...before].some((inode) => after.has(inode) && !held.has(inode))
}

// check is the box's word for the engine, the first that holds of:
// unreadable, too_many, forwarded, held, proxy_down, proxy_refused; else
// checked.
async function check(env, io) {
  const port = Number(env.CREWLET_REACH_PORT)
  const bounds = { route: Number(env.CREWLET_REACH_ROUTE_MS), answer: Number(env.CREWLET_REACH_ANSWER_MS) }
  const tokens = {}
  for (const pair of (env.CREWLET_REACH_TOKENS || "").split(",")) {
    const [route, token] = pair.split(":")
    if (route && token) tokens[route] = token
  }
  if (!(port > 0 && bounds.route > 0 && bounds.answer > 0 && tokens.loopback)) {
    throw new Error("the engine handed the check no port, bounds or tokens")
  }
  const ours = new Set((env.CREWLET_REACH_HOST || "").split(",").filter(Boolean).map(canon))
  const route4 = io.read("/proc/net/route", false)
  const route6 = io.read("/proc/net/ipv6_route", true)
  const hosts = io.read("/etc/hosts", true)
  if (route4 === null || route6 === null || hosts === null) return "unreadable"
  const map = io.read("/proc/self/uid_map", false)
  const beyond =
    map !== null && IDENTITY_MAP.test(map) ? [] : (env.CREWLET_REACH_BEYOND || "").split(",").filter(Boolean)
  const list = addresses(hosts, route4, route6, beyond).filter((a) => tokens[a.route])
  const resolved = tokens.proxy ? await resolve(io, proxies(env), bounds.answer) : []
  const found = [].concat(...resolved.map((n) => n.found))
  if (list.length > MAX_ADDRESSES || found.length > MAX_PROXY_ADDRESSES) return "too_many"
  const direct = list.map((a) => dial(io, a.address, port, [{ send: tokens[a.route] + "\n", want: line }], bounds))
  const through = found.map((p) =>
    Promise.all(
      LOOPBACK_TARGETS.flatMap((t) => asks(t + ":" + port, tokens.proxy)).map((s) => dial(io, p.host, p.port, s, bounds)),
    ),
  )
  const [outcomes, answers] = await Promise.all([Promise.all(direct), Promise.all(through)])
  // A proxy of this host that refused every loopback ask is asked elsewhere.
  // A refusal comes before the proxy dials, so an answer not heard within
  // bounds.route is the proxy's own dial: not refused.
  const suspects = found.filter((p, i) => ours.has(canon(p.host)) && answers[i].every(refused))
  const controls = await Promise.all(
    suspects.map((p) =>
      Promise.all(
        asks(ELSEWHERE + ":" + port, tokens.proxy).map((s) =>
          dial(io, p.host, p.port, s, { route: bounds.route, answer: bounds.route }),
        ),
      ),
    ),
  )
  const moved = forwarded(io)
  if (moved === null) return "unreadable"
  if (moved) return "forwarded"
  if (resolved.some((n) => n.held) || outcomes.concat(...answers).some((o) => o.how === "held")) return "held"
  // A proxy is down where none of its addresses took a connection: Chromium
  // goes on to a proxy's next address, as to an IPv4 one past an IPv6 one a
  // box without IPv6 cannot reach.
  let at = 0
  const down = resolved.some((n) => {
    const mine = answers.slice(at, (at += n.found.length))
    return mine.every((a) => a.every(nowhere))
  })
  if (down || controls.some((c) => c.some(nowhere))) return "proxy_down"
  return controls.some((c) => c.every(refused)) ? "proxy_refused" : "checked"
}

const box = {
  read(path, absentIsEmpty) {
    try {
      return fs.readFileSync(path, "utf8")
    } catch (err) {
      return absentIsEmpty && err.code === "ENOENT" ? "" : null
    }
  },
  list(path) {
    try {
      return fs.readdirSync(path)
    } catch (err) {
      return []
    }
  },
  link(path) {
    try {
      return fs.readlinkSync(path)
    } catch (err) {
      return ""
    }
  },
  connect: (host, port) => net.connect({ host, port }),
  lookup: (host, cb) => dns.lookup(host, { all: true }, cb),
  after(ms, f) {
    const t = setTimeout(f, ms)
    return () => clearTimeout(t)
  },
}

if (process.env.CREWLET_REACH_PORT) {
  setTimeout(() => process.exit(3), Number(process.env.CREWLET_REACH_DEADLINE_MS)).unref()
  check(process.env, box).then((word) => {
    const said = " kernel=" + process.env.CREWLET_REACH_KERNEL + " net=" + process.env.CREWLET_REACH_NET + " "
    process.stdout.write(LINE + said + word + "\n", () => process.exit(0))
  })
} else {
  module.exports = {
    ROUTES,
    MAX_ADDRESSES,
    MAX_PROXY_ADDRESSES,
    LOOPBACK_TARGETS,
    gateways,
    named,
    addresses,
    proxies,
    dial,
    resolve,
    check,
  }
}
