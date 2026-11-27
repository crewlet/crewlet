// jpeg.mjs: a frame's width and height as the hub reads them (internal/
// browser's CheckJPEG): a start of image first and an end of image last, then
// the markers walked as image/jpeg's DecodeConfig walks them, within the first
// headerBytes, to a baseline, extended or progressive frame header. Anything
// the hub would refuse as no JPEG is undefined, so the box drops it rather
// than post it. internal/browser/boxhelper/live's package doc carries the
// argument.

const SOI = 0xd8
const EOI = 0xd9
const SOF0 = 0xc0
const SOF1 = 0xc1
const SOF2 = 0xc2
const DHT = 0xc4
const DQT = 0xdb
const SOS = 0xda
const DRI = 0xdd
const APP0 = 0xe0
const APP14 = 0xee
const APP15 = 0xef
const COM = 0xfe
const RST0 = 0xd0
const RST7 = 0xd7

// MAX_TQ is the highest quantization table a frame header may name.
const MAX_TQ = 3

// Short is a walk that ran out of bytes: the header was not within reach.
class Short extends Error {}

// jpegSize is frame's {width, height}, or undefined where the hub would
// refuse it as no JPEG.
export function jpegSize(frame, headerBytes) {
  const n = frame.length
  if (n < 4 || frame[0] !== 0xff || frame[1] !== SOI || frame[n - 2] !== 0xff || frame[n - 1] !== EOI) return undefined
  try {
    return walk(frame.subarray(0, Math.min(n, headerBytes)))
  } catch (err) {
    if (err instanceof Short) return undefined
    throw err
  }
}

// walk is DecodeConfig's walk of b past its start of image: its size, or
// undefined for what it refuses.
function walk(b) {
  let at = 2
  const byte = () => {
    if (at >= b.length) throw new Short()
    return b[at++]
  }
  const skip = (len) => {
    if (at + len > b.length) throw new Short()
    at += len
  }
  const bytes = (len) => {
    if (at + len > b.length) throw new Short()
    at += len
    return b.subarray(at - len, at)
  }
  let jfif = false
  let sof
  for (;;) {
    let first = byte()
    let marker = byte()
    while (first !== 0xff) {
      first = marker
      marker = byte()
    }
    if (marker === 0) continue
    while (marker === 0xff) marker = byte()
    if (marker === EOI) return undefined
    if (marker >= RST0 && marker <= RST7) continue
    const length = (byte() << 8) + byte() - 2
    if (length < 0) return undefined
    switch (marker) {
      case SOF0:
      case SOF1:
      case SOF2: {
        if (sof !== undefined) return undefined
        sof = frameHeader(bytes(sofLength(length)), length)
        if (sof === undefined) return undefined
        if (jfif) return sof
        break
      }
      case DHT:
      case DQT:
      case DRI:
        skip(length)
        break
      case SOS:
        return sof
      case APP0:
        if (length < 5) {
          skip(length)
          break
        }
        jfif = String.fromCharCode(...bytes(5)) === "JFIF\0"
        skip(length - 5)
        break
      case APP14:
        skip(length)
        break
      default:
        if ((marker >= APP0 && marker <= APP15) || marker === COM) skip(length)
        else return undefined
    }
  }
}

// sofLength is the bytes a frame header of length holds before it is
// judged: none past its length, which a header of an unread size refuses.
function sofLength(length) {
  return length === 9 || length === 15 || length === 18 ? length : 0
}

// frameHeader is the size a frame header of length bytes, h, gives, or
// undefined for one DecodeConfig refuses: 8-bit, of one, three or four
// components, each named once, a table of at most MAX_TQ, and sampling it
// supports.
function frameHeader(h, length) {
  const comps = { 9: 1, 15: 3, 18: 4 }[length]
  if (comps === undefined || h[0] !== 8 || h[5] !== comps) return undefined
  const seen = []
  let maxH = 0
  let maxV = 0
  const factors = []
  for (let i = 0; i < comps; i++) {
    const id = h[6 + 3 * i]
    if (seen.includes(id)) return undefined
    seen.push(id)
    if (h[8 + 3 * i] > MAX_TQ) return undefined
    const hv = h[7 + 3 * i]
    let fh = hv >> 4
    let fv = hv & 0x0f
    if (fh < 1 || fh > 4 || fv < 1 || fv > 4 || fh === 3 || fv === 3) return undefined
    if (comps === 1) {
      fh = 1
      fv = 1
    } else if (comps === 4) {
      if (i === 0 && hv !== 0x11 && hv !== 0x22) return undefined
      if ((i === 1 || i === 2) && hv !== 0x11) return undefined
      if (i === 3 && (factors[0][0] !== fh || factors[0][1] !== fv)) return undefined
    }
    maxH = Math.max(maxH, fh)
    maxV = Math.max(maxV, fv)
    factors.push([fh, fv])
  }
  if (comps === 3 && factors.some(([fh, fv]) => maxH % fh !== 0 || maxV % fv !== 0)) return undefined
  return { width: (h[3] << 8) + h[4], height: (h[1] << 8) + h[2] }
}
