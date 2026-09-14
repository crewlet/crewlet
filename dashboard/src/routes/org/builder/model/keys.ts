/**
 * Node keys: how the builder names a seat or a unit across edits, replays,
 * rebases and a reload of the tab.
 *
 * A KEY IS AN ORIGIN, NOT A POSITION AND NOT A NAME. An operation says which
 * node it acts on, and it has to keep meaning the same node when the draft is
 * rebuilt from its base (every undo), when the base is replaced by a newer
 * revision (a rebase), and when the log is read back from storage after a
 * reload. So:
 *
 * - A node that exists in the base is keyed by the identity the ENGINE gives
 *   it: `seat:<handle>` with the handle the engine derived, `unit:<name>`.
 *   Those are the identities its memory, mailbox, schedules, onboarding pages
 *   and masked credentials attach to, so two revisions that hold "the same
 *   seat" in the engine's sense hold it under the same key, wherever it sits.
 *   A name is never the key of a seat: a rename keeps the handle (the builder
 *   pins it), and a different seat created under an old name gets a different
 *   handle and therefore a different key, so an operation recorded against
 *   the original never lands on it.
 * - The authored path is the key only where the engine's identity cannot
 *   name one node: a base stored before unit names had to be unique that holds
 *   two units of one name, a unit with no name, and a seat whose handle the
 *   engine has not reported. Such a key is honest for that one base and names
 *   nothing after a rebase, which then reports the operation's target as gone
 *   rather than guessing.
 * - A node an operation created is keyed by a value MINTED IN THE EVENT
 *   HANDLER and written into that operation. Never in a reducer (React may
 *   run a reducer twice) and never during replay (a replay must rebuild the
 *   same keys every time, or the operations after it address nothing).
 *
 * Keys live only in the builder. They are never written into the
 * configuration document: the engine has no field for them, and a document
 * carrying one would be refused as an unknown field.
 */

/** A builder node's stable name. */
export type NodeKey = string;

/** The company itself: the charter's node, and the parent of root seats and units. */
export const COMPANY_KEY: NodeKey = "company";

const SEAT = "seat:";
const UNIT = "unit:";
const SEAT_AT = "seat@";
const UNIT_AT = "unit@";
const MINTED = "new:";

/** An existing seat, by the handle the engine derived or the document declares. */
export function seatKey(handle: string): NodeKey {
  return SEAT + handle;
}

/** An existing unit, by its name. */
export function unitKey(name: string): NodeKey {
  return UNIT + name;
}

/** An existing seat the engine's identity cannot name, by its authored path in the base. */
export function seatPathKey(path: string): NodeKey {
  return SEAT_AT + path;
}

/** An existing unit the engine's identity cannot name, by its authored path in the base. */
export function unitPathKey(path: string): NodeKey {
  return UNIT_AT + path;
}

/** The handle an existing seat's key carries, or `undefined` for any other key. */
export function handleOfKey(key: NodeKey): string | undefined {
  return key.startsWith(SEAT) ? key.slice(SEAT.length) : undefined;
}

/** Whether a key was minted for a node an operation created. */
export function isMintedKey(value: unknown): value is NodeKey {
  return typeof value === "string" && MINTED_PATTERN.test(value);
}

/**
 * What a minted key's random part may contain. Bounded so a persisted log
 * cannot smuggle an arbitrarily long string through a field that is only an
 * identifier.
 */
const MINTED_PATTERN = /^new:[A-Za-z0-9_-]{1,64}$/;

/** Whether a value is shaped like any key this module produces. */
export function isNodeKey(value: unknown): value is NodeKey {
  if (typeof value !== "string") return false;
  if (value === COMPANY_KEY) return true;
  if (MINTED_PATTERN.test(value)) return true;
  for (const prefix of [SEAT, UNIT, SEAT_AT, UNIT_AT]) {
    if (value.startsWith(prefix) && value.length > prefix.length) return true;
  }
  return false;
}

/**
 * Where minted keys come from. Injected so a test can mint predictable keys
 * and the UI can use the browser's random source (`runtime.randomKeys`, over
 * `crypto.getRandomValues`, since `randomUUID` exists only in a secure
 * context), without this directory touching a global.
 */
export interface KeySource {
  /** A fresh random token: letters, digits, `_` and `-`, at most 64 characters. */
  next(): string;
}

/**
 * A key for a node the operation being built will create. Call it in the event
 * handler and put the result in the operation.
 */
export function mintKey(source: KeySource): NodeKey {
  const key = MINTED + source.next();
  if (!isMintedKey(key)) {
    throw new RangeError(
      `mintKey: the key source produced an unusable token: ${JSON.stringify(key)}`,
    );
  }
  return key;
}
