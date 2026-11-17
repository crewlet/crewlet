/**
 * YAML; a state carries an open quoted scalar (`'` or `"`), or a block
 * scalar's body with the indent it must pass (`b4`).
 */
import { Tokens, done, isSpace, skipSpace, step, word, words } from "./scan.ts";
import type { Grammar, Kind, Line, State } from "./types.ts";

const CONSTANTS = words(
  "true false null yes no on off ~ True False Null Yes No On Off TRUE FALSE NULL YES NO ON OFF",
);
const NUMBER =
  /[-+]?(?:0x[\da-fA-F_]+|0o[0-7_]+|\.inf|\.nan|\d[\d_]*(?:\.\d*)?(?:[eE][-+]?\d+)?|\.\d+(?:[eE][-+]?\d+)?)/y;
const DATE = /\d{4}-\d\d-\d\d(?:[Tt ]\d\d?:\d\d:\d\d(?:\.\d+)?(?:Z|[-+]\d\d?(?::\d\d)?)?)?/y;
const BLOCK = /[|>][-+0-9]*/y;
/** The deepest block scalar indent carried. */
const MAX_INDENT = 999;

/** Whether `pattern` matches all of `w`. */
const whole = (pattern: RegExp, w: string) => word(w, 0, pattern) === w.length && w.length > 0;

/** A quoted scalar's rest from `i`: the end of its close, or -1 when the line ends first. */
function closeOf(text: string, i: number, quote: string): number {
  while (i < text.length) {
    const ch = text[i];
    if (quote === '"' && ch === "\\") i += 2;
    else if (ch === quote) {
      if (quote === "'" && text[i + 1] === "'") i += 2;
      else return i + 1;
    } else i++;
  }
  return -1;
}

export const yaml: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    const indent = skipSpace(text, 0);
    let i = 0;
    // A BLOCK SCALAR'S BODY: every line indented past its key, and every blank line.
    if (state[0] === "b") {
      const at = Number(state.slice(1));
      if (String(at) === state.slice(1) && at <= MAX_INDENT) {
        if (indent === n || indent > at) {
          out.push(n, "string");
          return done(out, state);
        }
      }
    } else if (state === "'" || state === '"') {
      const end = closeOf(text, 0, state);
      if (end < 0) {
        out.push(n, "string");
        return done(out, state);
      }
      out.push((i = end), "string");
    }
    let open = "";
    let flow = 0;
    let start = true;
    while (i < n) {
      const ch = text[i]!;
      const prev = text[i - 1];
      if (isSpace(ch)) {
        out.push((i = skipSpace(text, i)), null);
        continue;
      }
      if (ch === "#" && (prev === undefined || isSpace(prev))) {
        out.push((i = n), "comment");
        break;
      }
      const next = text[i + 1];
      const spaced = next === undefined || isSpace(next);
      if (
        i === 0 &&
        (text.startsWith("---", 0) || text.startsWith("...", 0)) &&
        (n === 3 || isSpace(text[3]))
      ) {
        out.push((i = 3), "punctuation");
      } else if ((ch === "-" || ch === "?") && spaced && start) {
        out.push((i += 1), "punctuation");
      } else if (ch === ":" && spaced) {
        out.push((i += 1), "punctuation");
        start = true;
      } else if ("[]{},".includes(ch)) {
        flow += ch === "[" || ch === "{" ? 1 : ch === "]" || ch === "}" ? -1 : 0;
        out.push((i += 1), "punctuation");
        start = true;
      } else if (ch === "'" || ch === '"') {
        const end = closeOf(text, i + 1, ch);
        if (end < 0) {
          out.push((i = n), "string");
          open = ch;
        } else {
          const key = text[skipSpace(text, end)] === ":";
          out.push((i = end), key ? "function" : "string");
        }
        start = false;
      } else if ((ch === "&" || ch === "*" || ch === "!") && !spaced) {
        let end = i + 1;
        while (end < n && !isSpace(text[end]) && !"[]{},".includes(text[end]!))
          end = step(text, end);
        out.push((i = end), "type");
      } else if ((ch === "|" || ch === ">") && start) {
        const end = word(text, i, BLOCK);
        const after = skipSpace(text, end);
        if (after === n || text[after] === "#") {
          out.push((i = end), "punctuation");
          if (indent <= MAX_INDENT) open = `b${indent}`;
        } else out.push((i += 1), "punctuation");
      } else {
        // A PLAIN SCALAR runs to `: `, ` #`, the line's end, or in a flow to its punctuation.
        let end = i;
        while (end < n) {
          const c = text[end]!;
          if (c === ":" && (end + 1 === n || isSpace(text[end + 1]))) break;
          if (c === "#" && isSpace(text[end - 1])) break;
          if (flow > 0 && "[]{},".includes(c)) break;
          end = step(text, end);
        }
        if (end === i) end = step(text, i);
        let stop = end;
        while (stop > i && isSpace(text[stop - 1])) stop--;
        const w = text.slice(i, stop);
        const key = text[end] === ":";
        let kind: Kind | null = null;
        if (key) kind = "function";
        else if (CONSTANTS.has(w)) kind = "keyword";
        else if (whole(NUMBER, w) || whole(DATE, w)) kind = "number";
        out.push(stop, kind);
        out.push((i = end), null);
        start = false;
      }
    }
    return done(out, open);
  },
};
