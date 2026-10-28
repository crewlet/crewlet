/**
 * Whether two values decoded off the wire say the same thing: the same JSON,
 * compared structurally rather than by identity.
 *
 * WHY IT EXISTS: every live push decodes into new objects, so a row the engine
 * sent again unchanged (a seat's state re-sent beside another seat's change,
 * the same refusal reported once a second) was a new row to every screen
 * keyed on identity, which drew it again. The store keeps the row it holds
 * when the push says nothing new, and a screen deriving rows from it keeps
 * the rows it drew, so nothing a person sees moves unless something did.
 *
 * JSON's own values only: objects, arrays, strings, numbers, booleans and
 * null. A key whose value is `undefined` reads as a missing key, as
 * `JSON.stringify` would write it.
 */
export function sameValue(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (a === null || b === null || typeof a !== "object" || typeof b !== "object") return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((v, i) => sameValue(v, b[i]));
  }
  const left = a as Record<string, unknown>;
  const right = b as Record<string, unknown>;
  const keys = (o: Record<string, unknown>) => Object.keys(o).filter((k) => o[k] !== undefined);
  const lk = keys(left);
  if (lk.length !== keys(right).length) return false;
  return lk.every((k) => Object.hasOwn(right, k) && sameValue(left[k], right[k]));
}
