/**
 * Markdown; a state carries an open fence with its character and length
 * (`f\`4`), or an open HTML comment (`c`).
 */
import { Tokens, done, isDigit, isSpace, skipSpace, until } from "./scan.ts";
import type { Grammar, Line, State } from "./types.ts";

/** The longest fence carried. */
const MAX_FENCE = 99;

/** The end of the run of `ch` at `i`. */
function run(text: string, i: number, ch: string): number {
  while (text[i] === ch) i++;
  return i;
}

/** Inline code, links' targets and comments from `i`; the state the line leaves. */
function inline(out: Tokens, text: string, i: number): State {
  const n = text.length;
  // EVERY BACKTICK RUN BY ITS LENGTH, read once: a run closes at the next of its length.
  const runs = new Map<number, number[]>();
  for (let j = text.indexOf("`", i); j >= 0;) {
    const end = run(text, j, "`");
    const starts = runs.get(end - j) ?? [];
    runs.set(end - j, starts);
    starts.push(j);
    j = text.indexOf("`", end);
  }
  const next = new Map<number, number>();
  while (i < n) {
    const ch = text[i]!;
    if (ch === "`") {
      const end = run(text, i, "`");
      const k = end - i;
      const starts = runs.get(k) ?? [];
      let at = next.get(k) ?? 0;
      while (at < starts.length && starts[at]! <= i) at++;
      next.set(k, at + 1);
      if (at < starts.length) out.push((i = starts[at]! + k), "string");
      else out.push((i = end), "punctuation");
    } else if (text.startsWith("<!--", i)) {
      const end = until(text, i + 4, "-->");
      out.push(end < 0 ? n : end, "comment");
      if (end < 0) return "c";
      i = end;
    } else if (ch === "]" && text[i + 1] === "(") {
      out.push(i + 2, "punctuation");
      const end = until(text, i + 2, ")");
      out.push(end < 0 ? n : end - 1, "string");
      if (end >= 0) out.push(end, "punctuation");
      i = end < 0 ? n : end;
    } else if (ch === "[" || ch === "]" || ch === "|") {
      out.push((i += 1), "punctuation");
    } else {
      let end = i + 1;
      while (end < n && !"`<[]|".includes(text[end]!)) end++;
      out.push((i = end), null);
    }
  }
  return "";
}

export const markdown: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    const indent = skipSpace(text, 0);
    // INSIDE A FENCE: every line string, to a fence of its character at least as long.
    const fence = state[0] === "f" && (state[1] === "`" || state[1] === "~");
    const length = Number(state.slice(2));
    if (fence && String(length) === state.slice(2) && length >= 3 && length <= MAX_FENCE) {
      const end = run(text, indent, state[1]!);
      if (indent <= 3 && end - indent >= length && skipSpace(text, end) === n) {
        out.push(end, "punctuation");
        out.push(n, null);
        return done(out, "");
      }
      out.push(n, "string");
      return done(out, state);
    }
    let i = 0;
    if (state === "c") {
      const end = until(text, 0, "-->");
      out.push(end < 0 ? n : end, "comment");
      if (end < 0) return done(out, "c");
      i = end;
      return done(out, inline(out, text, i));
    }
    out.push(indent, null);
    i = indent;
    const ch = text[i];
    if (indent <= 3 && (ch === "`" || ch === "~")) {
      const end = run(text, i, ch);
      const info = text.slice(end);
      if (end - i >= 3 && (ch === "~" || !info.includes("`"))) {
        out.push(end, "punctuation");
        out.push(n, null);
        return done(out, end - i <= MAX_FENCE ? `f${ch}${end - i}` : "");
      }
    }
    if (indent <= 3 && ch === "#") {
      const end = run(text, i, "#");
      if (end - i <= 6 && (end === n || isSpace(text[end]))) {
        out.push(n, "keyword");
        return done(out, "");
      }
    }
    if (indent <= 3 && (ch === "-" || ch === "*" || ch === "_")) {
      let count = 0;
      let only = true;
      for (let j = i; j < n; j++) {
        if (text[j] === ch) count++;
        else if (!isSpace(text[j])) only = false;
      }
      if (only && count >= 3) {
        out.push(n, "punctuation");
        return done(out, "");
      }
    }
    // A QUOTE'S MARKERS, then a list item's.
    while (text[i] === ">") {
      out.push((i += 1), "punctuation");
      out.push((i = skipSpace(text, i)), null);
    }
    const c = text[i];
    if ((c === "-" || c === "*" || c === "+") && (i + 1 === n || isSpace(text[i + 1]))) {
      out.push((i += 1), "punctuation");
    } else if (isDigit(c)) {
      let j = i;
      while (isDigit(text[j]) && j - i < 9) j++;
      if ((text[j] === "." || text[j] === ")") && (j + 1 === n || isSpace(text[j + 1]))) {
        out.push((i = j + 1), "punctuation");
      }
    }
    return done(out, inline(out, text, i));
  },
};
