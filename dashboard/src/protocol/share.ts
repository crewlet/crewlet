/**
 * An answer that did not change keeps the objects it was drawn from.
 *
 * # Why identity is the thing worth keeping
 *
 * Every list in this product is drawn by the one grid
 * (`app/frame/DataGrid.tsx`), whose rows are memoised on the OBJECT each row
 * is drawn from — and never on its place: a row whose object is the one it was
 * drawn from last time is not drawn again, wherever it now sits. A poll
 * defeats that by construction — every answer is parsed afresh off the wire,
 * so a poll that brought back exactly what the screen already held handed the
 * grid two hundred new objects, and the grid drew two hundred rows to change
 * no pixel. Measured on the turns list and the audit under the development
 * build: every row of both, on every poll, whatever the answer said.
 *
 * So an answer is SHARED with the one it replaces before anything reads it:
 * every part of the new answer that is deep-equal to a part of the old one is
 * replaced by the old part. An unchanged answer comes back as the very object
 * the screen already holds, which renders nothing at all; an answer in which
 * one row moved comes back as a new list holding the old objects for every
 * row but that one, which draws that one row.
 *
 * # A row is matched by its content, not only by its place
 *
 * The obvious walk — compare each element with the one at the same index —
 * is what most implementations do, and it is wrong for the lists this product
 * polls most: a feed ordered newest-first. One new turn at the top moves every
 * other row down a place, so not one of them equals the element at its own
 * index, and every row is drawn again for an answer that changed by one. So
 * an element that does not equal the one at its own place is looked up among
 * EVERY element of the old list — bucketed by a short print of its leading
 * fields, then confirmed by the same deep comparison — and reused from
 * wherever it was.
 *
 * # Only data is walked
 *
 * An array and an object literal are looked inside; anything else — a `Map`,
 * a `Date`, a class instance, a function, and above all a React element, which
 * is an object literal whose `_owner` is a fiber that reaches the whole tree —
 * is kept only where it is the SAME value, never compared by its contents.
 * Answers off the socket are JSON and are all data; what a screen derives
 * from one may not be, and walking into a fiber to compare two of them would
 * be a walk of the application.
 *
 * Nothing here mutates either argument, and the result is always deep-equal to
 * `next`: sharing changes which objects an answer is made of, never what it
 * says.
 *
 * # Why it is in the protocol layer
 *
 * Every way an answer reaches a screen goes through it: a question
 * (`lib/useQuery.ts`), a REST read (`lib/restRead.ts`), the engine-health poll
 * (`lib/engineHealth.ts`) and every push the store replaces a slice with
 * (`./store.ts`). The store is here, and nothing here may import React — the
 * hook that shares a value a SCREEN derives is `lib/share.ts`.
 */

/**
 * `next`, with every part deep-equal to a part of `prev` replaced by that part.
 *
 * Returns `prev` itself when the two are deep-equal.
 */
export function share<T>(prev: unknown, next: T): T {
  return shareValue(prev, next) as T;
}

/** Whether the walk may look inside a value: an array or an object literal. */
function walkable(value: unknown): value is object {
  if (value === null || typeof value !== "object") return false;
  if (Array.isArray(value)) return true;
  const proto: unknown = Object.getPrototypeOf(value);
  if (proto !== Object.prototype && proto !== null) return false;
  // A REACT ELEMENT IS AN OBJECT LITERAL TOO, carrying the fiber that made it.
  return !("$$typeof" in value);
}

function shareValue(prev: unknown, next: unknown): unknown {
  if (Object.is(prev, next)) return prev;
  if (Array.isArray(prev) && Array.isArray(next)) return shareList(prev, next);
  if (walkable(prev) && walkable(next) && !Array.isArray(prev) && !Array.isArray(next)) {
    return shareRecord(prev as Record<string, unknown>, next as Record<string, unknown>);
  }
  return next;
}

function shareRecord(
  prev: Record<string, unknown>,
  next: Record<string, unknown>,
): Record<string, unknown> {
  const keys = Object.keys(next);
  // THE SAME KEYS, not merely the same values under next's keys: an object
  // that lost a key is a different answer even where every key it kept is
  // unchanged.
  let same = keys.length === Object.keys(prev).length;
  const out: Record<string, unknown> = {};
  for (const key of keys) {
    const had = Object.prototype.hasOwnProperty.call(prev, key);
    const value = had ? shareValue(prev[key], next[key]) : next[key];
    out[key] = value;
    if (!had || value !== prev[key]) same = false;
  }
  return same ? prev : out;
}

function shareList(prev: readonly unknown[], next: readonly unknown[]): readonly unknown[] {
  let same = prev.length === next.length;
  const out = new Array<unknown>(next.length);
  // BUILT ONLY ONCE SOMETHING HAS MOVED: an unchanged poll — the common case
  // by far — never prints anything.
  let elsewhere: Map<string, unknown[]> | null = null;
  for (let i = 0; i < next.length; i++) {
    const value = next[i];
    const kept = i < prev.length ? shareValue(prev[i], value) : value;
    out[i] = kept;
    if (i < prev.length && kept === prev[i]) continue;
    same = false;
    // NOT WHERE IT WAS: the same row somewhere else in the old list. See the
    // module doc — a feed with one new row at the top moves every other row.
    if (!walkable(value)) continue;
    elsewhere ??= buckets(prev);
    for (const candidate of elsewhere.get(print(value)) ?? []) {
      if (shareValue(candidate, value) === candidate) {
        out[i] = candidate;
        break;
      }
    }
  }
  return same ? prev : out;
}

/** Every walkable element of a list, by its [print]. */
function buckets(list: readonly unknown[]): Map<string, unknown[]> {
  const out = new Map<string, unknown[]>();
  for (const item of list) {
    if (!walkable(item)) continue;
    const key = print(item);
    const bucket = out.get(key);
    if (bucket) bucket.push(item);
    else out.set(key, [item]);
  }
  return out;
}

/**
 * How many of a row's own fields its [print] reads, and how much of each.
 *
 * Enough to tell the rows of one list apart, which is all a bucket is for: a
 * row off the wire leads with what identifies it — an id, a key, an instant,
 * each well under sixty-four characters (a uuid is 36) — and eight fields is
 * past every row's identity in this protocol. A collision costs one deep
 * comparison, never a wrong answer.
 */
const PRINT_FIELDS = 8;
const PRINT_CHARS = 64;

/**
 * Which bucket a value is looked for in: a SHALLOW print of it.
 *
 * A BUCKET, NOT A VERDICT: two deep-equal values always print alike, and a
 * match is confirmed by the deep comparison, so two values that print alike
 * and differ are told apart there. SHALLOW AND SHORT because it is taken of
 * every row of a list each time anything in that list moves, and a row may be
 * large — a phase record carries its whole prompts and response, and a full
 * print of each would be megabytes of string built to find one new row at the
 * top. Not `JSON.stringify` for the same reason, and because that reads a
 * `Map` as `{}` and walks into a React element's fiber until it meets a cycle.
 */
function print(value: object): string {
  if (Array.isArray(value)) return `[${value.length}`;
  const parts: string[] = [];
  for (const key of Object.keys(value).slice(0, PRINT_FIELDS)) {
    const field: unknown = (value as Record<string, unknown>)[key];
    let shown: string;
    if (field !== null && typeof field === "object") {
      shown = Array.isArray(field) ? `[${field.length}` : "{";
    } else {
      shown = `${(typeof field).charAt(0)}${String(field).slice(0, PRINT_CHARS)}`;
    }
    parts.push(`${JSON.stringify(key)}=${shown}`);
  }
  return parts.join(",");
}
