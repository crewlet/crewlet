/**
 * The dashboard's HMAC-SHA256 is RFC 2104's, byte for byte: a viewed file is
 * found by the digest the engine keeps of it, so a digest that differs on any
 * key or message reads a viewed file as unviewed.
 */

import { createHmac } from "node:crypto";
import { expect, test } from "vitest";
import { hmacSha256 } from "./hmac.ts";

const hex = (bytes: Uint8Array) =>
  Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
const fromHex = (text: string) => Uint8Array.from(text.match(/../g)!, (b) => parseInt(b, 16));
const utf8 = (text: string) => new TextEncoder().encode(text);
const filled = (n: number, byte: number) => new Uint8Array(n).fill(byte);

// RFC 4231'S OWN CASES 1, 2, 3, 4, 6 AND 7 (5 truncates its output); 6 and 7
// hold a key longer than the block, which is hashed first.
// Mutation: use a long key unhashed, and cases 6 and 7 fail.
test("the digest is RFC 4231's on its test cases", () => {
  const cases: [Uint8Array, Uint8Array, string][] = [
    [
      filled(20, 0x0b),
      utf8("Hi There"),
      "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7",
    ],
    [
      utf8("Jefe"),
      utf8("what do ya want for nothing?"),
      "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
    ],
    [
      filled(20, 0xaa),
      filled(50, 0xdd),
      "773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe",
    ],
    [
      fromHex("0102030405060708090a0b0c0d0e0f10111213141516171819"),
      filled(50, 0xcd),
      "82558a389a443c0ea4cc819899f2083a85f0faa3e578f8077a2e3ff46729665b",
    ],
    [
      filled(131, 0xaa),
      utf8("Test Using Larger Than Block-Size Key - Hash Key First"),
      "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54",
    ],
    [
      filled(131, 0xaa),
      utf8(
        "This is a test using a larger than block-size key and a larger than block-size data. " +
          "The key needs to be hashed before being used by the HMAC algorithm.",
      ),
      "9b09ffa71b942fcb27635fbcd5b0e944bfdc63644f0713938a7f51535c3a35e2",
    ],
  ];
  for (const [key, data, want] of cases) expect(hex(hmacSha256(key, data))).toBe(want);
});

// EVERY KEY AND MESSAGE LENGTH ACROSS THE BLOCK, against Node's own: a key of
// 63, 64 or 65 bytes is padded, kept or hashed, and a message crosses the
// inner hash's padding boundaries. Mutation: hash the key without padding it.
test("the digest is Node's for keys and messages across the block boundaries", () => {
  const bytes = Uint8Array.from({ length: 200 }, (_, i) => (i * 131 + 7) & 0xff);
  for (const k of [0, 1, 32, 63, 64, 65, 128, 200]) {
    for (let n = 0; n <= 140; n++) {
      const key = bytes.subarray(0, k);
      const data = bytes.subarray(200 - n);
      expect(hex(hmacSha256(key, data)), `key ${k}, message ${n}`).toBe(
        createHmac("sha256", key).update(data).digest("hex"),
      );
    }
  }
});
