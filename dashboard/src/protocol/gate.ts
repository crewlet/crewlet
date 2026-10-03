/**
 * The node gate — evicting a node and readmitting one — as the wire knows it:
 * the remedy vocabulary a gate answer speaks, the operation id a gesture is
 * finished under, and what a remedy means for that id.
 *
 * The engine's own values — the actions, which of them keep the id, and how
 * long a gesture may take — are declared in `../contract/gate.ts`, the one
 * home of what an engine gate holds (`internal/api`'s `gate_client_test.go`);
 * what is DONE with them is here. RELATIVE, because this directory is also
 * built alone as `protocol.js`, where the `~` alias does not exist. The
 * minter is held to a vector file both sides read.
 */

import { GATE_ACTIONS_KEEPING_OPERATION, type GATE_ACTIONS } from "../contract/gate.ts";

/** One of [GATE_ACTIONS]. A gate answer's `actions` is typed `string[]`, so an
 *  action a newer node sends is a value to show rather than a type error. */
export type GateAction = (typeof GATE_ACTIONS)[number];

/** Whether, once the operator has done `action`, the gesture is finished under
 *  its own operation id ([GATE_ACTIONS_KEEPING_OPERATION]). */
export function keepsOperation(action: string): boolean {
  return (GATE_ACTIONS_KEEPING_OPERATION as readonly string[]).includes(action);
}

/**
 * An operation id in the engine's grammar (`statelog.NewOpID`), from its parts.
 *
 * The UUIDv7 bit layout — the leading 48 bits the Unix millisecond, big-endian,
 * then the ten tail bytes with the version nibble 7 written into byte 6 and the
 * RFC 9562 variant into byte 8 — then `.` and the name, or the bare uuid for an
 * empty one. The millisecond is clamped to the 48 bits the layout holds, as
 * the engine's is.
 *
 * PURE, so the vector file the engine's own test reads
 * (`internal/statelog/testdata/opid_vectors.json`) pins it byte for byte: a
 * drift here is a failing test rather than a gesture refused `op_id_invalid`,
 * or accepted at another instant than the one it was minted at.
 */
export function layoutOpID(unixMs: number, tail: Uint8Array, name: string): string {
  if (tail.length !== 10) throw new RangeError("an operation id's tail is ten bytes");
  const ms = Math.min(Math.max(Math.trunc(unixMs), 0), 2 ** 48 - 1);
  const bytes = new Uint8Array(16);
  // 48 bits do not fit a bitwise operation, which is 32-bit; divide instead.
  let rest = ms;
  for (let i = 5; i >= 0; i--) {
    bytes[i] = rest % 256;
    rest = Math.floor(rest / 256);
  }
  bytes.set(tail, 6);
  bytes[6] = 0x70 | (bytes[6]! & 0x0f);
  bytes[8] = 0x80 | (bytes[8]! & 0x3f);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  const uuid = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(
    16,
    20,
  )}-${hex.slice(20)}`;
  return name ? `${uuid}.${name}` : uuid;
}

/**
 * A FRESH gesture's operation id, minted in the browser BEFORE the first
 * request — exactly as `crewlet retention evict` mints one on the workstation.
 *
 * # Why the dialog mints it rather than letting the route
 *
 * The node finishes a gesture whatever happens to the connection, so a request
 * that timed out or dropped has very likely done its work — and an id the
 * route minted comes back only in the answer that never arrived. The only way
 * on was then a second gesture under a fresh id, which writes every log the
 * first one reached again. Minted here, the id is in hand before anything is
 * sent, and "finish this gesture" is always the same request again.
 *
 * # Whose clock
 *
 * The browser's. The id carries its mint instant, which the engine reads to
 * decide whether its operation ledger can vouch for a retry, so a browser clock
 * far AHEAD of the fleet's could let a retry be re-decided across a ledger
 * loss — the same assumption, with the same margin, the engine already makes
 * of every node's clock and the command line of the workstation's.
 *
 * `crypto.getRandomValues` rather than `randomUUID`, because the dashboard is
 * served over plain HTTP wherever the engine is, and `randomUUID` exists only
 * in a secure context.
 */
export function newGateOpID(
  verb: "evict" | "readmit",
  node: string,
  now: number = Date.now(),
  random: (bytes: Uint8Array<ArrayBuffer>) => Uint8Array = (bytes) => crypto.getRandomValues(bytes),
): string {
  return layoutOpID(now, random(new Uint8Array(10)), `${verb}-${node}`);
}
