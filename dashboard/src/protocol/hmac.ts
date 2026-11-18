/**
 * HMAC-SHA256 (RFC 2104) over this module's sibling SHA-256: synchronous, for
 * the keyed digests a person's viewed files are kept by (`gitlet.ViewedKey`).
 * Its own module, so only the screens that digest import it.
 */

import { sha256 } from "./sha256.ts";

/** SHA-256's block, in bytes: a key past it is hashed first, a shorter one padded with zeros. */
const BLOCK = 64;

/** HMAC-SHA256 of `data` under `key`, as 32 bytes. */
export function hmacSha256(key: Uint8Array, data: Uint8Array): Uint8Array {
  const padded = new Uint8Array(BLOCK);
  padded.set(key.length > BLOCK ? sha256(key) : key);
  const inner = new Uint8Array(BLOCK + data.length);
  const outer = new Uint8Array(BLOCK + 32);
  for (let i = 0; i < BLOCK; i++) {
    inner[i] = padded[i]! ^ 0x36;
    outer[i] = padded[i]! ^ 0x5c;
  }
  inner.set(data, BLOCK);
  outer.set(sha256(inner), BLOCK);
  return sha256(outer);
}
