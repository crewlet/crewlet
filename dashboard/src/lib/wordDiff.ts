/**
 * Where an edited line changed: the one run a diff underlines.
 *
 * A line diff says WHICH lines changed; a reader comparing a removed line with
 * the line that replaced it still has to find the difference by eye, and on a
 * long line that is a sentence read twice. So when a removed line and an added
 * line are one line edited, the part of each that differs is underlined.
 *
 * ONE RUN A LINE, NEVER A CONFETTI OF THEM. The run is what lies between the
 * longest stretch the two lines share at their START and the longest they
 * share at their END. A word-by-word diff of the two (a longest common
 * subsequence over words) would mark every word the edit moved past, which
 * makes a small edit prettier and a rewritten paragraph unreadable: twenty
 * scattered marks over a sentence that is simply different. With one run, a
 * small edit is pinpointed and a rewrite is one plain underline at most.
 *
 * WHOLE TOKENS, so an underline starts and ends where a reader's eye does: a
 * run of letters, digits and joining marks is one token, a run of spaces is
 * one, and any other character is a token by itself — except an ideograph or
 * kana, each of which is its own word, since Chinese and Japanese put no space
 * between words and a sentence of them would otherwise be one token.
 *
 * A PAIR THAT SHARES NO WORD IS TWO LINES, NOT ONE LINE EDITED. When all the
 * two lines have in common at their ends is spaces and punctuation (an
 * indentation, a closing brace, a full stop), the "changed run" is the whole
 * of both, and underlining it says nothing the line's own mark has not said.
 * Nothing is underlined then.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives: what is
 * underlined is held by these tests rather than by a screen.
 */

/** A stretch of a line's text, as offsets into it: `start` inclusive, `end` exclusive. */
export interface Run {
  start: number;
  end: number;
}

/** What to underline on each line of a pair, or nothing on a side that only lost or gained. */
export interface ChangedRuns {
  before: Run | null;
  after: Run | null;
}

/** Nothing to underline on either side. */
const NONE: ChangedRuns = { before: null, after: null };

interface Token {
  text: string;
  start: number;
  end: number;
  /** Whether the token is a word: holds a letter or a digit. */
  word: boolean;
}

/**
 * One token: a single ideograph or kana; else a run of letters, digits,
 * joining marks and underscores (one stopping before any ideograph); else a
 * run of white space; else any one character.
 */
const TOKEN =
  /[\p{sc=Han}\p{sc=Hiragana}\p{sc=Katakana}]|(?:(?![\p{sc=Han}\p{sc=Hiragana}\p{sc=Katakana}])[\p{L}\p{N}\p{M}_])+|\s+|[^]/gu;

/** A token that carries meaning rather than spacing or punctuation. */
const WORD = /[\p{L}\p{N}]/u;

/** A line's tokens, in order, covering every character of it. */
export function tokens(text: string): Token[] {
  const out: Token[] = [];
  for (const m of text.matchAll(TOKEN)) {
    const start = m.index;
    out.push({ text: m[0], start, end: start + m[0].length, word: WORD.test(m[0]) });
  }
  return out;
}

/** The tokens `[from, to)` of a line as one run of its text, or null when there are none. */
function span(all: Token[], from: number, to: number): Run | null {
  if (from >= to) return null;
  return { start: all[from]!.start, end: all[to - 1]!.end };
}

/**
 * The run of `before` and the run of `after` that differ, when the two are one
 * line edited (see the package note): everything between their shared start
 * and their shared end, in whole tokens. A side whose run is empty — the text
 * was only inserted into the other — underlines nothing.
 */
export function changedRuns(before: string, after: string): ChangedRuns {
  const a = tokens(before);
  const b = tokens(after);
  let head = 0;
  while (head < a.length && head < b.length && a[head]!.text === b[head]!.text) head++;
  // The shared end stops where the shared start began, so a token is never
  // counted on both: "aa" against "aaa" shares two letters, not four.
  let tail = 0;
  while (
    tail < a.length - head &&
    tail < b.length - head &&
    a[a.length - 1 - tail]!.text === b[b.length - 1 - tail]!.text
  ) {
    tail++;
  }
  const shared = a.slice(0, head).concat(a.slice(a.length - tail));
  if (!shared.some((t) => t.word)) return NONE;
  return { before: span(a, head, a.length - tail), after: span(b, head, b.length - tail) };
}
