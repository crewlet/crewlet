/**
 * Where a run of prose spells out an address or a path.
 *
 * A word (a run of text between spaces, its surrounding brackets, quotes and
 * stops aside) that is a bare address (a scheme's `://`, a `www.` host) or a
 * path (one that starts with `/`, `./` or `../`, or has a slash after a part
 * that is no key) is something the writer wrote out, and a key or a name in
 * it (https://x.test/GOP-11, /worklet/GOP-11, worklet/GOP-11) is part of it,
 * never a mention. "GOP-10/GOP-11" is two keys, so a slash between keys
 * delimits rather than making a path.
 *
 * ONE RULE for both readers of prose that look for keys: the Dashboard
 * Assistant's record chips (`app/assistant/chunk/mentions.ts`) and the inline
 * tokens every Markdown surface draws (`lib/inlineTokens.ts`).
 */

/** A word of the run: what lies between spaces. */
const WORD = /\S+/gu;
/** What opens around a word without being part of it: brackets and quotes. */
const OPENS = new Set(['"', "'", "“", "‘", "(", "[", "{", "<"]);
/** What closes around a word without being part of it: brackets, quotes, stops. */
const CLOSES = new Set([".", ",", ";", ":", "!", "?", '"', "'", "”", "’", ")", "]", "}", ">"]);

/**
 * A word without what surrounds it.
 *
 * TWO INDEX WALKS, NEVER A PATTERN: the pattern this replaced (a lazy middle
 * between two runs of the same characters) retried the closing run from
 * every place the middle could end, so a word of 20,000 brackets and a letter
 * took most of a second, on every render of every reader of it.
 */
function inner(word: string): string {
  let from = 0;
  let to = word.length;
  while (from < to && OPENS.has(word[from]!)) from++;
  while (to > from && CLOSES.has(word[to - 1]!)) to--;
  return word.slice(from, to);
}
/** A bare address: a scheme's `://` or a `www.` host. */
const ADDRESS = /^(?:[a-z][a-z0-9+.-]*:\/\/|www\.)/iu;
/** A part of a word a key could be: a capital, then capitals and digits, then a number. */
const KEY_SHAPED = /^\p{Lu}[\p{Lu}\p{N}]*(?:-\p{N}+)?$/u;

/** Whether a word is an address or a path. */
function writtenOut(word: string): boolean {
  if (ADDRESS.test(word) || /^\.{0,2}\//u.test(word)) return true;
  const parts = word.split("/");
  return parts.slice(0, -1).some((part) => !KEY_SHAPED.test(part));
}

/** Where each address or path in `text` lies, as [start, end). */
export function writtenOutSpans(text: string): Array<readonly [number, number]> {
  const out: Array<readonly [number, number]> = [];
  for (const m of text.matchAll(WORD)) {
    const word = inner(m[0]);
    if (word !== "" && writtenOut(word)) out.push([m.index, m.index + m[0].length]);
  }
  return out;
}

/** Whether [at, end) overlaps any of `spans`. */
export function overlapsAny(
  spans: ReadonlyArray<readonly [number, number]>,
  at: number,
  end: number,
): boolean {
  return spans.some(([from, to]) => at < to && end > from);
}
