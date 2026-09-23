/**
 * The node gate — evicting a node and readmitting one — as the wire knows it:
 * the remedy vocabulary a gate answer speaks, the operation id a gesture is
 * finished under, and how long a gesture may take to answer.
 *
 * Every constant here is a COPY of something the engine owns, because this is
 * a separate build that cannot import a Go identifier — and each copy is held
 * to the engine's by a gate on the engine side (`internal/api`'s
 * `gate_client_test.go`), in both directions, so it cannot drift silently. The
 * minter is held to a vector file both sides read.
 */

/**
 * What an operator does about a log a gesture did not finish, or a gesture
 * refused before anything was written — `statelog.GateActions`.
 *
 * AN ACTION, NEVER A FLAG. The engine used to send one sentence in the command
 * line's words ("evict it with -force", "without -op-id"), and this dashboard
 * rendered it beside a dialog that has none of those flags. The engine now
 * says WHAT to do and each surface says HOW: here, as its own controls.
 */
export const GATE_ACTIONS = [
  "retry_same_op",
  "new_gesture",
  "force",
  "other_node",
  "reanchor",
  "set_capacity",
  "restore",
  "wait",
] as const;

/** One of [GATE_ACTIONS]. A gate answer's `actions` is typed `string[]`, so an
 *  action a newer node sends is a value to show rather than a type error. */
export type GateAction = (typeof GATE_ACTIONS)[number];

/**
 * The actions after which the gesture is finished under ITS OWN operation id —
 * `statelog.GateActionsKeepingOperation`.
 *
 * A log refused `log_full` cannot be finished by sending the gesture again at
 * once, and a surface that let go of the id then left the operator nothing
 * but a FRESH gesture once the ceiling was raised — which writes every log
 * that already held the first record again, re-dating each eviction and
 * restarting its fence window. So the id is kept for all of these, and only
 * `new_gesture` lets it go.
 */
export const GATE_ACTIONS_KEEPING_OPERATION = [
  "retry_same_op",
  "other_node",
  "reanchor",
  "set_capacity",
] as const;

/** Whether, once the operator has done `action`, the gesture is finished under
 *  its own operation id. */
export function keepsOperation(action: string): boolean {
  return (GATE_ACTIONS_KEEPING_OPERATION as readonly string[]).includes(action);
}

/**
 * How long one gesture's request may take before the dialog gives up on it.
 *
 * SEVENTY-FIVE SECONDS, the command line's own `gateRequestTimeout` and for its
 * reason: the engine bounds a gesture at a minute from its first record to its
 * last answer (`engine.GateBudget`), and the judgement before it and the round
 * trip around it are a coordination read and a request. Waiting past the
 * node's own bound is what makes its answer — every log's outcome — reach the
 * operator rather than a client timeout that knows none of it. The default
 * thirty seconds gave up on a gesture the node went on to finish.
 */
export const GATE_REQUEST_TIMEOUT_MS = 75_000;

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
