/**
 * The identifiers the engine mints, minted the same way.
 *
 * A seat's agent id is DERIVED, a UUIDv5 over the company's name and the
 * seat's handle (`org.DeriveAgentID`), so a budget row, a spend row and a
 * roster row name a seat by the id the engine would give it. Everything the
 * demo engine creates at runtime (a comment, a notice, a revision) takes a
 * fresh UUIDv7, which is what the engine writes for a record of its own.
 */

/** The engine's frozen namespace for agent ids (`internal/org/identity.go`). */
const AGENT_NAMESPACE = "c1ea9c6e-3f5d-4c9c-9e9f-1d2c3b4a5e6f";

/** SHA-1 of `bytes`, which UUIDv5 is defined over. Synchronous, unlike `crypto.subtle`. */
function sha1(bytes: Uint8Array): Uint8Array {
  const words = new Uint32Array(80);
  const length = bytes.length;
  const padded = new Uint8Array((((length + 8) >> 6) + 1) * 64);
  padded.set(bytes);
  padded[length] = 0x80;
  const view = new DataView(padded.buffer);
  view.setUint32(padded.length - 4, length * 8);
  let [h0, h1, h2, h3, h4] = [0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476, 0xc3d2e1f0];
  const rotl = (x: number, n: number) => (x << n) | (x >>> (32 - n));
  for (let block = 0; block < padded.length; block += 64) {
    for (let i = 0; i < 16; i++) words[i] = view.getUint32(block + i * 4);
    for (let i = 16; i < 80; i++) {
      words[i] = rotl(words[i - 3]! ^ words[i - 8]! ^ words[i - 14]! ^ words[i - 16]!, 1);
    }
    let [a, b, c, d, e] = [h0, h1, h2, h3, h4];
    for (let i = 0; i < 80; i++) {
      const [f, k] =
        i < 20
          ? [(b & c) | (~b & d), 0x5a827999]
          : i < 40
            ? [b ^ c ^ d, 0x6ed9eba1]
            : i < 60
              ? [(b & c) | (b & d) | (c & d), 0x8f1bbcdc]
              : [b ^ c ^ d, 0xca62c1d6];
      const t = (rotl(a, 5) + f + e + k + words[i]!) >>> 0;
      [e, d, c, b, a] = [d, c, rotl(b, 30) >>> 0, a, t];
    }
    h0 = (h0 + a) >>> 0;
    h1 = (h1 + b) >>> 0;
    h2 = (h2 + c) >>> 0;
    h3 = (h3 + d) >>> 0;
    h4 = (h4 + e) >>> 0;
  }
  const out = new Uint8Array(20);
  const outView = new DataView(out.buffer);
  [h0, h1, h2, h3, h4].forEach((h, i) => outView.setUint32(i * 4, h));
  return out;
}

const hex = (bytes: Uint8Array) =>
  Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");

function format(bytes: Uint8Array): string {
  const h = hex(bytes);
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20, 32)}`;
}

/** A name-based UUIDv5 (RFC 9562) over `name` in `namespace`. */
export function uuidV5(namespace: string, name: string): string {
  const ns = Uint8Array.from(namespace.replace(/-/g, "").match(/../g) ?? [], (b) =>
    parseInt(b, 16),
  );
  const input = new Uint8Array([...ns, ...new TextEncoder().encode(name)]);
  const bytes = sha1(input).slice(0, 16);
  bytes[6] = (bytes[6]! & 0x0f) | 0x50;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  return format(bytes);
}

/** The agent id the engine derives for a seat (`org.DeriveAgentID`). */
export function agentIdFor(orgName: string, handle: string): string {
  return orgName && handle ? uuidV5(AGENT_NAMESPACE, `${orgName}:${handle}`) : "";
}

/** A fresh UUIDv7, stamped with `at` (milliseconds). */
export function uuidV7(at: number = Date.now()): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  let ms = at;
  for (let i = 5; i >= 0; i--) {
    bytes[i] = ms % 256;
    ms = Math.floor(ms / 256);
  }
  bytes[6] = (bytes[6]! & 0x0f) | 0x70;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  return format(bytes);
}

/**
 * A stable UUID-shaped id for a fixture record, so a fixture's ids are the
 * same on every load and a deep link copied from one session opens the same
 * record in the next.
 */
export function fixtureId(name: string): string {
  return uuidV5(AGENT_NAMESPACE, `fixture:${name}`);
}
