// frames.mjs: relay.sock's records, for both its ends. A record is a u32
// big-endian header length, the header's UTF-8 JSON, then header.bytes of
// payload, which only a frame carries. A reader refuses a record past its
// bounds or of a type its end does not read; a writer writes one record at a
// time as the socket drains, its state and check-in slots each holding only
// the newest one not yet written. internal/browser/boxhelper/live's package
// doc carries the argument.

import { isObject } from "./common.mjs"

// VERSION is every record's v: a reader refuses a later one.
export const VERSION = 1

// FROM_RELAY and FROM_SUPERVISOR are the types each end sends, so the other
// reads them and nothing else.
export const FROM_RELAY = Object.freeze(["hello", "watch", "frame_ack", "bye"])
export const FROM_SUPERVISOR = Object.freeze(["hello", "refused", "frame", "state", "checkin"])

// record is header and payload as one record's bytes.
export function record(header, payload) {
  const json = Buffer.from(JSON.stringify(payload === undefined ? header : { ...header, bytes: payload.length }))
  const length = Buffer.alloc(4)
  length.writeUInt32BE(json.length)
  return payload === undefined ? Buffer.concat([length, json]) : Buffer.concat([length, json, payload])
}

// Refused is a record the reader will not read, and why, in the engine's
// words: whoever reads it closes the connection.
export class Refused extends Error {}

// Reader cuts a stream into records of types, each handed to onRecord as
// (header, payload): a header of at most headerBytes, a payload of at most
// payloadBytes and only on a frame. Anything else throws Refused from push,
// and every record after it is refused too.
export class Reader {
  constructor({ types, headerBytes, payloadBytes, onRecord }) {
    this.types = types
    this.headerBytes = headerBytes
    this.payloadBytes = payloadBytes
    this.onRecord = onRecord
    this.buffered = Buffer.alloc(0)
    this.refused = undefined
  }

  push(chunk) {
    if (this.refused !== undefined) throw this.refused
    this.buffered = this.buffered.length === 0 ? chunk : Buffer.concat([this.buffered, chunk])
    for (;;) {
      if (this.buffered.length < 4) return
      const length = this.buffered.readUInt32BE(0)
      if (length < 2 || length > this.headerBytes) this.refuse(`a header of ${length} bytes, past ${this.headerBytes}`)
      if (this.buffered.length < 4 + length) return
      const header = this.header(this.buffered.subarray(4, 4 + length))
      const bytes = header.t === "frame" ? header.bytes : 0
      if (this.buffered.length < 4 + length + bytes) return
      const payload = header.t === "frame" ? Buffer.from(this.buffered.subarray(4 + length, 4 + length + bytes)) : undefined
      this.buffered = this.buffered.subarray(4 + length + bytes)
      this.onRecord(header, payload)
    }
  }

  // header is raw read as one record's header, or Refused.
  header(raw) {
    let header
    try {
      header = JSON.parse(raw.toString("utf8"))
    } catch {
      this.refuse("a header that is no JSON")
    }
    if (!isObject(header) || !this.types.includes(header.t)) this.refuse("a record of a type this end does not read")
    if (header.v !== VERSION) this.refuse(`a record of version ${JSON.stringify(header.v)}, where this end reads ${VERSION}`)
    if (header.t === "frame") {
      if (!Number.isInteger(header.bytes) || header.bytes < 1 || header.bytes > this.payloadBytes) {
        this.refuse(`a frame of ${JSON.stringify(header.bytes)} bytes, past 1 to ${this.payloadBytes}`)
      }
    } else if (Object.hasOwn(header, "bytes")) {
      this.refuse(`a ${header.t} carrying bytes, which only a frame does`)
    }
    return header
  }

  refuse(why) {
    this.refused = new Refused(why)
    throw this.refused
  }
}

// SLOTS are the types of which only the newest one not yet written is kept.
const SLOTS = new Set(["state", "checkin"])

// Writer writes records to sock one at a time, each once the one before has
// drained; of a state or a check-in only the newest unwritten one is kept,
// in its place in the order.
export class Writer {
  constructor(sock) {
    this.sock = sock
    this.queue = []
    this.waiting = false
    sock.on("drain", () => {
      this.waiting = false
      this.flush()
    })
  }

  send(header, payload) {
    if (this.sock.destroyed) return
    const item = { t: header.t, bytes: record(header, payload) }
    const kept = SLOTS.has(header.t) ? this.queue.findIndex((q) => q.t === header.t) : -1
    if (kept >= 0) this.queue[kept] = item
    else this.queue.push(item)
    this.flush()
  }

  flush() {
    while (!this.waiting && this.queue.length > 0 && !this.sock.destroyed) {
      if (!this.sock.write(this.queue.shift().bytes)) this.waiting = true
    }
  }
}
