/**
 * Node keys: how the builder names a seat or a unit across edits, replays,
 * rebases and a reload of the tab.
 *
 * A KEY IS AN IDENTITY, NOT A POSITION AND NOT A NAME. An operation says which
 * node it acts on, and it has to keep meaning the same node when the draft is
 * rebuilt from its base (every undo), when the base is replaced by a newer
 * revision (a rebase), and when the log is read back from storage after a
 * reload. So:
 *
 * - A node that exists in the base is keyed by its identity in the document:
 *   `seat:<handle>` and `unit:<key>`. Those are what its memory, mailbox,
 *   schedules and masked credentials attach to, and what every reference to it
 *   names, and neither ever changes (see `identity.ts`): the engine writes
 *   both into every revision it stores, so two revisions that hold "the same
 *   seat" hold it under the same key, wherever it sits and whatever it is
 *   called. A different seat later given a removed seat's handle is the same
 *   identity to the engine as well, which is why a new node is never offered
 *   one the saved company holds.
 * - A node an operation created is keyed by a value MINTED IN THE EVENT
 *   HANDLER and written into that operation. Never in a reducer (React may
 *   run a reducer twice) and never during replay (a replay must rebuild the
 *   same keys every time, or the operations after it address nothing). Its
 *   handle or key is in its data from the moment it is added; once a save
 *   has stored it, the base read back keys it by that identity.
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
const MINTED = "new:";

/** An existing seat, by its handle. */
export function seatKey(handle: string): NodeKey {
  return SEAT + handle;
}

/** An existing unit, by its key. */
export function unitKey(id: string): NodeKey {
  return UNIT + id;
}

/** The handle an existing seat's key carries, or `undefined` for any other key. */
export function handleOfKey(key: NodeKey): string | undefined {
  return key.startsWith(SEAT) ? key.slice(SEAT.length) : undefined;
}

/** The unit key an existing unit's node key carries, or `undefined` for any other key. */
export function unitIdOfKey(key: NodeKey): string | undefined {
  return key.startsWith(UNIT) ? key.slice(UNIT.length) : undefined;
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
  for (const prefix of [SEAT, UNIT]) {
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
