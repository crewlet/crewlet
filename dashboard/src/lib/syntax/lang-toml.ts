/** TOML; a state carries an open multi-line string (`D` for `"""`, `L` for `'''`). */
import { Tokens, done, isSpace, isWordChar, skipSpace, step, until, word } from "./scan.ts";
import type { Grammar, Line, State } from "./types.ts";

const BARE = /[A-Za-z0-9_-]+/y;
const DATE = /\d{4}-\d\d-\d\d(?:[Tt ]\d\d:\d\d:\d\d(?:\.\d+)?(?:[Zz]|[-+]\d\d:\d\d)?)?/y;
const TIME = /\d\d:\d\d:\d\d(?:\.\d+)?/y;
const NUMBER =
  /[-+]?(?:0x[\da-fA-F_]+|0o[0-7_]+|0b[01_]+|inf|nan|\d[\d_]*(?:\.\d[\d_]*)?(?:[eE][-+]?\d[\d_]*)?)/y;
const MULTI: Record<string, string> = { D: '"""', L: "'''" };

/** The longest of the patterns matched at `i`, or `i`. */
const longest = (text: string, i: number, ...patterns: RegExp[]) =>
  Math.max(...patterns.map((p) => word(text, i, p)));

export const toml: Grammar = {
  line(text: string, state: State): Line {
    const out = new Tokens();
    const n = text.length;
    let i = 0;
    let open = "";
    /** The rest of a multi-line string from `j`; leaves it open at the line's end. */
    const multi = (j: number, which: string): number => {
      const end = until(text, j, MULTI[which]!, which === "D");
      if (end < 0) open = which;
      out.push(end < 0 ? n : end, "string");
      return end < 0 ? n : end;
    };
    if (state === "D" || state === "L") i = multi(0, state);
    if (open) return done(out, open);
    i = Math.max(i, skipSpace(text, i));
    out.push(i, null);
    // A TABLE'S HEADER, [a.b] or [[bin]]: its names are types.
    if (i === skipSpace(text, 0) && text[i] === "[") {
      const close = text[i + 1] === "[" ? "]]" : "]";
      const end = until(text, i, close);
      if (end > 0) {
        out.push(i + close.length, "punctuation");
        for (let j = i + close.length; j < end - close.length;) {
          const c = text[j]!;
          const to =
            c === "." || isSpace(c)
              ? j + 1
              : c === '"' || c === "'"
                ? until(text, j + 1, c)
                : word(text, j, BARE);
          const next = to > j ? Math.min(to, end - close.length) : step(text, j);
          out.push(next, c === "." ? "punctuation" : isSpace(c) ? null : "type");
          j = next;
        }
        out.push((i = end), "punctuation");
      }
    }
    let keyAt = true;
    let brackets = "";
    while (i < n) {
      const ch = text[i]!;
      if (isSpace(ch)) {
        out.push((i = skipSpace(text, i)), null);
      } else if (ch === "#") {
        out.push((i = n), "comment");
      } else if (text.startsWith('"""', i) || text.startsWith("'''", i)) {
        i = multi(i + 3, ch === '"' ? "D" : "L");
        keyAt = false;
      } else if (ch === '"' || ch === "'") {
        const end = until(text, i + 1, ch, ch === '"');
        const to = end < 0 ? n : end;
        const after = text[skipSpace(text, to)];
        out.push((i = to), keyAt && (after === "=" || after === ".") ? "function" : "string");
      } else if ("=.,[]{}".includes(ch)) {
        if (ch === "[" || ch === "{") brackets += ch;
        if (ch === "]" || ch === "}") brackets = brackets.slice(0, -1);
        if (ch === "{") keyAt = true;
        else if (ch === ",") keyAt = brackets.endsWith("{");
        else if (ch !== ".") keyAt = false;
        out.push((i += 1), "punctuation");
      } else {
        const bare = word(text, i, BARE);
        const after = text[skipSpace(text, bare)];
        if (bare > i && keyAt && (after === "=" || after === ".")) {
          out.push((i = bare), "function");
          continue;
        }
        keyAt = false;
        const literal = longest(text, i, DATE, TIME, NUMBER);
        if (literal > i && !isWordChar(text[literal])) out.push((i = literal), "number");
        else if (bare > i) {
          const w = text.slice(i, bare);
          out.push((i = bare), w === "true" || w === "false" ? "keyword" : null);
        } else out.push((i = step(text, i)), null);
      }
    }
    return done(out, open);
  },
};
