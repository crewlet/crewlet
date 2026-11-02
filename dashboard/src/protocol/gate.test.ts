/**
 * The operation id the dashboard mints for a gate gesture is the engine's
 * grammar, byte for byte.
 *
 * The engine's own test (`internal/statelog/opid_vectors_internal_test.go`)
 * holds its layout to the same vector file this reads, and holds every vector
 * to the rule the route applies to an id a caller brings (`CheckCallerOpID`)
 * and to the instant the ledger reads off it. So a drift here is a failing
 * test, rather than a gesture refused `op_id_invalid` — or accepted at another
 * instant than the one it was minted at.
 */

import { expect, test } from "vitest";
import { engineFile } from "~/test/engineFiles.ts";
import { layoutOpID, newGateOpID, opNamePart } from "./gate.ts";

interface Vector {
  unix_ms: number;
  tail: string;
  name: string;
  id: string;
}

const vectors = engineFile<Vector[]>("internal/statelog/testdata/opid_vectors.json");

interface NameVector {
  value: string;
  part: string;
}

const nameVectors = engineFile<NameVector[]>("internal/statelog/testdata/opname_vectors.json");

const hexBytes = (hex: string) => Uint8Array.from(hex.match(/../g)!, (b) => parseInt(b, 16));

test("the minter's layout matches every shared vector", () => {
  expect(vectors.length).toBeGreaterThan(0);
  for (const v of vectors) {
    expect(layoutOpID(v.unix_ms, hexBytes(v.tail), v.name)).toBe(v.id);
  }
});

// A VALUE GOES INTO A NAME AS THE ENGINE WRITES IT: as itself where it is
// one segment of the grammar, escaped where it is not — a node id that holds
// the dot which begins a step among them. The vector file is the engine's own
// (`statelog.OpNamePart`), so a drift on either side fails one suite or the
// other.
test("a value is written into a name as every shared vector says", () => {
  expect(nameVectors.some((v) => v.part === v.value)).toBe(true);
  expect(nameVectors.some((v) => v.part !== v.value)).toBe(true);
  for (const v of nameVectors) {
    expect(opNamePart(v.value)).toBe(v.part);
  }
});

// A FRESH GESTURE'S ID IS NAMED FOR WHAT IT IS, stamped with the browser's
// clock, and random in its tail — two gestures on one node never share one.
// And the node is ONE segment of its name: a node id may hold dots, and in the
// grammar a dot begins a step, so a dotted node is written escaped while the
// sign beside it stays readable.
test("a fresh gesture's id carries its sign, its node and its instant", () => {
  const now = Date.UTC(2026, 8, 23, 12, 0, 0, 123);
  const tail = hexBytes("00112233445566778899");
  const id = newGateOpID("readmit", "node-a.b_c-9", now, () => tail);
  expect(id).toBe(layoutOpID(now, tail, `readmit-${opNamePart("node-a.b_c-9")}`));
  expect(id).toMatch(
    /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.readmit-~[0-9a-f]{32}$/,
  );
  // THE INSTANT IS THE LEADING 48 BITS, which is what the engine reads.
  expect(parseInt(id.slice(0, 8) + id.slice(9, 13), 16)).toBe(now);

  expect(newGateOpID("evict", "node-4", now, () => tail)).toBe(
    layoutOpID(now, tail, "evict-node-4"),
  );
  expect(newGateOpID("evict", "node-4")).not.toBe(newGateOpID("evict", "node-4"));
});
