/**
 * The dashboard's SHA-256 is the standard's, byte for byte — the digest an
 * operation id's escaped name is cut from (`opNamePart` in `./gate.ts`), which
 * must match the engine's on every input or a gesture's id is named another
 * way than the node names it.
 */

import { createHash } from "node:crypto";
import { expect, test } from "vitest";
import { sha256 } from "./sha256.ts";

const hex = (bytes: Uint8Array) =>
  Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
const utf8 = (text: string) => new TextEncoder().encode(text);

// THE STANDARD'S OWN VECTORS (FIPS 180-4's examples, and the empty message).
test("the digest is the standard's on its own examples", () => {
  expect(hex(sha256(utf8("")))).toBe(
    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  );
  expect(hex(sha256(utf8("abc")))).toBe(
    "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
  );
  expect(hex(sha256(utf8("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq")))).toBe(
    "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
  );
});

// EVERY PADDING BOUNDARY, against Node's own implementation: the length word
// lands in the message's last block or forces another at 56 bytes into a
// block, and an off-by-one there is wrong on exactly the lengths it touches.
test("the digest is Node's over every length up to three blocks and a half", () => {
  const bytes = Uint8Array.from({ length: 224 }, (_, i) => (i * 131 + 7) & 0xff);
  for (let n = 0; n <= bytes.length; n++) {
    const message = bytes.subarray(0, n);
    expect(hex(sha256(message)), `length ${n}`).toBe(
      createHash("sha256").update(message).digest("hex"),
    );
  }
});
