/**
 * Moving one entry of a priority list, as the arithmetic a drag or a key
 * press comes down to.
 *
 * # The whole list is written, and most of it is not on screen
 *
 * `set_priorities` takes the WHOLE list, because an order is a statement about
 * every entry at once — so a reorder is always a new whole list, and the list
 * it is made from is the person's own stored one (`work_person.priorities`),
 * never the rows My work drew. Those are a view of it: the OPEN entries only,
 * and at most twenty of them. A reorder written from the rows would drop every
 * finished entry and every open one past the twentieth — a list of twenty-six
 * rewritten as twenty by a press that moved one row.
 *
 * So a move is stated RELATIVE TO AN ENTRY THE READER CAN SEE — before this
 * one, after that one — and applied to the stored list, where everything the
 * reader cannot see keeps its place around the one that moved.
 *
 * PURE FUNCTIONS OVER VALUES, so the rule is tested without a screen.
 */

/** Where the moved entry goes: immediately before one entry, or after it. */
export type Place = { before: string } | { after: string };

/**
 * `list` with `moved` taken out and put back at `place`, or null where the
 * move names something the list does not hold.
 *
 * NULL RATHER THAN THE LIST UNCHANGED, so a caller cannot send a write that
 * only looks like one: an anchor missing from the stored list is a screen out
 * of step with the record, and the answer is to read again, not to write the
 * order back as it was.
 *
 * EVERY COPY OF THE MOVED ENTRY GOES. A person record is also written whole by
 * paths that do not deduplicate, and the engine counts a doubled entry once —
 * so the move keeps exactly one, at the place the reader chose.
 */
export function moveTo(list: readonly string[], moved: string, place: Place): string[] | null {
  if (!list.includes(moved)) return null;
  const anchor = "before" in place ? place.before : place.after;
  if (anchor === moved) return null;
  const rest = list.filter((id) => id !== moved);
  const at = rest.indexOf(anchor);
  if (at < 0) return null;
  const into = "before" in place ? at : at + 1;
  return [...rest.slice(0, into), moved, ...rest.slice(into)];
}

/** Whether two lists hold the same entries in the same order. */
export function sameOrder(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((id, i) => id === b[i]);
}

/**
 * Where a row dropped over the drawn list lands: above the row the pointer was
 * over, or — `above` null — after the last one drawn.
 *
 * NULL FOR A DROP THAT MOVES NOTHING: onto itself, or onto the gap directly
 * beneath itself, which is where it already is. A write for either would be a
 * reorder that changed nothing and still stamped the list as touched.
 *
 * AFTER THE LAST ROW DRAWN, not at the end of the stored list: entries past
 * the twentieth are not drawn, and a row sent to the very end would vanish
 * behind them — dropped at the foot of the list, it lands at the foot of what
 * the reader can see.
 */
export function dropPlace(
  drawn: readonly string[],
  moved: string,
  above: string | null,
): Place | null {
  const at = drawn.indexOf(moved);
  if (at < 0) return null;
  if (above === null) {
    const last = drawn.filter((id) => id !== moved).at(-1);
    if (last === undefined || at === drawn.length - 1) return null;
    return { after: last };
  }
  if (above === moved || drawn[at + 1] === above) return null;
  return { before: above };
}

/**
 * One place up or down from the keyboard — `Alt` with an arrow on a focused
 * row, the same gesture the board's cards take — or null at either end.
 */
export function stepPlace(drawn: readonly string[], moved: string, up: boolean): Place | null {
  const at = drawn.indexOf(moved);
  if (at < 0) return null;
  if (up) return at > 0 ? { before: drawn[at - 1]! } : null;
  return at < drawn.length - 1 ? { after: drawn[at + 1]! } : null;
}
