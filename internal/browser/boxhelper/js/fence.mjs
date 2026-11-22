// fence.mjs: the engine's fence for words somebody else wrote, as the box
// draws it round a page's text before the model reads it: a port of
// internal/provenance's Fence, Neutralise and NewBoundary, under the one label
// a page in a seat's own browser carries. The box cannot run the engine's Go,
// so the port is held to it byte for byte by boxhelper's suite.
// internal/browser/boxhelper's package doc carries the argument.

import { randomBytes } from "node:crypto"

// ALPHABET is base32's, the alphabet a boundary is drawn from: 32 symbols, so
// a random byte masked to its low five bits picks each one uniformly.
const ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// BOUNDARY_LEN is a boundary's length, provenance's boundaryLen.
const BOUNDARY_LEN = 16

// TOKEN_REMOVED stands in for the boundary wherever the text carried it.
export const TOKEN_REMOVED = "[fence token removed]"

// MARKER_ESCAPE replaces the [ of anything in the text shaped like a fence
// marker, and MARKER_WORD is the word every marker opens with.
const MARKER_ESCAPE = "[escaped "
const MARKER_WORD = "untrusted"

// LABEL is the opening line's label for a page in a seat's own browser:
// provenance's source browser_page, read at external trust, its writer
// named by nobody.
const LABEL = "source=browser_page trust=external authors=unknown"

// newBoundary is a fresh boundary: sixteen symbols of ALPHABET, each from one
// byte of the system's cryptographic source.
export function newBoundary() {
  let b = ""
  for (const byte of randomBytes(BOUNDARY_LEN)) b += ALPHABET[byte & 31]
  return b
}

// validBoundary is whether b is a boundary newBoundary could have drawn.
export function validBoundary(b) {
  return typeof b === "string" && /^[A-Z2-7]{16}$/.test(b)
}

// fence is text inside a fence of boundary b: the opening line, the text
// neutralised, the fence's own newline unless the text is empty, and the
// closing line. An invalid boundary is replaced by a fresh one, never used.
export function fence(b, text) {
  if (!validBoundary(b)) b = newBoundary()
  const body = neutralise(b, text)
  return `[${MARKER_WORD}:${b} ${LABEL}]\n${body === "" ? "" : `${body}\n`}[/${MARKER_WORD}:${b}]`
}

// neutralise applies the fence's two escapes: the boundary, in any ASCII
// case, becomes TOKEN_REMOVED, and the [ of anything that opens a marker
// becomes MARKER_ESCAPE. Every comparison is with ASCII, and no code unit of a
// character past ASCII equals one, so the UTF-8 of the result is the bytes Go
// writes for the same text. An invalid boundary removes no token.
export function neutralise(b, text) {
  const token = validBoundary(b)
  const parts = []
  let last = 0
  for (let i = 0; i < text.length; ) {
    if (token && foldPrefix(text, i, b)) {
      parts.push(text.slice(last, i), TOKEN_REMOVED)
      i += b.length
      last = i
    } else if (text.charCodeAt(i) === 0x5b && opensMarker(text, i + 1)) {
      parts.push(text.slice(last, i), MARKER_ESCAPE)
      i += 1
      last = i
    } else {
      i += 1
    }
  }
  if (parts.length === 0) return text
  parts.push(text.slice(last))
  return parts.join("")
}

// opensMarker is whether s from i, what follows a [, reads as the inside of a
// fence marker: optional whitespace, an optional slash, optional whitespace,
// then the marker word in any case.
function opensMarker(s, i) {
  let j = skipSpace(s, i)
  if (j < s.length && s.charCodeAt(j) === 0x2f) j = skipSpace(s, j + 1)
  return foldPrefix(s, j, MARKER_WORD)
}

// skipSpace is the first index from i that holds none of Go's six ASCII
// whitespace bytes: space, \t, \n, \r, \f and \v.
function skipSpace(s, i) {
  while (i < s.length) {
    const c = s.charCodeAt(i)
    if (c !== 0x20 && (c < 0x09 || c > 0x0d)) return i
    i += 1
  }
  return i
}

// foldPrefix is whether s holds prefix at i, ignoring ASCII case alone: a
// locale's or Unicode's case folding would match a character past ASCII to an
// ASCII letter (the Kelvin sign to k), which Go's comparison never does.
function foldPrefix(s, i, prefix) {
  if (s.length - i < prefix.length) return false
  for (let k = 0; k < prefix.length; k++) {
    if (lower(s.charCodeAt(i + k)) !== lower(prefix.charCodeAt(k))) return false
  }
  return true
}

function lower(c) {
  return c >= 0x41 && c <= 0x5a ? c + 0x20 : c
}
