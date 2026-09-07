/**
 * Completing a `${NAME}` from the company's sealed entries.
 *
 * A field here takes either a value or a reference to one, and the reference
 * has to be typed exactly: a name off by a character resolves to nothing,
 * which reads as configured on every surface while the route it feeds refuses
 * every delivery. Nothing on the form knew the names, so the only way to get
 * one right was to open the Secrets screen in another tab and copy it across.
 */

/** Where a reference is being typed, and what has been typed of it. */
export interface Typing {
  /** Index of the `$` that opens it. */
  start: number;
  /** What follows the `$`, with any `{` and `}` stripped. */
  query: string;
}

/**
 * referenceAt finds the `${…}` under the caret, or nothing.
 *
 * IT SCANS BACK FROM THE CARET rather than matching the whole value, because
 * a field can hold a reference among other text and the one being edited is
 * the one being completed. The scan stops at whitespace and at a `}`: the
 * first because no name contains one, the second because a reference that is
 * already closed is finished, and offering to complete it again would replace
 * a name somebody had chosen.
 */
export function referenceAt(value: string, caret: number): Typing | null {
  const head = value.slice(0, caret);
  for (let i = head.length - 1; i >= 0; i--) {
    const ch = head.charAt(i);
    if (ch === "$") {
      const rest = head.slice(i + 1);
      // `${` and `$` both open one. Anything else after the `$` is a
      // literal dollar in a value, which is not a reference.
      const name = rest.startsWith("{") ? rest.slice(1) : rest;
      if (!/^[A-Za-z0-9_]*$/.test(name)) return null;
      return { start: i, query: name };
    }
    if (/\s/.test(ch) || ch === "}") return null;
  }
  return null;
}

/**
 * rank orders the names by how closely they match what has been typed.
 *
 * Three tiers, and the order is the order somebody means them in: a name that
 * STARTS with what was typed, then one that CONTAINS it, then one whose
 * letters appear in order. Case is ignored because a name is upper snake and
 * nobody holds shift to filter a list.
 *
 * An empty query keeps every name, in the order it arrived: the point of
 * typing `$` alone is to see what there is.
 */
export function rank(names: string[], query: string): string[] {
  const q = query.toLowerCase();
  if (q === "") return [...names];
  const scored: { name: string; tier: number }[] = [];
  for (const name of names) {
    const lower = name.toLowerCase();
    if (lower.startsWith(q)) {
      scored.push({ name, tier: 0 });
    } else if (lower.includes(q)) {
      scored.push({ name, tier: 1 });
    } else if (subsequence(lower, q)) {
      scored.push({ name, tier: 2 });
    }
  }
  // STABLE WITHIN A TIER, so a list does not reshuffle under somebody's
  // finger as they type a character that changes nothing about the order.
  return scored
    .map((s, i) => ({ ...s, i }))
    .sort((a, b) => a.tier - b.tier || a.i - b.i)
    .map((s) => s.name);
}

/** subsequence reports whether every character of q appears in order in s. */
function subsequence(s: string, q: string): boolean {
  let at = 0;
  for (const ch of q) {
    at = s.indexOf(ch, at);
    if (at < 0) return false;
    at += 1;
  }
  return true;
}

/**
 * complete writes the chosen name into the value as a whole `${NAME}`.
 *
 * CLOSED BRACES, always, and the caret lands after them: the engine resolves
 * a reference only when it is whole, and a half-written one is a literal that
 * reaches a vendor as the characters somebody typed.
 */
export function complete(
  value: string,
  caret: number,
  typing: Typing,
  name: string,
): { value: string; caret: number } {
  const written = "${" + name + "}";
  const head = value.slice(0, typing.start);
  // What follows the caret is kept, minus a `}` the person had already
  // typed: leaving it would close the reference twice.
  const tail = value.slice(caret).replace(/^\}/, "");
  return { value: head + written + tail, caret: head.length + written.length };
}
